package tui

import (
	"os"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	tea "charm.land/bubbletea/v2"
)

// pickerModel lets the user choose the device root directory with the
// bubbles filepicker. It starts in the previously used root, if any.
type pickerModel struct {
	fp        filepicker.Model
	cached    string
	choice    string
	cancelled bool
	width     int
	height    int
}

func newPickerModel(cached string) pickerModel {
	fp := filepicker.New()
	fp.DirAllowed = true
	fp.FileAllowed = false
	fp.ShowHidden = false

	start := cached
	if st, err := os.Stat(start); err != nil || !st.IsDir() {
		start = ""
	}
	if start == "" {
		if wd, err := os.Getwd(); err == nil {
			start = wd
		} else {
			start = "/"
		}
	}
	fp.CurrentDirectory = start
	return pickerModel{fp: fp, cached: cached}
}

func (m pickerModel) Init() tea.Cmd {
	return m.fp.Init()
}

func (m pickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.fp.AutoHeight = false // header/footer take a few rows
		m.fp.SetHeight(max(3, m.height-7))
		var cmd tea.Cmd
		m.fp, cmd = m.fp.Update(msg)
		return m, cmd

	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.cancelled = true
			return m, tea.Quit
		case "d":
			// Select the directory we are currently in.
			m.choice = m.fp.CurrentDirectory
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.fp, cmd = m.fp.Update(msg)
		// Enter on a selectable entry (directories only here) both records
		// the selection and descends into it; treat it as final.
		if ok, path := m.fp.DidSelectFile(msg); ok {
			m.choice = path
			return m, tea.Quit
		}
		return m, cmd
	}
	// Internal filepicker messages (directory listings, errors).
	var cmd tea.Cmd
	m.fp, cmd = m.fp.Update(msg)
	return m, cmd
}

func (m pickerModel) View() tea.View {
	var b strings.Builder
	b.WriteString(styleTitle.Render("Rockbox Database Builder") + styleDim.Render("  ·  choose device root") + "\n")
	b.WriteString(styleDim.Render("current: ") + truncate(m.fp.CurrentDirectory, max(0, m.width-12)) + "\n")
	if m.cached != "" {
		b.WriteString(styleDim.Render("last used: ") + truncate(m.cached, max(0, m.width-14)) + "\n")
	}
	b.WriteString(m.fp.View())
	b.WriteString("\n" + styleDim.Render("enter select/open dir · d pick current dir · h/j/k/l or arrows navigate · q quit"))
	v := tea.NewView(b.String())
	v.AltScreen = true
	v.WindowTitle = "rbdb — choose device root"
	return v
}

// PickRoot runs the interactive root-directory chooser. Returns the chosen
// directory, or "" if the user quit without choosing.
func PickRoot(cached string) (string, error) {
	p := tea.NewProgram(newPickerModel(cached))
	out, err := p.Run()
	if err != nil {
		return "", err
	}
	pm := out.(pickerModel)
	if pm.cancelled {
		return "", nil
	}
	return pm.choice, nil
}
