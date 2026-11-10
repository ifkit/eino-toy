package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

var answerStyle = func() ansi.StyleConfig {
	s := styles.NoTTYStyleConfig
	for _, h := range []*ansi.StyleBlock{&s.H1, &s.H2, &s.H3, &s.H4, &s.H5, &s.H6} {
		h.Prefix, h.Bold = "", new(true)
	}
	s.Strong = ansi.StylePrimitive{Bold: new(true)}
	s.Emph = ansi.StylePrimitive{Italic: new(true)}
	s.Code.Prefix, s.Code.Suffix = "", ""
	return s
}()

const systemPrompt = "You are a helpful assistant. Answer in Markdown."

var (
	styleMeta = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleUser = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleErr  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

type role int

const (
	sentSystem role = iota // sent to the API: shown gray
	sentUser
	thinking
	answer // rendered as Markdown
	failure
	debugKeys
)

type entry struct {
	role role
	text string
}

type streamMsg struct {
	sr *schema.StreamReader[*schema.Message]
}
type (
	chunkMsg struct{ msg *schema.Message }
	doneMsg  struct{ err error }
)

type app struct {
	chatModel model.BaseChatModel
	history   []*schema.Message // exactly what the API receives
	entries   []entry
	think     int // index of the thinking entry of the current turn, -1 if none
	reply     int // index of the answer entry of the current turn, -1 if none
	sr        *schema.StreamReader[*schema.Message]
	busy      bool
	input     textinput.Model
	view      viewport.Model
	md        *glamour.TermRenderer
	ready     bool
	debug     bool
	hideMeta  bool // hides system and thinking entries
}

func newApp(cm model.BaseChatModel, debug bool) *app {
	in := textinput.New()
	in.Prompt = "> "
	return &app{
		chatModel: cm,
		history:   []*schema.Message{schema.SystemMessage(systemPrompt)},
		entries:   []entry{{sentSystem, "system: " + systemPrompt}},
		think:     -1,
		reply:     -1,
		input:     in,
		debug:     debug,
	}
}

func (a *app) Init() tea.Cmd {
	return tea.Batch(a.input.Focus(), textinput.Blink)
}

func recv(sr *schema.StreamReader[*schema.Message]) tea.Cmd {
	return func() tea.Msg {
		m, err := sr.Recv()
		if err != nil {
			sr.Close()
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return doneMsg{err}
		}
		return chunkMsg{m}
	}
}

func (a *app) start() tea.Cmd {
	history := a.history
	return func() tea.Msg {
		sr, err := a.chatModel.Stream(context.Background(), history)
		if err != nil {
			return doneMsg{err}
		}
		return streamMsg{sr}
	}
}

// appendTo grows the entry at *idx, creating it on first use.
func (a *app) appendTo(idx *int, r role, s string) {
	if *idx < 0 {
		a.entries = append(a.entries, entry{r, ""})
		*idx = len(a.entries) - 1
	}
	a.entries[*idx].text += s
}

func (a *app) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	if a.debug {
		a.debugInput(msg)
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.view = viewport.New(viewport.WithWidth(msg.Width-1), viewport.WithHeight(msg.Height-2))
		a.input.SetWidth(msg.Width - 3)
		a.md, _ = glamour.NewTermRenderer(glamour.WithStyles(answerStyle), glamour.WithWordWrap(msg.Width-3))
		a.ready = true
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return a, tea.Quit
		case "ctrl+h":
			// Return early: textinput treats ctrl+h as backspace.
			a.hideMeta = !a.hideMeta
			a.refresh()
			return a, nil
		case "enter":
			text := strings.TrimSpace(a.input.Value())
			if a.busy || text == "" {
				break
			}
			a.input.Reset()
			a.history = append(a.history, schema.UserMessage(text))
			a.entries = append(a.entries, entry{sentUser, "user: " + text})
			a.think, a.reply, a.busy = -1, -1, true
			cmds = append(cmds, a.start())
		}
	case streamMsg:
		a.sr = msg.sr
		cmds = append(cmds, recv(a.sr))
	case chunkMsg:
		if msg.msg.ReasoningContent != "" {
			a.appendTo(&a.think, thinking, msg.msg.ReasoningContent)
		}
		if msg.msg.Content != "" {
			a.appendTo(&a.reply, answer, msg.msg.Content)
		}
		cmds = append(cmds, recv(a.sr))
	case doneMsg:
		a.busy = false
		if msg.err != nil {
			a.entries = append(a.entries, entry{failure, "error: " + msg.err.Error()})
		} else if a.reply >= 0 {
			a.history = append(a.history, schema.AssistantMessage(a.entries[a.reply].text, nil))
		}
	}

	if !a.ready {
		return a, nil
	}
	var c tea.Cmd
	a.input, c = a.input.Update(msg)
	cmds = append(cmds, c)
	a.view, c = a.view.Update(msg)
	cmds = append(cmds, c)
	a.refresh()
	return a, tea.Batch(cmds...)
}

func (a *app) debugInput(msg tea.Msg) {
	var s string
	switch m := msg.(type) {
	case tea.KeyPressMsg:
		s = fmt.Sprintf("key:%q", m.String())
	case tea.MouseMsg:
		s = "mouse:" + m.String()
	default:
		return
	}
	if n := len(a.entries); a.entries[n-1].role == debugKeys {
		a.entries[n-1].text += "  " + s
	} else {
		a.entries = append(a.entries, entry{debugKeys, "debug: " + s})
	}
}

func (a *app) refresh() {
	atEnd := a.view.AtBottom()
	var parts []string
	w := a.view.Width()
	for _, e := range a.entries {
		if a.hideMeta && (e.role == sentSystem || e.role == thinking) {
			continue
		}
		switch e.role {
		case debugKeys:
			parts = append(parts, styleMeta.Width(w).Render(e.text))
		case sentSystem:
			parts = append(parts, styleMeta.Width(w).Render(e.text))
		case sentUser:
			parts = append(parts, styleUser.Width(w).Render(e.text))
		case thinking:
			parts = append(parts, styleMeta.Width(w).Render(e.text))
		case failure:
			parts = append(parts, styleErr.Width(w).Render(e.text))
		case answer:
			out, err := a.md.Render(e.text)
			if err != nil {
				out = e.text
			}
			parts = append(parts, strings.TrimRight(out, "\n"))
		}
	}
	a.view.SetContent(strings.Join(parts, "\n\n"))
	if atEnd {
		a.view.GotoBottom()
	}
}

func (a *app) View() tea.View {
	v := tea.NewView(a.content())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (a *app) content() string {
	if !a.ready {
		return "starting..."
	}
	status := "PgUp/PgDn, wheel: scroll | Enter: send | Ctrl+H: hide system/thinking | Ctrl+C: quit"
	if a.busy {
		status = "waiting for model..."
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, a.view.View(), a.scrollbar()) + "\n" + styleMeta.Render(status) + "\n" + a.input.View()
}

func (a *app) scrollbar() string {
	h, total := a.view.Height(), a.view.TotalLineCount()
	thumb, top := h, 0
	if total > h {
		thumb = max(1, h*h/total)
		top = int(a.view.ScrollPercent() * float64(h-thumb))
	}
	rows := make([]string, h)
	for i := range rows {
		if i >= top && i < top+thumb {
			rows[i] = "█"
		} else {
			rows[i] = styleMeta.Render("│")
		}
	}
	return strings.Join(rows, "\n")
}

func main() {
	var (
		url   = flag.String("openai", "http://localhost:1234/v1", "OpenAI-compatible API base URL")
		name  = flag.String("model", "current", "Model name")
		key   = flag.String("key", "", "API key")
		debug = flag.Bool("debug", false, "Debug mode")
		agent = flag.String("acp", "", "ACP agent command, e.g. \"npx @zed-industries/claude-code-acp\" (replaces --openai)")
	)
	flag.Parse()

	var (
		cm  model.BaseChatModel
		err error
	)
	if *agent != "" {
		cwd, _ := os.Getwd()
		var a *acpModel
		if a, err = newACP(context.Background(), strings.Fields(*agent), cwd); err == nil {
			defer a.Close()
			cm = a
		}
	} else {
		cm, err = openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
			BaseURL: *url, APIKey: *key, Model: *name,
		})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := tea.NewProgram(newApp(cm, *debug)).Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
