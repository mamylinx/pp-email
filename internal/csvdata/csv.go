package csvdata

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

const bom = "\xef\xbb\xbf"

var bodyEscapes = strings.NewReplacer(
	`\\n`, `\n`, `\n`, "\n",
	`\\r`, `\r`, `\r`, "\r",
	`\\t`, `\t`, `\t`, "\t",
	`\\v`, `\v`, `\v`, "\v",
)

type span struct {
	start, end int
}

// Snapshot keeps the decoded CSV and the source bytes used to write recovery rows.
type Snapshot struct {
	Path    string
	Headers []string
	Records []Record

	source    []byte
	header    span
	bomLength int
}

type Record struct {
	Line   int
	Fields []string

	raw span
}

type Mapping struct {
	Name, Address, Subject, Body int
}

type Message struct {
	Record    Record
	Name      string
	Address   string
	Recipient string
	Subject   string
	Body      string
}

func Read(path string) (Snapshot, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("resolve CSV path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open CSV: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Snapshot{}, errors.New("choose a regular CSV file")
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open CSV: %w", err)
	}
	return parse(absolute, data)
}

func parse(path string, data []byte) (Snapshot, error) {
	if !utf8.Valid(data) {
		return Snapshot{}, errors.New("CSV must be UTF-8")
	}

	lineStarts := []int{0}
	for i, value := range data {
		if value == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	snapshot := Snapshot{Path: path, source: data}
	if bytes.HasPrefix(data, []byte(bom)) {
		snapshot.bomLength = len(bom)
	}

	reader := csv.NewReader(bytes.NewReader(data[snapshot.bomLength:]))
	header, err := reader.Read()
	if err == io.EOF {
		return Snapshot{}, errors.New("CSV needs a header and at least one data row")
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read CSV header: %w", err)
	}

	sourceRecord := func(fields []string) Record {
		line, _ := reader.FieldPos(0)
		start := max(lineStarts[line-1], snapshot.bomLength)
		end := int(reader.InputOffset()) + snapshot.bomLength
		return Record{Line: line, Fields: fields, raw: span{start, end}}
	}

	snapshot.Headers = header
	snapshot.header = sourceRecord(header).raw
	for {
		fields, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Snapshot{}, fmt.Errorf("read CSV: %w", err)
		}
		snapshot.Records = append(snapshot.Records, sourceRecord(fields))
	}
	if len(snapshot.Records) == 0 {
		return Snapshot{}, errors.New("CSV needs at least one data row")
	}
	return snapshot, nil
}

func (s Snapshot) WriteHeader(w io.Writer) error {
	if err := write(w, s.source[:s.bomLength]); err != nil {
		return err
	}
	return write(w, s.source[s.header.start:s.header.end])
}

func (s Snapshot) WriteRecord(w io.Writer, record Record) error {
	return write(w, s.source[record.raw.start:record.raw.end])
}

func write(w io.Writer, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

// Prepare collects all invalid source rows before the user can confirm the batch.
func (s Snapshot) Prepare(mapping Mapping) ([]Message, []error) {
	for _, column := range []int{mapping.Name, mapping.Address, mapping.Subject, mapping.Body} {
		if column < 0 || column >= len(s.Headers) {
			return nil, []error{errors.New("choose a CSV column for every message field")}
		}
	}

	messages := make([]Message, 0, len(s.Records))
	var issues []error
	for _, record := range s.Records {
		message, err := prepareMessage(record, mapping)
		if err != nil {
			issues = append(issues, fmt.Errorf("line %d: %w", record.Line, err))
			continue
		}
		messages = append(messages, message)
	}
	if len(issues) != 0 {
		return nil, issues
	}
	return messages, nil
}

func prepareMessage(record Record, mapping Mapping) (Message, error) {
	name := record.Fields[mapping.Name]
	address := record.Fields[mapping.Address]
	subject := record.Fields[mapping.Subject]
	body := record.Fields[mapping.Body]

	for _, field := range []struct{ label, value string }{
		{"recipient name", name}, {"email address", address}, {"subject", subject},
	} {
		if strings.ContainsFunc(field.value, unicode.IsControl) {
			return Message{}, fmt.Errorf("%s contains a control character", field.label)
		}
	}
	if strings.ContainsFunc(body, func(r rune) bool {
		return unicode.IsControl(r) && r != '\r' && r != '\n' && r != '\t' && r != '\v'
	}) {
		return Message{}, errors.New("body contains an unsupported control character")
	}

	address = strings.TrimSpace(address)
	mailbox, err := mail.ParseAddress(address)
	if err != nil || mailbox.Name != "" || mailbox.Address != address || !strings.Contains(address, "@") {
		return Message{}, errors.New("email address must contain one plain mailbox")
	}
	mailbox.Name = name
	return Message{
		Record: record, Name: name, Address: address, Recipient: mailbox.String(),
		Subject: subject, Body: bodyEscapes.Replace(body),
	}, nil
}
