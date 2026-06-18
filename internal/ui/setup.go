package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mcp-manager/internal/config"
)

type SetupModel struct {
	inputs  []textinput.Model
	focused int
	err     error
	done    bool
}

func NewSetupModel() SetupModel {
	inputs := make([]textinput.Model, 1)

	inputs[0] = textinput.New()
	inputs[0].Placeholder = "Ngrok Auth Token"
	inputs[0].Focus()
	inputs[0].CharLimit = 100
	inputs[0].Width = 50

	return SetupModel{
		inputs:  inputs,
		focused: 0,
	}
}

func (m SetupModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m SetupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			return m, tea.Quit
		case "tab", "shift+tab", "enter", "up", "down":
			s := msg.String()

			if s == "enter" && m.focused == len(m.inputs)-1 {
				if err := config.SaveSecret("ngrok_token", m.inputs[0].Value()); err != nil {
					m.err = fmt.Errorf("failed to save secrets: %v", err)
					return m, nil
				}
				m.done = true
				return m, tea.Quit
			}

			// Cycle focus
			if s == "up" || s == "shift+tab" {
				m.focused--
			} else {
				m.focused++
			}

			if m.focused > len(m.inputs)-1 {
				m.focused = 0
			} else if m.focused < 0 {
				m.focused = len(m.inputs) - 1
			}

			cmds := make([]tea.Cmd, len(m.inputs))
			for i := 0; i < len(m.inputs); i++ {
				if i == m.focused {
					cmds[i] = m.inputs[i].Focus()
				} else {
					m.inputs[i].Blur()
				}
			}

			return m, tea.Batch(cmds...)
		}
	}

	cmds := make([]tea.Cmd, len(m.inputs))
	for i := range m.inputs {
		m.inputs[i], cmds[i] = m.inputs[i].Update(msg)
	}

	return m, tea.Batch(cmds...)
}

func (m SetupModel) View() string {
	var b strings.Builder

	b.WriteString("Please configure your Ngrok token for first run:\n\n")

	labels := []string{"1. Ngrok Token        : "}
	for i := range m.inputs {
		label := labels[i]
		if i == m.focused {
			b.WriteString(selectedItemStyle.Render(label) + m.inputs[i].View() + "\n")
		} else {
			b.WriteString(label + m.inputs[i].View() + "\n")
		}
	}

	b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Render("[Tab/Arrows] Navigate  •  [Enter] Submit  •  [Esc] Quit") + "\n")

	if m.err != nil {
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("#FF5555")).Bold(true).Render(fmt.Sprintf("Error: %v", m.err)) + "\n")
	}

	return renderBox("INITIAL SECRET SETUP", b.String())
}
