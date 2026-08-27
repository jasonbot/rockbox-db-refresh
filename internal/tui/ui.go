package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	bubblesprogress "charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"rbdb/internal/progress"
	"rbdb/internal/shuffle"
)

// tickMsg drives periodic UI refreshes.
type tickMsg struct{}

// finishedMsg is sent after a brief delay so the TUI can render the
// final state before the program exits.
type finishedMsg struct{}

var (
	styleTitle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleActive = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	buttonStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("124"))
	buttonHot   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("196"))
	buttonDone  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("34"))
	logLineSkip = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	logLineDim  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
)

// TagNames maps tag ids to display names.
var TagNames = map[int]string{
	0: "artist", 1: "album", 2: "genre", 3: "title", 4: "filename",
	5: "composer", 6: "comment", 7: "albumartist", 8: "grouping",
	12: "canonicalartist", -1: "master index (database_idx.tcd)",
}

var tagFiles = []int{-1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 12}

// FileName returns the on-device file name for a tag (-1 = master index).
func FileName(tag int) string {
	if tag == -1 {
		return "database_idx.tcd"
	}
	return fmt.Sprintf("database_%d.tcd", tag)
}

// baseTUI holds shared state and methods for all TUI models.
type baseTUI struct {
	cancelFn    context.CancelFunc
	found       int
	done        int
	skipped     int
	failed      int
	current     string
	start       time.Time
	logLines    []string
	vp          viewport.Model
	bar         bubblesprogress.Model
	width       int
	height      int
	btnX0       int
	btnX1       int
	btnY        int
	cancelling  bool
	finished    bool
	buildErr    error
	wasAtBottom bool
}

func newBaseTUI(cancelFn context.CancelFunc) baseTUI {
	m := baseTUI{
		cancelFn: cancelFn,
		start:    time.Now(),
		bar:      bubblesprogress.New(bubblesprogress.WithWidth(34)),
	}
	m.vp = viewport.New()
	m.vp.MouseWheelEnabled = true
	return m
}

func (m *baseTUI) logf(format string, args ...any) {
	m.logLine(fmt.Sprintf(format, args...))
}

func (m *baseTUI) logLine(line string) {
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

func (m *baseTUI) handleTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *baseTUI) handleWindowSize(msg tea.WindowSizeMsg) {
	m.width, m.height = msg.Width, msg.Height
	m.vp.SetWidth(m.width - 2)
}

func (m *baseTUI) handleKeyPress(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	if m.finished {
		return tea.Quit, true
	}
	switch msg.String() {
	case "q", "ctrl+c":
		if m.cancelFn != nil && !m.cancelling {
			m.cancelling = true
			m.cancelFn()
		}
		return nil, true
	case "c":
		if !m.cancelling {
			m.cancelling = true
			m.logLine(logLineDim.Render("cancel requested…"))
			m.cancelFn()
		}
		return nil, true
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd, false
}

func (m *baseTUI) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	mouse := msg.Mouse()
	if msg.Button == tea.MouseLeft &&
		mouse.Y == m.btnY && mouse.X >= m.btnX0 && mouse.X <= m.btnX1 {
		if !m.cancelling && !m.finished {
			m.cancelling = true
			m.cancelFn()
		} else if m.finished {
			return tea.Quit
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd
}

func (m *baseTUI) handleMouseWheelKey(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return cmd
}

func (m *baseTUI) handleCancelled() tea.Cmd {
	m.logLine(logLineSkip.Render("cancelled"))
	return tea.Quit
}

func (m *baseTUI) handleDone(msg progress.Done) tea.Cmd {
	m.finished = true
	m.buildErr = msg.Err
	if msg.Err != nil {
		m.logLine(logLineSkip.Render("FAILED: " + msg.Err.Error()))
	}
	return m.handleTick()
}

func (m *baseTUI) renderProgressBar() string {
	pct := 0.0
	if m.found > 0 {
		pct = float64(m.done) / float64(m.found)
	}
	status := "scanning"
	if m.found > 0 {
		status = fmt.Sprintf("%d/%d files", m.done, m.found)
	}
	line := "overall " + m.bar.ViewAs(pct) + "  " + status
	if rem := eta(time.Since(m.start), m.done, m.found); rem != "" {
		line += styleDim.Render(fmt.Sprintf("  ·  eta %s", rem))
	}
	if m.cancelling {
		line += "  (cancelling)"
	}
	return line
}

func (m *baseTUI) renderCurrentLine() string {
	return styleDim.Render("current: ") + truncate(m.current, max(0, m.width-12))
}

func (m *baseTUI) renderLogAndButton(linesAbove int) string {
	var b strings.Builder
	b.WriteString("log ─" + strings.Repeat("─", max(0, m.width-8)) + "\n")

	vpH := max(3, m.height-linesAbove-2)

	n := len(m.logLines)
	start := 0
	if n > vpH {
		start = n - vpH
	}
	for _, line := range m.logLines[start:] {
		b.WriteString(line + "\n")
	}
	for remaining := vpH - (n - start); remaining > 0; remaining-- {
		b.WriteString("\n")
	}

	m.btnY = linesAbove + 1 + vpH

	label := "  ✖  CANCEL   press c · or click  "
	if m.cancelling {
		label = "  ⏳ CANCELLING…  "
	}
	if m.finished {
		if m.buildErr != nil {
			label = "  ✖  FAILED   press any key to exit  "
		} else {
			label = "  ✔  DONE   press any key to exit  "
		}
	}
	style := buttonStyle
	if m.cancelling {
		style = buttonHot
	}
	if m.finished {
		if m.buildErr != nil {
			style = buttonHot
		} else {
			style = buttonDone
		}
	}
	styled := style.Bold(true).Render(label)
	runes := []rune(label)
	pad := max(0, m.width-len(runes))
	left := pad / 2
	btnLine := strings.Repeat(" ", left) + styled
	m.btnX0, m.btnX1 = left, left+len(runes)-1
	for len([]rune(btnLine)) < m.width {
		btnLine += " "
	}
	b.WriteString(btnLine)
	return b.String()
}

// eta estimates remaining time from the average parse rate so far.
func eta(elapsed time.Duration, done, total int) string {
	if done <= 0 || total <= 0 || done >= total || elapsed <= 0 {
		return ""
	}
	rate := float64(done) / elapsed.Seconds()
	if rate <= 0 {
		return ""
	}
	return (time.Duration(float64(total-done)/rate) * time.Second).Round(time.Second).String()
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

// ---- DB model (original) ---------------------------------------------------

type tuiModel struct {
	base         baseTUI
	root         string
	refresh      bool
	kept         int
	refreshStats *progress.Refresh
	tagState     map[int]int
	files        table.Model
}

func newTUIModel(root string, cancelFn context.CancelFunc, refresh bool) tuiModel {
	m := tuiModel{
		base:     newBaseTUI(cancelFn),
		root:     root,
		refresh:  refresh,
		tagState: make(map[int]int),
	}
	for _, t := range tagFiles {
		m.tagState[t] = 0
	}

	cols := []table.Column{
		{Title: " ", Width: 2},
		{Title: "output file", Width: 18},
		{Title: "tag", Width: 17},
		{Title: "status", Width: 9},
	}
	w := 0
	for _, c := range cols {
		w += c.Width + 2
	}
	m.files = table.New(
		table.WithColumns(cols),
		table.WithHeight(len(tagFiles)+1),
		table.WithWidth(w),
	)
	st := table.DefaultStyles()
	st.Selected = lipgloss.NewStyle()
	m.files.SetStyles(st)

	m.base.logLine(styleDim.Render("scanning " + root + " …"))
	return m
}

func (m tuiModel) Init() tea.Cmd {
	return m.base.handleTick()
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, m.base.handleTick()

	case tea.WindowSizeMsg:
		m.base.handleWindowSize(msg)
		return m, nil

	case tea.KeyPressMsg:
		cmd, handled := m.base.handleKeyPress(msg)
		if handled {
			return m, cmd
		}
		return m, cmd

	case tea.MouseClickMsg:
		return m, m.base.handleMouseClick(msg)

	case tea.MouseWheelMsg, tea.KeyMsg:
		return m, m.base.handleMouseWheelKey(msg)

	case progress.Found:
		m.base.found = msg.N
		m.base.logf("found %d candidate audio files", msg.N)

	case progress.Parse:
		m.base.done, m.base.found = msg.Done, msg.Total
		m.base.current = msg.Path
		if msg.Reused {
			m.kept++
		}
		if m.base.done%2000 == 0 {
			m.base.logLine(logLineDim.Render(fmt.Sprintf("parsed %d/%d", m.base.done, m.base.found)))
		}

	case progress.Skip:
		m.base.skipped++
		m.base.logLine(logLineSkip.Render(fmt.Sprintf("SKIP %s (%v)", msg.Path, msg.Err)))

	case progress.TagStart:
		m.tagState[msg.Tag] = 1
		name, _ := TagNames[msg.Tag]
		m.base.logLine(logLineDim.Render("writing " + FileName(msg.Tag) + " (" + name + ")"))

	case progress.TagDone:
		m.tagState[msg.Tag] = 2

	case progress.Shuffle:
		if msg.Err != nil {
			m.base.logLine(logLineSkip.Render(fmt.Sprintf("SHUFFLE FAILED: %v", msg.Err)))
		} else {
			m.base.logLine(styleOK.Render(fmt.Sprintf("shuffled playlist installed: %d tracks (%s)", msg.N, shuffle.PlaylistFile)))
		}

	case progress.Refresh:
		m.refreshStats = &msg
		if msg.Removed > 0 || msg.Added > 0 || msg.Updated > 0 {
			m.base.logf("refresh delta: kept %d · updated %d · added %d · removed %d",
				msg.Kept, msg.Updated, msg.Added, msg.Removed)
		}

	case progress.Banner:
		for _, line := range msg.Lines {
			m.base.logLine(styleDim.Render(line))
		}

	case progress.Cancelled:
		return m, m.base.handleCancelled()

	case progress.Done:
		m.base.finished = true
		m.base.buildErr = msg.Err
		if msg.Err != nil {
			m.base.logLine(logLineSkip.Render("FAILED: " + msg.Err.Error()))
		} else if m.refreshStats != nil {
			rs := m.refreshStats
			m.base.logLine(styleOK.Render(fmt.Sprintf(
				"done: %d tracks written (%d kept, %d updated, %d added, %d removed), %d skipped",
				m.base.done, rs.Kept, rs.Updated, rs.Added, rs.Removed, m.base.skipped)))
		} else {
			m.base.logLine(styleOK.Render(fmt.Sprintf("done: %d tracks written, %d skipped", m.base.done, m.base.skipped)))
		}
		return m, m.base.handleTick()
	}
	return m, nil
}

func (m tuiModel) View() tea.View {
	var b strings.Builder
	subtitle := "  ·  tagcache writer"
	if m.refresh {
		subtitle = "  ·  incremental update"
	}
	b.WriteString(styleTitle.Render("Rockbox Database Builder") + styleDim.Render(subtitle) + "\n")
	root := m.root
	if len(root) > m.base.width-10 && m.base.width > 20 {
		root = "…" + root[len(root)-(m.base.width-10):]
	}
	b.WriteString(styleDim.Render("root: ") + root + "\n\n")

	b.WriteString(m.base.renderProgressBar() + "\n")
	b.WriteString(m.base.renderCurrentLine() + "\n")
	stats := fmt.Sprintf("parsed %s  skipped %s  elapsed %s",
		styleOK.Render(fmt.Sprint(m.base.done)),
		styleBad.Render(fmt.Sprint(m.base.skipped)),
		styleDim.Render(time.Since(m.base.start).Round(time.Second).String()))
	if m.refresh {
		stats += styleDim.Render(fmt.Sprintf("  kept %s", styleOK.Render(fmt.Sprint(m.kept))))
	}
	b.WriteString(stats + "\n")

	b.WriteString(styleTitle.Render("output files") + "\n")
	rows := make([]table.Row, 0, len(tagFiles))
	for _, t := range tagFiles {
		glyph, st := "·", styleDim.Render("pending")
		switch m.tagState[t] {
		case 1:
			glyph, st = "▶", styleActive.Render("writing")
		case 2:
			glyph, st = "✔", styleOK.Render("done")
		}
		name, purpose := FileName(t), TagNames[t]
		rows = append(rows, table.Row{glyph, name, purpose, st})
	}
	m.files.SetRows(rows)
	b.WriteString(m.files.View() + "\n")

	b.WriteString(m.base.renderLogAndButton(19))

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "rbdb — rockbox database builder"
	return v
}

// ---- Fix model -------------------------------------------------------------

type fixTUIModel struct {
	base baseTUI
	dir  string
}

func newFixTUIModel(dir string, cancelFn context.CancelFunc) fixTUIModel {
	m := fixTUIModel{
		base: newBaseTUI(cancelFn),
		dir:  dir,
	}
	m.base.logLine(styleDim.Render("fixing MP3s in " + dir + " …"))
	return m
}

func (m fixTUIModel) Init() tea.Cmd {
	return m.base.handleTick()
}

func (m fixTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, m.base.handleTick()

	case tea.WindowSizeMsg:
		m.base.handleWindowSize(msg)
		return m, nil

	case tea.KeyPressMsg:
		cmd, handled := m.base.handleKeyPress(msg)
		if handled {
			return m, cmd
		}
		return m, cmd

	case tea.MouseClickMsg:
		return m, m.base.handleMouseClick(msg)

	case tea.MouseWheelMsg, tea.KeyMsg:
		return m, m.base.handleMouseWheelKey(msg)

	case progress.Found:
		m.base.found = msg.N
		m.base.logLine(fmt.Sprintf("found %d MP3 files", msg.N))

	case progress.FileStart:
		m.base.done = msg.Done
		m.base.current = msg.Path

	case progress.FileDone:
		if msg.Err != nil {
			m.base.failed++
			m.base.logLine(logLineSkip.Render(fmt.Sprintf("ERROR %s: %v", filepath.Base(msg.Path), msg.Err)))
		} else if msg.Skipped {
			m.base.skipped++
			m.base.logLine(styleDim.Render(fmt.Sprintf("skip %s", filepath.Base(msg.Path))))
		} else {
			m.base.logLine(styleDim.Render(fmt.Sprintf("fixed %s", filepath.Base(msg.Path))))
		}

	case progress.ArtworkFetched:
		m.base.logLine(styleOK.Render(fmt.Sprintf("art fetched: %s", filepath.Base(msg.Path))))

	case progress.ArtworkSearch:
		m.base.logLine(styleDim.Render(fmt.Sprintf("searching art: %s", filepath.Base(msg.Path))))

	case progress.SkippedHasArt:
		m.base.logLine(styleDim.Render(fmt.Sprintf("has art: %s", filepath.Base(msg.Path))))

	case progress.MetadataNormalized:
		m.base.logLine(styleOK.Render(fmt.Sprintf("normalized: %s", filepath.Base(msg.Path))))

	case progress.Banner:
		for _, line := range msg.Lines {
			m.base.logLine(styleDim.Render(line))
		}

	case progress.Cancelled:
		return m, m.base.handleCancelled()

	case progress.Done:
		m.base.finished = true
		m.base.buildErr = msg.Err
		if msg.Err != nil {
			m.base.logLine(logLineSkip.Render("FAILED: " + msg.Err.Error()))
		} else {
			m.base.logLine(styleOK.Render(fmt.Sprintf("done: %d processed, %d skipped, %d failed",
				m.base.done, m.base.skipped, m.base.failed)))
		}
		return m, m.base.handleTick()
	}
	return m, nil
}

func (m fixTUIModel) View() tea.View {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Rockbox Fixer") + styleDim.Render("  ·  in-place MP3 repair") + "\n")
	dir := m.dir
	if len(dir) > m.base.width-10 && m.base.width > 20 {
		dir = "…" + dir[len(dir)-(m.base.width-10):]
	}
	b.WriteString(styleDim.Render("dir: ") + dir + "\n\n")

	b.WriteString(m.base.renderProgressBar() + "\n")
	b.WriteString(m.base.renderCurrentLine() + "\n")
	stats := fmt.Sprintf("processed %s  skipped %s  failed %s  elapsed %s",
		styleOK.Render(fmt.Sprint(m.base.done)),
		styleDim.Render(fmt.Sprint(m.base.skipped)),
		styleBad.Render(fmt.Sprint(m.base.failed)),
		styleDim.Render(time.Since(m.base.start).Round(time.Second).String()))
	b.WriteString(stats + "\n\n")

	b.WriteString(m.base.renderLogAndButton(7))

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "rbdb — rockbox fixer"
	return v
}

// ---- Sync model ------------------------------------------------------------

type syncTUIModel struct {
	base   baseTUI
	origin string
	dest   string
}

func newSyncTUIModel(origin, dest string, cancelFn context.CancelFunc) syncTUIModel {
	m := syncTUIModel{
		base:   newBaseTUI(cancelFn),
		origin: origin,
		dest:   dest,
	}
	m.base.logLine(styleDim.Render("syncing " + origin + " → " + dest + " …"))
	return m
}

func (m syncTUIModel) Init() tea.Cmd {
	return m.base.handleTick()
}

func (m syncTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, m.base.handleTick()

	case tea.WindowSizeMsg:
		m.base.handleWindowSize(msg)
		return m, nil

	case tea.KeyPressMsg:
		cmd, handled := m.base.handleKeyPress(msg)
		if handled {
			return m, cmd
		}
		return m, cmd

	case tea.MouseClickMsg:
		return m, m.base.handleMouseClick(msg)

	case tea.MouseWheelMsg, tea.KeyMsg:
		return m, m.base.handleMouseWheelKey(msg)

	case progress.Found:
		m.base.found = msg.N
		m.base.logLine(fmt.Sprintf("found %d files to sync", msg.N))

	case progress.FileStart:
		m.base.done = msg.Done
		m.base.current = msg.Path

	case progress.FileDone:
		if msg.Err != nil {
			m.base.failed++
			m.base.logLine(logLineSkip.Render(fmt.Sprintf("ERROR %s: %v", filepath.Base(msg.Path), msg.Err)))
		} else if msg.Skipped {
			m.base.skipped++
			m.base.logLine(styleDim.Render(fmt.Sprintf("skip %s", filepath.Base(msg.Path))))
		} else {
			m.base.logLine(styleDim.Render(fmt.Sprintf("synced %s", filepath.Base(msg.Path))))
		}

	case progress.ArtworkFetched:
		m.base.logLine(styleOK.Render(fmt.Sprintf("art fetched: %s", filepath.Base(msg.Path))))

	case progress.MetadataNormalized:
		m.base.logLine(styleOK.Render(fmt.Sprintf("normalized: %s", filepath.Base(msg.Path))))

	case progress.Banner:
		for _, line := range msg.Lines {
			m.base.logLine(styleDim.Render(line))
		}

	case progress.Cancelled:
		return m, m.base.handleCancelled()

	case progress.Done:
		m.base.finished = true
		m.base.buildErr = msg.Err
		if msg.Err != nil {
			m.base.logLine(logLineSkip.Render("FAILED: " + msg.Err.Error()))
		} else {
			m.base.logLine(styleOK.Render(fmt.Sprintf("done: %d synced, %d skipped, %d failed",
				m.base.done, m.base.skipped, m.base.failed)))
		}
		return m, m.base.handleTick()
	}
	return m, nil
}

func (m syncTUIModel) View() tea.View {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Rockbox Syncer") + styleDim.Render("  ·  source → destination conversion") + "\n")
	origin := m.origin
	if len(origin) > m.base.width-20 && m.base.width > 30 {
		origin = "…" + origin[len(origin)-(m.base.width-20):]
	}
	b.WriteString(styleDim.Render("origin: ") + origin + "\n")
	dest := m.dest
	if len(dest) > m.base.width-14 && m.base.width > 24 {
		dest = "…" + dest[len(dest)-(m.base.width-14):]
	}
	b.WriteString(styleDim.Render("dest:   ") + dest + "\n\n")

	b.WriteString(m.base.renderProgressBar() + "\n")
	b.WriteString(m.base.renderCurrentLine() + "\n")
	stats := fmt.Sprintf("synced %s  skipped %s  failed %s  elapsed %s",
		styleOK.Render(fmt.Sprint(m.base.done)),
		styleDim.Render(fmt.Sprint(m.base.skipped)),
		styleBad.Render(fmt.Sprint(m.base.failed)),
		styleDim.Render(time.Since(m.base.start).Round(time.Second).String()))
	b.WriteString(stats + "\n\n")

	b.WriteString(m.base.renderLogAndButton(8))

	v := tea.NewView(b.String())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "rbdb — rockbox syncer"
	return v
}

// ---- Public runners --------------------------------------------------------

func Run(root string, job func(ctx context.Context, send func(any)), refresh bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	p := tea.NewProgram(newTUIModel(root, cancel, refresh))
	go job(ctx, func(m any) { p.Send(m) })
	_, err := p.Run()
	cancel()
	return err
}

func RunFix(dir string, job func(ctx context.Context, send func(any))) error {
	ctx, cancel := context.WithCancel(context.Background())
	p := tea.NewProgram(newFixTUIModel(dir, cancel))
	go job(ctx, func(m any) { p.Send(m) })
	_, err := p.Run()
	cancel()
	return err
}

func RunSync(origin, dest string, job func(ctx context.Context, send func(any))) error {
	ctx, cancel := context.WithCancel(context.Background())
	p := tea.NewProgram(newSyncTUIModel(origin, dest, cancel))
	go job(ctx, func(m any) { p.Send(m) })
	_, err := p.Run()
	cancel()
	return err
}
