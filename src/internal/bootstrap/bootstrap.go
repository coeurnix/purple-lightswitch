package bootstrap

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	userAgent                 = "purple-lightswitch-bootstrap"
	latestReleaseAPIURL       = "https://api.github.com/repos/leejet/stable-diffusion.cpp/releases/latest"
	latestReleaseWebURL       = "https://github.com/leejet/stable-diffusion.cpp/releases/latest"
	downloadChunkSize         = 1024 * 1024
	extractChunkSize          = 1024 * 1024
	defaultListenHost         = "0.0.0.0"
	defaultStartPort          = 27071
	disconnectGrace           = 5 * time.Second
	maxUploadBytes      int64 = 25 << 20
)

type Progress struct {
	ID      string
	Label   string
	Phase   string
	Current int64
	Total   int64
	Done    bool
}

type Reporter struct {
	Log      func(string)
	Progress func(Progress)
}

type ReleaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type ReleaseInfo struct {
	TagName string         `json:"tag_name"`
	HTMLURL string         `json:"html_url"`
	Assets  []ReleaseAsset `json:"assets"`
}

type RuntimeAssets struct {
	BinTarget string
	BinDir    string
	CLIPath   string
	ModelPath string
	LLMPath   string
	VAEPath   string
}

type ModelsOptions struct {
	DryRun bool
	Force  bool
}

type BinsOptions struct {
	DryRun bool
	All    bool
	Target string
}

type modelDownload struct {
	Label        string
	URL          string
	RelativePath string
}

var (
	errUnsupportedOS = errors.New("unsupported operating system")

	targetPatterns = map[string]string{
		"win-avx2-x64":     "sd-master-*-bin-win-avx2-x64.zip",
		"win-rocm-x64":     "sd-master-*-bin-win-rocm-x64.zip",
		"win-cuda-x64":     "sd-master-*-bin-win-cuda12-x64.zip",
		"linux-vulkan-x64": "sd-master-*-bin-Linux-Ubuntu-*-x86_64-vulkan.zip",
		"osx-arm64":        "sd-master-*-bin-Darwin-macOS-*-arm64.zip",
	}
	allTargets = []string{
		"win-avx2-x64",
		"win-rocm-x64",
		"win-cuda-x64",
		"linux-vulkan-x64",
		"osx-arm64",
	}
	cudaRuntimeTarget  = "win-cuda-x64"
	cudaRuntimePattern = "cudart-sd-bin-win-cu12-x64.zip"
	cudaDLLPatterns    = []string{"cublas*.dll", "cuda*.dll"}
	modelDownloads     = []modelDownload{
		{
			Label:        "Z-Image-Turbo Q4_0 GGUF",
			URL:          "https://huggingface.co/leejet/Z-Image-Turbo-GGUF/resolve/main/z_image_turbo-Q4_0.gguf",
			RelativePath: filepath.Join("models", "z-image-turbo", "z_image_turbo-Q4_0.gguf"),
		},
		{
			Label:        "Qwen3 4B text encoder / LLM",
			URL:          "https://huggingface.co/unsloth/Qwen3-4B-Instruct-2507-GGUF/resolve/main/Qwen3-4B-Instruct-2507-Q4_K_M.gguf",
			RelativePath: filepath.Join("models", "qwen3-4b-instruct-2507", "Qwen3-4B-Instruct-2507-Q4_K_M.gguf"),
		},
		{
			Label:        "Z-Image-Turbo VAE",
			URL:          "https://huggingface.co/Tongyi-MAI/Z-Image-Turbo/resolve/main/vae/diffusion_pytorch_model.safetensors",
			RelativePath: filepath.Join("models", "z-image-turbo", "vae", "diffusion_pytorch_model.safetensors"),
		},
	}
	httpClient = &http.Client{}
)

func repoRoot() string {
	candidates := make([]string, 0, 4)
	if exePath, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exePath))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, wd)
	}

	seen := map[string]struct{}{}
	for _, start := range candidates {
		dir := start
		for {
			if dir == "" {
				break
			}
			if _, ok := seen[dir]; ok {
				break
			}
			seen[dir] = struct{}{}
			if looksLikeRepoRoot(dir) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func ProjectRoot() string {
	return repoRoot()
}

func looksLikeRepoRoot(dir string) bool {
	modelsPath := filepath.Join(dir, "models")
	binsPath := filepath.Join(dir, "sdcpp-bins")
	if info, err := os.Stat(modelsPath); err != nil || !info.IsDir() {
		return false
	}
	if info, err := os.Stat(binsPath); err != nil || !info.IsDir() {
		return false
	}
	return true
}

func DefaultListenHost() string {
	return defaultListenHost
}

func DefaultStartPort() int {
	return defaultStartPort
}

func DisconnectGracePeriod() time.Duration {
	return disconnectGrace
}

func MaxUploadSize() int64 {
	return maxUploadBytes
}

func ManualSetupAdvice() string {
	lines := []string{
		"Automatic setup failed. Download the required assets manually and place them in these locations:",
		"",
		fmt.Sprintf("stable-diffusion.cpp release binaries: %s", latestReleaseWebURL),
		"Place the host binaries in sdcpp-bins/<target>/ and ensure sd-cli is present.",
		"",
		"Models:",
	}
	for _, item := range modelDownloads {
		lines = append(lines, fmt.Sprintf("- %s -> %s", item.URL, filepath.ToSlash(item.RelativePath)))
	}
	return strings.Join(lines, "\n")
}

func EnsureDefaultRuntime(ctx context.Context, reporter Reporter) (RuntimeAssets, error) {
	targets, _, err := detectDefaultTargets()
	if err != nil {
		return RuntimeAssets{}, err
	}

	if err := DownloadBins(ctx, BinsOptions{Target: targets[0]}, reporter); err != nil {
		return RuntimeAssets{}, err
	}
	if err := DownloadModels(ctx, ModelsOptions{}, reporter); err != nil {
		return RuntimeAssets{}, err
	}
	return CurrentRuntimeAssets(targets[0])
}

func CurrentRuntimeAssets(target string) (RuntimeAssets, error) {
	root := repoRoot()
	binDir := filepath.Join(root, "sdcpp-bins", target)
	modelPath := filepath.Join(root, "models", "z-image-turbo", "z_image_turbo-Q4_0.gguf")
	llmPath := filepath.Join(root, "models", "qwen3-4b-instruct-2507", "Qwen3-4B-Instruct-2507-Q4_K_M.gguf")
	vaePath := filepath.Join(root, "models", "z-image-turbo", "vae", "diffusion_pytorch_model.safetensors")

	cliName := "sd-cli"
	if runtime.GOOS == "windows" {
		cliName = "sd-cli.exe"
	}
	cliPath := filepath.Join(binDir, cliName)

	required := []string{cliPath, modelPath, llmPath, vaePath}
	for _, item := range required {
		info, err := os.Stat(item)
		if err != nil || info.Size() <= 0 {
			return RuntimeAssets{}, fmt.Errorf("missing required runtime asset: %s", item)
		}
	}

	return RuntimeAssets{
		BinTarget: target,
		BinDir:    binDir,
		CLIPath:   cliPath,
		ModelPath: modelPath,
		LLMPath:   llmPath,
		VAEPath:   vaePath,
	}, nil
}

func DownloadModels(ctx context.Context, opts ModelsOptions, reporter Reporter) error {
	root := repoRoot()
	for _, item := range modelDownloads {
		dst := filepath.Join(root, item.RelativePath)
		if !opts.Force && fileExistsNonEmpty(dst) {
			logf(reporter, "Skipping %s: found %s", item.Label, filepath.ToSlash(item.RelativePath))
			continue
		}

		logf(reporter, "Downloading %s", item.Label)
		if opts.DryRun {
			logf(reporter, "[dry-run] %s -> %s", item.URL, filepath.ToSlash(item.RelativePath))
			continue
		}
		progressID := "model:" + filepath.Base(dst)
		if err := downloadToFile(ctx, item.URL, dst, progressID, item.Label, reporter); err != nil {
			return err
		}
	}
	return nil
}

func DownloadBins(ctx context.Context, opts BinsOptions, reporter Reporter) error {
	release, err := fetchLatestRelease(ctx)
	if err != nil {
		return err
	}

	var targets []string
	switch {
	case opts.All:
		targets = append(targets, allTargets...)
	case opts.Target != "":
		if _, ok := targetPatterns[opts.Target]; !ok {
			return fmt.Errorf("unknown target %q", opts.Target)
		}
		targets = []string{opts.Target}
	default:
		targets, _, err = detectDefaultTargets()
		if err != nil {
			return err
		}
	}

	logf(reporter, "Resolved stable-diffusion.cpp release %s", release.TagName)
	for _, target := range targets {
		if err := installTargetAsset(ctx, release, target, opts.DryRun, reporter); err != nil {
			return err
		}
		if target == cudaRuntimeTarget {
			if err := installCUDARuntime(ctx, release, opts.DryRun, reporter); err != nil {
				return err
			}
		}
	}
	return nil
}

func fetchLatestRelease(ctx context.Context) (ReleaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseAPIURL, nil)
	if err != nil {
		return ReleaseInfo{}, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return ReleaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ReleaseInfo{}, fmt.Errorf("github request failed: %s", resp.Status)
	}

	var release ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return ReleaseInfo{}, err
	}
	return release, nil
}

func installTargetAsset(ctx context.Context, release ReleaseInfo, target string, dryRun bool, reporter Reporter) error {
	root := repoRoot()
	destination := filepath.Join(root, "sdcpp-bins", target)
	pattern := targetPatterns[target]

	if directoryHasFiles(destination) {
		logf(reporter, "Skipping %s: directory is not empty", target)
		return nil
	}

	asset, ok := findAsset(release.Assets, pattern)
	if !ok {
		return fmt.Errorf("no asset matched %q", pattern)
	}

	logf(reporter, "Installing %s into sdcpp-bins/%s", asset.Name, target)
	if dryRun {
		logf(reporter, "[dry-run] %s -> sdcpp-bins/%s", asset.URL, target)
		return nil
	}

	zipPath, cleanup, err := downloadTempFile(ctx, asset.URL, "bin:"+target+":download", asset.Name, reporter)
	if err != nil {
		return err
	}
	defer cleanup()

	return extractZip(zipPath, destination, "bin:"+target+":extract", asset.Name, reporter)
}

func installCUDARuntime(ctx context.Context, release ReleaseInfo, dryRun bool, reporter Reporter) error {
	root := repoRoot()
	destination := filepath.Join(root, "sdcpp-bins", cudaRuntimeTarget)
	if hasCUDADLLs(destination) {
		logf(reporter, "Skipping CUDA runtime: found cublas* and cuda* DLLs in sdcpp-bins/%s", cudaRuntimeTarget)
		return nil
	}

	asset, ok := findAsset(release.Assets, cudaRuntimePattern)
	if !ok {
		return fmt.Errorf("no asset matched %q", cudaRuntimePattern)
	}

	logf(reporter, "Installing %s into sdcpp-bins/%s", asset.Name, cudaRuntimeTarget)
	if dryRun {
		logf(reporter, "[dry-run] %s -> sdcpp-bins/%s", asset.URL, cudaRuntimeTarget)
		return nil
	}

	zipPath, cleanup, err := downloadTempFile(ctx, asset.URL, "bin:"+cudaRuntimeTarget+":runtime-download", asset.Name, reporter)
	if err != nil {
		return err
	}
	defer cleanup()

	return extractZip(zipPath, destination, "bin:"+cudaRuntimeTarget+":runtime-extract", asset.Name, reporter)
}

func detectDefaultTargets() ([]string, string, error) {
	switch runtime.GOOS {
	case "windows":
		switch detectWindowsGPUKind() {
		case "nvidia":
			return []string{"win-cuda-x64"}, "Windows with NVIDIA GPU", nil
		case "amd":
			return []string{"win-rocm-x64"}, "Windows with AMD GPU", nil
		default:
			return []string{"win-avx2-x64"}, "Windows with no NVIDIA or AMD GPU detected", nil
		}
	case "darwin":
		return []string{"osx-arm64"}, "macOS host", nil
	case "linux":
		return []string{"linux-vulkan-x64"}, "Linux host", nil
	default:
		return nil, "", fmt.Errorf("%w: %s", errUnsupportedOS, runtime.GOOS)
	}
}

func detectWindowsGPUKind() string {
	if runtime.GOOS != "windows" {
		return "other"
	}

	cmd := exec.Command(
		"powershell",
		"-NoProfile",
		"-Command",
		"Get-CimInstance Win32_VideoController | Select-Object Name,AdapterCompatibility | ConvertTo-Json -Compress",
	)
	output, err := cmd.Output()
	if err != nil || len(output) == 0 {
		return "other"
	}

	var payload any
	if err := json.Unmarshal(output, &payload); err != nil {
		return "other"
	}

	var items []map[string]any
	switch value := payload.(type) {
	case []any:
		for _, item := range value {
			if entry, ok := item.(map[string]any); ok {
				items = append(items, entry)
			}
		}
	case map[string]any:
		items = append(items, value)
	}

	combined := strings.Builder{}
	for _, item := range items {
		for _, key := range []string{"Name", "AdapterCompatibility"} {
			if text, ok := item[key].(string); ok {
				combined.WriteString(strings.ToLower(text))
				combined.WriteByte(' ')
			}
		}
	}
	text := combined.String()
	switch {
	case strings.Contains(text, "nvidia"):
		return "nvidia"
	case strings.Contains(text, "advanced micro devices"),
		strings.Contains(text, "amd"),
		strings.Contains(text, "radeon"):
		return "amd"
	default:
		return "other"
	}
}

func downloadToFile(ctx context.Context, urlStr, destination, progressID, label string, reporter Reporter) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}

	tempPath, cleanup, err := createTempInDir(filepath.Dir(destination), filepath.Base(destination))
	if err != nil {
		return err
	}
	defer cleanup()

	if err := streamToPath(ctx, urlStr, tempPath, progressID, label, reporter); err != nil {
		return err
	}
	return os.Rename(tempPath, destination)
}

func downloadTempFile(ctx context.Context, urlStr, progressID, label string, reporter Reporter) (string, func(), error) {
	tempDir, err := os.MkdirTemp("", "purple-lightswitch-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = os.RemoveAll(tempDir)
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	name := path.Base(u.Path)
	if name == "" || name == "." || name == "/" {
		name = "download.bin"
	}
	tempPath := filepath.Join(tempDir, name)
	if err := streamToPath(ctx, urlStr, tempPath, progressID, label, reporter); err != nil {
		cleanup()
		return "", nil, err
	}
	return tempPath, cleanup, nil
}

func createTempInDir(dir, base string) (string, func(), error) {
	file, err := os.CreateTemp(dir, base+".*.part")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	cleanup := func() {
		_ = os.Remove(path)
	}
	return path, cleanup, nil
}

func streamToPath(ctx context.Context, urlStr, destination, progressID, label string, reporter Reporter) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download failed for %s: %s", label, resp.Status)
	}

	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer file.Close()

	total := resp.ContentLength
	var current int64
	for {
		buf := make([]byte, downloadChunkSize)
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := file.Write(buf[:n]); err != nil {
				return err
			}
			current += int64(n)
			progressf(reporter, Progress{
				ID:      progressID,
				Label:   label,
				Phase:   "download",
				Current: current,
				Total:   total,
			})
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}

	progressf(reporter, Progress{
		ID:      progressID,
		Label:   label,
		Phase:   "download",
		Current: current,
		Total:   total,
		Done:    true,
	})
	return nil
}

func extractZip(zipPath, destination, progressID, label string, reporter Reporter) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	destRoot, err := filepath.Abs(destination)
	if err != nil {
		return err
	}

	var total int64
	for _, member := range reader.File {
		if !member.FileInfo().IsDir() {
			total += int64(member.UncompressedSize64)
		}
	}

	var current int64
	for _, member := range reader.File {
		targetPath := filepath.Join(destination, member.Name)
		absPath, err := filepath.Abs(targetPath)
		if err != nil {
			return err
		}
		if absPath != destRoot && !strings.HasPrefix(absPath, destRoot+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to extract %q outside %s", member.Name, destination)
		}

		if member.FileInfo().IsDir() {
			if err := os.MkdirAll(targetPath, 0o755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return err
		}

		src, err := member.Open()
		if err != nil {
			return err
		}

		dst, err := os.Create(targetPath)
		if err != nil {
			src.Close()
			return err
		}

		for {
			buf := make([]byte, extractChunkSize)
			n, readErr := src.Read(buf)
			if n > 0 {
				if _, err := dst.Write(buf[:n]); err != nil {
					dst.Close()
					src.Close()
					return err
				}
				current += int64(n)
				progressf(reporter, Progress{
					ID:      progressID,
					Label:   label,
					Phase:   "extract",
					Current: current,
					Total:   total,
				})
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				dst.Close()
				src.Close()
				return readErr
			}
		}

		_ = dst.Close()
		_ = src.Close()
	}

	progressf(reporter, Progress{
		ID:      progressID,
		Label:   label,
		Phase:   "extract",
		Current: current,
		Total:   total,
		Done:    true,
	})
	return nil
}

func findAsset(assets []ReleaseAsset, pattern string) (ReleaseAsset, bool) {
	for _, asset := range assets {
		matched, err := path.Match(pattern, asset.Name)
		if err == nil && matched {
			return asset, true
		}
	}
	return ReleaseAsset{}, false
}

func directoryHasFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) > 0
}

func hasCUDADLLs(dir string) bool {
	found := map[string]bool{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		for _, pattern := range cudaDLLPatterns {
			matched, matchErr := path.Match(pattern, name)
			if matchErr == nil && matched {
				found[pattern] = true
			}
		}
		return nil
	})

	for _, pattern := range cudaDLLPatterns {
		if !found[pattern] {
			return false
		}
	}
	return true
}

func fileExistsNonEmpty(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func logf(reporter Reporter, format string, args ...any) {
	if reporter.Log != nil {
		reporter.Log(fmt.Sprintf(format, args...))
	}
}

func progressf(reporter Reporter, item Progress) {
	if reporter.Progress != nil {
		reporter.Progress(item)
	}
}
