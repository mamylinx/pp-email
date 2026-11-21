package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/filepicker"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"pp_email/internal/batch"
	"pp_email/internal/csvdata"
	"pp_email/internal/reports"
	"pp_email/internal/sendlimits"
)

type Mailer interface {
	batch.Adapter
	ListAccounts() ([]string, error)
}

type StopMsg struct{}

type screen int

const (
	accountScreen screen = iota
	fileScreen
	loadingScreen
	mappingScreen
	validationScreen
	previewScreen
	confirmScreen
	sendingScreen
	historyScreen
	resultScreen
)

const (
	minWidth   = 60
	minHeight  = 18
	splitWidth = 100
)

var fieldLabels = [...]string{"Recipient name", "Email address", "Subject", "Body"}
var bodyPreview = strings.NewReplacer("\r\n", "\n", "\t", "    ", "\r", "␍", "\v", "␋")

type accountsMsg struct {
	names []string
	err   error
}

type csvMsg struct {
	snapshot csvdata.Snapshot
	err      error
}

type Model struct {
	mailer Mailer
	open   batch.OpenRecovery
	limits *sendlimits.Limits
	screen screen
	draft  screen
	width  int
	height int
	styles theme
	help   bool

	input         textField
	searching     bool
	queries       map[screen]string
	previousQuery string

	directory string
	accounts  []string
	account   string
	choice    int
	loading   bool
	picker    filepicker.Model
	err       string

	snapshot   csvdata.Snapshot
	columns    [len(fieldLabels)]int
	field      int
	column     int
	messages   []csvdata.Message
	preview    browser
	detail     viewport.Model
	focus      bool
	draftFocus bool
	budget     sendlimits.Status
	budgetErr  error

	stop     chan struct{}
	events   <-chan batch.Event
	stopping bool
	started  time.Time
	progress batch.Progress
	report   reportView
}

func New(mailer Mailer, directory string, limits *sendlimits.Limits) *Model {
	picker := filepicker.New()
	picker.CurrentDirectory = directory
	picker.AutoHeight = false
	picker.ShowPermissions = false
	picker.ShowSize = false
	detail := viewport.New()
	detail.SoftWrap = true
	model := &Model{
		mailer: mailer, open: batch.CreateRecovery, limits: limits, loading: true,
		directory: directory, picker: picker, detail: detail,
		queries: make(map[screen]string), preview: newBrowser(), report: newReportView(),
	}
	model.setTheme(true)
	model.resize(80, 24)
	return model
}

func (m *Model) Init() tea.Cmd { return tea.Batch(m.listAccounts, tea.RequestBackgroundColor) }

func (m *Model) listAccounts() tea.Msg {
	names, err := m.mailer.ListAccounts()
	return accountsMsg{names: names, err: err}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case tea.BackgroundColorMsg:
		m.setTheme(msg.IsDark())
		return m, nil
	case StopMsg, tea.InterruptMsg:
		return m, m.requestStop(true)
	case accountsMsg:
		m.loading, m.accounts, m.err = false, msg.names, ""
		if msg.err != nil {
			m.err = msg.err.Error()
		} else if len(msg.names) == 0 {
			m.err = "Set up a named s-nail account, then press R to reload."
		}
		return m, nil
	case csvMsg:
		if msg.err != nil {
			m.screen, m.err = fileScreen, msg.err.Error()
			return m, nil
		}
		if msg.snapshot.Path != m.snapshot.Path {
			delete(m.queries, previewScreen)
			m.preview.setRows(nil, nil)
		}
		m.snapshot = msg.snapshot
		m.columns = [len(fieldLabels)]int{-1, -1, -1, -1}
		m.field, m.column, m.err = 0, 0, ""
		m.screen = mappingScreen
		return m, nil
	case batch.Event:
		if msg.Result == nil {
			m.progress = msg.Progress
			return m, waitEvent(m.events)
		}
		return m, m.finishRun(*msg.Result)
	case historyMsg:
		m.receiveHistory(msg)
		return m, nil
	case reportLoadedMsg:
		m.receiveReport(msg)
		return m, nil
	case reportSavedMsg:
		return m, m.receiveSave(msg)
	case exportedMsg:
		return m, m.receiveExport(msg)
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return m, m.requestStop(true)
		}
		// Keep the stop action available even when a window is too small to draw.
		if m.screen == sendingScreen {
			if msg.String() == "esc" || msg.String() == "q" {
				return m, m.requestStop(false)
			}
			if msg.String() == "?" && m.fits() {
				m.help = !m.help
			}
			return m, nil
		}
		if !m.fits() {
			if msg.String() == "q" && !m.searching && !m.report.exporting {
				return m, m.requestStop(false)
			}
			return m, nil
		}
		if m.searching || m.report.exporting {
			return m, m.updateInput(msg)
		}
		if m.help {
			if msg.String() == "esc" || msg.String() == "?" {
				m.help = false
			}
			return m, nil
		}
		return m, m.updateKey(msg)
	}
	if m.searching || m.report.exporting {
		return m, m.updateInput(msg)
	}
	if m.screen == fileScreen {
		return m, m.updateFile(msg)
	}
	return m, nil
}

func (m *Model) updateKey(msg tea.KeyPressMsg) tea.Cmd {
	action := msg.String()
	switch action {
	case "q":
		return m.requestStop(false)
	case "?":
		m.help = true
		return nil
	case "1":
		return m.openDraft()
	case "2":
		if m.screen != loadingScreen && m.screen != historyScreen {
			return m.openHistory()
		}
	case "/":
		if m.searchable() {
			return m.startSearch()
		}
	}
	switch m.screen {
	case accountScreen:
		return m.updateAccount(action)
	case fileScreen:
		return m.updateFile(msg)
	case mappingScreen:
		return m.updateMapping(action)
	case validationScreen:
		switch action {
		case "m", "esc":
			m.screen = mappingScreen
		case "f":
			m.screen = fileScreen
			return m.picker.Init()
		default:
			return m.scrollDetail(msg)
		}
	case previewScreen:
		switch action {
		case "esc":
			if m.focus {
				m.focus = false
			} else {
				m.screen = mappingScreen
			}
		case "enter":
			m.budget, m.budgetErr = m.limits.Status(m.account)
			m.screen = confirmScreen
		case "tab", "shift+tab":
			m.toggleDetail(&m.preview)
		default:
			return m.browse(msg, &m.preview, m.previewMessage)
		}
	case confirmScreen:
		switch action {
		case "esc":
			m.screen = previewScreen
			m.resize(m.width, m.height)
		case "s":
			return m.startBatch()
		}
	case historyScreen:
		return m.updateHistory(msg)
	case resultScreen:
		return m.updateReport(msg)
	}
	return nil
}

func (m *Model) openDraft() tea.Cmd {
	if m.screen != historyScreen && m.screen != resultScreen {
		return nil
	}
	m.report.pendingID = ""
	m.screen, m.focus = m.draft, m.draftFocus
	m.resize(m.width, m.height)
	switch m.screen {
	case fileScreen:
		return m.picker.Init()
	case validationScreen:
		m.validateMapping()
	case previewScreen:
		m.previewMessage()
	}
	return nil
}

func (m *Model) accountChoices() []string {
	var choices []string
	for _, name := range m.accounts {
		if matches(m.queries[accountScreen], name) {
			choices = append(choices, name)
		}
	}
	return choices
}

func (m *Model) updateAccount(action string) tea.Cmd {
	if m.loading {
		return nil
	}
	choices := m.accountChoices()
	switch action {
	case "r":
		m.loading, m.choice, m.err = true, 0, ""
		return m.listAccounts
	case "enter":
		if len(choices) > 0 {
			m.account = choices[m.choice]
			m.err, m.screen = "", fileScreen
			return m.picker.Init()
		}
	default:
		m.choice = moveChoice(action, m.choice, len(choices))
	}
	return nil
}

func (m *Model) updateFile(msg tea.Msg) tea.Cmd {
	if key, ok := msg.(tea.KeyPressMsg); ok && key.String() == "esc" {
		m.screen, m.err = accountScreen, ""
		return nil
	}
	var cmd tea.Cmd
	m.picker, cmd = m.picker.Update(msg)
	if selected, path := m.picker.DidSelectFile(msg); selected {
		m.screen, m.err = loadingScreen, ""
		return func() tea.Msg {
			snapshot, err := csvdata.Read(path)
			return csvMsg{snapshot: snapshot, err: err}
		}
	}
	return cmd
}

func (m *Model) updateMapping(action string) tea.Cmd {
	switch action {
	case "esc":
		if m.field == 0 {
			m.screen = fileScreen
			return m.picker.Init()
		}
		m.selectField(m.field - 1)
	case "shift+tab", "left":
		m.selectField((m.field + len(fieldLabels) - 1) % len(fieldLabels))
	case "tab", "right":
		m.columns[m.field] = m.column
		m.selectField((m.field + 1) % len(fieldLabels))
	case "enter":
		m.columns[m.field] = m.column
		if m.field+1 < len(fieldLabels) {
			m.selectField(m.field + 1)
		} else {
			for field, column := range m.columns {
				if column < 0 {
					m.selectField(field)
					return nil
				}
			}
			m.validateMapping()
		}
	default:
		m.column = moveChoice(action, m.column, len(m.snapshot.Headers))
	}
	return nil
}

func (m *Model) selectField(field int) {
	m.field = field
	if column := m.columns[field]; column >= 0 {
		m.column = column
	}
}

func (m *Model) validateMapping() {
	mapping := csvdata.Mapping{
		Name: m.columns[0], Address: m.columns[1],
		Subject: m.columns[2], Body: m.columns[3],
	}
	messages, issues := m.snapshot.Prepare(mapping)
	if len(issues) > 0 {
		lines := make([]string, 0, len(issues))
		for _, issue := range issues {
			lines = append(lines, "! "+reports.CleanText(issue.Error()))
		}
		m.screen = validationScreen
		m.resize(m.width, m.height)
		m.setDetail(strings.Join(lines, "\n"))
		return
	}
	m.messages = messages
	m.screen, m.focus = previewScreen, false
	m.refreshPreview()
	m.resize(m.width, m.height)
}

func moveChoice(action string, current, count int) int {
	switch action {
	case "up", "k":
		return max(0, current-1)
	case "down", "j":
		return min(max(0, count-1), current+1)
	case "home":
		return 0
	case "end":
		return max(0, count-1)
	default:
		return current
	}
}

func (m *Model) refreshPreview() {
	query := m.queries[previewScreen]
	var rows []table.Row
	var indices []int
	for i, message := range m.messages {
		line := strconv.Itoa(message.Record.Line)
		if matches(query, message.Name, message.Address, message.Subject, line) {
			rows = append(rows, table.Row{line, iconMail + reports.CleanText(message.Address), reports.CleanText(message.Subject)})
			indices = append(indices, i)
		}
	}
	m.preview.setRows(rows, indices)
	m.previewMessage()
}

func (m *Model) previewMessage() {
	if selected, ok := m.preview.selected(); ok {
		message := m.messages[selected]
		to := message.Address
		if message.Name != "" {
			to = message.Name + " <" + message.Address + ">"
		}
		m.setDetail("→ To: " + reports.CleanText(to) + "\n≡ Subject: " + reports.CleanText(message.Subject) +
			"\n\n" + cleanMultiline(bodyPreview.Replace(message.Body)))
	} else {
		m.setDetail("")
	}
}

func cleanMultiline(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = reports.CleanText(line)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) setDetail(content string) {
	m.detail.SetContent(content)
	m.detail.GotoTop()
}

func (m *Model) scrollDetail(msg tea.KeyPressMsg) tea.Cmd {
	var cmd tea.Cmd
	m.detail, cmd = m.detail.Update(msg)
	return cmd
}

func (m *Model) toggleDetail(list *browser) {
	if _, ok := list.selected(); ok {
		m.focus = !m.focus
	}
}

func (m *Model) browse(msg tea.KeyPressMsg, list *browser, selectDetail func()) tea.Cmd {
	if m.focus {
		return m.scrollDetail(msg)
	}
	cursor := list.table.Cursor()
	var cmd tea.Cmd
	list.table, cmd = list.table.Update(msg)
	if list.table.Cursor() != cursor {
		selectDetail()
	}
	return cmd
}

func (m *Model) searchable() bool {
	return m.screen == previewScreen || m.screen == historyScreen || m.screen == resultScreen ||
		(m.screen == accountScreen && !m.loading && len(m.accounts) > 0)
}

func (m *Model) startSearch() tea.Cmd {
	if m.report.runInfo {
		m.selectReportRow()
	}
	m.focus, m.report.runInfo = false, false
	m.resize(m.width, m.height)
	m.searching = true
	m.previousQuery = m.queries[m.screen]
	m.input.SetValue(m.previousQuery)
	return nil
}

func (m *Model) updateInput(msg tea.Msg) tea.Cmd {
	if m.report.exporting {
		return m.updateExport(msg)
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			m.setQuery(m.previousQuery)
			fallthrough
		case "enter":
			m.searching = false
			return nil
		}
	}
	m.input.Update(msg)
	if m.input.Value() != m.queries[m.screen] {
		m.setQuery(m.input.Value())
	}
	return nil
}

func (m *Model) setQuery(value string) {
	m.queries[m.screen] = value
	switch m.screen {
	case accountScreen:
		m.choice = 0
	case previewScreen:
		m.refreshPreview()
	case historyScreen:
		m.refreshHistory()
	case resultScreen:
		m.refreshReportRows()
	}
}

func matches(query string, values ...string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func (m *Model) budgetAllowsBatch() bool {
	return m.budgetErr == nil && (m.budget.MaxPer24Hours == 0 || len(m.messages) <= m.budget.Remaining)
}

func (m *Model) startBatch() tea.Cmd {
	if m.screen != confirmScreen || len(m.messages) == 0 || !m.budgetAllowsBatch() || m.report.saving {
		return nil
	}
	m.screen, m.stopping, m.started = sendingScreen, false, time.Now()
	m.progress = batch.Progress{Counts: batch.Counts{Unattempted: len(m.messages)}}
	m.stop = make(chan struct{})
	m.events = batch.Run(m.account, m.snapshot, m.messages, m.mailer, m.open, m.stop, m.limits)
	return waitEvent(m.events)
}

func waitEvent(events <-chan batch.Event) tea.Cmd {
	return func() tea.Msg {
		if event, ok := <-events; ok {
			return event
		}
		return nil
	}
}

func (m *Model) requestStop(interrupt bool) tea.Cmd {
	if m.screen == sendingScreen {
		if !m.stopping {
			close(m.stop)
			m.stopping = true
		}
		return nil
	}
	if m.report.saving || m.report.writing {
		m.report.quitAfterIO = true
		return nil
	}
	if interrupt && m.report.exporting {
		m.cancelExport()
		return nil
	}
	return tea.Quit
}

func emailCount(count int) string {
	if count == 1 {
		return "1 email"
	}
	return fmt.Sprintf("%d emails", count)
}
