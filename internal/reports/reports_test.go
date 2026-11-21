package reports

import (
	"bytes"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"pp_email/internal/batch"
	"pp_email/internal/csvdata"
	"pp_email/internal/smail"
)

func TestLinkAndSyncDirectoryFailure(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "temp")
	path := filepath.Join(dir, "export.csv")
	if err := os.WriteFile(source, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Linking uses the paths. A closed directory handle fails only at sync.
	handle, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	err = linkAndSync(source, path, handle)
	if !errors.Is(err, ErrDirectorySync) || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("directory sync failure: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "report" {
		t.Fatalf("published file: got %q, %v", data, err)
	}
}

func TestNewCopiesUnrecordedLines(t *testing.T) {
	lines := []int{4}
	report := New(Input{Result: batch.Result{UnrecordedLines: lines}})
	lines[0] = 8
	if !slices.Equal(report.UnrecordedLines, []int{4}) {
		t.Fatalf("report unrecorded lines changed: %v", report.UnrecordedLines)
	}
}

func TestReportStoreAndSpreadsheetExport(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	messages := []csvdata.Message{
		{
			Record:  csvdata.Record{Line: 2},
			Name:    "Personal Name",
			Address: "private@example.invalid",
			Subject: "Private subject",
			Body:    "Private body",
		},
		{Record: csvdata.Record{Line: 4}},
	}
	result := batch.Result{
		Counts:          batch.Counts{Accepted: 1, Unconfirmed: 1},
		UnconfirmedRows: []batch.Issue{{Line: 4, Reason: "  +reason\x1b"}},
	}
	first := New(Input{
		Account:  "=account",
		Source:   "/private/source/@send.csv",
		Started:  t0,
		Finished: t0.Add(time.Second),
		Result:   result,
		Messages: messages,
	})
	second := New(Input{
		Account:  "other account",
		Source:   "another.csv",
		Started:  t0,
		Finished: t0.Add(2 * time.Second),
		Result:   result,
		Messages: messages,
	})
	if err := Save(first); err != nil {
		t.Fatal(err)
	}
	if err := Save(second); err != nil {
		t.Fatal(err)
	}
	dir, err := directory()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, skipped, err := List()
	if err != nil || skipped != 1 || len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("list = %+v, skipped = %d, error = %v", items, skipped, err)
	}
	loaded, err := Load(first.ID)
	if err != nil || !slices.Equal(loaded.Rows, first.Rows) || loaded.Rows[1].Outcome != smail.Unconfirmed {
		t.Fatalf("loaded report = %+v, error = %v", loaded, err)
	}
	path := filepath.Join(dir, first.ID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"counts":{"Accepted":1,"Unconfirmed":1,"Unattempted":0}`)) {
		t.Errorf("saved report changed the version 1 count fields: %s", data)
	}
	privateValues := []string{
		"Personal Name", "private@example.invalid", "Private subject",
		"Private body", "/private/source/",
	}
	for _, private := range privateValues {
		if bytes.Contains(data, []byte(private)) {
			t.Errorf("saved report contains %q", private)
		}
	}
	for path, mode := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Errorf("permissions of %s = %v, %v", path, info, err)
		}
	}

	export := filepath.Join(t.TempDir(), "report.csv")
	if err := Export(export, loaded); err != nil {
		t.Fatal(err)
	}
	if err := Export(export, second); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite = %v", err)
	}
	data, err = os.ReadFile(export)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil || len(rows) != 3 || rows[1][0] != first.ID || rows[1][2] != "'=account" ||
		rows[1][3] != "'@send.csv" || rows[2][4] != "4" || rows[2][5] != "Unconfirmed" ||
		!strings.HasPrefix(rows[2][6], "'  +reason") || strings.ContainsRune(rows[2][6], '\x1b') {
		t.Fatalf("export rows = %v, %v", rows, err)
	}
}
