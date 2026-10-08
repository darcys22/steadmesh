package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/darcys22/steadmesh/connectors/terminal"
)

// chatTUI runs the interactive chat: the conversation prints into the
// terminal's normal scrollback above an input box, and the representative's
// markdown is rendered.
func chatTUI(ctx context.Context, cl *chatClient, user, connKey string) error {
	return runChatTUI(ctx, newChatModel(ctx, cl, user, connKey, lipgloss.HasDarkBackground(os.Stdin, os.Stdout)))
}

func runChatTUI(ctx context.Context, m *chatModel, opts ...tea.ProgramOption) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cl := m.cl
	p := tea.NewProgram(m, append([]tea.ProgramOption{tea.WithContext(ctx)}, opts...)...)
	go cl.receive(ctx,
		func(ev terminal.Event) { p.Send(incomingMsg(ev)) },
		func(status string) { p.Send(statusMsg(status)) })
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

type (
	incomingMsg terminal.Event
	statusMsg   string
	sentMsg     struct{ err error }
)

type chatStyles struct {
	you, rep, time, status, errorLine, help lipgloss.Style
}

func newChatStyles(dark bool) chatStyles {
	ld := lipgloss.LightDark(dark)
	dim := ld(lipgloss.Color("244"), lipgloss.Color("243"))
	return chatStyles{
		you:       lipgloss.NewStyle().Bold(true).Foreground(ld(lipgloss.Color("25"), lipgloss.Color("39"))),
		rep:       lipgloss.NewStyle().Bold(true).Foreground(ld(lipgloss.Color("90"), lipgloss.Color("213"))),
		time:      lipgloss.NewStyle().Foreground(dim),
		status:    lipgloss.NewStyle().Foreground(dim).Italic(true),
		errorLine: lipgloss.NewStyle().Foreground(ld(lipgloss.Color("160"), lipgloss.Color("203"))),
		help:      lipgloss.NewStyle().Foreground(dim),
	}
}

type chatModel struct {
	ctx           context.Context
	cl            *chatClient
	user, connKey string
	dark          bool
	styles        chatStyles

	input    textarea.Model
	spinner  spinner.Model
	markdown *glamour.TermRenderer
	width    int

	waiting bool     // a message was sent and no reply has arrived yet
	history []string // sent messages, oldest first
	recall  int      // index into history while browsing with up/down
	draft   string   // input saved when history browsing started
}

func newChatModel(ctx context.Context, cl *chatClient, user, connKey string, dark bool) *chatModel {
	in := textarea.New()
	in.Placeholder = "Message your representative"
	in.ShowLineNumbers = false
	in.Prompt = "› "
	in.DynamicHeight = true
	in.MinHeight = 1
	in.MaxHeight = 8
	in.SetHeight(1)
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	st := textarea.DefaultStyles(dark)
	st.Focused.CursorLine = lipgloss.NewStyle()
	in.SetStyles(st)

	m := &chatModel{
		ctx: ctx, cl: cl, user: user, connKey: connKey, dark: dark, styles: newChatStyles(dark),
		input:   in,
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(newChatStyles(dark).status)),
		width:   80,
	}
	m.resize(80)
	return m
}

func (m *chatModel) Init() tea.Cmd {
	intro := m.styles.status.Render(fmt.Sprintf("Chatting with %s's representative over %s.", m.user, m.connKey))
	return tea.Batch(m.input.Focus(), tea.Println(intro))
}

func (m *chatModel) resize(width int) {
	m.width = max(width, 20)
	m.input.SetWidth(m.width)
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(map[bool]string{true: "dark", false: "light"}[m.dark]),
		glamour.WithWordWrap(m.width-4))
	if err == nil {
		m.markdown = r
	}
}

func (m *chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width)
		return m, nil

	case incomingMsg:
		m.waiting = false
		return m, tea.Println(m.header(m.styles.rep, "representative", msg.At) + "\n" + m.renderMarkdown(msg.Text))

	case statusMsg:
		return m, tea.Println(m.styles.status.Render("  " + string(msg)))

	case sentMsg:
		if msg.err != nil {
			m.waiting = false
			return m, tea.Println(m.styles.errorLine.Render("  not delivered: " + msg.err.Error()))
		}
		return m, nil

	case spinner.TickMsg:
		if !m.waiting {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "ctrl+d":
			if m.input.Value() == "" {
				return m, tea.Quit
			}
		case "enter":
			return m, m.submit()
		case "up":
			if m.input.Line() == 0 && m.browse(-1) {
				return m, nil
			}
		case "down":
			if m.input.Line() == m.input.LineCount()-1 && m.browse(1) {
				return m, nil
			}
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// submit prints the typed message into the conversation and sends it.
func (m *chatModel) submit() tea.Cmd {
	text := strings.TrimSpace(m.input.Value())
	if text == "" {
		return nil
	}
	m.input.Reset()
	m.history = append(m.history, text)
	m.recall, m.draft = len(m.history), ""
	start := !m.waiting
	m.waiting = true
	cl, ctx := m.cl, m.ctx
	cmds := []tea.Cmd{
		tea.Println(m.header(m.styles.you, "you", time.Now()) + "\n" + indent(ansi.Wrap(text, m.width-2, ""))),
		func() tea.Msg { return sentMsg{err: cl.send(ctx, text)} },
	}
	if start {
		cmds = append(cmds, m.spinner.Tick)
	}
	return tea.Sequence(cmds[0], tea.Batch(cmds[1:]...))
}

// browse moves through sent messages; it reports whether it changed the input.
func (m *chatModel) browse(delta int) bool {
	next := m.recall + delta
	if len(m.history) == 0 || next < 0 || next > len(m.history) || next == m.recall {
		return false
	}
	if m.recall == len(m.history) {
		m.draft = m.input.Value()
	}
	m.recall = next
	if next == len(m.history) {
		m.input.SetValue(m.draft)
	} else {
		m.input.SetValue(m.history[next])
	}
	return true
}

func (m *chatModel) header(style lipgloss.Style, name string, at time.Time) string {
	if at.IsZero() {
		at = time.Now()
	}
	return "\n" + style.Render(name) + " " + m.styles.time.Render(at.Local().Format("15:04"))
}

func (m *chatModel) renderMarkdown(text string) string {
	if m.markdown != nil {
		if out, err := m.markdown.Render(text); err == nil {
			return trimPadding(strings.Trim(out, "\n"))
		}
	}
	return indent(ansi.Wrap(text, m.width-2, ""))
}

// trailingPad matches the spaces (and the styling around them) that glamour
// pads each line with to the wrap width.
var trailingPad = regexp.MustCompile(`(?:[ \t]|\x1b\[[0-9;]*m)+$`)

// trimPadding removes trailing padding so copied text has no trailing
// spaces, resetting the style where padding was removed.
func trimPadding(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if t := trailingPad.ReplaceAllString(l, ""); t != l {
			lines[i] = t + "\x1b[0m"
		}
	}
	return strings.Join(lines, "\n")
}

func indent(s string) string {
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}

func (m *chatModel) View() tea.View {
	var b strings.Builder
	if m.waiting {
		b.WriteString(m.spinner.View() + m.styles.status.Render("waiting for the representative") + "\n")
	}
	b.WriteString(m.input.View() + "\n")
	b.WriteString(m.styles.help.Render("enter send · alt+enter new line · ↑ history · ctrl+c quit"))
	return tea.NewView(b.String())
}
