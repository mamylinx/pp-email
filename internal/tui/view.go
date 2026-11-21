package tui

import (
	"errors"
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

const (
	iconWidth          = 2
	bodyWidthInset     = 4
	bodyChromeLines    = 5
	fileChromeLines    = 4
	accountChromeLines = 3
	mappingChromeLines = 10
	previewChromeLines = 4
	historyChromeLines = 4
	resultChromeLines  = 9
	runInfoChromeLines = 2
	detailChromeLines  = 3
	inputWidthInset    = 10

	listSplitPercent    = 56
	paneGap             = 3
	addressMaxWidth     = 36
	addressSplitPercent = 45
	minColumnWidth      = 5

	lineColumnWidth     = 6
	finishedColumnWidth = 16
	statusColumnWidth   = 11
	countColumnWidth    = 4
	outcomeColumnWidth  = 13
	previewColumnCount  = 3
	historyColumnCount  = 7
	reportColumnCount   = 3
	historyFixedWidth   = finishedColumnWidth + statusColumnWidth +
		3*countColumnWidth + historyColumnCount
	previewFixedWidth = lineColumnWidth + previewColumnCount
	reportFixedWidth  = lineColumnWidth + outcomeColumnWidth + reportColumnCount

	fieldLabelWidth       = 18
	mappingColumnMaxWidth = 24
	mappingWidthOffset    = 19
	progressBarMaxWidth   = 46
)

type theme struct {
	title    lipgloss.Style
	active   lipgloss.Style
	selected lipgloss.Style
	muted    lipgloss.Style
	help     help.Model
}

func (m *Model) setTheme(dark bool) {
	plain := lipgloss.NewStyle()
	next := theme{
		title:    plain.Bold(true),
		active:   plain.Bold(true),
		selected: plain.Bold(true).Reverse(true),
		muted:    plain,
		help:     help.New(),
	}
	if os.Getenv("NO_COLOR") == "" {
		accent := lipgloss.LightDark(dark)(lipgloss.Color("#56C9CE"), lipgloss.Color("#176D77"))
		selectionText := lipgloss.LightDark(dark)(lipgloss.Color("#101C20"), lipgloss.Color("#FFFFFF"))
		muted := lipgloss.LightDark(dark)(lipgloss.Color("#A0ABB3"), lipgloss.Color("#57636A"))
		next.active = next.active.Foreground(accent)
		next.selected = plain.Bold(true).Foreground(selectionText).Background(accent)
		next.muted = next.muted.Foreground(muted)
	}
	next.help.Styles = help.Styles{
		ShortKey:       next.active,
		ShortDesc:      plain,
		ShortSeparator: next.muted,
		Ellipsis:       next.muted,
	}
	next.help.ShortSeparator = "  "
	next.help.SetWidth(m.bodyWidth())
	m.styles = next
	m.styleDetail()
	directoryStyle := plain.Transform(func(name string) string { return name + "/" })
	m.picker.Styles = filepicker.Styles{
		Directory:      directoryStyle,
		EmptyDirectory: plain.SetString("No files here. Use ← to go up."),
	}

	tableStyles := table.Styles{
		Header: next.muted.PaddingRight(1).
			BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).
			BorderBottomForeground(next.muted.GetForeground()),
		Cell:     plain.PaddingRight(1),
		Selected: next.selected,
	}
	for _, list := range []*browser{&m.preview, &m.report.history, &m.report.rows} {
		list.table.SetStyles(tableStyles)
	}
}

func (m *Model) fits() bool      { return m.width >= minWidth && m.height >= minHeight }
func (m *Model) split() bool     { return m.width >= splitWidth }
func (m *Model) bodyWidth() int  { return max(1, m.width-bodyWidthInset) }
func (m *Model) bodyHeight() int { return max(2, m.height-bodyChromeLines) }

func (m *Model) resize(width, height int) {
	m.width, m.height = width, height
	if !m.fits() {
		return
	}
	w, h := m.bodyWidth(), m.bodyHeight()
	m.styles.help.SetWidth(w)
	m.input.width = w - inputWidthInset
	m.picker.SetHeight(h - fileChromeLines)
	m.picker, _ = m.picker.Update(tea.WindowSizeMsg{Width: width, Height: height})
	listWidth, detailWidth := w, w
	if m.split() {
		listWidth = w * listSplitPercent / 100
		detailWidth = w - listWidth - paneGap
	}

	addressWidth := min(addressMaxWidth, listWidth*addressSplitPercent/100) + iconWidth
	setTable(&m.preview, listWidth, h-previewChromeLines,
		table.Column{Title: "Line", Width: lineColumnWidth},
		table.Column{Title: "Recipient", Width: addressWidth},
		table.Column{Title: "Subject", Width: listWidth - addressWidth - previewFixedWidth})

	nameWidth := (w - historyFixedWidth) / 2
	setTable(&m.report.history, w, h-historyChromeLines,
		table.Column{Title: "Finished", Width: finishedColumnWidth},
		table.Column{Title: "Account", Width: max(minColumnWidth, nameWidth)},
		table.Column{Title: "CSV", Width: max(minColumnWidth, w-historyFixedWidth-nameWidth)},
		table.Column{Title: "Status", Width: statusColumnWidth},
		table.Column{Title: "A", Width: countColumnWidth},
		table.Column{Title: "U", Width: countColumnWidth},
		table.Column{Title: "N", Width: countColumnWidth})

	setTable(&m.report.rows, listWidth, h-resultChromeLines,
		table.Column{Title: "Line", Width: lineColumnWidth},
		table.Column{Title: "Outcome", Width: outcomeColumnWidth},
		table.Column{Title: "Diagnostic", Width: listWidth - reportFixedWidth})

	m.styleDetail()
	switch m.screen {
	case previewScreen:
		m.detail.SetWidth(detailWidth)
		m.detail.SetHeight(h - previewChromeLines)
		m.detail.SetYOffset(m.detail.YOffset())
	case resultScreen:
		if m.report.runInfo {
			m.detail.SetWidth(w)
			m.detail.SetHeight(h - runInfoChromeLines)
		} else {
			m.detail.SetWidth(detailWidth)
			m.detail.SetHeight(h - resultChromeLines)
		}
	default:
		m.detail.SetWidth(w)
		m.detail.SetHeight(h - detailChromeLines)
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
		} else if m.report.isExportOpen {
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
	return iconAccount + reports.CleanText(m.account) +
		"  /  " + iconFile + reports.CleanText(filepath.Base(m.snapshot.Path))
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
	return strings.Join([]string{
		title,
		m.searchLine("Search accounts"),
		"",
		m.choiceList(choices, m.choice, m.bodyHeight()-accountChromeLines, choiceListStyle{
			icon:  iconAccount,
			empty: "No matching accounts.",
		}),
	}, "\n")
}

func (m *Model) fileListView() string {
	if m.isReadingDirectory {
		return "Reading directory…"
	}
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
		columnWidth := min(mappingColumnMaxWidth, (m.bodyWidth()-mappingWidthOffset)/2)
		if col := m.columns[field]; col >= 0 {
			value = fixedText(headers[col], columnWidth) + "   " +
				reports.CleanText(m.snapshot.Records[0].Fields[col])
		}
		row := "  " + fixedText(fieldIcons[field]+label, fieldLabelWidth) + " " + value
		if field == m.field {
			row = m.highlightRow(row)
		}
		lines = append(lines, row)
	}
	sample := reports.CleanText(m.snapshot.Records[0].Fields[m.column])
	if sample == "" {
		sample = "(empty)"
	}
	lines = append(lines,
		"",
		"≡ Sample: "+sample,
		m.styles.muted.Render(iconFile+"CSV columns"),
		m.choiceList(headers, m.column, m.bodyHeight()-mappingChromeLines, choiceListStyle{icon: "# "}),
	)
	return strings.Join(lines, "\n")
}

func fixedText(value string, width int) string {
	value = ansi.Truncate(value, width, "…")
	return value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
}

func (m *Model) highlightRow(row string) string {
	return m.styles.selected.Render(fixedText(row, m.bodyWidth()))
}

type choiceListStyle struct {
	icon  string
	empty string
}

func (m *Model) choiceList(choices []string, current, visible int, style choiceListStyle) string {
	if len(choices) == 0 {
		return style.empty
	}
	start := min(max(0, current-visible+1), max(0, len(choices)-visible))
	lines := make([]string, 0, visible)
	for i := start; i < min(len(choices), start+visible); i++ {
		row := "  " + style.icon + choices[i]
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
	search := m.searchLine("Search name, address, subject or line")
	return strings.Join([]string{title, m.contextLine(), search, body}, "\n")
}

func (m *Model) browserView(b *browser, listTitle, detailTitle, empty string) string {
	listHeading := m.paneHeading(listTitle, !m.isDetailFocused)
	detailHeading := m.paneHeading(detailTitle, m.isDetailFocused)
	list := empty
	if len(b.indices) > 0 {
		list = b.table.View()
	}
	if !m.split() {
		if m.isDetailFocused {
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
		m.step(5, iconSend+"Send "+emailCount(len(m.messages))),
		"",
		iconAccount + "Account: " + reports.CleanText(m.account),
		iconFile + "CSV: " + reports.CleanText(filepath.Base(m.snapshot.Path)),
		"",
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
		lines = append(lines, fmt.Sprintf(
			"This batch needs %s. Remaining sends: %d.",
			emailCount(len(m.messages)),
			m.budget.Remaining,
		))
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
	width := min(progressBarMaxWidth, m.bodyWidth())
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
	return strings.Join([]string{
		title,
		m.searchLine("Search account or CSV filename"),
		m.styles.muted.Render(outcomeLegend()),
		note,
		list,
	}, "\n")
}

func (m *Model) resultView() string {
	run := m.report.current
	title := m.styles.title.Render(iconReport+"Batch report") +
		"  " + statusIcon(run.Status) + reports.CleanText(string(run.Status))
	if m.report.runInfo {
		return title + "\n\n" + m.detail.View()
	}
	recovery := recoveryLines(run, m.bodyWidth())
	filters := make([]string, 0, len(outcomeFilters))
	for i, filter := range outcomeFilters {
		label := filter.icon + filter.title()
		if i == m.report.filter {
			label = m.styles.selected.Render(label)
		}
		filters = append(filters, label)
	}
	note := m.report.exportNotice
	if note == "" {
		saveErr := m.report.failedSaves[run.ID].err
		switch {
		case run.ID == m.report.latest.ID && m.report.saving:
			note = "Saving report…"
		case errors.Is(saveErr, reports.ErrDirectorySync):
			note = "History file created, but the directory could not be synced. Press E to export."
		case errors.Is(saveErr, reports.ErrPublishedCleanup):
			note = "History file created, but cleanup failed."
		case saveErr != nil:
			note = "Could not save to history. Press E to export this run."
		case len(run.UnrecordedLines) > 0:
			note = fmt.Sprintf("Source line %d could not be saved to recovery. Press I for details.", run.UnrecordedLines[0])
		case run.FirstUnattemptedLine > 0:
			note = fmt.Sprintf("First unattempted source line: %d", run.FirstUnattemptedLine)
		}
	}
	context := iconAccount + reports.CleanText(run.Account) +
		"  /  " + iconFile + reports.CleanText(run.Source)
	return strings.Join([]string{
		title,
		context,
		countsText(run.Counts),
		recovery[0],
		recovery[1],
		reports.CleanText(note),
		strings.Join(filters, "   "),
		m.searchLine("Search source line"),
		m.browserView(&m.report.rows, "▥ Outcomes", "≡ Row detail", "No matching rows."),
	}, "\n")
}

func recoveryLines(report reports.Report, width int) [2]string {
	var lines [2]string
	if report.RecoveryPath == "" {
		return lines
	}
	wrapped := strings.Split(lipgloss.Wrap(recoveryText(report), width, ""), "\n")
	copy(lines[:], wrapped)
	if len(wrapped) > len(lines) {
		lines[1] = ansi.Truncate(lines[1]+" …", width, "…")
	}
	return lines
}

func (m *Model) exportView() string {
	title := m.styles.title.Render(iconExport + "Export run as CSV")
	if m.report.exportPath != "" {
		status := m.report.exportNotice
		if status == "" {
			status = iconAccept + "Report saved to"
		}
		return title + "\n\n" + lipgloss.Wrap(status+"\n"+reports.CleanText(m.report.exportPath), m.bodyWidth(), "")
	}
	status := m.report.exportNotice
	if m.report.isWritingExport {
		status = "Saving report…"
	}
	lines := []string{
		title,
		fmt.Sprintf("All %d source rows will be exported.", len(m.report.current.Rows)),
		"",
		iconFolder + "Path: " + m.input.View(""),
		"",
		lipgloss.Wrap(reports.CleanText(status), m.bodyWidth(), ""),
	}
	return strings.Join(lines, "\n")
}

func (m *Model) footer() string {
	if m.help {
		return "Esc Close help"
	}
	if m.report.isExportOpen {
		if m.report.exportPath != "" {
			return "Enter Done  ·  Esc Back"
		}
		if m.report.isWritingExport {
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
		return []key.Binding{
			choose,
			shortcut("Enter", iconFile, "Open CSV"),
			shortcut("←", iconFolder, "Parent folder"),
			back,
		}
	case mappingScreen:
		return []key.Binding{
			choose,
			shortcut("Enter", "↵ ", "Map & continue"),
			shortcut("Tab", "⇥ ", "Next field"),
			back,
		}
	case validationScreen:
		return []key.Binding{
			scroll,
			shortcut("M", "≡ ", "Remap columns"),
			shortcut("F", iconFile, "Choose another CSV"),
			back,
		}
	case previewScreen:
		pane := "Switch pane"
		if !m.split() {
			pane = "Preview"
			if m.isDetailFocused {
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
		return []key.Binding{
			choose,
			shortcut("Enter", iconReport, "Open report"),
			search,
			shortcut("R", "↻ ", "Reload"),
			back,
		}
	case resultScreen:
		if m.report.runInfo {
			return []key.Binding{scroll, shortcut("E", iconExport, "Export CSV"), back}
		}
		movement := shortcut("←/→", "≡ ", "Filter")
		pane := "View"
		if m.isDetailFocused {
			movement = scroll
			pane = "Rows"
		}
		return []key.Binding{movement, shortcut("Tab", "⇄ ", pane), search,
			shortcut("E", iconExport, "Export"), shortcut("I", "ⓘ ", "Info")}
	}
	return nil
}

func (m *Model) bottomLine() string {
	if m.help || m.searching || m.report.isExportOpen {
		return ""
	}
	if m.screen == sendingScreen {
		return "? Help"
	}
	if len(m.report.failedSaves) > 0 && m.screen != resultScreen {
		if len(m.report.failedSaves) == 1 && errors.Is(m.report.failedSaves[m.report.latest.ID].err, reports.ErrDirectorySync) {
			return "History file created, but the directory could not be synced."
		}
		return "Some reports had save errors. Open Reports for details."
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
