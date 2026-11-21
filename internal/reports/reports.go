// Package reports stores batch outcomes in run history and exports them as CSV.
package reports

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"pp_email/internal/appstate"
	"pp_email/internal/batch"
	"pp_email/internal/csvdata"
	"pp_email/internal/smail"
)

const version = 1

// ErrDirectorySync indicates that the file exists but its directory could not be synced.
var ErrDirectorySync = errors.New("report created but directory sync failed")

// ErrPublishedCleanup indicates that the report exists and cleanup failed.
var ErrPublishedCleanup = errors.New("report created but cleanup failed")

type Status string

const (
	Completed Status = "Completed"
	Stopped   Status = "Stopped"
	Aborted   Status = "Aborted"
)

type Summary struct {
	ID       string       `json:"id"`
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Account  string       `json:"account"`
	Source   string       `json:"source"`
	Status   Status       `json:"status"`
	Counts   batch.Counts `json:"counts"`
}

type Report struct {
	Version int `json:"version"`
	Summary
	Error                string               `json:"error,omitempty"`
	RecoveryPath         string               `json:"recovery_path,omitempty"`
	RecoveryStatus       batch.RecoveryStatus `json:"recovery_status,omitempty"`
	UnrecordedLines      []int                `json:"unrecorded_lines,omitempty"`
	FirstUnattemptedLine int                  `json:"first_unattempted_line,omitempty"`
	Rows                 []batch.RowResult    `json:"rows"`
}

type Input struct {
	Account  string
	Source   string
	Started  time.Time
	Finished time.Time
	Result   batch.Result
	Messages []csvdata.Message
}

func New(input Input) Report {
	status := Completed
	switch {
	case input.Result.Error != "":
		status = Aborted
	case input.Result.Stopped:
		status = Stopped
	}
	rows := input.Result.Rows(input.Messages)
	for i := range rows {
		rows[i].Reason = CleanText(rows[i].Reason)
	}
	return Report{
		Version: version,
		Summary: Summary{
			ID:       rand.Text(),
			Started:  input.Started.UTC(),
			Finished: input.Finished.UTC(),
			Account:  CleanText(input.Account),
			Source:   CleanText(filepath.Base(input.Source)),
			Status:   status,
			Counts:   input.Result.Counts,
		},
		Error:                CleanText(input.Result.Error),
		RecoveryPath:         input.Result.RecoveryPath,
		RecoveryStatus:       input.Result.RecoveryStatus,
		UnrecordedLines:      slices.Clone(input.Result.UnrecordedLines),
		FirstUnattemptedLine: input.Result.FirstUnattemptedLine,
		Rows:                 rows,
	}
}

func OutcomeLabel(outcome smail.Outcome) string {
	switch outcome {
	case smail.Accepted:
		return "Accepted"
	case smail.Unconfirmed:
		return "Unconfirmed"
	default:
		return "Unattempted"
	}
}

func CleanText(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return '�'
		}
		return r
	}, value)
}

func directory() (string, error) {
	base, err := appstate.Directory()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "reports"), nil
}

func Save(report Report) error {
	if !validID(report.ID) {
		return errors.New("invalid report id")
	}
	dir, err := directory()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := checkDirectory(dir); err != nil {
		return err
	}
	return publish(filepath.Join(dir, report.ID+".json"), func(w io.Writer) error {
		return json.NewEncoder(w).Encode(report)
	})
}

func List() ([]Summary, int, error) {
	dir, err := directory()
	if err != nil {
		return nil, 0, err
	}
	if err := checkDirectory(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	summaries := make([]Summary, 0, len(entries))
	var skipped int
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		report, err := read(filepath.Join(dir, entry.Name()))
		if err != nil {
			skipped++
			continue
		}
		summaries = append(summaries, report.Summary)
	}
	Sort(summaries)
	return summaries, skipped, nil
}

func Sort(summaries []Summary) {
	slices.SortFunc(summaries, func(a, b Summary) int {
		if byTime := b.Finished.Compare(a.Finished); byTime != 0 {
			return byTime
		}
		return strings.Compare(b.ID, a.ID)
	})
}

func Load(id string) (Report, error) {
	if !validID(id) {
		return Report{}, errors.New("invalid report id")
	}
	dir, err := directory()
	if err != nil {
		return Report{}, err
	}
	if err := checkDirectory(dir); err != nil {
		return Report{}, err
	}
	return read(filepath.Join(dir, id+".json"))
}

func validID(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\")
}

func checkDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("the reports directory must be private (mode 0700)")
	}
	return nil
}

func read(path string) (Report, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Report{}, err
	}
	info, err := file.Stat()
	if err != nil {
		return Report{}, errors.Join(err, file.Close())
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Report{}, errors.Join(errors.New("the report must be a private regular file"), file.Close())
	}
	data, readErr := io.ReadAll(file)
	if err := errors.Join(readErr, file.Close()); err != nil {
		return Report{}, err
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, err
	}
	formatValid := report.Version == version && report.ID+".json" == filepath.Base(path)
	timesValid := !report.Started.IsZero() && !report.Finished.IsZero()
	statusValid := report.Status == Completed || report.Status == Stopped || report.Status == Aborted
	recognized := formatValid && timesValid && statusValid && validRows(report)
	if !recognized {
		return Report{}, errors.New("unrecognized report format")
	}
	return report, nil
}

func validRows(report Report) bool {
	var counts batch.Counts
	lastLine := 0
	for _, row := range report.Rows {
		if row.Line <= lastLine {
			return false
		}
		lastLine = row.Line
		switch row.Outcome {
		case smail.Accepted:
			counts.Accepted++
		case smail.Unconfirmed:
			counts.Unconfirmed++
		case smail.Unattempted:
			counts.Unattempted++
		default:
			return false
		}
	}
	return counts == report.Counts && lastLine != 0
}

func Export(path string, report Report) error {
	return publish(path, func(w io.Writer) error {
		csvFile := csv.NewWriter(w)
		header := []string{
			"run_id", "timestamp", "account", "source_file",
			"source_line", "outcome", "diagnostic",
		}
		if err := csvFile.Write(header); err != nil {
			return err
		}
		for _, row := range report.Rows {
			if err := csvFile.Write([]string{
				spreadsheetText(report.ID), report.Finished.Format(time.RFC3339), spreadsheetText(report.Account),
				spreadsheetText(report.Source), strconv.Itoa(row.Line), OutcomeLabel(row.Outcome), spreadsheetText(row.Reason),
			}); err != nil {
				return err
			}
		}
		csvFile.Flush()
		return csvFile.Error()
	})
}

func spreadsheetText(value string) string {
	value = CleanText(value)
	trimmed := strings.TrimLeftFunc(value, unicode.IsSpace)
	if trimmed != "" && strings.ContainsRune("=+-@＝＋－＠", []rune(trimmed)[0]) {
		return "'" + value
	}
	return value
}

// publish writes a private file and exposes it only after the contents are complete.
func publish(path string, write func(io.Writer) error) (err error) {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	var published bool
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = joinCleanupError(err, fmt.Errorf("close report directory: %w", closeErr), published)
		}
	}()
	file, err := os.CreateTemp(dir.Name(), ".pp-email-report-*")
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = joinCleanupError(err, fmt.Errorf("remove temporary report file: %w", removeErr), published)
		}
	}()
	writeErr := write(file)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	err = linkAndSync(file.Name(), path, dir)
	published = err == nil || errors.Is(err, ErrDirectorySync)
	return err
}

func joinCleanupError(err, cleanupErr error, published bool) error {
	if published && !errors.Is(err, ErrPublishedCleanup) {
		err = errors.Join(err, ErrPublishedCleanup)
	}
	return errors.Join(err, cleanupErr)
}

func linkAndSync(source, path string, dir *os.File) error {
	if err := os.Link(source, path); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("%w: %w", ErrDirectorySync, err)
	}
	return nil
}
