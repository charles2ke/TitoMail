// Package auth verifies user credentials with brute-force protection that is
// shared by the web client, SMTP submission and IMAP.
package auth

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/charles2ke/TitoMail/internal/store"
)

// ErrThrottled is returned when too many failed attempts were made recently.
var ErrThrottled = errors.New("too many failed login attempts, try again later")

// Limiter blocks a key after max failures within window. The block lasts
// until the window that started with the first failure ends.
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
	start    time.Time
}

// NewLimiter returns a Limiter that allows max failures per window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, entries: map[string]*entry{}}
}

// Allowed reports whether none of the keys is currently blocked.
func (l *Limiter) Allowed(keys ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, k := range keys {
		if e := l.entries[k]; e != nil && now.Sub(e.start) < l.window && e.failures >= l.max {
			return false
		}
	}
	return true
}

// Failure records a failed attempt for each key.
func (l *Limiter) Failure(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Sub(l.sweep) > l.window {
		for k, e := range l.entries {
			if now.Sub(e.start) >= l.window {
				delete(l.entries, k)
			}
		}
		l.sweep = now
	}
	for _, k := range keys {
		e := l.entries[k]
		if e == nil || now.Sub(e.start) >= l.window {
			e = &entry{start: now}
			l.entries[k] = e
		}
		e.failures++
	}
}

// Reset forgets the failures recorded for the keys.
func (l *Limiter) Reset(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.entries, k)
	}
}

// Authenticator checks passwords, throttling by client network and by
// account so that neither a single client nor a distributed attack can try
// unlimited passwords.
type Authenticator struct {
	st      *store.Store
	byIP    *Limiter
	byLogin *Limiter
}

// New returns an Authenticator allowing 10 failures per client network and
// 20 per account in 15 minutes.
func New(st *store.Store) *Authenticator {
	return &Authenticator{
		st:      st,
		byIP:    NewLimiter(10, 15*time.Minute),
		byLogin: NewLimiter(20, 15*time.Minute),
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
	login := strings.ToLower(strings.TrimSpace(username))
	if !a.byIP.Allowed(ipKey) || !a.byLogin.Allowed(login) {
		return nil, ErrThrottled
	}
	u, err := a.st.Authenticate(ctx, login, password)
	if errors.Is(err, store.ErrInvalidCredentials) {
		a.byIP.Failure(ipKey)
		a.byLogin.Failure(login)
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	a.byLogin.Reset(login)
	return u, nil
}
