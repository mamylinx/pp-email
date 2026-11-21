package batch

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"pp_email/internal/csvdata"
	"pp_email/internal/sendlimits"
	"pp_email/internal/smail"
)

type Adapter interface {
	CheckAccount(account string) (smail.Endpoint, error)
	Send(account string, endpoint smail.Endpoint, message csvdata.Message) smail.SendResult
}

type RecoveryFile interface {
	io.WriteCloser
	io.Seeker
	Name() string
	Sync() error
	Truncate(size int64) error
}

type OpenRecovery func(directory string) (RecoveryFile, error)

func CreateRecovery(directory string) (RecoveryFile, error) {
	return os.CreateTemp(directory, "pp-email-failures-*.csv")
}

type Counts struct {
	Accepted, Unconfirmed, Unattempted int
}

type Phase int

const (
	CheckingSetup Phase = iota
	WaitingForLimit
	Submitting
)

type Progress struct {
	Counts
	Line  int
	Phase Phase
}

type Issue struct {
	Line   int
	Reason string
}

type RecoveryStatus string

const (
	RecoveryComplete   RecoveryStatus = "complete"
	RecoveryPrefix     RecoveryStatus = "complete earlier rows only"
	RecoveryUnverified RecoveryStatus = "unverified"
)

type Result struct {
	Counts
	UnconfirmedRows      []Issue
	UnrecordedLines      []int
	FirstUnattemptedLine int
	RecoveryPath         string
	RecoveryStatus       RecoveryStatus
	Error                string
	Stopped              bool
}

// RowResult describes an outcome without retaining the message or recipient.
type RowResult struct {
	Line    int           `json:"line"`
	Outcome smail.Outcome `json:"outcome"`
	Reason  string        `json:"diagnostic,omitempty"`
}

// Rows combines the attempted prefix recorded by Run with the original source order.
func (r Result) Rows(messages []csvdata.Message) []RowResult {
	reasons := make(map[int]string, len(r.UnconfirmedRows))
	for _, issue := range r.UnconfirmedRows {
		reasons[issue.Line] = issue.Reason
	}
	attempted := r.Accepted + r.Unconfirmed
	rows := make([]RowResult, len(messages))
	for i, message := range messages {
		line := message.Record.Line
		row := RowResult{Line: line, Outcome: smail.Unattempted}
		if reason, ok := reasons[line]; ok {
			row.Outcome, row.Reason = smail.Unconfirmed, reason
		} else if i < attempted {
			row.Outcome = smail.Accepted
		} else if i == attempted {
			row.Reason = r.Error
		}
		rows[i] = row
	}
	return rows
}

type Event struct {
	Progress Progress
	Result   *Result
}

func Run(account string, snapshot csvdata.Snapshot, messages []csvdata.Message,
	adapter Adapter, open OpenRecovery, stop <-chan struct{}, limits *sendlimits.Limits,
) <-chan Event {
	events := make(chan Event, 1)
	go func() {
		defer close(events)
		result := run(account, snapshot, messages, adapter, open, stop, limits, events)
		events <- Event{Result: &result}
	}()
	return events
}

func run(account string, snapshot csvdata.Snapshot, messages []csvdata.Message,
	adapter Adapter, open OpenRecovery, stop <-chan struct{}, limits *sendlimits.Limits, events chan<- Event,
) (result Result) {
	result.Unattempted = len(messages)
	defer func() {
		if result.Unattempted > 0 {
			result.FirstUnattemptedLine = messages[len(messages)-result.Unattempted].Record.Line
		}
	}()

	if stopping(stop) || len(messages) == 0 {
		result.Stopped = stopping(stop)
		return result
	}
	endpoint, err := adapter.CheckAccount(account)
	if stopping(stop) {
		result.Stopped = true
		return result
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}

	session, err := limits.Begin(account, len(messages))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer func() {
		if err := session.Close(); err != nil {
			addError(&result, err.Error())
		}
	}()

	file, err := open(filepath.Dir(snapshot.Path))
	if err != nil {
		result.Error = "could not create a recovery CSV beside the source file"
		return result
	}
	result.RecoveryPath = file.Name()
	result.RecoveryStatus = RecoveryComplete
	defer func() {
		if err := file.Close(); err != nil {
			result.RecoveryStatus = RecoveryUnverified
			addError(&result, "could not close the recovery CSV")
		}
		if result.RecoveryStatus == RecoveryComplete && result.Unconfirmed == 0 {
			if err := os.Remove(file.Name()); err != nil {
				addError(&result, "could not remove the header-only recovery CSV")
				return
			}
			result.RecoveryPath = ""
			result.RecoveryStatus = ""
		}
	}()

	offset, err := writeAndSync(file, snapshot.WriteHeader)
	if err != nil {
		result.Error = "could not write the recovery CSV header"
		result.RecoveryStatus = RecoveryUnverified
		return result
	}

	for _, message := range messages {
		if stopping(stop) {
			result.Stopped = true
			break
		}
		if session.Pacing() {
			events <- Event{Progress: Progress{Counts: result.Counts, Line: message.Record.Line, Phase: WaitingForLimit}}
		}
		if err := session.WaitAndReserve(stop); err != nil {
			if errors.Is(err, sendlimits.ErrStopped) {
				result.Stopped = true
			} else {
				result.Error = err.Error()
			}
			break
		}
		if stopping(stop) {
			result.Stopped = true
			break
		}
		events <- Event{Progress: Progress{Counts: result.Counts, Line: message.Record.Line, Phase: Submitting}}
		if stopping(stop) {
			result.Stopped = true
			break
		}
		session.MarkStarted()
		outcome := adapter.Send(account, endpoint, message)
		switch outcome.Outcome {
		case smail.Accepted:
			result.Accepted++
			result.Unattempted--
		case smail.Unconfirmed:
			result.Unconfirmed++
			result.Unattempted--
			result.UnconfirmedRows = append(result.UnconfirmedRows, Issue{
				Line: message.Record.Line, Reason: outcome.Reason,
			})
			next, err := writeAndSync(file, func(w io.Writer) error {
				return snapshot.WriteRecord(w, message.Record)
			})
			if err != nil {
				result.UnrecordedLines = append(result.UnrecordedLines, message.Record.Line)
				result.Error = "could not save a row to the recovery CSV"
				result.RecoveryStatus = RecoveryPrefix
				if file.Truncate(offset) != nil || file.Sync() != nil {
					result.RecoveryStatus = RecoveryUnverified
				}
				return result
			}
			offset = next
		}
		if outcome.Stop || outcome.Outcome == smail.Unattempted {
			result.Error = outcome.Reason
			break
		}
	}
	if stopping(stop) {
		result.Stopped = true
	}
	return result
}

func writeAndSync(file RecoveryFile, write func(io.Writer) error) (int64, error) {
	if err := write(file); err != nil {
		return 0, err
	}
	offset, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	return offset, file.Sync()
}

func addError(result *Result, message string) {
	if result.Error == "" {
		result.Error = message
		return
	}
	result.Error += ". " + message
}

func stopping(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}
