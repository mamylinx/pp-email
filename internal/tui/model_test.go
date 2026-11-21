package tui

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"pp_email/internal/batch"
	"pp_email/internal/csvdata"
	"pp_email/internal/reports"
	"pp_email/internal/sendlimits"
	"pp_email/internal/smail"
)

type testMailer struct {
	check func() error
	send  func(csvdata.Message) smail.SendResult
}

func (testMailer) ListAccounts() ([]string, error) {
	return []string{"work", "personal"}, nil
}

func (m testMailer) CheckAccount(string) (smail.Endpoint, error) {
	if m.check != nil {
		return smail.Endpoint{}, m.check()
	}
	return smail.Endpoint{}, nil
}

func (m testMailer) Send(_ string, _ smail.Endpoint, message csvdata.Message) smail.SendResult {
	return m.send(message)
}

func press(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code})
}

func openFixture(t *testing.T, mailer testMailer, row string) *Model {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	directory := t.TempDir()
	csv := "Field,Field,Topic,Content\n" + row + "\n"
	if err := os.WriteFile(filepath.Join(directory, "send.csv"), []byte(csv), 0o600); err != nil {
		t.Fatal(err)
	}
	model := New(mailer, directory, nil)
	model.Update(model.listAccounts())
	model.Update(press(tea.KeyDown))
	if model.choice != 1 {
		t.Fatalf("account selection did not move: %d", model.choice)
	}
	requireHighlight(t, model, "  ◎ personal")
	_, cmd := model.Update(press(tea.KeyEnter))
	if cmd == nil || model.screen != fileScreen || model.account != "personal" {
		t.Fatalf("account selection: screen %d, account %q", model.screen, model.account)
	}
	model.Update(cmd())
	requireHighlight(t, model, "  ▤ send.csv")
	requireView(t, model, "↑/↓ Choose", "Enter Open CSV", "← Parent folder", "? Help")
	_, cmd = model.Update(press(tea.KeyEnter))
	if cmd == nil || model.screen != loadingScreen {
		t.Fatalf("file picker did not select the fixture: screen %d", model.screen)
	}
	model.Update(cmd())
	if model.screen != mappingScreen || !strings.Contains(ansi.Strip(model.View().Content), "2. Field") {
		t.Fatalf("mapping did not label duplicated headers: %s", ansi.Strip(model.View().Content))
	}
	requireHighlight(t, model, "  ◎ Recipient name")
	requireHighlight(t, model, "  # 1. Field")
	model.Update(press(tea.KeyEnter))
	for range 3 {
		model.Update(press(tea.KeyDown))
		model.Update(press(tea.KeyEnter))
	}
	return model
}

func drainRun(t *testing.T, model *Model, cmd tea.Cmd) {
	t.Helper()
	for cmd != nil {
		_, cmd = model.Update(cmd())
	}
	if model.screen != resultScreen {
		t.Fatalf("run did not reach results: screen %d", model.screen)
	}
}

func TestFlowConfirmsOneRunAndShowsRecovery(t *testing.T) {
	called := make(chan csvdata.Message, 2)
	mailer := testMailer{send: func(message csvdata.Message) smail.SendResult {
		called <- message
		return smail.SendResult{Outcome: smail.Unconfirmed, Reason: "s-nail timed out"}
	}}
	model := openFixture(t, mailer, `Ada,ada@example.invalid,Update,Hello Ada\r\nSecond line\tcolumn\vvertical\rreturn`)
	model.Update(tea.WindowSizeMsg{Width: 120, Height: 32})
	if model.screen != previewScreen || !strings.Contains(ansi.Strip(model.View().Content), "Hello Ada") ||
		!strings.Contains(ansi.Strip(model.View().Content), "Second line    column␋vertical␍return") ||
		!strings.Contains(model.detail.GetContent(), "Hello Ada\nSecond line") ||
		!strings.Contains(ansi.Strip(model.View().Content), "Ada <ada@example.invalid>") {
		t.Fatalf("wrong preview: %s", ansi.Strip(model.View().Content))
	}
	model.Update(press(tea.KeyEnter))
	if !strings.Contains(ansi.Strip(model.View().Content), "Send 1 email") {
		t.Fatalf("missing confirmation: %s", ansi.Strip(model.View().Content))
	}
	_, cmd := model.Update(press('s'))
	_, repeated := model.Update(press('s'))
	if cmd == nil || repeated != nil || model.screen != sendingScreen {
		t.Fatalf("confirmation did not enter a single send run: screen %d", model.screen)
	}
	drainRun(t, model, cmd)
	if len(called) != 1 {
		t.Fatalf("sent %d messages", len(called))
	}
	message := <-called
	if message.Body != "Hello Ada\r\nSecond line\tcolumn\vvertical\rreturn" ||
		message.Record.Line != 2 {
		t.Fatalf("wrong message: %+v", message)
	}
	model.Update(tea.WindowSizeMsg{Width: max(120, len(model.report.current.RecoveryPath)+40), Height: 24})
	for _, want := range []string{"Unconfirmed: 1", "Line 2:", "Recovery CSV", "pp-email-failures-"} {
		if !strings.Contains(ansi.Strip(model.View().Content), want) {
			t.Errorf("results missing %q: %s", want, ansi.Strip(model.View().Content))
		}
	}
}

func TestBudgetConfirmationAndRunnerRecheck(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "limits.json")
	config := `{"personal":{"messages_per_minute":6,"max_per_24_hours":1}}`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	limits, err := sendlimits.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	row := "Ada,ada@example.invalid,Update,Hello Ada"
	model := openFixture(t, testMailer{}, row)
	model.limits = limits
	model.Update(press(tea.KeyEnter))
	for _, want := range []string{"Pacing: 6 emails per minute", "1 of 1 remaining", "S Send"} {
		if !strings.Contains(ansi.Strip(model.View().Content), want) {
			t.Fatalf("initial confirmation missing %q: %s", want, ansi.Strip(model.View().Content))
		}
	}

	session, err := limits.Begin("personal", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.WaitAndReserve(nil); err != nil {
		t.Fatal(errors.Join(err, session.Close()))
	}
	session.MarkStarted()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	_, cmd := model.Update(press('s'))
	drainRun(t, model, cmd)
	if model.report.current.Counts.Unattempted != 1 || !strings.Contains(model.report.current.Error, "0 remain") {
		t.Fatalf("stale budget should block the batch: %+v", model.report.current)
	}

	model = openFixture(t, testMailer{}, row)
	model.limits = limits
	model.Update(press(tea.KeyEnter))
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "This batch needs 1 email. Remaining sends: 0.") || strings.Contains(view, "S Send") {
		t.Fatalf("exhausted budget confirmation: %s", view)
	}
	if _, cmd := model.Update(press('s')); cmd != nil || model.screen != confirmScreen {
		t.Fatal("exhausted budget allowed sending")
	}
}

func TestInvalidInputBlocksConfirmation(t *testing.T) {
	model := openFixture(t, testMailer{}, "Ada,missing,Update,Hello Ada")
	if model.screen != validationScreen || !strings.Contains(ansi.Strip(model.View().Content), "line 2") {
		t.Fatalf("missing validation: %s", ansi.Strip(model.View().Content))
	}
	model.Update(press('s'))
	if model.screen != validationScreen || model.events != nil {
		t.Fatalf("invalid input advanced to %d", model.screen)
	}
}

func TestStopWhileStartingWaitsForResult(t *testing.T) {
	release := make(chan struct{})
	mailer := testMailer{check: func() error { <-release; return nil }}
	model := openFixture(t, mailer, "Ada,ada@example.invalid,Update,Hello Ada")
	model.Update(press(tea.KeyEnter))
	_, cmd := model.Update(press('s'))
	_, quit := model.Update(StopMsg{})
	model.Update(StopMsg{})
	close(release)
	if quit != nil || model.screen != sendingScreen || !model.stopping {
		t.Fatalf("stop did not keep the running model alive: %+v", model.report.current)
	}
	drainRun(t, model, cmd)
	if model.report.current.Status != reports.Stopped || model.report.current.Counts.Unattempted != 1 {
		t.Fatalf("unexpected stopped run: %+v", model.report.current)
	}
}

func TestUnrecordedRowAndUnverifiedPathAreVisible(t *testing.T) {
	model := New(testMailer{}, t.TempDir(), nil)
	model.showReport(reports.New(reports.Input{
		Account:  "work",
		Source:   "sample.csv",
		Started:  time.Now(),
		Finished: time.Now(),
		Result: batch.Result{
			Counts:          batch.Counts{Accepted: 1, Unconfirmed: 1, Unattempted: 1},
			UnrecordedLines: []int{3}, FirstUnattemptedLine: 4,
			RecoveryStatus: batch.RecoveryUnverified, RecoveryPath: "/tmp/example.csv",
		},
	}))
	model.Update(press('i'))
	content := ansi.Strip(model.View().Content)
	for _, want := range []string{"unverified", "/tmp/example.csv", "line 3 could not be saved", "source line: 4",
		"Accepted means the SMTP server accepted the submission."} {
		if !strings.Contains(content, want) {
			t.Errorf("result did not include %q: %s", want, content)
		}
	}
}

func TestSearchReportsAndExportWorkflow(t *testing.T) {
	for _, size := range []tea.WindowSizeMsg{{Width: 60, Height: 18}, {Width: 80, Height: 24}, {Width: 120, Height: 32}} {
		t.Run(fmt.Sprintf("%dx%d", size.Width, size.Height), func(t *testing.T) {
			if size.Width == 80 {
				t.Setenv("NO_COLOR", "1")
			}
			attempted := []int{}
			mailer := testMailer{send: func(message csvdata.Message) smail.SendResult {
				attempted = append(attempted, message.Record.Line)
				if message.Record.Line == 4 {
					return smail.SendResult{Outcome: smail.Unconfirmed, Reason: "s-nail timed out"}
				}
				return smail.SendResult{Outcome: smail.Accepted}
			}}
			longLine := strings.Repeat("x", size.Width)
			body := "Second\n" + longLine + "\n" + strings.Repeat("Body line\n", size.Height) + "End of preview"
			row := "Ada,ada@example.invalid,Start,\"First\nline\"\n" +
				"Quinn,quinn@example.invalid,Follow-up,\"" + body + "\""
			model := openFixture(t, mailer, row)
			model.Update(size)
			requireView(t, model,
				"✉ Review 2 emails",
				"Enter Continue to confirmation",
				"⌕ Search name, address, subject or line",
			)
			requireHighlight(t, model, "1 + New batch")
			requireHighlight(t, model, "▥ Messages")
			requireHighlight(t, model, "2      ✉ ada@")
			requireHeaderRule(t, model, &model.preview, "Recipient")
			if model.split() {
				requireView(t, model, "✉ Message preview", "┌", "→ To: Ada")
				model.Update(press('/'))
				typeText(model, "absent")
				requireView(t, model, "No matching messages.", "Select a row to inspect it.")
				if strings.ContainsRune(model.View().Content, '┌') {
					t.Fatal("empty preview showed a frame")
				}
				model.Update(press(tea.KeyEscape))
			}
			model.Update(press('/'))
			typeText(model, "quinn")
			requireView(t, model, "Enter Apply search")
			model.Update(press(tea.KeyEnter))
			if len(model.preview.indices) != 1 || model.preview.indices[0] != 1 {
				t.Fatalf("search: %v", model.preview.indices)
			}
			model.Update(press(tea.KeyTab))
			border := strings.Repeat("─", model.detail.Width()-2)
			wrapped := strings.Repeat("x", model.detail.Width()-4)
			requireView(t, model, "┌"+border+"┐", "└"+border+"┘",
				"→ To: Quinn <quinn@example.invalid>", "≡ Subject: Follow-up", "Second", "│ "+wrapped+" │")
			requireHighlight(t, model, "✉ Message preview")
			for range 4 {
				model.Update(press(tea.KeyPgDown))
			}
			requireView(t, model, "End of preview", "└"+border+"┘")
			model.Update(press(tea.KeyEnter))
			requireView(t, model, "➤ Send 2 emails", "S Send")
			_, cmd := model.Update(press('s'))
			model.Update(tea.WindowSizeMsg{Width: 45, Height: 10})
			requireView(t, model, "Resize to at least", "Sending continues")
			model.Update(size)
			drainRun(t, model, cmd)
			if !slices.Equal(attempted, []int{2, 4}) {
				t.Fatalf("batch sent source lines %v", attempted)
			}
			requireView(t, model, "✓ Accepted: 1", "? Unconfirmed: 1", "▤ Recovery CSV", "s-nail timed out",
				"≡ All   ✓ Accepted   ? Unconfirmed   – Unattempted", "E Export", "I Info")
			requireHighlight(t, model, "2      ✓ Accepted")
			requireHeaderRule(t, model, &model.report.rows, "Diagnostic")
			model.Update(press(tea.KeyTab))
			requireView(t, model, "Row detail", "Line 2: Accepted", "E Export", "I Info")
			if strings.ContainsRune(model.View().Content, '┌') {
				t.Fatal("report row detail retained the message preview frame")
			}

			// Start another model to exercise actual on-disk history, including partial read failure.
			dir := filepath.Join(os.Getenv("XDG_STATE_HOME"), "pp-email", "reports")
			if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			reopened := New(mailer, model.directory, nil)
			reopened.Update(size)
			advance(t, reopened, press('2'))
			requireView(t, reopened, "▥ Run history", "Skipped 1 unreadable", "✓ Completed")
			requireHighlight(t, reopened, "2 ▥ Reports")
			requireHighlight(t, reopened, model.report.current.Finished.Local().Format("2006-01-02 15:04"))
			requireHeaderRule(t, reopened, &reopened.report.history, "Finished")
			reopened.Update(press('/'))
			typeText(reopened, "absent")
			requireView(t, reopened, "No matching runs")
			reopened.Update(press(tea.KeyEscape))
			reopened.Update(press('/'))
			typeText(reopened, "SEND.CSV")
			reopened.Update(press(tea.KeyEnter))
			advance(t, reopened, press(tea.KeyEnter))
			reopened.Update(press(tea.KeyRight))
			reopened.Update(press(tea.KeyRight))
			requireHighlight(t, reopened, "? Unconfirmed")
			if len(reopened.report.rows.indices) != 1 {
				t.Fatalf("filtered report rows: %v", reopened.report.rows.indices)
			}
			reopened.Update(press(tea.KeyTab))
			requireView(t, reopened, "Line 4: Unconfirmed")
			if size.Height > 18 {
				requireView(t, reopened, "s-nail timed out")
			}
			reopened.Update(press('e'))
			requireView(t, reopened, "Enter Save CSV", "Esc Cancel")
			reopened.Update(tea.KeyPressMsg(tea.Key{Code: 'u', Mod: tea.ModCtrl}))
			typeText(reopened, "out.csv")
			advance(t, reopened, press(tea.KeyEnter))
			requireView(t, reopened, "↓ Export run as CSV", "✓ Report saved to", "out.csv", "Enter Done")
			data, err := os.ReadFile(filepath.Join(model.directory, "out.csv"))
			if err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
			if err != nil || len(rows) != 3 || rows[1][4] != "2" || rows[2][4] != "4" {
				t.Fatalf("export should contain both source rows: %v, %v", rows, err)
			}
		})
	}
}

func requireView(t *testing.T, model *Model, fragments ...string) {
	t.Helper()
	view := ansi.Strip(model.View().Content)
	for _, fragment := range fragments {
		if !strings.Contains(view, fragment) {
			t.Errorf("screen does not contain %q:\n%s", fragment, view)
		}
	}
	lines := strings.Split(view, "\n")
	if len(lines) > model.height {
		t.Fatalf("screen exceeds %d lines:\n%s", model.height, view)
	}
	for _, line := range lines {
		if ansi.StringWidth(line) > model.width {
			t.Fatalf("screen exceeds %d columns: %q", model.width, line)
		}
	}
}

func requireHeaderRule(t *testing.T, model *Model, b *browser, header string) {
	t.Helper()
	view := ansi.Strip(model.View().Content)
	lines := strings.Split(view, "\n")
	rule := "  " + strings.Repeat("─", b.table.Width())
	for i, line := range lines[:len(lines)-1] {
		if strings.Contains(line, header) && strings.HasPrefix(lines[i+1], rule) {
			return
		}
	}
	t.Errorf("missing full-width rule below %q:\n%s", header, view)
}

func requireHighlight(t *testing.T, model *Model, text string) {
	t.Helper()
	prefix, _, wrapped := strings.Cut(model.styles.selected.Render(text), text)
	if !wrapped || prefix == "" {
		t.Fatalf("selection style does not wrap %q", text)
	}
	if !strings.Contains(model.View().Content, prefix+text) {
		t.Errorf("missing selection highlight on %q:\n%s", text, ansi.Strip(model.View().Content))
	}
}

func typeText(model *Model, value string) {
	for _, r := range value {
		model.Update(press(r))
	}
}

func advance(t *testing.T, model *Model, key tea.KeyPressMsg) {
	t.Helper()
	_, cmd := model.Update(key)
	if cmd == nil {
		t.Fatalf("keypress %q did not start an operation", key.String())
	}
	model.Update(cmd())
}

func TestQuitWhileReportWritesArePending(t *testing.T) {
	failure := errors.New("write failed")
	for _, tc := range []struct {
		name          string
		first, second tea.Msg
		wantQuit      bool
	}{
		{"save fails first", reportSavedMsg{failure}, exportedMsg{path: "out.csv"}, false},
		{"export fails first", exportedMsg{err: failure}, reportSavedMsg{}, false},
		{"save fails last", exportedMsg{path: "out.csv"}, reportSavedMsg{failure}, false},
		{"export fails last", reportSavedMsg{}, exportedMsg{err: failure}, false},
		{"both succeed", reportSavedMsg{}, exportedMsg{path: "out.csv"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Model{report: reportView{saving: true, isWritingExport: true}}
			if _, cmd := m.Update(StopMsg{}); cmd != nil {
				t.Fatal("quit before either write finished")
			}
			if _, cmd := m.Update(tc.first); cmd != nil {
				t.Fatal("quit before both writes finished")
			}
			_, cmd := m.Update(tc.second)
			if (cmd != nil) != tc.wantQuit {
				t.Fatalf("got quit command %t, want %t", cmd != nil, tc.wantQuit)
			}
			if cmd != nil {
				if _, ok := cmd().(tea.QuitMsg); !ok {
					t.Fatal("expected a quit message")
				}
			}
		})
	}
}

func TestExportDirectorySyncFailure(t *testing.T) {
	m := New(testMailer{}, t.TempDir(), nil)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.screen = resultScreen
	m.beginExport()
	m.Update(exportedMsg{path: "out.csv", err: reports.ErrDirectorySync})
	requireView(t, m, "directory could not be synced", "out.csv", "Enter Done")
	if _, cmd := m.Update(press(tea.KeyEnter)); cmd != nil || m.report.isExportOpen {
		t.Fatal("expected Enter to close the export notice")
	}
}

func TestReportCanBeExportedWhenHistoryCannotBeSaved(t *testing.T) {
	model := openFixture(t, testMailer{send: func(csvdata.Message) smail.SendResult {
		return smail.SendResult{Outcome: smail.Accepted}
	}}, "Ada,ada@example.invalid,Update,Hello Ada")
	statePath := filepath.Join(os.Getenv("XDG_STATE_HOME"), "pp-email")
	if err := os.WriteFile(statePath, []byte("blocks state directory creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	model.Update(press(tea.KeyEnter))
	_, cmd := model.Update(press('s'))
	drainRun(t, model, cmd)
	requireView(t, model, "Could not save to history", "Accepted: 1")
	model.Update(press('e'))
	advance(t, model, press(tea.KeyEnter))
	requireView(t, model, "Report saved to")
}

func TestFilePickerIgnoresKeysDuringDirectoryRead(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"first.csv", "second.csv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("name\nAda\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := New(testMailer{}, dir, nil)
	m.Update(m.listAccounts())
	_, read := m.Update(press(tea.KeyEnter))
	m.Update(press(tea.KeyDown))
	if _, cmd := m.Update(press(tea.KeyEnter)); cmd != nil {
		t.Fatal("selected a file before the directory loaded")
	}
	m.Update(read())
	if got := m.picker.HighlightedPath(); got != filepath.Join(dir, "first.csv") {
		t.Fatalf("selection after early key press: %q", got)
	}
	m.Update(press(tea.KeyDown))
	m.Update(press(tea.KeyEscape))
	if err := os.Remove(filepath.Join(dir, "second.csv")); err != nil {
		t.Fatal(err)
	}
	advance(t, m, press(tea.KeyEnter))
	advance(t, m, press(tea.KeyEnter))
	if m.screen != mappingScreen || m.snapshot.Path != filepath.Join(dir, "first.csv") {
		t.Fatalf("selection after directory shrank: screen %v, file %q", m.screen, m.snapshot.Path)
	}
}

func TestEarlierUnsavedReportsRemainAvailableForExport(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	blocker := filepath.Join(state, "pp-email")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(testMailer{}, t.TempDir(), nil)
	finish := func(line int) string {
		m.started = time.Now()
		m.messages = []csvdata.Message{{Record: csvdata.Record{Line: line}}}
		save := m.finishRun(batch.Result{Counts: batch.Counts{Accepted: 1}})
		m.Update(save())
		return m.report.latest.ID
	}
	first, second := finish(2), finish(3)
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	newest := finish(4)
	advance(t, m, press('2'))
	if len(m.report.summaries) != 3 {
		t.Fatalf("history after two failed saves and one successful save: %+v", m.report.summaries)
	}
	for _, id := range []string{first, second} {
		i := slices.IndexFunc(m.report.summaries, func(s reports.Summary) bool { return s.ID == id })
		if i < 0 {
			t.Fatalf("missing unsaved report %s", id)
		}
		m.report.history.table.SetCursor(i)
		m.Update(press(tea.KeyEnter))
		if m.report.current.ID != id {
			t.Fatalf("opened %q, want %q", m.report.current.ID, id)
		}
		requireView(t, m, "Could not save to history")
		m.Update(press('e'))
		advance(t, m, press(tea.KeyEnter))
		data, err := os.ReadFile(m.report.exportPath)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
		if err != nil || len(rows) != 2 || rows[1][0] != id {
			t.Fatalf("export of %s: %v, %v", id, rows, err)
		}
		m.Update(press(tea.KeyEnter))
		advance(t, m, press('2'))
	}
	if _, err := reports.Load(newest); err != nil {
		t.Fatalf("latest saved report: %v", err)
	}
}
