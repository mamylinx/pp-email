package batch

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"pp_email/internal/csvdata"
	"pp_email/internal/sendlimits"
	"pp_email/internal/smail"
)

const sampleHeader = "Name,Address,Subject,Body\r\n"
const sampleFirst = "One,one@example.invalid,Hi,first\r\n"
const sampleMiddle = "Two,two@example.invalid,Hi,\"second\r\nline\"\r\n"
const sampleLast = "Three,three@example.invalid,Hi,last\r\n"

type fakeMailer struct {
	check func(string) error
	send  func(csvdata.Message) smail.SendResult
}

func (m fakeMailer) CheckAccount(account string) (smail.Endpoint, error) {
	if m.check != nil {
		return smail.Endpoint{}, m.check(account)
	}
	return smail.Endpoint{}, nil
}

func (m fakeMailer) Send(_ string, _ smail.Endpoint, message csvdata.Message) smail.SendResult {
	return m.send(message)
}

func fixture(t *testing.T) (csvdata.Snapshot, []csvdata.Message) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.csv")
	if err := os.WriteFile(path, []byte(sampleHeader+sampleFirst+sampleMiddle+sampleLast), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := csvdata.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	messages, issues := snapshot.Prepare(csvdata.Mapping{Name: 0, Address: 1, Subject: 2, Body: 3})
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	return snapshot, messages
}

func testRequest(snapshot csvdata.Snapshot, messages []csvdata.Message, mailer Adapter) Request {
	return Request{
		Account:  "chosen",
		Snapshot: snapshot,
		Messages: messages,
		Adapter:  mailer,
		Open:     CreateRecovery,
	}
}

func completed(t *testing.T, events <-chan Event) (Result, []int) {
	t.Helper()
	lines := []int{}
	for event := range events {
		if event.Result != nil {
			return *event.Result, lines
		}
		if event.Progress.Phase == Submitting {
			lines = append(lines, event.Progress.Line)
		}
	}
	t.Fatal("run ended without a result")
	return Result{}, nil
}

func TestPacingWaitCanBeStopped(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "limits.json")
	if err := os.WriteFile(path, []byte(`{"chosen":{"messages_per_minute":1,"max_per_24_hours":3}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	limits, err := sendlimits.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, messages := fixture(t)
	mailer := fakeMailer{send: func(csvdata.Message) smail.SendResult {
		return smail.SendResult{Outcome: smail.Unconfirmed, Reason: "timed out"}
	}}
	stop := make(chan struct{})
	t.Cleanup(func() {
		if !stopping(stop) {
			close(stop)
		}
	})
	request := testRequest(snapshot, messages, mailer)
	request.Stop = stop
	request.Limits = limits
	events := Run(request)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("batch ended without a result")
			}
			if event.Result != nil {
				result := *event.Result
				if !result.Stopped || result.Error != "" || result.Unconfirmed != 1 ||
					result.Unattempted != 2 || result.FirstUnattemptedLine != messages[1].Record.Line {
					t.Fatalf("stopped batch: %+v", result)
				}
				status, err := limits.Status("chosen")
				if err != nil || status.Remaining != 2 {
					t.Fatalf("the unconfirmed attempt should count once: %+v, %v", status, err)
				}
				return
			}
			if event.Progress.Line == messages[1].Record.Line && event.Progress.Phase == WaitingForLimit {
				close(stop)
			}
		case <-timer.C:
			t.Fatal("batch did not stop during pacing")
		}
	}
}

func TestBatchContinuesAfterUnconfirmedRow(t *testing.T) {
	snapshot, messages := fixture(t)
	dir := filepath.Dir(snapshot.Path)
	errs := make(chan string, 1)
	mailer := fakeMailer{send: func(message csvdata.Message) smail.SendResult {
		switch message.Record.Line {
		case 3:
			return smail.SendResult{Outcome: smail.Unconfirmed, Reason: "timed out"}
		case 5:
			paths, _ := filepath.Glob(filepath.Join(dir, "pp-email-failures-*.csv"))
			if len(paths) != 1 {
				errs <- "missing recovery CSV during the last send"
				break
			}
			recovered, err := os.ReadFile(paths[0])
			if err != nil || string(recovered) != sampleHeader+sampleMiddle {
				errs <- "middle record was not written before advancing"
			}
		}
		return smail.SendResult{Outcome: smail.Accepted}
	}}
	result, lines := completed(t, Run(testRequest(snapshot, messages, mailer)))
	select {
	case message := <-errs:
		t.Error(message)
	default:
	}
	if result.Counts != (Counts{Accepted: 2, Unconfirmed: 1}) ||
		!slices.Equal(lines, []int{2, 3, 5}) || result.RecoveryStatus != RecoveryComplete ||
		!reflect.DeepEqual(result.UnconfirmedRows, []Issue{{Line: 3, Reason: "timed out"}}) {
		t.Fatalf("result = %+v, progress lines = %v", result, lines)
	}
	info, err := os.Stat(result.RecoveryPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery file permissions: %v, %v", info, err)
	}
	recovery, err := csvdata.Read(result.RecoveryPath)
	if err != nil || len(recovery.Records) != 1 || recovery.Records[0].Fields[0] != "Two" {
		t.Fatalf("reopened recovery = %+v, %v", recovery, err)
	}
}

func TestSuccessfulBatchRemovesEmptyRecovery(t *testing.T) {
	snapshot, messages := fixture(t)
	mailer := fakeMailer{send: func(csvdata.Message) smail.SendResult {
		return smail.SendResult{Outcome: smail.Accepted}
	}}
	result, _ := completed(t, Run(testRequest(snapshot, messages, mailer)))
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(snapshot.Path), "pp-email-failures-*.csv"))
	if result.Accepted != 3 || result.RecoveryPath != "" || result.Error != "" || err != nil || len(paths) != 0 {
		t.Fatalf("result = %+v, files = %v, %v", result, paths, err)
	}
}

func TestStopsBeforeNextRow(t *testing.T) {
	for _, duringStartup := range []bool{true, false} {
		snapshot, messages := fixture(t)
		stop := make(chan struct{})
		mailer := fakeMailer{
			check: func(string) error {
				if duringStartup {
					close(stop)
				}
				return nil
			},
			send: func(csvdata.Message) smail.SendResult {
				close(stop)
				return smail.SendResult{Outcome: smail.Unconfirmed, Reason: "timed out"}
			},
		}
		request := testRequest(snapshot, messages, mailer)
		request.Stop = stop
		result, _ := completed(t, Run(request))
		if duringStartup {
			if !result.Stopped || result.Unattempted != 3 || result.FirstUnattemptedLine != 2 || result.RecoveryPath != "" {
				t.Fatalf("startup stop = %+v", result)
			}
		} else if !result.Stopped || result.Unconfirmed != 1 || result.Unattempted != 2 ||
			result.FirstUnattemptedLine != 3 || result.RecoveryStatus != RecoveryComplete {
			t.Fatalf("send stop = %+v", result)
		}
	}
}

type faultyFile struct {
	*os.File
	writes      int
	shortAt     int
	rollbackErr bool
	closeErr    bool
}

func (f *faultyFile) Write(data []byte) (int, error) {
	f.writes++
	if f.writes == f.shortAt {
		return f.File.Write(data[:len(data)/2])
	}
	return f.File.Write(data)
}

func (f *faultyFile) Truncate(size int64) error {
	if f.rollbackErr {
		return errors.New("test rollback error")
	}
	return f.File.Truncate(size)
}

func (f *faultyFile) Close() error {
	err := f.File.Close()
	if f.closeErr {
		return errors.New("test close error")
	}
	return err
}

func TestRecoveryFileErrors(t *testing.T) {
	for _, test := range []struct {
		name                  string
		shortAt               int
		rollbackErr, closeErr bool
		want                  RecoveryStatus
	}{
		{"short write", 3, false, false, RecoveryPrefix},
		{"rollback failure", 3, true, false, RecoveryUnverified},
		{"close failure", 0, false, true, RecoveryUnverified},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot, messages := fixture(t)
			open := func(dir string) (RecoveryFile, error) {
				file, err := os.CreateTemp(dir, "pp-email-failures-*.csv")
				if err != nil {
					return nil, err
				}
				return &faultyFile{File: file, shortAt: test.shortAt, rollbackErr: test.rollbackErr, closeErr: test.closeErr}, nil
			}
			mailer := fakeMailer{send: func(csvdata.Message) smail.SendResult {
				return smail.SendResult{Outcome: smail.Unconfirmed, Reason: "submission unconfirmed"}
			}}
			request := testRequest(snapshot, messages, mailer)
			request.Open = open
			result, _ := completed(t, Run(request))
			if result.Error == "" || result.RecoveryStatus != test.want || result.RecoveryPath == "" {
				t.Fatalf("missing file error: %+v", result)
			}
			if test.shortAt != 0 {
				if result.Unconfirmed != 2 || result.FirstUnattemptedLine != 5 ||
					!slices.Equal(result.UnrecordedLines, []int{3}) {
					t.Fatalf("short write counts: %+v", result)
				}
				want := []RowResult{
					{Line: 2, Outcome: smail.Unconfirmed, Reason: "submission unconfirmed"},
					{Line: 3, Outcome: smail.Unconfirmed, Reason: "submission unconfirmed"},
					{Line: 5, Outcome: smail.Unattempted, Reason: result.Error},
				}
				if rows := result.Rows(messages); !slices.Equal(rows, want) {
					t.Fatalf("reported rows = %+v", rows)
				}
				data, err := os.ReadFile(result.RecoveryPath)
				if err != nil || !strings.HasPrefix(string(data), sampleHeader+sampleFirst) ||
					(!test.rollbackErr && string(data) != sampleHeader+sampleFirst) {
					t.Fatalf("recovery bytes = %q, %v", data, err)
				}
			} else if result.Unconfirmed != 3 || result.Unattempted != 0 {
				t.Fatalf("close error changed outcome counts: %+v", result)
			}
		})
	}
}

func TestGuardRefusalDoesNotCreateRecoveryRow(t *testing.T) {
	snapshot, messages := fixture(t)
	mailer := fakeMailer{send: func(csvdata.Message) smail.SendResult {
		return smail.SendResult{Outcome: smail.Unattempted, Stop: true, Reason: "SMTP transport changed"}
	}}
	result, lines := completed(t, Run(testRequest(snapshot, messages, mailer)))
	if result.Unattempted != 3 || result.FirstUnattemptedLine != 2 ||
		result.RecoveryPath != "" || !slices.Equal(lines, []int{2}) {
		t.Fatalf("guard refusal result = %+v, lines = %v", result, lines)
	}
}
