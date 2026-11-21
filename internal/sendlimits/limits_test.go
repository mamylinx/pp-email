package sendlimits

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const exampleConfig = `{"work":{"messages_per_minute":6,"max_per_24_hours":2}}`

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limits.json")
	if limits, err := Load(path); err != nil || limits != nil {
		t.Fatalf("missing config: %v, %v", limits, err)
	}
	for _, content := range []string{
		`null`, `[`, `{} {}`, `{"work":null}`,
		`{"work":{"messages_per_minute":6}}`,
		`{"work":{"messages_per_minute":0,"max_per_24_hours":2}}`,
		`{"work":{"messages_per_minute":6,"max_per_24_hours":2,"typo":1}}`,
	} {
		writeConfig(t, path, content)
		if _, err := Load(path); err == nil {
			t.Errorf("invalid config accepted: %s", content)
		}
	}

	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	writeConfig(t, path, exampleConfig)
	limits, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := limits.Status("work")
	if limits.stateDir != filepath.Join(stateHome, "pp-email") ||
		err != nil || status != (Status{Policy{6, 2}, 2}) {
		t.Fatalf("initial state: %s, %+v, %v", limits.stateDir, status, err)
	}
	if session, err := limits.Begin("other", 100); session != nil || err != nil {
		t.Fatalf("unconfigured account: %v, %v", session, err)
	}
	if _, err := os.Stat(limits.stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status and unconfigured account should omit state directory: %v", err)
	}
}

func TestLoadSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "limits.json")
	target := filepath.Join(dir, "policy.json")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("broken limits symlink: got %v, want a missing target error", err)
	}
	t.Setenv("XDG_STATE_HOME", dir)
	writeConfig(t, target, exampleConfig)
	limits, err := Load(path)
	if err != nil || limits == nil {
		t.Fatalf("valid limits symlink: got %v, %v", limits, err)
	}
}

func TestPacingAndRollingBudgetAcrossSessions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "limits.json")
	writeConfig(t, path, exampleConfig)
	now := time.Date(2026, time.November, 21, 0, 0, 0, 0, time.UTC)
	var lastDelay time.Duration
	load := func() *Limits {
		limits, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		limits.now = func() time.Time { return now }
		limits.wait = func(delay time.Duration, _ <-chan struct{}) error {
			lastDelay = max(delay, 0)
			now = now.Add(lastDelay)
			return nil
		}
		return limits
	}
	begin := func(limits *Limits, size int) *Session {
		session, err := limits.Begin("work", size)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { session.lock.Close() })
		return session
	}
	reserve := func(session *Session, wantDelay time.Duration) {
		if err := session.WaitAndReserve(nil); err != nil || lastDelay != wantDelay {
			t.Fatalf("reserve: delay %s, want %s, error %v", lastDelay, wantDelay, err)
		}
	}
	closeSession := func(session *Session) {
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	}

	limits := load()
	first := begin(limits, 1)
	if _, err := load().Begin("work", 1); err == nil || !strings.Contains(err.Error(), "another pp-email instance") {
		t.Fatalf("overlapping session: %v", err)
	}
	reserve(first, 0)
	now = now.Add(2 * time.Second)
	firstStart := now
	first.MarkStarted()
	closeSession(first)

	limits = load()
	info, err := os.Stat(limits.accountPath("work") + ".json")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger permissions: %v", info.Mode().Perm())
	}
	status, err := limits.Status("work")
	if err != nil || status.Remaining != 1 {
		t.Fatalf("status after restart: %+v, %v", status, err)
	}
	if _, err := limits.Begin("work", 2); err == nil || !strings.Contains(err.Error(), "1 remain") {
		t.Fatalf("batch larger than remaining budget: %v", err)
	}
	second := begin(limits, 1)
	reserve(second, 10*time.Second)
	second.MarkStarted()
	closeSession(second)

	now = firstStart.Add(24 * time.Hour)
	status, err = limits.Status("work")
	if err != nil || status.Remaining != 1 {
		t.Fatalf("rolling budget after expiry: %+v, %v", status, err)
	}
	third := begin(limits, 1)
	reserve(third, 0)
	closeSession(third)
	status, err = limits.Status("work")
	if err != nil || status.Remaining != 1 {
		t.Fatalf("stopped before starting: %+v, %v", status, err)
	}
}

func TestPacingWithinSessionAndPendingReservation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "limits.json")
	writeConfig(t, path, exampleConfig)
	limits, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	limits.now = func() time.Time { return now }
	var delay time.Duration
	limits.wait = func(wait time.Duration, stop <-chan struct{}) error {
		delay = max(0, wait)
		now = now.Add(delay)
		return waitFor(0, stop)
	}
	session, err := limits.Begin("work", 2)
	if err != nil {
		t.Fatal(err)
	}
	defer session.lock.Close()
	if err := session.WaitAndReserve(nil); err != nil || delay != 0 {
		t.Fatalf("first send delay %s, error %v", delay, err)
	}
	session.MarkStarted()
	now = now.Add(3 * time.Second)
	if err := session.WaitAndReserve(nil); err != nil || delay != 7*time.Second {
		t.Fatalf("second send delay %s, error %v", delay, err)
	}
	// Releasing only the lock simulates termination before settling the attempt.
	if err := session.lock.Close(); err != nil {
		t.Fatal(err)
	}
	status, err := limits.Status("work")
	if err != nil || status.Remaining != 0 {
		t.Fatalf("pending reservation lost: %+v, %v", status, err)
	}
	now = now.Add(24 * time.Hour)
	restarted, err := limits.Begin("work", 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	})
	stop := make(chan struct{})
	close(stop)
	if err := restarted.WaitAndReserve(stop); !errors.Is(err, ErrStopped) {
		t.Fatalf("pacing cancellation: %v", err)
	}
}

func TestRepeatedClosePreservesLaterAttempts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "limits.json")
	writeConfig(t, path, exampleConfig)
	limits, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	limits.wait = func(time.Duration, <-chan struct{}) error { return nil }
	send := func() *Session {
		t.Helper()
		s, err := limits.Begin("work", 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WaitAndReserve(nil); err != nil {
			t.Fatal(err)
		}
		s.MarkStarted()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := send()
	send()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.WaitAndReserve(nil); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("reserve after close: got %v, want closed session error", err)
	}
	status, err := limits.Status("work")
	if err != nil || status.Remaining != 0 {
		t.Fatalf("budget after repeated close: %+v, %v", status, err)
	}
}
