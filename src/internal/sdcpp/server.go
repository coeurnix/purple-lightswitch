package sdcpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"purple-lightswitch/internal/bootstrap"
)

type LogFunc func(string)

type Server struct {
	assets bootstrap.RuntimeAssets
	logf   LogFunc

	mu     sync.Mutex
	cmd    *exec.Cmd
	done   chan error
	port   int
	client *http.Client
	caps   Capabilities
}

type Capabilities struct {
	Defaults GenerationDefaults `json:"defaults"`
	Features Features           `json:"features"`
	Limits   Limits             `json:"limits"`
}

type GenerationDefaults struct {
	Width           int                 `json:"width"`
	Height          int                 `json:"height"`
	Strength        float64             `json:"strength"`
	SampleParams    SampleParams        `json:"sample_params"`
	VAETilingParams VAETilingParameters `json:"vae_tiling_params"`
}

type Features struct {
	CancelQueued     bool `json:"cancel_queued"`
	CancelGenerating bool `json:"cancel_generating"`
	InitImage        bool `json:"init_image"`
	VAETiling        bool `json:"vae_tiling"`
}

type Limits struct {
	MaxQueueSize int `json:"max_queue_size"`
	MinWidth     int `json:"min_width"`
	MaxWidth     int `json:"max_width"`
	MinHeight    int `json:"min_height"`
	MaxHeight    int `json:"max_height"`
}

type ImageGenRequest struct {
	Prompt          string              `json:"prompt"`
	NegativePrompt  string              `json:"negative_prompt,omitempty"`
	InitImage       string              `json:"init_image,omitempty"`
	Width           int                 `json:"width"`
	Height          int                 `json:"height"`
	Seed            int                 `json:"seed"`
	Strength        float64             `json:"strength"`
	OutputFormat    string              `json:"output_format"`
	SampleParams    SampleParams        `json:"sample_params"`
	VAETilingParams VAETilingParameters `json:"vae_tiling_params,omitempty"`
}

type SampleParams struct {
	SampleSteps int                `json:"sample_steps"`
	Guidance    GuidanceParameters `json:"guidance"`
}

type GuidanceParameters struct {
	TxtCFG            float64 `json:"txt_cfg"`
	DistilledGuidance float64 `json:"distilled_guidance"`
}

type VAETilingParameters struct {
	Enabled       bool    `json:"enabled"`
	TargetOverlap float64 `json:"target_overlap,omitempty"`
}

type SubmitResponse struct {
	ID      string `json:"id"`
	PollURL string `json:"poll_url"`
	Status  string `json:"status"`
}

type JobStatus struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	QueuePosition int        `json:"queue_position"`
	Error         *JobError  `json:"error"`
	Result        *JobResult `json:"result"`
	Created       int64      `json:"created"`
	Started       int64      `json:"started"`
	Completed     int64      `json:"completed"`
}

type JobError struct {
	Message string `json:"message"`
}

type JobResult struct {
	OutputFormat string        `json:"output_format"`
	Images       []ResultImage `json:"images"`
}

type ResultImage struct {
	Index   int    `json:"index"`
	B64JSON string `json:"b64_json"`
}

func New(assets bootstrap.RuntimeAssets, logf LogFunc) *Server {
	if logf == nil {
		logf = func(string) {}
	}
	return &Server{
		assets: assets,
		logf:   logf,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.cmd != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	port, err := randomLoopbackPort()
	if err != nil {
		return err
	}

	args := []string{
		"--listen-ip", "127.0.0.1",
		"--listen-port", strconv.Itoa(port),
		"--diffusion-model", s.assets.ModelPath,
		"--vae", s.assets.VAEPath,
		"--llm", s.assets.LLMPath,
		"--offload-to-cpu",
		"--cfg-scale", "1.0",
		"--guidance", "4.2",
		"--steps", "7",
		"--diffusion-fa",
		"--vae-conv-direct",
		"-v",
	}
	if strings.Contains(s.assets.BinTarget, "cuda") {
		args = append(args, "--rng", "cuda", "--sampler-rng", "cuda")
	}

	cmd := exec.CommandContext(ctx, s.commandPath(), args...)
	cmd.Dir = s.assets.BinDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	s.mu.Lock()
	s.cmd = cmd
	s.done = make(chan error, 1)
	s.port = port
	s.mu.Unlock()

	go s.readLogs(stdout)
	go s.readLogs(stderr)
	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		s.cmd = nil
		done := s.done
		s.done = nil
		s.mu.Unlock()
		if done != nil {
			done <- err
		}
	}()

	caps, err := s.waitForReady(ctx)
	if err != nil {
		_ = s.Close()
		return err
	}
	s.mu.Lock()
	s.caps = caps
	s.mu.Unlock()
	s.logf("sd-server ready")
	return nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	cmd := s.cmd
	done := s.done
	s.cmd = nil
	s.done = nil
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		_ = killProcessTreeWindows(cmd.Process.Pid)
		_ = cmd.Process.Kill()
		waitDone(done, 1500*time.Millisecond)
		return nil
	}
	_ = cmd.Process.Signal(os.Interrupt)
	if waitDone(done, 2*time.Second) {
		return nil
	}
	_ = cmd.Process.Kill()
	waitDone(done, 1500*time.Millisecond)
	return nil
}

func (s *Server) Restart(ctx context.Context) error {
	s.logf("restarting sd-server")
	if err := s.Close(); err != nil {
		return err
	}
	return s.Start(ctx)
}

func (s *Server) Capabilities() Capabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

func (s *Server) Submit(ctx context.Context, req ImageGenRequest) (SubmitResponse, error) {
	var response SubmitResponse
	if err := s.doJSON(ctx, http.MethodPost, "/sdcpp/v1/img_gen", req, &response); err != nil {
		return SubmitResponse{}, err
	}
	return response, nil
}

func (s *Server) PollJob(ctx context.Context, jobID string) (JobStatus, error) {
	var status JobStatus
	if err := s.doJSON(ctx, http.MethodGet, "/sdcpp/v1/jobs/"+jobID, nil, &status); err != nil {
		return JobStatus{}, err
	}
	return status, nil
}

func (s *Server) CancelJob(ctx context.Context, jobID string) error {
	return s.doJSON(ctx, http.MethodPost, "/sdcpp/v1/jobs/"+jobID+"/cancel", map[string]any{}, nil)
}

func DecodeFirstImage(result *JobResult) ([]byte, error) {
	if result == nil || len(result.Images) == 0 {
		return nil, errors.New("sd-server returned no images")
	}
	return base64.StdEncoding.DecodeString(result.Images[0].B64JSON)
}

func (s *Server) waitForReady(ctx context.Context) (Capabilities, error) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return Capabilities{}, ctx.Err()
		default:
		}

		var caps Capabilities
		if err := s.doJSON(ctx, http.MethodGet, "/sdcpp/v1/capabilities", nil, &caps); err == nil {
			return caps, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return Capabilities{}, errors.New("sd-server did not become ready in time")
}

func (s *Server) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, s.baseURL()+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if len(data) == 0 {
			return fmt.Errorf("sd-server request failed: %s", resp.Status)
		}
		return fmt.Errorf("sd-server request failed: %s", strings.TrimSpace(string(data)))
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (s *Server) baseURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return "http://127.0.0.1:" + strconv.Itoa(s.port)
}

func (s *Server) commandPath() string {
	name := "sd-server"
	if strings.EqualFold(filepath.Ext(s.assets.CLIPath), ".exe") {
		name += ".exe"
	}
	return filepath.Join(s.assets.BinDir, name)
}

func (s *Server) readLogs(reader io.Reader) {
	buffer := bufio.NewReader(reader)
	var current bytes.Buffer
	flush := func() {
		line := strings.TrimSpace(current.String())
		current.Reset()
		if line == "" {
			return
		}
		s.logf(line)
	}

	for {
		b, err := buffer.ReadByte()
		if errors.Is(err, io.EOF) {
			flush()
			return
		}
		if err != nil {
			return
		}
		if b == '\r' || b == '\n' {
			flush()
			continue
		}
		current.WriteByte(b)
	}
}

func randomLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitDone(done <-chan error, timeout time.Duration) bool {
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func killProcessTreeWindows(pid int) error {
	cmd := exec.Command("taskkill.exe", "/T", "/F", "/PID", strconv.Itoa(pid))
	return cmd.Run()
}
