package smail

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"pp_email/internal/csvdata"
)

func TestAccountListing(t *testing.T) {
	adapter := New("/fixture/s-nail", []string{"MAILRC=/fixture/rc"})
	adapter.launch = func(call invocation) observation {
		want := []string{"-X", `\account`, "-X", `\exit 0`, "-.", probeAddress}
		if !slices.Equal(call.args, want) || call.body != "" || call.timeout != probeTimeout {
			t.Fatalf("unexpected listing call: %+v", call)
		}
		output := "account first {\n  set password=secret\n}\n\n" +
			"account null {\n}\naccount second {\n}\n"
		return observation{success: true, stdout: output}
	}
	names, err := adapter.ListAccounts()
	if err != nil || !slices.Equal(names, []string{"first", "second"}) {
		t.Fatalf("listing = %v, %v", names, err)
	}
	if _, err := parseAccounts("account first {\nset password=secret"); err == nil ||
		strings.Contains(err.Error(), "secret") {
		t.Fatalf("missing sanitized parsing error: %v", err)
	}
}

func TestProbeAndSendContract(t *testing.T) {
	adapter := New("/fixture/s-nail", []string{"MAILRC=/fixture/rc"})
	adapter.lookup = func(_ context.Context, network, host string) ([]netip.Addr, error) {
		if network != "ip" || host != "smtp.example.invalid" {
			t.Fatalf("unexpected lookup: %s %s", network, host)
		}
		return []netip.Addr{netip.MustParseAddr("192.0.2.18")}, nil
	}
	adapter.localAddresses = func() ([]netip.Addr, error) { return nil, nil }
	message := csvdata.Message{Recipient: "Name <to@example.invalid>", Subject: "subject", Body: "~s literal\n.\nbody"}
	calls := 0
	adapter.launch = func(call invocation) observation {
		calls++
		tag := envValue(call.env, "PP_EMAIL_PROBE_TAG")
		if tag == "" || call.path != "/fixture/s-nail" {
			t.Fatalf("missing tagged invocation: %+v", call)
		}
		if calls == 1 {
			want := []string{"-A", "chosen", "-X", `echo "$PP_EMAIL_PROBE_TAG=$mta"`, "-X", `\exit 0`, "-.", probeAddress}
			if !slices.Equal(call.args, want) || call.body != "" || call.timeout != probeTimeout {
				t.Fatalf("unexpected probe: %+v", call)
			}
			return observation{success: true, stdout: tag + "=smtps://user:secret@smtp.example.invalid:465\n"}
		}
		want := []string{"-A", "chosen", "-X", guardScript, "-s", message.Subject, "-.", message.Recipient}
		if !slices.Equal(call.args, want) || call.body != message.Body || call.timeout != sendTimeout {
			t.Fatalf("unexpected send: %+v", call)
		}
		pattern := envValue(call.env, "PP_EMAIL_MTA_RE")
		re, err := regexp.CompilePOSIX(pattern)
		if err != nil || strings.Contains(pattern, "secret") ||
			!re.MatchString("smtps://user:new@smtp.example.invalid:465") ||
			re.MatchString("smtps://smtp.example.invalid") || re.MatchString("/usr/bin/false") {
			t.Fatalf("incorrect guard pattern %q: %v", pattern, err)
		}
		return observation{started: true, success: true, stdout: tag + " READY\n"}
	}
	endpoint, err := adapter.CheckAccount("chosen")
	if err != nil {
		t.Fatal(err)
	}
	if got := adapter.Send("chosen", endpoint, message); got.Outcome != Accepted || got.Stop {
		t.Fatalf("submit = %+v", got)
	}
	if calls != 2 {
		t.Fatalf("made %d calls, want 2", calls)
	}
}

func TestEndpointRestrictions(t *testing.T) {
	adapter := New("", nil)
	adapter.localAddresses = func() ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("198.51.100.10")}, nil
	}
	adapter.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.18"), netip.MustParseAddr("::1")}, nil
	}
	for _, raw := range []string{
		"", "/usr/bin/false", "http://smtp.example.invalid", "smtp://",
		"smtp://name:", "smtp://name:0", "smtp://name:65536", "smtp://name/path", "smtp://user:secret@example.invalid?cache",
	} {
		if _, _, err := smtpURL(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("unexpected URL result for %q: %v", raw, err)
		}
	}
	hosts := []string{
		"localhost.", "127.0.0.1", "0.0.0.0", "::1",
		"::ffff:198.51.100.10", "mixed.example.invalid",
	}
	for _, host := range hosts {
		if err := adapter.checkRemoteHost(host); err == nil {
			t.Errorf("allowed local host %q", host)
		}
	}
	if err := adapter.checkRemoteHost("2001:db8::13"); err != nil {
		t.Fatal(err)
	}
	u, authority, err := smtpURL("submissions://user@[2001:db8::13]:465")
	if err != nil || u.Hostname() != "2001:db8::13" || authority != "[2001:db8::13]:465" {
		t.Fatalf("IPv6 URL = %v, %q, %v", u, authority, err)
	}
}

func TestOutcomes(t *testing.T) {
	for _, test := range []struct {
		name   string
		obs    observation
		result Outcome
		stop   bool
	}{
		{
			name:   "start error",
			obs:    observation{},
			result: Unattempted,
			stop:   true,
		},
		{
			name:   "guard",
			obs:    observation{started: true, exitCode: 71, stdout: "tag BLOCKED\n"},
			result: Unattempted,
			stop:   true,
		},
		{
			name:   "success",
			obs:    observation{started: true, success: true, stdout: "tag READY\n"},
			result: Accepted,
			stop:   false,
		},
		{
			name:   "nonzero after ready",
			obs:    observation{started: true, exitCode: 71, stdout: "tag READY\n"},
			result: Unconfirmed,
			stop:   false,
		},
		{
			name:   "timeout after ready",
			obs:    observation{started: true, timedOut: true, stdout: "tag READY\n"},
			result: Unconfirmed,
			stop:   false,
		},
		{
			name:   "signal after ready",
			obs:    observation{started: true, signalled: true, stdout: "tag READY\n"},
			result: Unconfirmed,
			stop:   false,
		},
		{
			name:   "no marker",
			obs:    observation{started: true, success: true, stdout: "secret\n"},
			result: Unconfirmed,
			stop:   true,
		},
		{
			name:   "conflicting markers",
			obs:    observation{started: true, exitCode: 71, stdout: "tag BLOCKED\ntag READY\n"},
			result: Unconfirmed,
			stop:   true,
		},
		{
			name:   "repeated marker",
			obs:    observation{started: true, exitCode: 71, stdout: "tag BLOCKED\ntag BLOCKED\n"},
			result: Unconfirmed,
			stop:   true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := classify(test.obs, "tag")
			if result.Outcome != test.result || result.Stop != test.stop || strings.Contains(result.Reason, "secret") {
				t.Fatalf("wrong result: %+v", result)
			}
		})
	}
}

func TestLaunchKillsProcessGroup(t *testing.T) {
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result := run(invocation{
		path: path, args: []string{"-test.run=^TestSmailHelper$"},
		env: append(os.Environ(), "PP_EMAIL_TEST_HELPER=parent"), timeout: 2 * time.Second,
	})
	if !result.started || !result.timedOut || !result.signalled {
		t.Fatalf("expected a timed-out parent: %+v", result)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(result.stdout))
	if err != nil || pid <= 0 {
		t.Fatalf("could not read helper PID: %q", result.stdout)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) || strings.Contains(string(stat), ") Z ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("descendant PID %d survived timeout", pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func TestSmailHelper(_ *testing.T) {
	switch os.Getenv("PP_EMAIL_TEST_HELPER") {
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestSmailHelper$")
		child.Env = append(os.Environ(), "PP_EMAIL_TEST_HELPER=child")
		if child.Start() != nil {
			os.Exit(2)
		}
		fmt.Println(child.Process.Pid)
		time.Sleep(time.Hour)
	case "child":
		time.Sleep(time.Hour)
	}
}

func TestInstalledSmailDryRun(t *testing.T) {
	if os.Getenv("PP_EMAIL_TEST_SNAIL") != "1" {
		t.Skip("set PP_EMAIL_TEST_SNAIL=1 to exercise the installed s-nail")
	}
	path, err := exec.LookPath("s-nail")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	config := filepath.Join(directory, "mailrc")
	err = os.WriteFile(config, []byte(`set v15-compat=yes
if send
  account local {
    set mta=/usr/bin/false
  }
  account remote {
    set from=sender@example.invalid
    set mta=smtps://smtp.example.invalid:465 smtp-auth=none
  }
endif
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	adapter := New(path, []string{"HOME=" + directory, "MAILRC=" + config, "LC_ALL=C", "PATH=/usr/bin:/bin"})
	adapter.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
	}
	adapter.localAddresses = func() ([]netip.Addr, error) { return nil, nil }
	var last observation
	var dryRun invocation
	adapter.launch = func(call invocation) observation {
		if index := slices.Index(call.args, guardScript); index >= 0 {
			call.args[index] = strings.Replace(guardScript, "skipempty debug", "skipempty", 1)
			call.args = append([]string{"-:u", "-d"}, call.args...)
			dryRun = call
		} else {
			call.args = append([]string{"-:u"}, call.args...)
		}
		last = run(call)
		return last
	}
	names, err := adapter.ListAccounts()
	slices.Sort(names)
	if err != nil || !slices.Equal(names, []string{"local", "remote"}) {
		t.Fatalf("discover: %v, %v", names, err)
	}
	if _, err := adapter.CheckAccount("local"); err == nil {
		t.Fatal("allowed local preflight")
	}
	endpoint, err := adapter.CheckAccount("remote")
	if err != nil {
		t.Fatal(err)
	}
	csvPath := filepath.Join(directory, "messages.csv")
	source := "Name,Address,Subject,Body\n" +
		`Zoë,recipient@example.invalid,Dry run,~s literal\n.\nfirst\r\nsecond\tcolumn\vvertical\rreturn\n` + "\n"
	if err := os.WriteFile(csvPath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := csvdata.Read(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	messages, issues := snapshot.Prepare(csvdata.Mapping{Name: 0, Address: 1, Subject: 2, Body: 3})
	if len(messages) != 1 || len(issues) != 0 {
		t.Fatalf("prepare = %v, %v", messages, issues)
	}
	message := messages[0]
	if got := adapter.Send("remote", endpoint, message); got.Outcome != Accepted {
		t.Fatalf("remote dry-run adapter result: %+v", got)
	}
	if want := "~s literal\n.\nfirst\r\nsecond\tcolumn\vvertical\rreturn\n"; dryRun.body != want {
		t.Errorf("s-nail stdin = %q, want %q", dryRun.body, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, dryRun.path, dryRun.args...)
	cmd.Env = dryRun.env
	cmd.Stdin = strings.NewReader(dryRun.body)
	trace, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run trace: %v\n%s", err, trace)
	}
	for _, want := range []string{
		"READ 10 bytes <~s literal>", "READ 1 bytes <.>", "recipient@example.invalid>",
		"Content-Type: text/plain", "=0Bvertical=0Dreturn",
	} {
		if !strings.Contains(string(trace), want) {
			t.Errorf("dry-run trace missing %q:\n%s", want, trace)
		}
	}
	if got := adapter.Send("local", endpoint, message); got.Outcome != Unattempted || last.exitCode != 71 {
		t.Fatalf("local dry-run adapter result: %+v, exit %d", got, last.exitCode)
	}
}

func envValue(env []string, key string) string {
	for index := len(env) - 1; index >= 0; index-- {
		if value, ok := strings.CutPrefix(env[index], key+"="); ok {
			return value
		}
	}
	return ""
}
