package csvdata

import (
	"bytes"
	"strings"
	"testing"
)

var ordered = Mapping{Name: 0, Address: 1, Subject: 2, Body: 3}

func TestRecoveryPreservesRawCSV(t *testing.T) {
	header := "Field,Field,Subject,Body\r\n"
	first := "Zoë,zoe@example.invalid,\"Hi, Zoë\",\"first\r\nsecond\"\r\n"
	last := "Chris,chris@example.invalid,Follow-up,last"
	data := bom + "\r\n" + header + "\r\n" + first + "\r\n" + last

	snapshot, err := parse("source.csv", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	messages, issues := snapshot.Prepare(ordered)
	if len(issues) != 0 {
		t.Fatal(issues)
	}
	if snapshot.Headers[0] != snapshot.Headers[1] || messages[0].Record.Line != 4 ||
		messages[1].Record.Line != 7 || messages[0].Name != "Zoë" ||
		messages[0].Address != "zoe@example.invalid" || messages[0].Body != "first\nsecond" {
		t.Fatalf("unexpected input: %#v", messages)
	}

	var recovered bytes.Buffer
	if err := snapshot.WriteHeader(&recovered); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.WriteRecord(&recovered, messages[0].Record); err != nil {
		t.Fatal(err)
	}
	if got, want := recovered.String(), bom+header+first; got != want {
		t.Fatalf("recovery = %q, want %q", got, want)
	}
	reopened, err := parse("recovery.csv", recovered.Bytes())
	if err != nil || len(reopened.Records) != 1 {
		t.Fatalf("cannot reopen recovery CSV: %+v, %v", reopened, err)
	}
}

func TestImportErrors(t *testing.T) {
	for _, test := range []struct {
		input, want string
	}{
		{"", "header"},
		{"Name,Email\n", "data row"},
		{"Name,Email\nmissing\n", "line 2"},
		{"Name,Email\n\"open,quote", "line 2"},
		{"Name,Email\n\xff,example", "UTF-8"},
	} {
		if _, err := parse("source.csv", []byte(test.input)); err == nil ||
			!strings.Contains(err.Error(), test.want) {
			t.Errorf("input %q: got %v, want error containing %q", test.input, err, test.want)
		}
	}
}

func TestBodyEscapesPreserveSource(t *testing.T) {
	header := "Name,Email,Subject,Body\n"
	row := `Ada,ada@example.invalid,Literal\nsubject,"First\nSecond` + "\n" +
		`Third\r\nFourth\ttab\vvertical\rreturn\nLiterals: \\n \\r \\t \\v and \x"` + "\n"
	snapshot, err := parse("source.csv", []byte(header+row))
	if err != nil {
		t.Fatal(err)
	}
	messages, issues := snapshot.Prepare(ordered)
	if len(messages) != 1 || len(issues) != 0 {
		t.Fatalf("prepare = %v, %v", messages, issues)
	}
	message := messages[0]
	if want := "First\nSecond\nThird\r\nFourth\ttab\vvertical\rreturn\nLiterals: \\n \\r \\t \\v and \\x"; message.Body != want {
		t.Errorf("body = %q, want %q", message.Body, want)
	}
	if message.Subject != `Literal\nsubject` {
		t.Errorf("subject = %q", message.Subject)
	}
	var recovered bytes.Buffer
	if err := snapshot.WriteRecord(&recovered, message.Record); err != nil {
		t.Fatal(err)
	}
	if recovered.String() != row {
		t.Errorf("recovery row = %q, want %q", recovered.String(), row)
	}
}

func TestMessageValidation(t *testing.T) {
	for _, test := range []struct {
		row, want string
	}{
		{", alice@example.invalid ,,", ""},
		{"A,alice@example.invalid,Hi,\"one\ttwo\nthree\vfour\rreturn\"", ""},
		{"A,user,Hi,Body", "email address"},
		{"A,\"Alice <alice@example.invalid>\",Hi,Body", "email address"},
		{"A,\"a@example.invalid,b@example.invalid\",Hi,Body", "email address"},
		{"A,alice@example.invalid,\"Hi\nBcc: x\",Body", "subject"},
		{"A,alice@example.invalid,Hi,\x00", "body"},
	} {
		snapshot, err := parse("source.csv", []byte("Name,Email,Subject,Body\n"+test.row))
		if err != nil {
			t.Fatal(err)
		}
		messages, issues := snapshot.Prepare(ordered)
		if test.want == "" {
			if len(messages) != 1 || len(issues) != 0 {
				t.Errorf("input %q: %v", test.row, issues)
			}
		} else if len(issues) != 1 || len(messages) != 0 ||
			!strings.Contains(issues[0].Error(), test.want) || !strings.Contains(issues[0].Error(), "line 2") {
			t.Errorf("input %q: got %v, want line 2 and %q", test.row, issues, test.want)
		}
		if messages, issues := snapshot.Prepare(Mapping{Name: -1}); len(messages) != 0 || len(issues) != 1 {
			t.Fatalf("missing mapping returned %v, %v", messages, issues)
		}
	}
}
