package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mcp-manager/internal/audit"
	"mcp-manager/internal/config"
	"mcp-manager/internal/process"
	"mcp-manager/internal/safe"
)

var (
	docStyle = lipgloss.NewStyle().Margin(1, 2)
	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFFFF")).
			Background(lipgloss.Color("#FF007F")). // Bright Pink
			Padding(0, 1).
			Bold(true)
	selectedItemStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#BD93F9")). // Purple
				Bold(true)
)

type uiState int

const (
	stateList uiState = iota
	stateDetails
	stateNewProject
	stateAuditLog
)

type projectItem struct {
	name, desc string
	isAction   bool
	actionType string // "add", "logs", "quit"
	project    config.ProjectConfig
}

func (p projectItem) Title() string       { return p.name }
func (p projectItem) Description() string { return p.desc }
func (p projectItem) FilterValue() string { return p.name }

type Model struct {
	list            list.Model
	state           uiState
	pm              *process.ProcessManager
	selectedProject config.ProjectConfig

	// New Project Form
	formInputs  []textinput.Model
	formFocused int
	formErr     error

	// Details View Messages
	detailsMsg string

	// Audit Logs
	auditEntries []string

	// Visibility toggle for Bearer Token
	revealToken bool
}

// Background Task Messages
type startProjectResult struct {
	url string
	err error
}

type stopProjectResult struct {
	err error
}

func startProjectCmd(pm *process.ProcessManager, proj config.ProjectConfig) tea.Cmd {
	return func() tea.Msg {
		url, err := pm.StartProject(proj)
		return startProjectResult{url: url, err: err}
	}
}

func stopProjectCmd(pm *process.ProcessManager, name string) tea.Cmd {
	return func() tea.Msg {
		err := pm.StopProject(name)
		return stopProjectResult{err: err}
	}
}

func NewModel(pm *process.ProcessManager) Model {
	m := Model{
		state: stateList,
		pm:    pm,
	}

	d := list.NewDefaultDelegate()
	d.Styles.SelectedTitle = selectedItemStyle
	d.Styles.SelectedDesc = selectedItemStyle.Copy().Foreground(lipgloss.Color("#8BE9FD")) // Cyan-ish

	l := list.New([]list.Item{}, d, 0, 0)
	l.Title = "MCP MANAGER"
	l.Styles.Title = titleStyle
	l.SetShowStatusBar(false)

	m.list = l
	m.refreshList()

	return m
}

func (m *Model) refreshList() {
	projects := config.GetProjects()
	var items []list.Item

	for _, p := range projects {
		status := "Stopped"
		url := m.pm.GetPublicURL(p.Name)
		if url != "" {
			status = "Running: " + url
		}
		items = append(items, projectItem{
			name:    p.Name,
			desc:    fmt.Sprintf("Path: %s | Tunnel: %s | Status: %s", p.Path, p.TunnelType, status),
			project: p,
		})
	}

	items = append(items, projectItem{name: "+ Add New Project", desc: "Create a new local MCP workspace configuration", isAction: true, actionType: "add"})
	items = append(items, projectItem{name: "View System Audit Logs", desc: "View tail end of gateway JSON-RPC packets", isAction: true, actionType: "logs"})
	items = append(items, projectItem{name: "Quit", desc: "Exit the application", isAction: true, actionType: "quit"})

	m.list.SetItems(items)
}

func (m *Model) refreshAuditLogs() {
	lines, err := audit.ReadLastEntries(15)
	if err != nil {
		m.auditEntries = []string{"Failed to read logs: " + err.Error()}
		return
	}

	var formatted []string
	for _, line := range lines {
		var entry audit.AuditEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			formatted = append(formatted, line)
			continue
		}

		timeStr := entry.Timestamp.Format("15:04:05")
		dirSymbol := "→"
		color := "#50FA7B" // Green (request)
		if entry.Direction == "response" {
			dirSymbol = "←"
			color = "#FF79C6" // Pink (response)
		}

		method := entry.Method
		if method == "" {
			method = "JSON-RPC"
		}

		payloadStr := string(entry.Payload)
		if len(payloadStr) > 55 {
			payloadStr = payloadStr[:52] + "..."
		}

		lineStr := fmt.Sprintf("[%s] %s %s %s",
			lipgloss.NewStyle().Foreground(lipgloss.Color("#6272A4")).Render(timeStr),
			lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Bold(true).Render(dirSymbol),
			lipgloss.NewStyle().Foreground(lipgloss.Color("#F1FA8C")).Render(method),
			lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2")).Render(payloadStr),
		)
		formatted = append(formatted, lineStr)
	}
	m.auditEntries = formatted
}

func (m *Model) initNewProjectForm() {
	inputs := make([]textinput.Model, 2)

	inputs[0] = textinput.New()
	inputs[0].Placeholder = "Project Name (e.g. workspace1)"
	inputs[0].Focus()
	inputs[0].CharLimit = 50

	inputs[1] = textinput.New()
	inputs[1].Placeholder = "Absolute Path (e.g. C:\\projects\\workspace1)"
	inputs[1].CharLimit = 150

	m.formInputs = inputs
	m.formFocused = 0
	m.formErr = nil
}

func (m Model) Init() tea.Cmd {
	return nil
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

		switch m.state {
		case stateList:
			switch msg.String() {
			case "q":
				return m, tea.Quit
			case "esc":
				return m, nil // Consume Esc in list view so it does not clear filter or reset
			case "enter":
				sel, ok := m.list.SelectedItem().(projectItem)
				if ok {
					if sel.isAction {
						switch sel.actionType {
						case "add":
							m.initNewProjectForm()
							m.state = stateNewProject
							return m, nil
						case "logs":
							m.refreshAuditLogs()
							m.state = stateAuditLog
							return m, nil
						case "quit":
							return m, tea.Quit
						}
					} else {
						m.selectedProject = sel.project
						m.detailsMsg = ""
						m.state = stateDetails
						return m, nil
					}
				}
			}

		case stateDetails:
			switch msg.String() {
			case "esc":
				m.refreshList()
				m.state = stateList
				return m, nil
			case "enter":
				m.detailsMsg = "Starting/stopping project proxy & tunnel..."
				url := m.pm.GetPublicURL(m.selectedProject.Name)
				if url == "" {
					return m, startProjectCmd(m.pm, m.selectedProject)
				} else {
					return m, stopProjectCmd(m.pm, m.selectedProject.Name)
				}
			case "u", "U":
				url := m.pm.GetPublicURL(m.selectedProject.Name)
				if url != "" {
					targetSSE := url + "/sse"
					if err := copyToClipboard(targetSSE); err == nil {
						m.detailsMsg = "Copied SSE URL to clipboard!"
					} else {
						m.detailsMsg = "Failed to copy: " + err.Error()
					}
				}
			case "c", "C":
				url := m.pm.GetPublicURL(m.selectedProject.Name)
				if url != "" {
					adminToken, _ := config.GetSecret("admin_token")
					targetSSE := url + "/sse?token=" + adminToken
					if err := copyToClipboard(targetSSE); err == nil {
						m.detailsMsg = "Copied Claude URL to clipboard!"
					} else {
						m.detailsMsg = "Failed to copy: " + err.Error()
					}
				}
			case "t", "T":
				adminToken, _ := config.GetSecret("admin_token")
				if adminToken != "" {
					if err := copyToClipboard(adminToken); err == nil {
						m.detailsMsg = "Copied Bearer Token to clipboard!"
					} else {
						m.detailsMsg = "Failed to copy: " + err.Error()
					}
				}
			case "h", "H":
				adminToken, _ := config.GetSecret("admin_token")
				if adminToken != "" {
					if err := copyToClipboard("Bearer " + adminToken); err == nil {
						m.detailsMsg = "Copied Auth Header to clipboard!"
					} else {
						m.detailsMsg = "Failed to copy: " + err.Error()
					}
				}
			case "v", "V":
				m.revealToken = !m.revealToken
				if m.revealToken {
					m.detailsMsg = "Token revealed!"
				} else {
					m.detailsMsg = "Token hidden!"
				}
			}
			return m, nil

		case stateNewProject:
			switch msg.String() {
			case "esc":
				m.state = stateList
				return m, nil
			case "tab", "shift+tab", "up", "down":
				s := msg.String()
				if s == "up" || s == "shift+tab" {
					m.formFocused--
				} else {
					m.formFocused++
				}

				if m.formFocused > len(m.formInputs)-1 {
					m.formFocused = 0
				} else if m.formFocused < 0 {
					m.formFocused = len(m.formInputs) - 1
				}

				cmds := make([]tea.Cmd, len(m.formInputs))
				for i := range m.formInputs {
					if i == m.formFocused {
						cmds[i] = m.formInputs[i].Focus()
					} else {
						m.formInputs[i].Blur()
					}
				}
				return m, tea.Batch(cmds...)

			case "enter":
				if m.formFocused < len(m.formInputs)-1 {
					m.formFocused++
					cmds := make([]tea.Cmd, len(m.formInputs))
					for i := range m.formInputs {
						if i == m.formFocused {
							cmds[i] = m.formInputs[i].Focus()
						} else {
							m.formInputs[i].Blur()
						}
					}
					return m, tea.Batch(cmds...)
				}

				name := strings.TrimSpace(m.formInputs[0].Value())
				path := strings.TrimSpace(m.formInputs[1].Value())

				if name == "" || path == "" {
					m.formErr = fmt.Errorf("project name and path are required")
					return m, nil
				}

				err := config.SaveProject(config.ProjectConfig{
					Name:       name,
					Path:       path,
					TunnelType: "ngrok",
				})
				if err != nil {
					m.formErr = err
					return m, nil
				}

				m.refreshList()
				m.state = stateList
				return m, nil
			}

			var cmd tea.Cmd
			m.formInputs[m.formFocused], cmd = m.formInputs[m.formFocused].Update(msg)
			return m, cmd

		case stateAuditLog:
			switch msg.String() {
			case "esc":
				m.state = stateList
				return m, nil
			case "r", "R":
				m.refreshAuditLogs()
				return m, nil
			}
			return m, nil
		}

	// Async command responses
	case startProjectResult:
		if msg.err != nil {
			m.detailsMsg = "Error starting project: " + msg.err.Error()
		} else {
			m.detailsMsg = "Project started successfully!"
		}
		m.refreshList()
		return m, nil

	case stopProjectResult:
		if msg.err != nil {
			m.detailsMsg = "Error stopping project: " + msg.err.Error()
		} else {
			m.detailsMsg = "Project stopped successfully!"
		}
		m.refreshList()
		return m, nil

	case tea.WindowSizeMsg:
		h, v := docStyle.GetFrameSize()
		m.list.SetSize(msg.Width-h, msg.Height-v)
	}

	// Sub-model updates
	var cmd tea.Cmd
	if m.state == stateList {
		m.list, cmd = m.list.Update(msg)
	} else if m.state == stateNewProject {
		cmds := make([]tea.Cmd, len(m.formInputs))
		for i := range m.formInputs {
			m.formInputs[i], cmds[i] = m.formInputs[i].Update(msg)
		}
		cmd = tea.Batch(cmds...)
	}

	return m, cmd
}

func maskToken(token string) string {
	if len(token) <= 12 {
		return "****"
	}
	prefix := "bearer_"
	if strings.HasPrefix(token, prefix) {
		suffixLen := 4
		if len(token) > len(prefix)+suffixLen {
			return prefix + "****" + token[len(token)-suffixLen:]
		}
	}
	return token[:4] + "****" + token[len(token)-4:]
}

func renderBox(title string, content string) string {
	lines := strings.Split(content, "\n")
	maxWidth := lipgloss.Width(title) + 4
	for _, l := range lines {
		w := lipgloss.Width(l)
		if w > maxWidth {
			maxWidth = w
		}
	}

	titleWidth := lipgloss.Width(titleStyle.Render(" "+title+" "))
	var b strings.Builder
	topBorder := "╭" + titleStyle.Render(" "+title+" ") + strings.Repeat("─", maxWidth-titleWidth+2) + "╮"
	b.WriteString(topBorder + "\n")

	for _, l := range lines {
		w := lipgloss.Width(l)
		padding := maxWidth - w
		b.WriteString("│ " + l + strings.Repeat(" ", padding) + " │\n")
	}

	b.WriteString("╰" + strings.Repeat("─", maxWidth) + "╯")
	return b.String()
}

func (m Model) View() string {
	switch m.state {
	case stateList:
		return docStyle.Render(m.list.View())
	case stateDetails:
		return m.detailsView()
	case stateNewProject:
		return m.newProjectView()
	case stateAuditLog:
		return m.auditLogView()
	default:
		return ""
	}
}

func (m Model) detailsView() string {
	p := m.selectedProject
	status := "Stopped"
	statusColor := "#FF5555" // Red
	url := m.pm.GetPublicURL(p.Name)
	if url != "" {
		status = "Running"
		statusColor = "#50FA7B" // Green
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Local Workspace Path : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#F1FA8C")).Render(p.Path)))
	b.WriteString(fmt.Sprintf("Secure Tunnel Type   : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#BD93F9")).Render(p.TunnelType)))
	b.WriteString(fmt.Sprintf("Status               : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color(statusColor)).Bold(true).Render(status)))

	if url != "" {
		b.WriteString(fmt.Sprintf("Public Gateway URL   : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Render(url)))
		adminToken, _ := config.GetSecret("admin_token")
		displayToken := maskToken(adminToken)
		if m.revealToken {
			displayToken = adminToken
		}
		b.WriteString(fmt.Sprintf("Bearer Token         : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#FF79C6")).Render(displayToken)))

		// Notion Connection Tutorial
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C")).Bold(true).Render("Notion Integration Tutorial:") + "\n")
		b.WriteString("  1. In Notion, go to Settings -> Connections -> Add MCP Server\n")
		b.WriteString(fmt.Sprintf("  2. Select Connection Type: %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#50FA7B")).Render("SSE")))
		
		targetSSE := url + "/sse"
		b.WriteString(fmt.Sprintf("  3. Set Server URL        : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Render(targetSSE)))
		b.WriteString("  4. Set Auth Type         : Choose 'Header-based / Custom Header' or 'Bearer Token'\n")
		b.WriteString("     • If using Header-based / Custom Header:\n")
		b.WriteString(fmt.Sprintf("       Header Name         : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#F1FA8C")).Render("Authorization")))
		b.WriteString(fmt.Sprintf("       Header Value        : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#FF79C6")).Render("Bearer "+displayToken)))
		b.WriteString("     • If using Bearer Token (if supported natively):\n")
		b.WriteString(fmt.Sprintf("       Token               : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#FF79C6")).Render(displayToken)))

		// Claude Connection Tutorial
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C")).Bold(true).Render("Claude Integration Tutorial:") + "\n")
		b.WriteString("  1. In Claude, go to Settings -> Connectors -> Add custom connector\n")
		b.WriteString(fmt.Sprintf("  2. Set Name              : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#50FA7B")).Render(p.Name)))
		b.WriteString(fmt.Sprintf("  3. Set Remote MCP URL    : %s\n", lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Render(targetSSE+"?token="+displayToken)))
		b.WriteString("  4. Click 'Add' (Leave OAuth settings blank)\n")
	}


	b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#BD93F9")).Bold(true).Render("Controls:") + "\n")
	if url == "" {
		b.WriteString("  [Enter] Start Project Proxy & Tunnel\n")
	} else {
		b.WriteString("  [Enter] Stop Project Proxy & Tunnel\n")
		b.WriteString("  [U]     Copy SSE URL to Clipboard\n")
		b.WriteString("  [C]     Copy Claude URL (with token) to Clipboard\n")
		b.WriteString("  [T]     Copy Bearer Token to Clipboard\n")
		b.WriteString("  [H]     Copy Authorization Header to Clipboard\n")
		b.WriteString("  [V]     Toggle Token Visibility\n")
	}
	b.WriteString("  [Esc]   Back to Project List\n")

	if m.detailsMsg != "" {
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#F1FA8C")).Italic(true).Render(m.detailsMsg) + "\n")
	}

	return renderBox("PROJECT DETAILS: "+strings.ToUpper(p.Name), b.String())
}

func (m Model) newProjectView() string {
	var b strings.Builder

	labels := []string{"1. Project Name               : ", "2. Local Path                 : "}
	for i := range m.formInputs {
		label := labels[i]
		if i == m.formFocused {
			b.WriteString(selectedItemStyle.Render(label) + m.formInputs[i].View() + "\n")
		} else {
			b.WriteString(label + m.formInputs[i].View() + "\n")
		}
	}

	b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Render("[Tab/Arrows] Navigate  •  [Enter] Submit  •  [Esc] Cancel") + "\n")

	if m.formErr != nil {
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5555")).Bold(true).Render(fmt.Sprintf("Error: %v", m.formErr)) + "\n")
	}

	return renderBox("ADD NEW CONFIGURATION", b.String())
}

func (m Model) auditLogView() string {
	var b strings.Builder

	if len(m.auditEntries) == 0 {
		b.WriteString("No logs recorded yet.\n")
	} else {
		for _, entry := range m.auditEntries {
			b.WriteString(entry + "\n")
		}
	}

	b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Render("[Esc] Back  •  [R] Refresh") + "\n")
	return renderBox("GATEWAY AUDIT LOGS", b.String())
}

// NewProgram initializes and returns the Bubble Tea program with the specified process manager
func NewProgram(pm *process.ProcessManager) *tea.Program {
	return tea.NewProgram(NewModel(pm), tea.WithAltScreen())
}

func copyToClipboard(text string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("clip")
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "linux":
		cmd = exec.Command("xclip", "-selection", "clipboard")
	default:
		return fmt.Errorf("unsupported platform")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}

	safe.Go(func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, text)
	})

	return cmd.Run()
}

