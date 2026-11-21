package smail

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pp_email/internal/csvdata"
)

const (
	probeAddress       = "pp-email-probe@example.invalid"
	probeTimeout       = 10 * time.Second
	sendTimeout        = 120 * time.Second
	guardBlockedStatus = 71
)

// guardScript is an s-nail command script. Each command stays on one line
// because line breaks separate commands.
const guardScript = `set errexit
wysh if "$mta" =~ "$PP_EMAIL_MTA_RE"
  set sendwait nosave fullnames ttycharset=utf-8 expandaddr=fail,-all,+addr,failinvaddr
  unset autocc autobcc on-compose-enter on-compose-splice on-compose-leave on-compose-cleanup message-inject-head message-inject-tail dot skipempty debug
  echo "$PP_EMAIL_PROBE_TAG READY"
else
  echo "$PP_EMAIL_PROBE_TAG BLOCKED"
  exit 71
endif`

type Outcome int

const (
	Unattempted Outcome = iota
	Accepted
	Unconfirmed
)

type SendResult struct {
	Outcome Outcome
	Reason  string
	Stop    bool
}

type Endpoint struct {
	pattern string
}

type invocation struct {
	path    string
	args    []string
	env     []string
	body    string
	timeout time.Duration
}

type observation struct {
	stdout    string
	started   bool
	success   bool
	exitCode  int
	timedOut  bool
	signalled bool
}

type Adapter struct {
	path           string
	env            []string
	launch         func(invocation) observation
	lookup         func(context.Context, string, string) ([]netip.Addr, error)
	localAddresses func() ([]netip.Addr, error)
}

func New(path string, env []string) *Adapter {
	return &Adapter{
		path: path, env: slices.Clone(env), launch: run,
		lookup: net.DefaultResolver.LookupNetIP, localAddresses: interfaceAddresses,
	}
}

func (a *Adapter) ListAccounts() ([]string, error) {
	result := a.launch(invocation{
		path: a.path, env: a.env, timeout: probeTimeout,
		args: []string{"-X", `\account`, "-X", `\exit 0`, "-.", probeAddress},
	})
	if !result.success {
		return nil, errors.New("s-nail could not list accounts; check the configuration")
	}
	return parseAccounts(result.stdout)
}

func parseAccounts(output string) ([]string, error) {
	names := []string{}
	var account string
	for line := range strings.Lines(output) {
		line = strings.TrimRight(line, "\r\n")
		if account != "" {
			if line == "}" {
				if !strings.EqualFold(account, "null") {
					names = append(names, account)
				}
				account = ""
			}
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, found := strings.CutPrefix(line, "account ")
		if found {
			name, found = strings.CutSuffix(name, " {")
		}
		if !found || name == "" {
			return nil, errors.New("s-nail returned an unrecognized account listing")
		}
		account = name
	}
	if account != "" {
		return nil, errors.New("s-nail returned an incomplete account listing")
	}
	return names, nil
}

func (a *Adapter) CheckAccount(account string) (Endpoint, error) {
	tag := newTag()
	result := a.launch(invocation{
		path: a.path, env: a.environment("PP_EMAIL_PROBE_TAG=" + tag), timeout: probeTimeout,
		args: []string{"-A", account, "-X", `echo "$PP_EMAIL_PROBE_TAG=$mta"`,
			"-X", `\exit 0`, "-.", probeAddress},
	})
	if !result.success {
		return Endpoint{}, errors.New("s-nail could not check the selected account")
	}
	raw := ""
	count := 0
	for line := range strings.Lines(result.stdout) {
		if value, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), tag+"="); ok {
			raw = value
			count++
		}
	}
	if count != 1 {
		return Endpoint{}, errors.New("s-nail did not report the account's effective transport")
	}

	parsed, authority, err := smtpURL(raw)
	if err != nil {
		return Endpoint{}, err
	}
	if err := a.checkRemoteHost(parsed.Hostname()); err != nil {
		return Endpoint{}, err
	}
	return Endpoint{
		pattern: "^" + regexp.QuoteMeta(parsed.Scheme) + "://([^/@[:space:]]+@)?" +
			regexp.QuoteMeta(authority) + "$",
	}, nil
}

func smtpURL(raw string) (*url.URL, string, error) {
	invalid := errors.New("the selected account must use a remote SMTP URL")
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, "", invalid
	}
	switch parsed.Scheme {
	case "smtp", "smtps", "submission", "submissions":
	default:
		return nil, "", invalid
	}
	scheme, authority, ok := strings.Cut(raw, "://")
	matchesScheme := ok && scheme == parsed.Scheme && parsed.Hostname() != ""
	cleanAuthority := !strings.ContainsAny(authority, "/?#") &&
		!strings.ContainsFunc(authority, func(r rune) bool {
			return r <= ' ' || r == '\x7f'
		})
	if !matchesScheme || !cleanAuthority {
		return nil, "", invalid
	}
	if user, host, ok := strings.Cut(authority, "@"); ok {
		if user == "" || strings.Contains(host, "@") {
			return nil, "", invalid
		}
		authority = host
	}
	port := parsed.Port()
	if port != "" || strings.HasSuffix(authority, ":") {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil || number == 0 {
			return nil, "", invalid
		}
	}
	return parsed, authority, nil
}

func (a *Adapter) checkRemoteHost(host string) error {
	normalized := strings.TrimSuffix(strings.ToLower(host), ".")
	if normalized == "localhost" || strings.HasSuffix(normalized, ".localhost") {
		return errors.New("the selected SMTP server is local")
	}
	address, err := netip.ParseAddr(host)
	addresses := []netip.Addr{address}
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		addresses, err = a.lookup(ctx, "ip", host)
		if err != nil || len(addresses) == 0 {
			return errors.New("could not resolve the selected SMTP server")
		}
	}

	local, err := a.localAddresses()
	if err != nil {
		return errors.New("could not check the local network addresses")
	}
	for _, address := range addresses {
		address = address.WithZone("").Unmap()
		unusable := !address.IsValid() || address.IsUnspecified() || address.IsLoopback()
		if unusable || slices.Contains(local, address) {
			return errors.New("the selected SMTP server resolves to a local or unspecified address")
		}
	}
	return nil
}

func interfaceAddresses() ([]netip.Addr, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	result := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		var ip net.IP
		switch address := address.(type) {
		case *net.IPNet:
			ip = address.IP
		case *net.IPAddr:
			ip = address.IP
		default:
			return nil, errors.New("unrecognized local network address")
		}
		parsed, ok := netip.AddrFromSlice(ip)
		if !ok {
			return nil, errors.New("unrecognized local IP address")
		}
		result = append(result, parsed.Unmap())
	}
	return result, nil
}

func (a *Adapter) Send(account string, endpoint Endpoint, message csvdata.Message) SendResult {
	if endpoint.pattern == "" {
		return SendResult{
			Outcome: Unattempted,
			Reason:  "the account has no approved SMTP endpoint",
			Stop:    true,
		}
	}
	tag := newTag()
	result := a.launch(invocation{
		path: a.path, env: a.environment("PP_EMAIL_PROBE_TAG="+tag, "PP_EMAIL_MTA_RE="+endpoint.pattern),
		timeout: sendTimeout, body: message.Body,
		args: []string{"-A", account, "-X", guardScript, "-s", message.Subject, "-.", message.Recipient},
	})
	return classify(result, tag)
}

func classify(result observation, tag string) SendResult {
	if !result.started {
		return SendResult{
			Outcome: Unattempted,
			Reason:  "s-nail could not start",
			Stop:    true,
		}
	}
	ready, blocked := 0, 0
	for line := range strings.Lines(result.stdout) {
		switch strings.TrimRight(line, "\r\n") {
		case tag + " READY":
			ready++
		case tag + " BLOCKED":
			blocked++
		}
	}
	blockedByGuard := ready == 0 && blocked == 1 &&
		result.exitCode == guardBlockedStatus && !result.timedOut
	if blockedByGuard {
		return SendResult{
			Outcome: Unattempted,
			Reason:  "the SMTP transport changed before submission",
			Stop:    true,
		}
	}
	if ready != 1 || blocked != 0 {
		return SendResult{
			Outcome: Unconfirmed,
			Reason:  "s-nail did not complete the SMTP startup check",
			Stop:    true,
		}
	}
	switch {
	case result.timedOut:
		return SendResult{Outcome: Unconfirmed, Reason: "s-nail timed out"}
	case result.signalled:
		return SendResult{Outcome: Unconfirmed, Reason: "s-nail was interrupted"}
	case result.success:
		return SendResult{Outcome: Accepted}
	case result.exitCode > 0:
		return SendResult{
			Outcome: Unconfirmed,
			Reason:  fmt.Sprintf("s-nail exited with status %d", result.exitCode),
		}
	default:
		return SendResult{Outcome: Unconfirmed, Reason: "s-nail could not report completion"}
	}
}

func newTag() string {
	return rand.Text()
}

func (a *Adapter) environment(extra ...string) []string {
	return append(slices.Clone(a.env), extra...)
}

func run(call invocation) observation {
	cmd := exec.Command(call.path, call.args...)
	cmd.Env = call.env
	cmd.Stdin = strings.NewReader(call.body)
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Start(); err != nil {
		return observation{}
	}
	timer := time.NewTimer(call.timeout)
	defer timer.Stop()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	result := observation{started: true}
	select {
	case err = <-done:
	case <-timer.C:
		result.timedOut = true
		// A transport child can still hold the pipes after the parent exits.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		err = <-done
	}
	result.stdout = stdout.String()
	result.success = err == nil && !result.timedOut
	result.exitCode = cmd.ProcessState.ExitCode()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		result.signalled = status.Signaled()
	}
	return result
}
