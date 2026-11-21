// Package sendlimits enforces per-account pacing and rolling 24-hour send budgets.
package sendlimits

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"pp_email/internal/appstate"
)

const budgetWindow = 24 * time.Hour

var ErrStopped = errors.New("sending stopped")

type Policy struct {
	MessagesPerMinute int `json:"messages_per_minute"`
	MaxPer24Hours     int `json:"max_per_24_hours"`
}

func (p Policy) interval() time.Duration {
	return (time.Minute-1)/time.Duration(p.MessagesPerMinute) + 1
}

type Limits struct {
	policies map[string]Policy
	stateDir string
	now      func() time.Time
	wait     func(time.Duration, <-chan struct{}) error
}

func Load(path string) (*Limits, error) {
	var policies map[string]Policy
	if err := readJSON(path, &policies); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			info, statErr := os.Lstat(path)
			switch {
			case errors.Is(statErr, os.ErrNotExist):
				return nil, nil
			case statErr != nil:
				return nil, statErr
			case info.Mode()&os.ModeSymlink != 0:
				return nil, fmt.Errorf("resolve send limits symlink: %w", err)
			}
		}
		return nil, err
	}
	if policies == nil {
		return nil, errors.New("expected an object keyed by s-nail account name")
	}
	for account, policy := range policies {
		validPolicy := account != "" &&
			policy.MessagesPerMinute > 0 &&
			policy.MaxPer24Hours > 0
		if !validPolicy {
			return nil, fmt.Errorf(
				"account %q needs a name and positive messages_per_minute and max_per_24_hours values",
				account,
			)
		}
	}
	if len(policies) == 0 {
		return nil, nil
	}
	stateDir, err := appstate.Directory()
	if err != nil {
		return nil, err
	}
	return &Limits{
		policies: policies, stateDir: stateDir,
		now: time.Now, wait: waitFor,
	}, nil
}

type Status struct {
	Policy
	Remaining int
}

func (l *Limits) Status(account string) (Status, error) {
	policy, ok := l.policy(account)
	if !ok {
		return Status{}, nil
	}
	history, err := readLedger(l.accountPath(account) + ".json")
	if err != nil {
		return Status{Policy: policy}, err
	}
	history.expire(l.now())
	return Status{Policy: policy, Remaining: max(0, policy.MaxPer24Hours-len(history.Attempts))}, nil
}

func (l *Limits) policy(account string) (Policy, bool) {
	if l == nil {
		return Policy{}, false
	}
	policy, ok := l.policies[account]
	return policy, ok
}

func (l *Limits) accountPath(account string) string {
	return filepath.Join(l.stateDir, fmt.Sprintf("%x", sha256.Sum256([]byte(account))))
}

type ledger struct {
	Attempts []time.Time `json:"attempts"`
	Pending  bool        `json:"pending,omitempty"`
}

func (l *ledger) expire(now time.Time) {
	cutoff := now.Add(-budgetWindow)
	kept := l.Attempts[:0]
	for _, attemptedAt := range l.Attempts {
		if attemptedAt.After(cutoff) {
			kept = append(kept, attemptedAt)
		}
	}
	l.Attempts = kept
}

// Session holds the budget lock for a batch and must not be copied.
type Session struct {
	noCopy    noCopy
	limits    *Limits
	policy    Policy
	lock      *os.File
	path      string
	history   ledger
	nextStart time.Time
	reserved  bool
	dirty     bool
}

// noCopy lets go vet detect copies of an active session.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// Begin checks that the batch fits within the account's remaining send budget
// and holds the account lock until the caller closes the session.
// It returns a nil session and a nil error when the account has no configured limits.
func (l *Limits) Begin(account string, size int) (session *Session, err error) {
	policy, ok := l.policy(account)
	if !ok {
		return nil, nil
	}
	if err := os.MkdirAll(l.stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create send budget directory: %w", err)
	}
	base := l.accountPath(account)
	lock, err := os.OpenFile(base+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open send budget lock: %w", err)
	}
	defer func() {
		if session == nil {
			if closeErr := lock.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("close send budget lock: %w", closeErr))
			}
		}
	}()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another pp-email instance is sending with this account")
		}
		return nil, fmt.Errorf("lock send budget: %w", err)
	}
	path := base + ".json"
	history, err := readLedger(path)
	if err != nil {
		return nil, err
	}
	now := l.now()
	var nextStart time.Time
	if history.Pending {
		// A prior process exited before recording when its last attempt started.
		nextStart = now.Add(policy.interval())
	} else if len(history.Attempts) > 0 {
		nextStart = history.Attempts[len(history.Attempts)-1].Add(policy.interval())
	}
	history.expire(now)
	remaining := max(0, policy.MaxPer24Hours-len(history.Attempts))
	if size > remaining {
		return nil, fmt.Errorf("batch size %d exceeds the rolling 24-hour budget of %d remaining", size, remaining)
	}
	return &Session{
		limits: l, policy: policy, lock: lock, path: path,
		history: history, nextStart: nextStart,
	}, nil
}

func (s *Session) WaitAndReserve(stop <-chan struct{}) error {
	if s == nil {
		return nil
	}
	if s.lock == nil {
		return os.ErrClosed
	}
	if err := s.limits.wait(s.nextStart.Sub(s.limits.now()), stop); err != nil {
		return err
	}
	now := s.limits.now()
	s.history.expire(now)
	history := ledger{Attempts: append(s.history.Attempts, now), Pending: true}
	if err := writeLedger(s.path, history); err != nil {
		return err
	}
	s.history = history
	s.reserved, s.dirty = true, true
	return nil
}

// NeedsDelay reports whether the next reservation is scheduled for the future.
func (s *Session) NeedsDelay() bool {
	return s != nil && s.lock != nil && s.nextStart.After(s.limits.now())
}

// MarkStarted records the start just before the SMTP invocation. The next
// reservation or Close persists it. Until then the reservation counts on disk.
func (s *Session) MarkStarted() {
	if s == nil || s.lock == nil {
		return
	}
	now := s.limits.now()
	s.history.Attempts[len(s.history.Attempts)-1] = now
	s.history.Pending = false
	s.nextStart = now.Add(s.policy.interval())
	s.reserved = false
}

func (s *Session) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	lock := s.lock
	s.lock = nil
	var err error
	if s.dirty {
		if s.reserved {
			s.history.Attempts = s.history.Attempts[:len(s.history.Attempts)-1]
			s.history.Pending = false
		}
		err = writeLedger(s.path, s.history)
	}
	return errors.Join(err, lock.Close())
}

func waitFor(delay time.Duration, stop <-chan struct{}) error {
	if delay <= 0 {
		select {
		case <-stop:
			return ErrStopped
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-stop:
		return ErrStopped
	case <-timer.C:
		return nil
	}
}

func readJSON(path string, target any) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close json file: %w", closeErr))
		}
	}()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return errors.New("unexpected data after JSON value")
	}
	return nil
}

func readLedger(path string) (ledger, error) {
	var history *ledger
	if err := readJSON(path, &history); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ledger{}, nil
		}
		return ledger{}, fmt.Errorf("read send budget ledger: %w", err)
	}
	if history == nil || history.Attempts == nil {
		return ledger{}, fmt.Errorf("invalid send budget ledger at %s", path)
	}
	if history.Pending && len(history.Attempts) == 0 {
		return ledger{}, fmt.Errorf("invalid send budget ledger at %s", path)
	}
	for _, attempt := range history.Attempts {
		if attempt.IsZero() {
			return ledger{}, fmt.Errorf("invalid attempt timestamp in send budget ledger at %s", path)
		}
	}
	return *history, nil
}

func writeLedger(path string, history ledger) error {
	if err := replaceLedger(path, history); err != nil {
		return fmt.Errorf("save send budget ledger: %w", err)
	}
	return nil
}

func replaceLedger(path string, history ledger) (err error) {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close send budget directory: %w", closeErr))
		}
	}()
	file, err := os.CreateTemp(dir.Name(), "attempts-*")
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			if removeErr := os.Remove(file.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("remove temporary send budget file: %w", removeErr))
			}
		}
	}()
	writeErr := json.NewEncoder(file).Encode(history)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	renamed = true
	return dir.Sync()
}
