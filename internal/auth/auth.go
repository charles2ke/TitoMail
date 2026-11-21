// Package auth verifies user credentials with brute-force protection that is
// shared by the web client, SMTP submission and IMAP.
package auth

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"time"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// ErrThrottled is returned when too many failed attempts were made recently.
var ErrThrottled = errors.New("too many failed login attempts, try again later")

// Limiter blocks a key after max failures within window. The block lasts
// until the window that started with the first failure ends. Attempts that
// have begun but not ended count as failures, so that concurrent attempts
// cannot exceed the limit either.
type Limiter struct {
	max    int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
	sweep   time.Time
}

type entry struct {
	failures int
	start    time.Time // of the window, set by its first failure
	pending  int       // attempts begun but not ended
}

// NewLimiter returns a Limiter that allows max failures per window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, entries: map[string]*entry{}}
}

// current returns the failures of e in the window that is open at now.
func (l *Limiter) current(e *entry, now time.Time) int {
	if now.Sub(e.start) >= l.window {
		return 0
	}
	return e.failures
}

// Begin starts an attempt for each key. It returns false, starting none, if
// any key is blocked. Each successful Begin must be followed by End.
func (l *Limiter) Begin(keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, k := range keys {
		if e := l.entries[k]; e != nil && l.current(e, now)+e.pending >= l.max {
			return false
		}
	}
	for _, k := range keys {
		e := l.entries[k]
		if e == nil {
			e = &entry{}
			l.entries[k] = e
		}
		e.pending++
	}
	return true
}

// End finishes attempts started with Begin, recording a failure for each key
// if failed is set.
func (l *Limiter) End(failed bool, keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.sweep) > l.window {
		for k, e := range l.entries {
			if e.pending == 0 && l.current(e, now) == 0 {
				delete(l.entries, k)
			}
		}
		l.sweep = now
	}
	for _, k := range keys {
		e := l.entries[k]
		if e == nil {
			continue
		}
		e.pending--
		if failed {
			if l.current(e, now) == 0 {
				e.failures, e.start = 0, now
			}
			e.failures++
		}
		if e.pending == 0 && l.current(e, now) == 0 {
			delete(l.entries, k)
		}
	}
}

// Reset forgets the failures recorded for the keys.
func (l *Limiter) Reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if e := l.entries[k]; e != nil {
			e.failures = 0
			if e.pending == 0 {
				delete(l.entries, k)
			}
		}
	}
}

// Authenticator checks passwords, throttling by client network and by
// account so that neither a single client nor a distributed attack can try
// unlimited passwords.
type Authenticator struct {
	st      *store.Store
	byIP    *Limiter
	byLogin *Limiter
	// checks limits concurrent password checks, which are CPU-bound by
	// design, so that bursts of logins queue instead of starving the server.
	checks chan struct{}
}

// New returns an Authenticator allowing 10 failures per client network and
// 20 per account in 15 minutes.
func New(st *store.Store) *Authenticator {
	return &Authenticator{
		st:      st,
		byIP:    NewLimiter(10, 15*time.Minute),
		byLogin: NewLimiter(20, 15*time.Minute),
		checks:  make(chan struct{}, runtime.GOMAXPROCS(0)),
	}
}

// ClientKey identifies the client network of a remote address: the IPv4
// address, or the /64 prefix of an IPv6 address.
func ClientKey(remote string) string {
	host := remote
	if h, _, err := net.SplitHostPort(remote); err == nil {
		host = h
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}

// Login verifies credentials for a client connecting from remote (an
// "ip:port" or bare IP). It returns store.ErrInvalidCredentials or
// ErrThrottled on failure.
func (a *Authenticator) Login(ctx context.Context, remote, username, password string) (*store.User, error) {
	ipKey := ClientKey(remote)
	// The account limit is keyed on the address the store looks up, so that
	// every spelling of a username that reaches an account shares it.
	login := email.NormalizeAddress(username)
	if !a.byIP.Begin(ipKey) {
		return nil, ErrThrottled
	}
	if !a.byLogin.Begin(login) {
		a.byIP.End(false, ipKey)
		return nil, ErrThrottled
	}
	failed := false
	defer func() {
		a.byIP.End(failed, ipKey)
		a.byLogin.End(failed, login)
	}()
	u, err := a.check(ctx, login, password)
	if errors.Is(err, store.ErrInvalidCredentials) {
		failed = true
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	a.byLogin.Reset(login)
	return u, nil
}

// check verifies a password, waiting while too many checks are running.
func (a *Authenticator) check(ctx context.Context, login, password string) (*store.User, error) {
	select {
	case a.checks <- struct{}{}:
		defer func() { <-a.checks }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a.st.Authenticate(ctx, login, password)
}
