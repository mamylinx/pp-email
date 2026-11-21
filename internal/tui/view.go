package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"pp_email/internal/batch"
	"pp_email/internal/reports"
)

const (
	iconAccount = "◎ "
	iconFile    = "▤ "
	iconFolder  = "▱ "
	iconMail    = "✉ "
	iconReport  = "▥ "
	iconSend    = "➤ "
	iconSearch  = "⌕ "
	iconExport  = "↓ "
	iconAccept  = "✓ "
	iconPending = "? "
	iconSkipped = "– "
)

type browser struct {
	table   table.Model
	indices []int
}

func newBrowser() browser { return browser{table: table.New(table.WithFocused(true))} }

func (b *browser) selected() (int, bool) {
	if cursor := b.table.Cursor(); cursor >= 0 && cursor < len(b.indices) {
		return b.indices[cursor], true
	}
	return 0, false
}

func (b *browser) setRows(rows []table.Row, indices []int) {
	previous, hadSelection := b.selected()
	b.indices = indices
	b.table.SetRows(rows)
	for cursor, index := range indices {
		if hadSelection && index == previous {
			b.table.SetCursor(cursor)
			return
		}
	}
	b.table.GotoTop()
}

type theme struct {
	title    lipgloss.Style
	active   lipgloss.Style
	selected lipgloss.Style
	muted    lipgloss.Style
	help     help.Model
}

func (m *Model) setTheme(dark bool) {
	plain := lipgloss.NewStyle()
	theme := theme{title: plain.Bold(true), active: plain.Bold(true), selected: plain.Bold(true).Reverse(true), muted: plain,
		help: help.New()}
	if os.Getenv("NO_COLOR") == "" {
		accent := lipgloss.LightDark(dark)(lipgloss.Color("#56C9CE"), lipgloss.Color("#176D77"))
		selectionText := lipgloss.LightDark(dark)(lipgloss.Color("#101C20"), lipgloss.Color("#FFFFFF"))
		muted := lipgloss.LightDark(dark)(lipgloss.Color("#A0ABB3"), lipgloss.Color("#57636A"))
		theme.active = theme.active.Foreground(accent)
		theme.selected = plain.Bold(true).Foreground(selectionText).Background(accent)
		theme.muted = theme.muted.Foreground(muted)
	}
	theme.help.Styles = help.Styles{
		ShortKey: theme.active, ShortDesc: plain, ShortSeparator: theme.muted, Ellipsis: theme.muted,
	}
	theme.help.ShortSeparator = "  "
	theme.help.SetWidth(m.bodyWidth())
	m.styles = theme
	m.styleDetail()
	directoryStyle := plain.Transform(func(name string) string { return name + "/" })
	m.picker.Styles = filepicker.Styles{
		Directory: directoryStyle, EmptyDirectory: plain.SetString("No files here. Use ← to go up."),
	}

	tableStyles := table.Styles{
		Header: theme.muted.PaddingRight(1).
			BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).
			BorderBottomForeground(theme.muted.GetForeground()),
		Cell:     plain.PaddingRight(1),
		Selected: theme.selected,
	}
	for _, list := range []*browser{&m.preview, &m.report.history, &m.report.rows} {
		list.table.SetStyles(tableStyles)
	}
}

func (m *Model) fits() bool      { return m.width >= minWidth && m.height >= minHeight }
func (m *Model) split() bool     { return m.width >= splitWidth }
func (m *Model) bodyWidth() int  { return max(1, m.width-4) }
func (m *Model) bodyHeight() int { return max(2, m.height-5) }

func (m *Model) resize(width, height int) {
	m.width, m.height = width, height
	if !m.fits() {
		return
	}
	w, h := m.bodyWidth(), m.bodyHeight()
	m.styles.help.SetWidth(w)
	m.input.width = w - 10
	m.picker.SetHeight(h - 4)
	m.picker, _ = m.picker.Update(tea.WindowSizeMsg{Width: width, Height: height})
	listWidth, detailWidth := w, w
	if m.split() {
		listWidth = w * 56 / 100
		detailWidth = w - listWidth - 3
	}

	addressWidth := min(36, listWidth*45/100) + 2
	setTable(&m.preview, listWidth, h-4,
		table.Column{Title: "Line", Width: 6},
		table.Column{Title: "Recipient", Width: addressWidth},
		table.Column{Title: "Subject", Width: listWidth - addressWidth - 9})

	nameWidth := (w - 46) / 2
	setTable(&m.report.history, w, h-4,
		table.Column{Title: "Finished", Width: 16},
		table.Column{Title: "Account", Width: max(5, nameWidth)}, table.Column{Title: "CSV", Width: max(5, w-46-nameWidth)},
		table.Column{Title: "Status", Width: 11}, table.Column{Title: "A", Width: 4},
		table.Column{Title: "U", Width: 4}, table.Column{Title: "N", Width: 4})

	setTable(&m.report.rows, listWidth, h-9,
		table.Column{Title: "Line", Width: 6},
		table.Column{Title: "Outcome", Width: 13},
		table.Column{Title: "Diagnostic", Width: listWidth - 22})

	m.styleDetail()
	switch m.screen {
	case previewScreen:
		m.detail.SetWidth(detailWidth)
		m.detail.SetHeight(h - 4)
		m.detail.SetYOffset(m.detail.YOffset())
	case resultScreen:
		if m.report.runInfo {
			m.detail.SetWidth(w)
			m.detail.SetHeight(h - 2)
		} else {
			m.detail.SetWidth(detailWidth)
			m.detail.SetHeight(h - 9)
		}
	default:
		m.detail.SetWidth(w)
		m.detail.SetHeight(h - 3)
	}
}

func (m *Model) styleDetail() {
	m.detail.Style = lipgloss.NewStyle()
	if m.screen == previewScreen {
		m.detail.Style = m.detail.Style.Border(lipgloss.NormalBorder()).Padding(0, 1).
			BorderForeground(m.styles.muted.GetForeground())
	}
}

func setTable(b *browser, width, height int, columns ...table.Column) {
	b.table.SetColumns(columns)
	b.table.SetWidth(width)
	b.table.SetHeight(height)
}

func (m *Model) View() tea.View {
	var content string
	if !m.fits() {
		prompt := "Resize to at least 60 × 18."
		if m.screen == sendingScreen {
			prompt += "\nSending continues. Esc stops the batch."
		}
		content = fitLines(prompt, max(1, m.width), max(1, m.height))
	} else {
		body := m.body()
		if m.help {
			body = m.helpView()
		} else if m.report.exporting {
			body = m.exportView()
		}
		width, height := m.bodyWidth(), m.bodyHeight()
		rule := m.styles.muted.Render(strings.Repeat("─", width))
		lines := []string{m.navigation(), rule}
		fitted := strings.Split(fitLines(body, width, height), "\n")
		lines = append(lines, fitted...)
		for range height - len(fitted) {
			lines = append(lines, "")
		}
		lines = append(lines, rule, m.footer(), m.bottomLine())
		for i := range lines {
			if lines[i] != "" {
				lines[i] = "  " + lines[i]
			}
		}
		content = strings.Join(lines, "\n")
	}
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (m *Model) navigation() string {
	draft, history := "+ New batch", iconReport+"Reports"
	if m.screen != sendingScreen && m.screen != loadingScreen {
		draft, history = "1 "+draft, "2 "+history
	}
	if m.screen == historyScreen || m.screen == resultScreen {
		history = m.styles.selected.Render(history)
	} else {
		draft = m.styles.selected.Render(draft)
	}
	return m.styles.title.Render("pp-email") + "    " + draft + "    " + history
}

func (m *Model) body() string {
	switch m.screen {
	case accountScreen:
		return m.accountView()
	case fileScreen:
		return strings.Join([]string{
			m.step(2, iconFile+"Choose a CSV file"),
			iconAccount + "Account: " + reports.CleanText(m.account),
			iconFolder + reports.CleanText(m.picker.CurrentDirectory),
			reports.CleanText(m.err),
			m.fileListView(),
		}, "\n")
	case loadingScreen:
		return m.step(2, iconFile+"Reading CSV…")
	case mappingScreen:
		return m.mappingView()
	case validationScreen:
		return m.step(3, "! Fix the CSV rows below") + "\n" + m.contextLine() + "\n\n" + m.detail.View()
	case previewScreen:
		return m.previewView()
	case confirmScreen:
		return m.confirmView()
	case sendingScreen:
		return m.sendingView()
	case historyScreen:
		return m.historyView()
	case resultScreen:
		return m.resultView()
	}
	return ""
}

func (m *Model) step(number int, title string) string {
	return m.styles.title.Render(title) + m.styles.muted.Render(fmt.Sprintf("  %d of 5", number))
}

func (m *Model) contextLine() string {
	return iconAccount + reports.CleanText(m.account) + "  /  " + iconFile + reports.CleanText(filepath.Base(m.snapshot.Path))
}

func (m *Model) accountView() string {
	title := m.step(1, iconAccount+"Choose an account")
	switch {
	case m.loading:
		return title + "\n\nReading s-nail accounts…"
	case m.err != "":
		return title + "\n\n" + lipgloss.Wrap(reports.CleanText(m.err), m.bodyWidth(), "")
	}
	choices := m.accountChoices()
	return strings.Join([]string{title, m.searchLine("Search accounts"), "",
		m.choiceList(choices, iconAccount, m.choice, m.bodyHeight()-3, "No matching accounts.")}, "\n")
}

func (m *Model) fileListView() string {
	// Apply styling after cleaning the file picker text so it keeps the highlight.
	lines := strings.Split(cleanMultiline(m.picker.View()), "\n")
	for i, line := range lines {
		name, selected := strings.CutPrefix(line, m.picker.Cursor+" ")
		if !selected {
			var listed bool
			name, listed = strings.CutPrefix(line, "  ")
			if !listed {
				continue
			}
		}
		lines[i] = "  " + iconFile + name
		if selected {
			lines[i] = m.highlightRow(lines[i])
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) mappingView() string {
	headers := make([]string, len(m.snapshot.Headers))
	for i, name := range m.snapshot.Headers {
		headers[i] = fmt.Sprintf("%d. %s", i+1, reports.CleanText(name))
	}
	lines := []string{m.step(3, "≡ Map CSV columns"), m.contextLine(), ""}
	fieldIcons := [...]string{iconAccount, "→ ", "≡ ", iconFile}
	for field, label := range fieldLabels {
		value := "Choose a column"
		columnWidth := min(24, (m.bodyWidth()-19)/2)
		if col := m.columns[field]; col >= 0 {
			value = fixedText(headers[col], columnWidth) + "   " +
				reports.CleanText(m.snapshot.Records[0].Fields[col])
		}
		row := "  " + fixedText(fieldIcons[field]+label, 18) + " " + value
		if field == m.field {
			row = m.highlightRow(row)
		}
		lines = append(lines, row)
	}
	sample := reports.CleanText(m.snapshot.Records[0].Fields[m.column])
	if sample == "" {
		sample = "(empty)"
	}
	lines = append(lines, "", "≡ Sample: "+sample, m.styles.muted.Render(iconFile+"CSV columns"),
		m.choiceList(headers, "# ", m.column, m.bodyHeight()-10, ""))
	return strings.Join(lines, "\n")
}

func fixedText(value string, width int) string {
	value = ansi.Truncate(value, width, "…")
	return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
}

func (m *Model) highlightRow(row string) string {
	return m.styles.selected.Render(fixedText(row, m.bodyWidth()))
}

func (m *Model) choiceList(choices []string, icon string, current, visible int, empty string) string {
	if len(choices) == 0 {
		return empty
	}
	start := min(max(0, current-visible+1), max(0, len(choices)-visible))
	var lines []string
	for i := start; i < min(len(choices), start+visible); i++ {
		row := "  " + icon + choices[i]
		if i == current {
			row = m.highlightRow(row)
		}
		lines = append(lines, row)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) searchLine(placeholder string) string {
	if m.searching {
		return iconSearch + m.input.View(placeholder)
	}
	if query := m.queries[m.screen]; query != "" {
		return iconSearch + reports.CleanText(query)
	}
	return m.styles.muted.Render(iconSearch + placeholder + "  /")
}

func (m *Model) previewView() string {
	title := m.step(4, iconMail+"Review "+emailCount(len(m.messages)))
	body := m.browserView(&m.preview, "▥ Messages", iconMail+"Message preview", "No matching messages.")
	return strings.Join([]string{title, m.contextLine(), m.searchLine("Search name, address, subject or line"), body}, "\n")
}

func (m *Model) browserView(b *browser, listTitle, detailTitle, empty string) string {
	listHeading := m.paneHeading(listTitle, !m.focus)
	detailHeading := m.paneHeading(detailTitle, m.focus)
	list := empty
	if len(b.indices) > 0 {
		list = b.table.View()
	}
	if !m.split() {
		if m.focus {
			return detailHeading + "\n" + m.detail.View()
		}
		return listHeading + "\n" + list
	}
	detail := "Select a row to inspect it."
	if len(b.indices) > 0 {
		detail = m.detail.View()
	}
	left := lipgloss.NewStyle().Width(b.table.Width()).Render(listHeading + "\n" + list)
	right := detailHeading + "\n" + detail
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "   ", right)
}

func (m *Model) paneHeading(title string, focused bool) string {
	if focused {
		return m.styles.selected.Render(title)
	}
	return m.styles.title.Render(title)
}

func (m *Model) confirmView() string {
	lines := []string{
		m.step(5, iconSend+"Send "+emailCount(len(m.messages))), "",
		iconAccount + "Account: " + reports.CleanText(m.account), iconFile + "CSV: " + reports.CleanText(filepath.Base(m.snapshot.Path)), "",
	}
	if m.budget.MaxPer24Hours == 0 {
		lines = append(lines, "◷ Pacing and send budget: No limits configured")
	} else {
		lines = append(lines, fmt.Sprintf("◷ Pacing: %s per minute", emailCount(m.budget.MessagesPerMinute)))
		if m.budgetErr == nil {
			lines = append(lines, fmt.Sprintf("# Send budget: %d of %d remaining in the last 24 hours",
				m.budget.Remaining, m.budget.MaxPer24Hours))
		}
	}
	if m.budgetErr != nil {
		lines = append(lines, lipgloss.Wrap(reports.CleanText(m.budgetErr.Error()), m.bodyWidth(), ""))
	} else if !m.budgetAllowsBatch() {
		lines = append(lines, fmt.Sprintf("This batch needs %s. Remaining sends: %d.", emailCount(len(m.messages)), m.budget.Remaining))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) sendingView() string {
	status := "◷ Checking setup"
	switch {
	case m.stopping:
		status = "■ Stopping after the current operation"
	case m.progress.Phase == batch.WaitingForLimit:
		status = "◷ Waiting for send limit"
	case m.progress.Phase == batch.Submitting:
		status = iconSend + "Submitting email"
	}
	if m.progress.Line > 0 {
		status += fmt.Sprintf("  ·  Source line %d", m.progress.Line)
	}
	attempted := m.progress.Accepted + m.progress.Unconfirmed
	total := attempted + m.progress.Unattempted
	width := min(46, m.bodyWidth())
	filled := int(float64(width) * float64(attempted) / float64(max(1, total)))
	bar := m.styles.active.Render(strings.Repeat("━", filled)) + m.styles.muted.Render(strings.Repeat("─", width-filled))
	return strings.Join([]string{
		m.styles.title.Render(iconSend + "Sending email"), m.contextLine(), "", status,
		fmt.Sprintf("%d of %d attempted", attempted, total), bar, "", countsText(m.progress.Counts),
	}, "\n")
}

func (m *Model) historyView() string {
	title := m.styles.title.Render(iconReport + "Run history")
	note := reports.CleanText(m.report.historyNote)
	if m.report.pendingID != "" {
		note = "Opening report…"
	} else if m.report.loading {
		note = "Reading reports…"
	}
	list := m.report.history.table.View()
	switch {
	case len(m.report.summaries) == 0 && !m.report.loading:
		list = "No runs yet. Completed and stopped batches appear here."
	case len(m.report.history.indices) == 0 && !m.report.loading:
		list = "No matching runs."
	}
	return strings.Join([]string{title, m.searchLine("Search account or CSV filename"),
		m.styles.muted.Render(iconAccept + "A Accepted  ·  " + iconPending + "U Unconfirmed  ·  " + iconSkipped + "N Unattempted"), note, list}, "\n")
}

func (m *Model) resultView() string {
	run := m.report.current
	title := m.styles.title.Render(iconReport+"Batch report") + "  " + statusIcon(run.Status) + reports.CleanText(run.Status)
	if m.report.runInfo {
		return title + "\n\n" + m.detail.View()
	}
	recovery := []string{"", ""}
	if run.RecoveryPath != "" {
		pathLines := strings.Split(lipgloss.Wrap(recoveryText(run), m.bodyWidth(), ""), "\n")
		copy(recovery, pathLines)
		if len(pathLines) > 2 {
			recovery[1] = ansi.Truncate(recovery[1]+" …", m.bodyWidth(), "…")
		}
	}
	var filters []string
	for i, filter := range outcomeFilters {
		filter = outcomeIcon(filter) + filter
		if i == m.report.filter {
			filter = m.styles.selected.Render(filter)
		}
		filters = append(filters, filter)
	}
	note := m.report.exportNotice
	if note == "" {
		switch {
		case run.ID == m.report.latest.ID && m.report.saving:
			note = "Saving report…"
		case run.ID == m.report.latest.ID && m.report.saveErr != nil:
			note = "Could not save to history. Press E to export this run."
		case len(run.UnrecordedLines) > 0:
			note = fmt.Sprintf("Source line %d could not be saved to recovery. Press I for details.", run.UnrecordedLines[0])
		case run.FirstUnattemptedLine > 0:
			note = fmt.Sprintf("First unattempted source line: %d", run.FirstUnattemptedLine)
		}
	}
	return strings.Join([]string{title, iconAccount + reports.CleanText(run.Account) + "  /  " + iconFile + reports.CleanText(run.Source),
		countsText(run.Counts),
		recovery[0], recovery[1], reports.CleanText(note), strings.Join(filters, "   "),
		m.searchLine("Search source line"),
		m.browserView(&m.report.rows, "▥ Outcomes", "≡ Row detail", "No matching rows.")}, "\n")
}

func (m *Model) exportView() string {
	title := m.styles.title.Render(iconExport + "Export run as CSV")
	if m.report.exportPath != "" {
		return title + "\n\n" + iconAccept + "Report saved to\n" + lipgloss.Wrap(reports.CleanText(m.report.exportPath), m.bodyWidth(), "")
	}
	status := m.report.exportNotice
	if m.report.writing {
		status = "Saving report…"
	}
	return strings.Join([]string{title, fmt.Sprintf("All %d source rows will be exported.", len(m.report.current.Rows)), "",
		iconFolder + "Path: " + m.input.View(""), "", lipgloss.Wrap(reports.CleanText(status), m.bodyWidth(), "")}, "\n")
}

func (m *Model) footer() string {
	if m.help {
		return "Esc Close help"
	}
	if m.report.exporting {
		if m.report.exportPath != "" {
			return "Enter Done  ·  Esc Back"
		}
		if m.report.writing {
			return "Saving export…"
		}
		return "Enter Save CSV  ·  Esc Cancel"
	}
	if m.searching {
		return "Enter Apply search  ·  Esc Cancel"
	}
	return ansi.Truncate(m.styles.help.ShortHelpView(m.bindings(false)), m.bodyWidth(), "…")
}

func (m *Model) bindings(showIcons bool) []key.Binding {
	shortcut := func(keys, icon, action string) key.Binding {
		if showIcons {
			action = icon + action
		}
		return key.NewBinding(key.WithKeys(keys), key.WithHelp(keys, action))
	}
	choose := shortcut("↑/↓", "◇ ", "Choose")
	back := shortcut("Esc", "↩ ", "Back")
	search := shortcut("/", iconSearch, "Find")
	scroll := shortcut("↑/↓", "↕ ", "Scroll")
	switch m.screen {
	case accountScreen:
		return []key.Binding{choose, shortcut("Enter", "↵ ", "Continue"), search, shortcut("R", "↻ ", "Reload")}
	case fileScreen:
		return []key.Binding{choose, shortcut("Enter", iconFile, "Open CSV"), shortcut("←", iconFolder, "Parent folder"), back}
	case mappingScreen:
		return []key.Binding{choose, shortcut("Enter", "↵ ", "Map & continue"), shortcut("Tab", "⇥ ", "Next field"), back}
	case validationScreen:
		return []key.Binding{scroll, shortcut("M", "≡ ", "Remap columns"), shortcut("F", iconFile, "Choose another CSV"), back}
	case previewScreen:
		pane := "Switch pane"
		if !m.split() {
			pane = "Preview"
			if m.focus {
				pane = "Messages"
			}
		}
		return []key.Binding{shortcut("Enter", "↵ ", "Continue to confirmation"), shortcut("Tab", "⇄ ", pane), search}
	case confirmScreen:
		if m.budgetAllowsBatch() && !m.report.saving {
			return []key.Binding{shortcut("S", iconSend, "Send"), shortcut("Esc", iconMail, "Review messages")}
		}
		return []key.Binding{shortcut("Esc", iconMail, "Review messages")}
	case sendingScreen:
		if !m.stopping {
			return []key.Binding{shortcut("Esc", "■ ", "Stop after current message")}
		}
	case historyScreen:
		return []key.Binding{choose, shortcut("Enter", iconReport, "Open report"), search, shortcut("R", "↻ ", "Reload"), back}
	case resultScreen:
		if m.report.runInfo {
			return []key.Binding{scroll, shortcut("E", iconExport, "Export CSV"), back}
		}
		movement := shortcut("←/→", "≡ ", "Filter")
		pane := "View"
		if m.focus {
			movement = scroll
			pane = "Rows"
		}
		return []key.Binding{movement, shortcut("Tab", "⇄ ", pane), search,
			shortcut("E", iconExport, "Export"), shortcut("I", "ⓘ ", "Info")}
	}
	return nil
}

func (m *Model) bottomLine() string {
	if m.help || m.searching || m.report.exporting {
		return ""
	}
	if m.screen == sendingScreen {
		return "? Help"
	}
	if m.report.saveErr != nil && m.screen != resultScreen {
		return "Latest run has an unsaved report. Open Reports to export."
	}
	hint := "? Help  ·  Q Quit"
	if m.screen == previewScreen || m.screen == resultScreen {
		hint = "↑/↓ Move  ·  Esc Back  ·  " + hint
	}
	return m.styles.muted.Render(hint)
}

func (m *Model) helpView() string {
	lines := []string{m.styles.title.Render("ⓘ Keyboard shortcuts"), ""}
	for _, binding := range m.bindings(true) {
		info := binding.Help()
		lines = append(lines, fmt.Sprintf("  %-10s %s", info.Key, info.Desc))
	}
	switch m.screen {
	case previewScreen, resultScreen, historyScreen:
		lines = append(lines, "  ↑/↓        ↕ Move or scroll", "  PgUp/PgDn  ↕ Page up or down", "  Esc        ↩ Back")
	case mappingScreen:
		lines = append(lines, "  Shift+Tab  ⇤ Previous field")
	}
	if m.screen != sendingScreen && m.screen != loadingScreen {
		lines = append(lines, "  1          + New batch", "  2          "+iconReport+"Reports", "  Q          × Quit")
	}
	return strings.Join(lines, "\n")
}

func fitLines(text string, width, height int) string {
	lines := strings.SplitN(text, "\n", height+1)
	lines = lines[:min(height, len(lines))]
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "…")
	}
	return strings.Join(lines, "\n")
}
