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

type Request struct {
	Account  string
	Snapshot csvdata.Snapshot
	Messages []csvdata.Message
	Adapter  Adapter
	Open     OpenRecovery
	Stop     <-chan struct{}
	Limits   *sendlimits.Limits
}

func Run(request Request) <-chan Event {
	events := make(chan Event, 1)
	go func() {
		defer close(events)
		result := run(request, events)
		events <- Event{Result: &result}
	}()
	return events
}

func run(request Request, events chan<- Event) (result Result) {
	result.Unattempted = len(request.Messages)
	defer func() {
		if result.Unattempted > 0 {
			index := len(request.Messages) - result.Unattempted
			result.FirstUnattemptedLine = request.Messages[index].Record.Line
		}
	}()

	if stopping(request.Stop) || len(request.Messages) == 0 {
		result.Stopped = stopping(request.Stop)
		return result
	}
	endpoint, err := request.Adapter.CheckAccount(request.Account)
	if stopping(request.Stop) {
		result.Stopped = true
		return result
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}

	session, err := request.Limits.Begin(request.Account, len(request.Messages))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer func() {
		if err := session.Close(); err != nil {
			addError(&result, err.Error())
		}
	}()

	file, err := request.Open(filepath.Dir(request.Snapshot.Path))
	if err != nil {
		result.Error = "could not create a recovery CSV beside the source file"
		return result
	}
	result.RecoveryPath = file.Name()
	result.RecoveryStatus = RecoveryComplete
	defer closeRecovery(file, &result)

	offset, err := writeAndSync(file, request.Snapshot.WriteHeader)
	if err != nil {
		result.Error = "could not write the recovery CSV header"
		result.RecoveryStatus = RecoveryUnverified
		return result
	}

	sender := sender{
		request:  request,
		endpoint: endpoint,
		session:  session,
		file:     file,
		offset:   offset,
		result:   &result,
		events:   events,
	}
	sender.sendAll()
	if stopping(request.Stop) {
		result.Stopped = true
	}
	return result
}

func closeRecovery(file RecoveryFile, result *Result) {
	if err := file.Close(); err != nil {
		result.RecoveryStatus = RecoveryUnverified
		addError(result, "could not close the recovery CSV")
	}
	if result.RecoveryStatus == RecoveryComplete && result.Unconfirmed == 0 {
		if err := os.Remove(file.Name()); err != nil {
			addError(result, "could not remove the header-only recovery CSV")
			return
		}
		result.RecoveryPath = ""
		result.RecoveryStatus = ""
	}
}

type sender struct {
	request  Request
	endpoint smail.Endpoint
	session  *sendlimits.Session
	file     RecoveryFile
	offset   int64
	result   *Result
	events   chan<- Event
}

func (s *sender) sendAll() {
	for _, message := range s.request.Messages {
		if stopping(s.request.Stop) {
			s.result.Stopped = true
			break
		}
		if s.session.Pacing() {
			s.events <- Event{Progress: Progress{
				Counts: s.result.Counts,
				Line:   message.Record.Line,
				Phase:  WaitingForLimit,
			}}
		}
		if err := s.session.WaitAndReserve(s.request.Stop); err != nil {
			if errors.Is(err, sendlimits.ErrStopped) {
				s.result.Stopped = true
			} else {
				s.result.Error = err.Error()
			}
			break
		}
		if stopping(s.request.Stop) {
			s.result.Stopped = true
			break
		}
		s.events <- Event{Progress: Progress{
			Counts: s.result.Counts,
			Line:   message.Record.Line,
			Phase:  Submitting,
		}}
		if stopping(s.request.Stop) {
			s.result.Stopped = true
			break
		}
		s.session.MarkStarted()
		outcome := s.request.Adapter.Send(s.request.Account, s.endpoint, message)
		switch outcome.Outcome {
		case smail.Accepted:
			s.result.Accepted++
			s.result.Unattempted--
		case smail.Unconfirmed:
			s.result.Unconfirmed++
			s.result.Unattempted--
			s.result.UnconfirmedRows = append(s.result.UnconfirmedRows, Issue{
				Line:   message.Record.Line,
				Reason: outcome.Reason,
			})
			next, err := writeAndSync(s.file, func(w io.Writer) error {
				return s.request.Snapshot.WriteRecord(w, message.Record)
			})
			if err != nil {
				s.result.UnrecordedLines = append(s.result.UnrecordedLines, message.Record.Line)
				s.result.Error = "could not save a row to the recovery CSV"
				s.result.RecoveryStatus = RecoveryPrefix
				if s.file.Truncate(s.offset) != nil || s.file.Sync() != nil {
					s.result.RecoveryStatus = RecoveryUnverified
				}
				return
			}
			s.offset = next
		}
		if outcome.Stop || outcome.Outcome == smail.Unattempted {
			s.result.Error = outcome.Reason
			break
		}
	}
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
