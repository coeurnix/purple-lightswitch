package runtime

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"purple-lightswitch/internal/bootstrap"
	"purple-lightswitch/internal/sdcpp"
)

type Config struct {
	ListenHost string
	Port       int
	Password   string
}

type Event struct {
	Time        time.Time
	Kind        string
	Message     string
	Connections int
	Queue       QueueStats
	Job         *JobSnapshot
}

type App struct {
	cfg     Config
	assets  bootstrap.RuntimeAssets
	sink    func(Event)
	dataDir string

	mgr    *manager
	sd     *sdcpp.Server
	server *http.Server

	listener net.Listener

	mu               sync.Mutex
	clients          map[string]map[*clientConn]struct{}
	disconnectTimers map[string]*time.Timer
	activeJobID      string
	activeProgress   renderProgressState
	jobStatuses      map[string]string
	shuttingDown     bool
}

type Preset struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Tagline        string  `json:"tagline"`
	Prompt         string  `json:"-"`
	NegativePrompt string  `json:"-"`
	Strength       float64 `json:"-"`
	Guidance       float64 `json:"-"`
	Steps          int     `json:"-"`
}

type Job struct {
	ID              string
	ClientID        string
	PresetID        string
	PresetName      string
	Status          string
	Error           string
	InputPath       string
	OutputPath      string
	OutputURL       string
	Prompt          string
	Negative        string
	Width           int
	Height          int
	Strength        float64
	Guidance        float64
	Steps           int
	ProgressCurrent int
	ProgressTotal   int
	ProgressPercent int
	ProgressPhase   string
	CreatedAt       time.Time
	StartedAt       *time.Time
	EndedAt         *time.Time
	cancel          context.CancelFunc
}

type JobSnapshot struct {
	ID              string     `json:"id"`
	ClientID        string     `json:"clientId"`
	PresetID        string     `json:"presetId"`
	PresetName      string     `json:"presetName"`
	Status          string     `json:"status"`
	Error           string     `json:"error,omitempty"`
	OutputURL       string     `json:"outputUrl,omitempty"`
	QueuePosition   int        `json:"queuePosition"`
	Width           int        `json:"width"`
	Height          int        `json:"height"`
	ProgressCurrent int        `json:"progressCurrent"`
	ProgressTotal   int        `json:"progressTotal"`
	ProgressPercent int        `json:"progressPercent"`
	ProgressPhase   string     `json:"progressPhase"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
}

type renderPhase string

const (
	phaseEncode   renderPhase = "vae"
	phaseBuffer   renderPhase = "buffer"
	phaseGenerate renderPhase = "generate"
)

type renderProgressState struct {
	Phase   renderPhase
	Current int
	Total   int
	Percent int
}

type clientConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

type helloMessage struct {
	Type             string   `json:"type"`
	ClientID         string   `json:"clientId"`
	Presets          []Preset `json:"presets"`
	RequiresPassword bool     `json:"requiresPassword"`
}

type jobsMessage struct {
	Type string        `json:"type"`
	Jobs []JobSnapshot `json:"jobs"`
}

type transformResponse struct {
	ClientID string      `json:"clientId"`
	Job      JobSnapshot `json:"job"`
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(_ *http.Request) bool { return true },
	}
	indexTemplate         = template.Must(template.ParseFS(webFS, "web/index.html"))
	renderProgressPattern = regexp.MustCompile(`\b(\d+)/(\d+)\b`)
)

func New(cfg Config, assets bootstrap.RuntimeAssets, sink func(Event)) *App {
	if cfg.ListenHost == "" {
		cfg.ListenHost = bootstrap.DefaultListenHost()
	}
	if sink == nil {
		sink = func(Event) {}
	}
	return &App{
		cfg:              cfg,
		assets:           assets,
		sink:             sink,
		dataDir:          filepath.Join(bootstrap.ProjectRoot(), "data", "jobs"),
		clients:          map[string]map[*clientConn]struct{}{},
		disconnectTimers: map[string]*time.Timer{},
		jobStatuses:      map[string]string{},
	}
}

func (a *App) Start(ctx context.Context) (string, error) {
	if err := os.MkdirAll(a.dataDir, 0o755); err != nil {
		return "", err
	}

	if a.assets.CLIPath != "" {
		a.sd = sdcpp.New(a.assets, a.handleSDLog)
		if err := a.sd.Start(ctx); err != nil {
			return "", err
		}
	}

	a.mgr = newManager(a.runJob, a.handleJobState, a.broadcastClientJobs)

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/app.css", a.handleCSS)
	mux.HandleFunc("/app.js", a.handleJS)
	mux.HandleFunc("/ws", a.handleWebsocket)
	mux.HandleFunc("/api/transform", a.handleTransform)
	mux.HandleFunc("/api/jobs/", a.handleJobAction)
	mux.Handle("/media/", http.StripPrefix("/media/", http.FileServer(http.Dir(filepath.Join(bootstrap.ProjectRoot(), "data")))))

	a.server = &http.Server{
		Handler:           a.loggingMiddleware(a.authMiddleware(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	listener, actualPort, err := listenOn(a.cfg.ListenHost, a.cfg.Port)
	if err != nil {
		return "", err
	}
	a.listener = listener
	a.cfg.Port = actualPort

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.Shutdown(shutdownCtx)
	}()

	go func() {
		if err := a.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.emit("error", fmt.Sprintf("HTTP server stopped: %v", err), nil)
		}
	}()

	a.emit("server", fmt.Sprintf("Listening on %s", a.URL()), nil)
	return a.URL(), nil
}

func (a *App) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	if a.shuttingDown {
		a.mu.Unlock()
		return nil
	}
	a.shuttingDown = true
	clientConns := a.snapshotAllClientConnsLocked()
	for clientID, timer := range a.disconnectTimers {
		if timer != nil {
			timer.Stop()
		}
		delete(a.disconnectTimers, clientID)
	}
	a.mu.Unlock()

	for _, conn := range clientConns {
		_ = conn.conn.Close()
	}

	if a.listener != nil {
		_ = a.listener.Close()
	}
	if a.mgr != nil {
		a.mgr.Close()
	}
	if a.sd != nil {
		_ = a.sd.Close()
	}
	if a.server != nil {
		return a.server.Close()
	}
	return nil
}

func (a *App) URL() string {
	host := a.cfg.ListenHost
	if host == "" || host == "0.0.0.0" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(a.cfg.Port))
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Presets []Preset
	}{
		Presets: presets,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) handleCSS(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/app.css")
	if err != nil {
		http.Error(w, "missing asset", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(data)
}

func (a *App) handleJS(w http.ResponseWriter, r *http.Request) {
	data, err := webFS.ReadFile("web/app.js")
	if err != nil {
		http.Error(w, "missing asset", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(data)
}

func (a *App) handleWebsocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	clientID := sanitizeClientID(r.URL.Query().Get("client_id"))
	if clientID == "" {
		clientID = newID()
	}

	client := &clientConn{conn: conn}
	a.registerClient(clientID, client)
	defer a.unregisterClient(clientID, client)

	_ = client.writeJSON(helloMessage{
		Type:             "hello",
		ClientID:         clientID,
		Presets:          presets,
		RequiresPassword: a.cfg.Password != "",
	})
	_ = client.writeJSON(jobsMessage{
		Type: "jobs",
		Jobs: a.mgr.JobsForClient(clientID),
	})

	conn.SetReadLimit(1 << 20)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	})

	pingTicker := time.NewTicker(25 * time.Second)
	defer pingTicker.Stop()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-readDone:
			return
		case <-pingTicker.C:
			if err := client.writeControl(websocket.PingMessage, []byte("ping")); err != nil {
				return
			}
		}
	}
}

func (a *App) handleTransform(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.sd == nil {
		http.Error(w, "stable-diffusion runtime is unavailable", http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, bootstrap.MaxUploadSize())
	if err := r.ParseMultipartForm(bootstrap.MaxUploadSize()); err != nil {
		http.Error(w, "failed to parse upload", http.StatusBadRequest)
		return
	}

	clientID := sanitizeClientID(r.FormValue("client_id"))
	if clientID == "" {
		clientID = newID()
	}

	preset, ok := findPreset(r.FormValue("preset"))
	if !ok {
		http.Error(w, "unknown preset", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("photo")
	if err != nil {
		http.Error(w, "missing photo", http.StatusBadRequest)
		return
	}
	defer file.Close()

	jobID := newID()
	jobDir := filepath.Join(a.dataDir, jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	inputPath := filepath.Join(jobDir, "input.png")
	width, height, err := normalizeAndSaveImage(file, inputPath)
	if err != nil {
		http.Error(w, "unsupported or invalid image", http.StatusBadRequest)
		return
	}

	outputPath := filepath.Join(jobDir, "output.png")
	job := &Job{
		ID:         jobID,
		ClientID:   clientID,
		PresetID:   preset.ID,
		PresetName: preset.Name,
		Status:     "queued",
		InputPath:  inputPath,
		OutputPath: outputPath,
		OutputURL:  "/" + filepath.ToSlash(filepath.Join("media", "jobs", jobID, "output.png")),
		Prompt:     preset.Prompt,
		Negative:   preset.NegativePrompt,
		Width:      width,
		Height:     height,
		Strength:   preset.Strength,
		Guidance:   preset.Guidance,
		Steps:      preset.Steps,
		CreatedAt:  time.Now(),
	}

	snap := a.mgr.Enqueue(job)
	a.writeJSON(w, http.StatusAccepted, transformResponse{
		ClientID: clientID,
		Job:      snap,
	})
}

func (a *App) handleJobAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	trimmed := strings.TrimPrefix(r.URL.Path, "/api/jobs/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[1] != "cancel" {
		http.NotFound(w, r)
		return
	}

	var body struct {
		ClientID string `json:"clientId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	snap, err := a.mgr.CancelJob(parts[0], sanitizeClientID(body.ClientID), "canceled")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a.writeJSON(w, http.StatusOK, snap)
}

func (a *App) handleJobState(job JobSnapshot, stats QueueStats) {
	a.mu.Lock()
	previousStatus := a.jobStatuses[job.ID]
	a.jobStatuses[job.ID] = job.Status
	connections := a.connectionCountLocked()
	a.mu.Unlock()
	if previousStatus != job.Status {
		a.emit("job", fmt.Sprintf("Job %s is now %s", shortID(job.ID), job.Status), &job)
	}
	a.sink(Event{
		Time:        time.Now(),
		Kind:        "stats",
		Connections: connections,
		Queue:       stats,
		Job:         &job,
	})
}

func (a *App) broadcastClientJobs(clientID string, jobs []JobSnapshot) {
	a.mu.Lock()
	conns := a.snapshotClientConnsLocked(clientID)
	a.mu.Unlock()

	payload := jobsMessage{Type: "jobs", Jobs: jobs}
	for _, conn := range conns {
		if err := conn.writeJSON(payload); err != nil {
			_ = conn.conn.Close()
		}
	}
}

func (a *App) registerClient(clientID string, conn *clientConn) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if timer := a.disconnectTimers[clientID]; timer != nil {
		timer.Stop()
		delete(a.disconnectTimers, clientID)
	}

	if a.clients[clientID] == nil {
		a.clients[clientID] = map[*clientConn]struct{}{}
	}
	a.clients[clientID][conn] = struct{}{}

	a.sink(Event{
		Time:        time.Now(),
		Kind:        "client",
		Message:     fmt.Sprintf("Client connected: %s", shortID(clientID)),
		Connections: a.connectionCountLocked(),
		Queue:       a.currentQueueStats(),
	})
}

func (a *App) unregisterClient(clientID string, conn *clientConn) {
	a.mu.Lock()
	if set := a.clients[clientID]; set != nil {
		delete(set, conn)
		if len(set) == 0 {
			delete(a.clients, clientID)
			a.disconnectTimers[clientID] = time.AfterFunc(bootstrap.DisconnectGracePeriod(), func() {
				if _, err := a.mgr.CancelClientJobs(clientID, "page closed"); err == nil {
					a.logf("Canceled queued work for %s after disconnect", shortID(clientID))
				}
			})
		}
	}
	count := a.connectionCountLocked()
	stats := a.currentQueueStats()
	a.mu.Unlock()

	a.sink(Event{
		Time:        time.Now(),
		Kind:        "client",
		Message:     fmt.Sprintf("Client disconnected: %s", shortID(clientID)),
		Connections: count,
		Queue:       stats,
	})
}

func (a *App) snapshotClientConnsLocked(clientID string) []*clientConn {
	set := a.clients[clientID]
	conns := make([]*clientConn, 0, len(set))
	for conn := range set {
		conns = append(conns, conn)
	}
	return conns
}

func (a *App) snapshotAllClientConnsLocked() []*clientConn {
	conns := make([]*clientConn, 0)
	for _, set := range a.clients {
		for conn := range set {
			conns = append(conns, conn)
		}
	}
	return conns
}

func (a *App) connectionCountLocked() int {
	total := 0
	for _, set := range a.clients {
		total += len(set)
	}
	return total
}

func (a *App) runJob(ctx context.Context, job *Job) error {
	if a.sd == nil {
		return errors.New("sd-server is not running")
	}

	a.setActiveJob(job.ID)
	defer a.clearActiveJob(job.ID)

	a.logf("Rendering %s at %dx%d with %s", shortID(job.ID), job.Width, job.Height, job.PresetName)

	inputBytes, err := os.ReadFile(job.InputPath)
	if err != nil {
		return err
	}

	req := sdcpp.ImageGenRequest{
		Prompt:         job.Prompt,
		NegativePrompt: job.Negative,
		InitImage:      encodePNGDataURL(inputBytes),
		Width:          job.Width,
		Height:         job.Height,
		Seed:           -1,
		Strength:       job.Strength,
		OutputFormat:   "png",
		SampleParams: sdcpp.SampleParams{
			SampleSteps: job.Steps,
			Guidance: sdcpp.GuidanceParameters{
				TxtCFG:            1.0,
				DistilledGuidance: job.Guidance,
			},
		},
		VAETilingParams: sdcpp.VAETilingParameters{
			Enabled:       true,
			TargetOverlap: 0.5,
		},
	}

	submitCtx, cancelSubmit := context.WithTimeout(ctx, 30*time.Second)
	resp, err := a.sd.Submit(submitCtx, req)
	cancelSubmit()
	if err != nil {
		return err
	}

	lastStatus := resp.Status
	jobID := resp.ID
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.abortCurrentGeneration(jobID, lastStatus)
			return context.Canceled
		case <-ticker.C:
		}

		pollCtx, cancelPoll := context.WithTimeout(context.Background(), 10*time.Second)
		status, err := a.sd.PollJob(pollCtx, jobID)
		cancelPoll()
		if err != nil {
			return err
		}

		lastStatus = status.Status
		switch status.Status {
		case "queued", "generating":
			continue
		case "completed":
			imageBytes, err := sdcpp.DecodeFirstImage(status.Result)
			if err != nil {
				return err
			}
			return os.WriteFile(job.OutputPath, imageBytes, 0o644)
		case "canceled":
			return context.Canceled
		default:
			if status.Error != nil && status.Error.Message != "" {
				return errors.New(status.Error.Message)
			}
			return fmt.Errorf("sd-server job ended with status %s", status.Status)
		}
	}
}

func (a *App) abortCurrentGeneration(jobID, status string) {
	if a.sd == nil {
		return
	}
	if a.isShuttingDown() {
		return
	}
	caps := a.sd.Capabilities()
	cancelCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch status {
	case "queued":
		if caps.Features.CancelQueued {
			_ = a.sd.CancelJob(cancelCtx, jobID)
		}
	case "generating":
		if caps.Features.CancelGenerating {
			_ = a.sd.CancelJob(cancelCtx, jobID)
			return
		}
		_ = a.sd.Restart(context.Background())
		a.logf("Restarted sd-server to stop an in-flight generation")
	}
}

func outputURLForStatus(job *Job) string {
	if job.Status == "completed" {
		return job.OutputURL
	}
	return ""
}

func listenOn(host string, port int) (net.Listener, int, error) {
	if port > 0 {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		return listener, port, err
	}

	start := bootstrap.DefaultStartPort()
	for candidate := start; candidate < start+50; candidate++ {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(candidate)))
		if err == nil {
			return listener, candidate, nil
		}
	}

	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, 0, err
	}
	addr := listener.Addr().(*net.TCPAddr)
	return listener, addr.Port, nil
}

func findPreset(id string) (Preset, bool) {
	for _, preset := range presets {
		if preset.ID == id {
			return preset, true
		}
	}
	return Preset{}, false
}

func (a *App) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func sanitizeClientID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 {
		value = value[:64]
	}
	for _, ch := range value {
		if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' && ch != '_' {
			return ""
		}
	}
	return value
}

func newID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func encodeBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func encodePNGDataURL(data []byte) string {
	return "data:image/png;base64," + encodeBase64(data)
}

func extractRenderProgress(line string) (int, int, bool) {
	match := renderProgressPattern.FindStringSubmatch(line)
	if len(match) != 3 {
		return 0, 0, false
	}
	current, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, 0, false
	}
	total, err := strconv.Atoi(match[2])
	if err != nil || total <= 0 {
		return 0, 0, false
	}
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	return current, total, true
}

func (a *App) handleSDLog(line string) {
	a.sink(Event{
		Time:    time.Now(),
		Kind:    "sdcpp",
		Message: line,
		Queue:   a.currentQueueStats(),
	})

	if a.mgr == nil {
		return
	}

	jobID, progress, ok := a.advanceActiveProgress(line)
	if jobID == "" {
		return
	}
	if !ok {
		return
	}
	a.mgr.UpdateJobProgress(jobID, progress.Current, progress.Total, progress.Percent, string(progress.Phase))
}

func (a *App) setActiveJob(jobID string) {
	a.mu.Lock()
	a.activeJobID = jobID
	a.activeProgress = renderProgressState{}
	a.mu.Unlock()
}

func (a *App) clearActiveJob(jobID string) {
	a.mu.Lock()
	if a.activeJobID == jobID {
		a.activeJobID = ""
		a.activeProgress = renderProgressState{}
	}
	a.mu.Unlock()
}

func (a *App) currentActiveJobID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.activeJobID
}

func (a *App) advanceActiveProgress(line string) (string, renderProgressState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.activeJobID == "" {
		return "", renderProgressState{}, false
	}

	updated, changed := advanceRenderProgress(a.activeProgress, line)
	if !changed {
		return a.activeJobID, a.activeProgress, false
	}
	a.activeProgress = updated
	return a.activeJobID, updated, true
}

func advanceRenderProgress(state renderProgressState, line string) (renderProgressState, bool) {
	lower := strings.ToLower(line)
	updated := state
	changed := false

	switch {
	case strings.Contains(lower, "generate_image completed"),
		strings.Contains(lower, "decode_first_stage completed"):
		updated = renderProgressState{
			Phase:   phaseGenerate,
			Current: 1,
			Total:   1,
			Percent: 100,
		}
		return updated, updated != state
	case strings.Contains(lower, "z_image compute buffer size"):
		if updated.Phase != phaseBuffer || updated.Percent < 30 {
			updated.Phase = phaseBuffer
			updated.Current = 0
			updated.Total = 0
			updated.Percent = max(updated.Percent, 30)
			changed = true
		}
	case strings.Contains(lower, "decoding "),
		strings.Contains(lower, "sampling completed"),
		strings.Contains(lower, "generating 1 latent images completed"):
		if updated.Phase != phaseGenerate || updated.Percent < 50 {
			updated.Phase = phaseGenerate
			updated.Current = 0
			updated.Total = 0
			updated.Percent = max(updated.Percent, 50)
			changed = true
		}
	case strings.Contains(lower, "vae compute buffer size"):
		if updated.Phase == "" {
			updated.Phase = phaseEncode
			updated.Percent = 0
			changed = true
		} else if updated.Phase == phaseBuffer {
			updated.Phase = phaseGenerate
			updated.Current = 0
			updated.Total = 0
			updated.Percent = max(updated.Percent, 50)
			changed = true
		}
	}

	current, total, ok := extractRenderProgress(line)
	if !ok {
		return updated, changed
	}

	if updated.Phase == "" {
		updated.Phase = phaseEncode
		changed = true
	}

	percent := weightedPhasePercent(updated.Phase, current, total)
	if updated.Current != current || updated.Total != total || updated.Percent != percent {
		updated.Current = current
		updated.Total = total
		updated.Percent = percent
		changed = true
	}

	return updated, changed
}

func weightedPhasePercent(phase renderPhase, current, total int) int {
	if total <= 0 {
		switch phase {
		case phaseBuffer:
			return 30
		case phaseGenerate:
			return 50
		default:
			return 0
		}
	}

	ratio := float64(current) / float64(total)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}

	switch phase {
	case phaseBuffer:
		return 30 + int(ratio*20.0)
	case phaseGenerate:
		return 50 + int(ratio*50.0)
	default:
		return int(ratio * 30.0)
	}
}

func (a *App) emit(kind, message string, job *JobSnapshot) {
	a.sink(Event{
		Time:    time.Now(),
		Kind:    kind,
		Message: message,
		Job:     job,
		Queue:   a.currentQueueStats(),
	})
}

func (a *App) isShuttingDown() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.shuttingDown
}

func (a *App) logf(format string, args ...any) {
	a.sink(Event{
		Time:    time.Now(),
		Kind:    "log",
		Message: fmt.Sprintf(format, args...),
		Queue:   a.currentQueueStats(),
	})
}

func (a *App) currentQueueStats() QueueStats {
	if a.mgr == nil {
		return QueueStats{}
	}
	return a.mgr.Stats()
}

func (c *clientConn) writeJSON(value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(value)
}

func (c *clientConn) writeControl(messageType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteControl(messageType, data, time.Now().Add(10*time.Second))
}

func (a *App) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/api/") {
			a.logf("%s %s", r.Method, r.URL.Path)
		}
	})
}

func (a *App) authMiddleware(next http.Handler) http.Handler {
	if a.cfg.Password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(a.cfg.Password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Purple Lightswitch"`)
			http.Error(w, "authorization required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
