package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Messages sent from the job goroutine to the UI.
type msgFound struct{ n int }

type msgParse struct {
	done, total int
	path        string
}

type msgSkip struct {
	path string
	err  error
}
type msgTagStart struct{ tag int }
type msgTagDone struct{ tag int }
type msgDone struct{ err error }
type msgCancelled struct{}
type tickMsg struct{}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleActive = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	buttonStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("52"))
	buttonHot   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("88"))
	logLineSkip = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	logLineDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

var tagNames = map[int]string{
	0: "artist", 1: "album", 2: "genre", 3: "title", 4: "filename",
	5: "composer", 6: "comment", 7: "albumartist", 8: "grouping",
	12: "canonicalartist", -1: "master index (database_idx.tcd)",
}

var tagFiles = []int{-1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 12}

func fileName(tag int) string {
	if tag == -1 {
		return "database_idx.tcd"
	}
	return fmt.Sprintf("database_%d.tcd", tag)
}

type tuiModel struct {
	root     string
	cancelFn context.CancelFunc

	found, done, skipped int
	current              string
	start                time.Time

	tagState map[int]int // -1..12 -> 0 pending, 1 writing, 2 done

	logLines []string
	vp       viewport.Model
	bar      progress.Model

	width, height int
	btnX0, btnX1  int
	cancelling    bool
	finished      bool
	buildErr      error
	wasAtBottom   bool
}

func newTUIModel(root string, cancelFn context.CancelFunc) tuiModel {
	m := tuiModel{
		root:     root,
		cancelFn: cancelFn,
		start:    time.Now(),
		tagState: make(map[int]int),
		bar:      progress.New(progress.WithWidth(34)),
	}
	for _, t := range tagFiles {
		m.tagState[t] = 0
	}
	m.vp = viewport.New()
	m.vp.MouseWheelEnabled = true
	m.logLine(styleDim.Render("scanning " + root + " …"))
	return m
}

func (m *tuiModel) logf(format string, args ...any) {
	var line string
	if len(args) == 0 {
		line = format
	} else {
		line = fmt.Sprintf(format, args...)
	}
	m.logLines = append(m.logLines, line)
	if len(m.logLines) > 2000 {
		m.logLines = m.logLines[len(m.logLines)-1000:]
	}
	m.wasAtBottom = m.vp.AtBottom()
	m.vp.SetContent(strings.Join(m.logLines, "\n"))
	if m.wasAtBottom {
		m.vp.GotoBottom()
	}
}

// logLine appends an already-formatted line.
func (m *tuiModel) logLine(line string) {
	m.logLines = append(m.logLines, line)
	if len(m.logLines) > 2000 {
		m.logLines = m.logLines[len(m.logLines)-1000:]
	}
	m.wasAtBottom = m.vp.AtBottom()
	m.vp.SetContent(strings.Join(m.logLines, "\n"))
	if m.wasAtBottom {
		m.vp.GotoBottom()
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.vp.SetWidth(m.width - 2)
		m.vp.SetHeight(max(3, m.height/2-2))
		return m, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			if m.cancelFn != nil && !m.finished {
				m.cancelling = true
				m.cancelFn()
			}
			return m, tea.Quit
		case "c":
			if !m.cancelling && !m.finished {
				m.cancelling = true
				m.logLine(logLineDim.Render("cancel requested…"))
				m.cancelFn()
			}
			return m, nil
		default:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}

	case tea.MouseClickMsg:
		mouse := msg.Mouse()
		if msg.Button == tea.MouseLeft &&
			mouse.Y == m.height-1 && mouse.X >= m.btnX0 && mouse.X <= m.btnX1 {
			if !m.cancelling && !m.finished {
				m.cancelling = true
				m.cancelFn()
			} else if m.finished {
				return m, tea.Quit
			}
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case tea.MouseWheelMsg, tea.KeyMsg:
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd

	case msgFound:
		m.found = msg.n
		m.logf("found %d candidate audio files", msg.n)

	case msgParse:
		m.done, m.found = msg.done, msg.total
		m.current = msg.path
		if m.done%2000 == 0 {
			m.logLine(logLineDim.Render(fmt.Sprintf("parsed %d/%d", m.done, msg.total)))
		}

	case msgSkip:
		m.skipped++
		m.logLine(logLineSkip.Render(fmt.Sprintf("SKIP %s (%v)", msg.path, msg.err)))

	case msgTagStart:
		m.tagState[msg.tag] = 1
		name, _ := tagNames[msg.tag]
		m.logLine(logLineDim.Render("writing " + fileName(msg.tag) + " (" + name + ")"))

	case msgTagDone:
		m.tagState[msg.tag] = 2

	case msgCancelled:
		m.logLine(logLineSkip.Render("cancelled"))
		return m, tea.Quit

	case msgDone:
		m.finished = true
		m.buildErr = msg.err
		if msg.err != nil {
			m.logLine(logLineSkip.Render("FAILED: " + msg.err.Error()))
		} else {
			m.logLine(styleOK.Render(fmt.Sprintf("done: %d tracks written, %d skipped", m.done, m.skipped)))
		}
		return m, tea.Quit
	}
	return m, nil
}

func (m tuiModel) View() tea.View {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Rockbox Database Builder") + styleDim.Render("  ·  tagcache writer") + "\n")
	root := m.root
	if len(root) > m.width-10 && m.width > 20 {
		root = "…" + root[len(root)-(m.width-10):]
	}
	b.WriteString(styleDim.Render("root: ") + root + "\n\n")

	pct := 0.0
	if m.found > 0 {
		pct = float64(m.done) / float64(m.found)
	}
	status := "scanning"
	if m.found > 0 {
		status = fmt.Sprintf("%d/%d files", m.done, m.found)
	}
	if m.cancelling {
		status += "  (cancelling)"
	}
	b.WriteString("overall " + m.bar.ViewAs(pct) + "  " + status + "\n")
	b.WriteString(styleDim.Render("current: ") + truncate(m.current, max(0, m.width-12)) + "\n")
	b.WriteString(fmt.Sprintf("parsed %s  skipped %s  elapsed %s\n\n",
		styleOK.Render(fmt.Sprint(m.done)),
		styleBad.Render(fmt.Sprint(m.skipped)),
		styleDim.Render(time.Since(m.start).Round(time.Second).String())))

	b.WriteString(styleTitle.Render("output files") + "\n")
	for _, t := range tagFiles {
		glyph, st := "·", styleDim.Render("pending")
		switch m.tagState[t] {
		case 1:
			glyph, st = "▶", styleActive.Render("writing")
		case 2:
			glyph, st = "✔", styleOK.Render("done")
		}
		name, purpose := fileName(t), tagNames[t]
		b.WriteString(fmt.Sprintf(" %s %-18s %-16s %s\n", glyph, name, purpose, st))
	}

	half := "\nlog ─" + strings.Repeat("─", max(0, m.width-8)) + "\n"
	b.WriteString(half)

	// Reserve: header+table (~17 lines), log viewport, bottom button bar.
	reserved := 19
	vpH := max(3, m.height-reserved-1)
	m.vp.SetHeight(vpH)
	b.WriteString(m.vp.View() + "\n")

	label := "[ Cancel ]  ← click, or press c"
	if m.cancelling {
		label = "[ Cancelling… ]"
	}
	if m.finished {
		label = "[ Done ]  press q to exit"
	}
	styled := buttonStyle.Render(label)
	if m.cancelling {
		styled = buttonHot.Render(label)
	}
	line := "  " + styled
	m.btnX0, m.btnX1 = 2, 2+len([]rune(label))-1
	for len([]rune(line)) < m.width-1 {
		line += " "
	}
	b.WriteString(line)

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "rbdb — rockbox database builder"
	return v
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return "…" + string(r[len(r)-w+1:])
}

// runTUI drives the bubbletea interface; the job runs in a goroutine that
// streams progress messages into the program.
func runTUI(root string, job func(ctx context.Context, send func(any))) error {
	ctx, cancel := context.WithCancel(context.Background())
	p := tea.NewProgram(newTUIModel(root, cancel))
	go job(ctx, func(m any) { p.Send(m) })
	_, err := p.Run()
	cancel()
	return err
}
