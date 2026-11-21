package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"

	"pp_email/internal/batch"
	"pp_email/internal/csvdata"
	"pp_email/internal/reports"
	"pp_email/internal/smail"
)

type historyMsg struct {
	summaries []reports.Summary
	skipped   int
	err       error
}

type reportLoadedMsg struct {
	id     string
	report reports.Report
	err    error
}

type reportSavedMsg struct{ err error }
type exportedMsg struct {
	path string
	err  error
}

type reportView struct {
	history      browser
	rows         browser
	summaries    []reports.Summary
	loading      bool
	pendingID    string
	historyNote  string
	latest       reports.Report
	current      reports.Report
	saving       bool
	saveErr      error
	filter       int
	runInfo      bool
	exporting    bool
	writing      bool
	quitAfterIO  bool
	exportNotice string
	exportPath   string
}

var outcomeFilters = [...]string{"All", "Accepted", "Unconfirmed", "Unattempted"}

func newReportView() reportView {
	return reportView{history: newBrowser(), rows: newBrowser()}
}

func (m *Model) finishRun(result batch.Result) tea.Cmd {
	latest := reports.New(m.account, m.snapshot.Path, m.started, time.Now(), result, m.messages)
	m.events, m.stop = nil, nil
	m.help = false
	m.report.latest, m.report.saving, m.report.saveErr = latest, true, nil
	m.resetDraft()
	m.showReport(latest)
	return func() tea.Msg { return reportSavedMsg{err: reports.Save(latest)} }
}

func (m *Model) resetDraft() {
	m.draft, m.draftFocus, m.choice = accountScreen, false, 0
	m.account, m.err = "", ""
	m.messages = nil
	m.snapshot = csvdata.Snapshot{}
	m.preview.setRows(nil, nil)
	delete(m.queries, accountScreen)
	delete(m.queries, previewScreen)
}

func (m *Model) receiveSave(msg reportSavedMsg) tea.Cmd {
	m.report.saving, m.report.saveErr = false, msg.err
	if m.screen == historyScreen {
		m.includeLatest()
		m.refreshHistory()
	}
	if m.report.quitAfterIO && !m.report.writing {
		m.report.quitAfterIO = false
		if msg.err == nil {
			return tea.Quit
		}
	}
	return nil
}

func (m *Model) openHistory() tea.Cmd {
	if m.screen != historyScreen && m.screen != resultScreen {
		m.draft, m.draftFocus = m.screen, m.focus
	}
	m.screen, m.focus, m.report.runInfo = historyScreen, false, false
	m.resize(m.width, m.height)
	return m.loadHistory()
}

func (m *Model) loadHistory() tea.Cmd {
	if m.report.loading {
		return nil
	}
	m.report.loading, m.report.historyNote = true, ""
	return func() tea.Msg {
		items, skipped, err := reports.List()
		return historyMsg{summaries: items, skipped: skipped, err: err}
	}
}

func (m *Model) receiveHistory(msg historyMsg) {
	m.report.loading = false
	m.report.summaries = msg.summaries
	switch {
	case msg.err != nil:
		m.report.historyNote = "Could not read report history. Press R to try again. " + msg.err.Error()
	case msg.skipped > 0:
		m.report.historyNote = fmt.Sprintf("Skipped %d unreadable reports.", msg.skipped)
	}
	m.includeLatest()
	m.refreshHistory()
}

func (m *Model) includeLatest() {
	if m.report.latest.ID == "" {
		return
	}
	for _, summary := range m.report.summaries {
		if summary.ID == m.report.latest.ID {
			return
		}
	}
	m.report.summaries = append(m.report.summaries, m.report.latest.Summary)
	reports.Sort(m.report.summaries)
}

func (m *Model) refreshHistory() {
	query := m.queries[historyScreen]
	var rows []table.Row
	var indices []int
	for i, item := range m.report.summaries {
		if matches(query, item.Account, item.Source) {
			rows = append(rows, table.Row{
				"", item.Finished.Local().Format("2006-01-02 15:04"),
				reports.CleanText(item.Account), reports.CleanText(item.Source), reports.CleanText(item.Status),
				strconv.Itoa(item.Counts.Accepted), strconv.Itoa(item.Counts.Unconfirmed), strconv.Itoa(item.Counts.Unattempted),
			})
			indices = append(indices, i)
		}
	}
	m.report.history.setRows(rows, indices)
}

func (m *Model) updateHistory(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return m.openDraft()
	case "r":
		return m.loadHistory()
	case "enter":
		i, ok := m.report.history.selected()
		if !ok || m.report.pendingID != "" {
			return nil
		}
		id := m.report.summaries[i].ID
		if id == m.report.latest.ID {
			m.showReport(m.report.latest)
			return nil
		}
		m.report.pendingID = id
		return func() tea.Msg {
			report, err := reports.Load(id)
			return reportLoadedMsg{id: id, report: report, err: err}
		}
	default:
		var cmd tea.Cmd
		m.report.history.table, cmd = m.report.history.table.Update(msg)
		return cmd
	}
}

func (m *Model) receiveReport(msg reportLoadedMsg) {
	if m.screen != historyScreen || msg.id != m.report.pendingID {
		return
	}
	m.report.pendingID = ""
	if msg.err != nil {
		m.report.historyNote = "Could not open that run. Press R to reload history. " + msg.err.Error()
		return
	}
	m.showReport(msg.report)
}

func (m *Model) showReport(report reports.Report) {
	m.screen, m.focus, m.report.runInfo = resultScreen, false, false
	m.searching = false
	m.report.current, m.report.filter, m.report.exportNotice = report, 0, ""
	delete(m.queries, resultScreen)
	m.resize(m.width, m.height)
	m.refreshReportRows()
}

func (m *Model) refreshReportRows() {
	query := m.queries[resultScreen]
	var rows []table.Row
	var indices []int
	for i, row := range m.report.current.Rows {
		label := reports.OutcomeLabel(row.Outcome)
		line := strconv.Itoa(row.Line)
		if (m.report.filter == 0 || label == outcomeFilters[m.report.filter]) && matches(query, line) {
			rows = append(rows, table.Row{"", line, label, reports.CleanText(row.Reason)})
			indices = append(indices, i)
		}
	}
	m.report.rows.setRows(rows, indices)
	if !m.report.runInfo {
		m.selectReportRow()
	}
}

func (m *Model) selectReportRow() {
	if i, ok := m.report.rows.selected(); ok {
		row := m.report.current.Rows[i]
		detail := fmt.Sprintf("Line %d: %s", row.Line, reports.OutcomeLabel(row.Outcome))
		if row.Reason != "" {
			detail += "\n\n" + reports.CleanText(row.Reason)
		}
		if row.Outcome == smail.Unconfirmed {
			detail += "\n\nVerify acceptance before resending."
		}
		if slices.Contains(m.report.current.UnrecordedLines, row.Line) {
			detail += "\n\nThis row could not be saved to the recovery CSV."
		}
		m.setDetail(detail)
	} else {
		m.setDetail("")
	}
}

func (m *Model) updateReport(msg tea.KeyPressMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		if m.report.runInfo {
			m.report.runInfo, m.focus = false, false
			m.resize(m.width, m.height)
			m.selectReportRow()
		} else if m.focus {
			m.focus = false
		} else {
			return m.openHistory()
		}
	case "i":
		m.report.runInfo, m.focus = true, true
		m.resize(m.width, m.height)
		m.setDetail(runDetail(m.report.current))
	case "left", "right":
		if !m.focus {
			direction := 1
			if msg.String() == "left" {
				direction = -1
			}
			m.report.filter = (m.report.filter + direction + len(outcomeFilters)) % len(outcomeFilters)
			m.refreshReportRows()
		}
	case "enter", "tab", "shift+tab":
		if !m.report.runInfo {
			m.toggleDetail(&m.report.rows)
		}
	case "e":
		return m.beginExport()
	default:
		return m.browse(msg, &m.report.rows, m.selectReportRow)
	}
	return nil
}

func (m *Model) beginExport() tea.Cmd {
	m.report.exporting = true
	m.report.exportNotice, m.report.exportPath = "", ""
	run := m.report.current
	m.input.SetValue(filepath.Join(m.directory, "pp-email-report-"+run.ID+".csv"))
	return nil
}

func (m *Model) updateExport(msg tea.Msg) tea.Cmd {
	if m.report.writing {
		return nil
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			m.cancelExport()
			return nil
		case "enter":
			if m.report.exportPath != "" {
				m.cancelExport()
				return nil
			}
			value := m.input.Value()
			if strings.TrimSpace(value) == "" {
				m.report.exportNotice = "Enter a file path."
				return nil
			}
			if !filepath.IsAbs(value) {
				value = filepath.Join(m.directory, value)
			}
			path := filepath.Clean(value)
			report := m.report.current
			m.report.writing = true
			return func() tea.Msg { return exportedMsg{path: path, err: reports.Export(path, report)} }
		}
	}
	if m.report.exportPath == "" {
		m.input.Update(msg)
	}
	return nil
}

func (m *Model) receiveExport(msg exportedMsg) tea.Cmd {
	m.report.writing = false
	switch {
	case errors.Is(msg.err, os.ErrExist):
		m.report.exportNotice = "A file already exists there. Choose a different name."
	case msg.err != nil:
		m.report.exportNotice = "Could not export report. " + msg.err.Error()
	default:
		m.report.exportPath = msg.path
	}
	if m.report.quitAfterIO && !m.report.saving {
		m.report.quitAfterIO = false
		if msg.err == nil {
			return tea.Quit
		}
	}
	return nil
}

func (m *Model) cancelExport() {
	m.report.exporting = false
	m.report.exportNotice, m.report.exportPath = "", ""
}

func runDetail(report reports.Report) string {
	var lines []string
	lines = append(lines, "Status: "+reports.CleanText(report.Status),
		"Account: "+reports.CleanText(report.Account), "CSV: "+reports.CleanText(report.Source))
	if !report.Started.IsZero() {
		lines = append(lines, "Started: "+report.Started.Local().Format(time.RFC3339),
			"Finished: "+report.Finished.Local().Format(time.RFC3339))
	}
	lines = append(lines, "", countsText(report.Counts), "Accepted means the SMTP server accepted the submission.")
	if report.Error != "" {
		lines = append(lines, "", reports.CleanText(report.Error))
	}
	if report.RecoveryPath != "" {
		lines = append(lines, "", recoveryText(report))
	}
	for _, line := range report.UnrecordedLines {
		lines = append(lines, fmt.Sprintf("Source line %d could not be saved to the recovery CSV.", line))
	}
	if report.FirstUnattemptedLine > 0 {
		lines = append(lines, fmt.Sprintf("First unattempted source line: %d", report.FirstUnattemptedLine))
	}
	return strings.Join(lines, "\n")
}

func recoveryText(report reports.Report) string {
	return fmt.Sprintf("Recovery CSV (%s): %s", reports.CleanText(string(report.RecoveryStatus)), reports.CleanText(report.RecoveryPath))
}

func countsText(counts batch.Counts) string {
	return fmt.Sprintf("Accepted: %d   Unconfirmed: %d   Unattempted: %d", counts.Accepted, counts.Unconfirmed, counts.Unattempted)
}
