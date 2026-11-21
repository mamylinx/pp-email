package reports

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"pp_email/internal/appstate"
	"pp_email/internal/batch"
	"pp_email/internal/csvdata"
	"pp_email/internal/smail"
)

const version = 1

type Summary struct {
	ID       string       `json:"id"`
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Account  string       `json:"account"`
	Source   string       `json:"source"`
	Status   string       `json:"status"`
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

func New(account, source string, started, finished time.Time, result batch.Result, messages []csvdata.Message) Report {
	status := "Completed"
	switch {
	case result.Error != "":
		status = "Aborted"
	case result.Stopped:
		status = "Stopped"
	}
	rows := result.Rows(messages)
	for i := range rows {
		rows[i].Reason = CleanText(rows[i].Reason)
	}
	return Report{
		Version: version, Summary: Summary{
			ID: rand.Text(), Started: started.UTC(), Finished: finished.UTC(),
			Account: CleanText(account), Source: CleanText(filepath.Base(source)), Status: status, Counts: result.Counts,
		},
		Error: CleanText(result.Error), RecoveryPath: result.RecoveryPath, RecoveryStatus: result.RecoveryStatus,
		UnrecordedLines: result.UnrecordedLines, FirstUnattemptedLine: result.FirstUnattemptedLine, Rows: rows,
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
	var summaries []Summary
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
	if id == "" || strings.ContainsAny(id, "/\\") {
		return Report{}, errors.New("invalid report ID")
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
	info, err := os.Lstat(path)
	if err != nil {
		return Report{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return Report{}, errors.New("the report must be a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		return Report{}, err
	}
	if report.Version != version || report.ID+".json" != filepath.Base(path) ||
		report.Started.IsZero() || report.Finished.IsZero() || !validRows(report) {
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
		if err := csvFile.Write([]string{"run_id", "timestamp", "account", "source_file", "source_line", "outcome", "diagnostic"}); err != nil {
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
func publish(path string, write func(io.Writer) error) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	file, err := os.CreateTemp(dir.Name(), ".pp-email-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	writeErr := write(file)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := os.Link(file.Name(), path); err != nil {
		return err
	}
	return dir.Sync()
}
