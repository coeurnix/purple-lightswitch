package tui

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"purple-lightswitch/internal/bootstrap"
	appRuntime "purple-lightswitch/internal/runtime"
)

type bootstrapLogMsg struct {
	message string
}

type bootstrapProgressMsg struct {
	progress bootstrap.Progress
}

type runtimeEventMsg struct {
	event appRuntime.Event
}

type startResultMsg struct {
	app     *appRuntime.App
	address string
	err     error
}

type shutdownResultMsg struct {
	err error
}

type model struct {
	ctx    context.Context
	cancel context.CancelFunc

	cfg         appRuntime.Config
	interactive bool
	stage       string
	started     bool
	quitting    bool

	events chan tea.Msg
	app    *appRuntime.App

	hostInput     textinput.Model
	portInput     textinput.Model
	passwordInput textinput.Model
	focusIndex    int

	spinner spinner.Model
	width   int
	height  int

	progress    map[string]bootstrap.Progress
	logs        []string
	jobs        map[string]appRuntime.JobSnapshot
	address     string
	connections int
	queueStats  appRuntime.QueueStats
	fatal       string
}

var (
	frameStyle = lipgloss.NewStyle().Padding(1, 2)
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#ffd580")).
			Background(lipgloss.Color("#3d124f")).
			Padding(0, 1)
	subtleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#b7a8c8"))
	panelStyle  = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#7d4cc9")).
			Padding(1, 2)
	goodStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#8cf4c1"))
	badStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff9aaa"))
	accent    = lipgloss.NewStyle().Foreground(lipgloss.Color("#f86cb6"))
)

func Run(parent context.Context, cfg appRuntime.Config, interactive bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	hostInput := textinput.New()
	hostInput.Prompt = "Listen host: "
	hostInput.SetValue(cfg.ListenHost)
	if hostInput.Value() == "" {
		hostInput.SetValue(bootstrap.DefaultListenHost())
	}

	portInput := textinput.New()
	portInput.Prompt = "Port: "
	portInput.Placeholder = fmt.Sprintf("%d", bootstrap.DefaultStartPort())

	passwordInput := textinput.New()
	passwordInput.Prompt = "Password: "
	passwordInput.EchoMode = textinput.EchoPassword
	passwordInput.EchoCharacter = '•'
	passwordInput.Placeholder = "optional"
	passwordInput.SetValue(cfg.Password)

	spin := spinner.New()
	spin.Spinner = spinner.Dot
	spin.Style = accent

	m := &model{
		ctx:           ctx,
		cancel:        cancel,
		cfg:           cfg,
		interactive:   interactive,
		stage:         "boot",
		events:        make(chan tea.Msg, 256),
		hostInput:     hostInput,
		portInput:     portInput,
		passwordInput: passwordInput,
		progress:      map[string]bootstrap.Progress{},
		jobs:          map[string]appRuntime.JobSnapshot{},
		spinner:       spin,
	}
	if interactive {
		m.stage = "prompt"
	}

	program := tea.NewProgram(m, tea.WithAltScreen())
	_, err := program.Run()
	return err
}

func (m *model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.spinner.Tick, listen(m.events)}
	if !m.interactive {
		m.startApp()
	}
	return tea.Batch(cmds...)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case bootstrapLogMsg:
		m.pushLog(msg.message)
		return m, listen(m.events)
	case bootstrapProgressMsg:
		m.progress[msg.progress.ID] = msg.progress
		if msg.progress.Done {
			m.pushLog(fmt.Sprintf("%s %s complete", msg.progress.Label, msg.progress.Phase))
		}
		return m, listen(m.events)
	case runtimeEventMsg:
		if msg.event.Message != "" {
			m.pushLog(msg.event.Message)
		}
		m.connections = msg.event.Connections
		m.queueStats = msg.event.Queue
		if msg.event.Job != nil {
			m.jobs[msg.event.Job.ID] = *msg.event.Job
		}
		return m, listen(m.events)
	case startResultMsg:
		if msg.err != nil {
			m.stage = "fatal"
			m.fatal = msg.err.Error() + "\n\n" + bootstrap.ManualSetupAdvice()
			return m, listen(m.events)
		}
		m.app = msg.app
		m.address = msg.address
		m.stage = "running"
		m.pushLog("Purple Lightswitch is live.")
		return m, listen(m.events)
	case shutdownResultMsg:
		if msg.err != nil {
			m.pushLog(fmt.Sprintf("Shutdown warning: %v", msg.err))
		}
		m.cancel()
		return m, tea.Quit
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "ctrl+d", "esc", "q", "Q":
		if m.quitting {
			return m, nil
		}
		m.quitting = true
		if m.app != nil {
			m.stage = "stopping"
			m.pushLog("Stopping Purple Lightswitch...")
			go func(app *appRuntime.App) {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				m.events <- shutdownResultMsg{err: app.Shutdown(shutdownCtx)}
			}(m.app)
			return m, nil
		}
		m.cancel()
		return m, tea.Quit
	}

	if m.stage != "prompt" {
		return m, nil
	}

	switch msg.String() {
	case "tab", "shift+tab", "up", "down":
		if msg.String() == "shift+tab" || msg.String() == "up" {
			m.focusIndex--
		} else {
			m.focusIndex++
		}
		if m.focusIndex < 0 {
			m.focusIndex = 2
		}
		if m.focusIndex > 2 {
			m.focusIndex = 0
		}
		m.syncFocus()
		return m, nil
	case "enter":
		port := 0
		if strings.TrimSpace(m.portInput.Value()) != "" {
			value, err := strconv.Atoi(strings.TrimSpace(m.portInput.Value()))
			if err != nil {
				m.pushLog("Port must be an integer.")
				return m, nil
			}
			port = value
		}
		m.cfg.ListenHost = strings.TrimSpace(m.hostInput.Value())
		m.cfg.Port = port
		m.cfg.Password = m.passwordInput.Value()
		m.stage = "boot"
		m.startApp()
		return m, nil
	}

	var cmd tea.Cmd
	switch m.focusIndex {
	case 0:
		m.hostInput, cmd = m.hostInput.Update(msg)
	case 1:
		m.portInput, cmd = m.portInput.Update(msg)
	case 2:
		m.passwordInput, cmd = m.passwordInput.Update(msg)
	}
	return m, cmd
}

func (m *model) View() string {
	switch m.stage {
	case "prompt":
		return frameStyle.Render(m.renderPrompt())
	case "fatal":
		return frameStyle.Render(m.renderFatal())
	default:
		return frameStyle.Render(m.renderDashboard())
	}
}

func (m *model) renderPrompt() string {
	m.syncFocus()
	var sections []string
	sections = append(sections,
		titleStyle.Render("Purple Lightswitch"),
		"",
		"Interactive launch mode",
		"Press Enter to start, Tab to move between fields, Esc to quit.",
		"",
		m.hostInput.View(),
		m.portInput.View()+"  "+subtleStyle.Render("(blank means auto-pick starting at 27071)"),
		m.passwordInput.View(),
	)
	return strings.Join(sections, "\n")
}

func (m *model) renderFatal() string {
	return strings.Join([]string{
		titleStyle.Render("Purple Lightswitch"),
		"",
		badStyle.Render("Startup failed"),
		"",
		m.fatal,
	}, "\n")
}

func (m *model) renderDashboard() string {
	contentWidth := max(56, m.width-8)
	bodyWidth := max(36, contentWidth-6)

	statusBody := m.renderStatusPanel(bodyWidth)
	logBody := m.renderLogPanel(bodyWidth, m.availableLogLines(countLines(statusBody)))

	return strings.Join([]string{
		titleStyle.Render("Purple Lightswitch"),
		subtleStyle.Render("Renderer status"),
		"",
		panelStyle.Width(contentWidth).Render(statusBody),
		"",
		panelStyle.Width(contentWidth).Render(logBody),
	}, "\n")
}

func (m *model) renderStatusPanel(width int) string {
	address := m.address
	if address == "" {
		address = fmt.Sprintf("%s starting up", m.spinner.View())
	}

	statusText := "Online"
	statusStyle := goodStyle
	switch m.stage {
	case "boot":
		statusText = fmt.Sprintf("%s Preparing runtime", m.spinner.View())
		statusStyle = accent
	case "stopping":
		statusText = fmt.Sprintf("%s Stopping services", m.spinner.View())
		statusStyle = accent
	case "fatal":
		statusText = "Startup failed"
		statusStyle = badStyle
	}

	lines := []string{
		fmt.Sprintf("Address: %s", address),
		fmt.Sprintf("Status: %s", statusStyle.Render(statusText)),
		fmt.Sprintf("Clients: %d", m.connections),
		fmt.Sprintf("Queue: %d queued, %d rendering", m.queueStats.Queued, m.queueStats.Running),
	}

	if job, ok := m.focusJob(); ok {
		lines = append(lines, m.renderJobSummary(job))
	}

	if downloads := m.renderDownloadSummary(width); len(downloads) > 0 {
		lines = append(lines, "")
		lines = append(lines, downloads...)
	}

	lines = append(lines, "", subtleStyle.Render("Stop: Esc, Q, or Ctrl-C"))
	return strings.Join(lines, "\n")
}

func (m *model) renderJobSummary(job appRuntime.JobSnapshot) string {
	label := fmt.Sprintf("Current: %s  %s", shortID(job.ID), job.PresetName)
	switch job.Status {
	case "queued":
		if job.QueuePosition > 0 {
			return label + subtleStyle.Render(fmt.Sprintf("  waiting in queue (%d ahead)", job.QueuePosition))
		}
		return label + subtleStyle.Render("  waiting in queue")
	case "running":
		if job.ProgressPercent > 0 {
			if job.ProgressTotal > 0 {
				return label + accent.Render(fmt.Sprintf("  %s %d%% (%d/%d)", phaseLabel(job.ProgressPhase), job.ProgressPercent, job.ProgressCurrent, job.ProgressTotal))
			}
			return label + accent.Render(fmt.Sprintf("  %s %d%%", phaseLabel(job.ProgressPhase), job.ProgressPercent))
		}
		return label + accent.Render("  rendering")
	case "completed":
		return label + goodStyle.Render("  result ready")
	case "failed":
		return label + badStyle.Render("  failed")
	case "canceled":
		return label + badStyle.Render("  canceled")
	default:
		return label + subtleStyle.Render("  "+job.Status)
	}
}

func (m *model) renderDownloadSummary(width int) []string {
	if len(m.progress) == 0 {
		return nil
	}
	items := make([]bootstrap.Progress, 0, len(m.progress))
	for _, item := range m.progress {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })

	lines := []string{"Setup"}
	barWidth := max(16, min(28, width-18))
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("%s [%s]", item.Label, item.Phase))
		lines = append(lines, renderProgressBar(item.Current, item.Total, barWidth))
	}
	return lines
}

func (m *model) renderLogPanel(width, limit int) string {
	lines := []string{"Activity"}
	logs := m.tailLogs(limit)
	if hidden := len(m.logs) - len(logs); hidden > 0 {
		lines = append(lines, subtleStyle.Render(fmt.Sprintf("%d older lines hidden", hidden)))
	}
	if len(logs) == 0 {
		lines = append(lines, subtleStyle.Render("No activity yet."))
		return strings.Join(lines, "\n")
	}
	for _, line := range logs {
		lines = append(lines, subtleStyle.Render(trimToWidth(line, width)))
	}
	return strings.Join(lines, "\n")
}

func (m *model) availableLogLines(statusLines int) int {
	if m.height <= 0 {
		return 14
	}
	available := m.height - statusLines - 12
	if available < 6 {
		return 6
	}
	return available
}

func (m *model) focusJob() (appRuntime.JobSnapshot, bool) {
	for _, job := range m.sortedJobs(len(m.jobs)) {
		if job.Status == "running" {
			return job, true
		}
	}
	for _, job := range m.sortedJobs(len(m.jobs)) {
		if job.Status == "queued" {
			return job, true
		}
	}
	items := m.sortedJobs(1)
	if len(items) == 0 {
		return appRuntime.JobSnapshot{}, false
	}
	return items[0], true
}

func (m *model) tailLogs(limit int) []string {
	if len(m.logs) <= limit {
		return append([]string(nil), m.logs...)
	}
	return append([]string(nil), m.logs[len(m.logs)-limit:]...)
}

func (m *model) sortedJobs(limit int) []appRuntime.JobSnapshot {
	items := make([]appRuntime.JobSnapshot, 0, len(m.jobs))
	for _, job := range m.jobs {
		items = append(items, job)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.After(items[j].CreatedAt)
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (m *model) pushLog(message string) {
	if strings.TrimSpace(message) == "" {
		return
	}
	timestamped := fmt.Sprintf("%s  %s", time.Now().Format("15:04:05"), message)
	m.logs = append(m.logs, timestamped)
	if len(m.logs) > 60 {
		m.logs = m.logs[len(m.logs)-60:]
	}
}

func (m *model) startApp() {
	if m.started {
		return
	}
	m.started = true
	go func() {
		reporter := bootstrap.Reporter{
			Log: func(message string) {
				m.events <- bootstrapLogMsg{message: message}
			},
			Progress: func(progress bootstrap.Progress) {
				m.events <- bootstrapProgressMsg{progress: progress}
			},
		}

		assets, err := bootstrap.EnsureDefaultRuntime(m.ctx, reporter)
		if err != nil {
			m.events <- startResultMsg{err: err}
			return
		}

		app := appRuntime.New(m.cfg, assets, func(event appRuntime.Event) {
			m.events <- runtimeEventMsg{event: event}
		})
		address, err := app.Start(m.ctx)
		m.events <- startResultMsg{app: app, address: address, err: err}
	}()
}

func (m *model) syncFocus() {
	inputs := []*textinput.Model{&m.hostInput, &m.portInput, &m.passwordInput}
	for index, input := range inputs {
		if index == m.focusIndex {
			input.Focus()
		} else {
			input.Blur()
		}
	}
}

func listen(events <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		return <-events
	}
}

func renderProgressBar(current, total int64, width int) string {
	if width < 10 {
		width = 10
	}
	if total <= 0 {
		return accent.Render("[" + strings.Repeat("•", width/3) + "]")
	}
	ratio := float64(current) / float64(total)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(width))
	if filled > width {
		filled = width
	}
	bar := accent.Render(strings.Repeat("█", filled)) + subtleStyle.Render(strings.Repeat("░", width-filled))
	return fmt.Sprintf("[%s] %3d%%", bar, int(ratio*100))
}

func trimToWidth(value string, width int) string {
	if width < 8 {
		width = 8
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	return string(runes[:width-3]) + "..."
}

func countLines(value string) int {
	if value == "" {
		return 0
	}
	return strings.Count(value, "\n") + 1
}

func phaseLabel(phase string) string {
	switch phase {
	case "vae":
		return "preparing"
	case "buffer":
		return "rendering"
	case "generate":
		return "finishing"
	default:
		return "rendering"
	}
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
