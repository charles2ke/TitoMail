// Package server wires the TitoMail services together: storage, delivery,
// SMTP, IMAP and the web interface.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/charles2ke/TitoMail/internal/api"
	"github.com/charles2ke/TitoMail/internal/auth"
	"github.com/charles2ke/TitoMail/internal/config"
	"github.com/charles2ke/TitoMail/internal/delivery"
	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/events"
	"github.com/charles2ke/TitoMail/internal/imapd"
	"github.com/charles2ke/TitoMail/internal/smtpd"
	"github.com/charles2ke/TitoMail/internal/store"
)

// DatabaseFile is the name of the SQLite database inside the data directory.
const DatabaseFile = "titomail.db"

// TrashRetention is how long messages stay in Trash and Junk before they
// are deleted permanently.
const TrashRetention = 30 * 24 * time.Hour

// Options configures Run.
type Options struct {
	Config  config.Config
	Static  fs.FS
	Version string
	Logger  *slog.Logger
	// Ready is called with the bound address of every listener once all
	// services accept connections. Keys are http, https, smtp, submission,
	// submissions, imap and imaps.
	Ready func(addrs map[string]string)
	// Resolver overrides DNS lookups (tests).
	Resolver delivery.Resolver
	// Dial overrides outbound connections (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// OpenStore opens the database in dataDir.
func OpenStore(dataDir string) (*store.Store, error) {
	return store.Open(filepath.Join(dataDir, DatabaseFile))
}

// EnsureDomain adds name as a local domain unless it already is one.
func EnsureDomain(ctx context.Context, st *store.Store, name string) (*store.Domain, error) {
	d, err := st.DomainByName(ctx, name)
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return st.CreateDomain(ctx, name)
}

type service struct {
	name  string
	addr  string
	ln    net.Listener
	serve func(net.Listener) error
}

// Run starts all services and blocks until ctx is cancelled or a service
// fails, then shuts everything down gracefully.
func Run(ctx context.Context, opts Options) error {
	cfg := opts.Config
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	st, err := OpenStore(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer st.Close()

	setupToken, err := bootstrap(ctx, st, cfg, log)
	if err != nil {
		return err
	}

	webTLS, mailTLS, acmeMgr, err := tlsSetup(cfg, st, log)
	if err != nil {
		return err
	}

	dopts := delivery.Options{
		Hostname:        cfg.Hostname,
		QueueMaxAge:     cfg.QueueMaxAge,
		DisableOutbound: cfg.DisableOutbound,
		OutboundPort:    cfg.OutboundPort,
		Resolver:        opts.Resolver,
		Dial:            opts.Dial,
		Logger:          log.With("service", "delivery"),
	}
	if cfg.RelayHost != "" {
		dopts.Relay = &delivery.Relay{Host: cfg.RelayHost, Port: cfg.RelayPort, Username: cfg.RelayUsername,
			Password: cfg.RelayPassword, TLS: cfg.RelayTLS}
	}
	d := delivery.New(st, dopts)
	authn := auth.New(st)
	hub := events.New(st)
	defer hub.Close()

	smtpSrv := smtpd.New(st, d, authn, smtpd.Options{
		Hostname:          cfg.Hostname,
		TLSConfig:         mailTLS,
		AllowInsecureAuth: cfg.AllowInsecureAuth,
		MaxMessageBytes:   cfg.MaxMessageBytes,
		AuthChecks:        cfg.InboundAuthChecks,
		Logger:            log.With("service", "smtp"),
	})
	imapSrv := imapd.New(st, imapd.Options{
		TLSConfig:         mailTLS,
		AllowInsecureAuth: cfg.AllowInsecureAuth,
		MaxMessageBytes:   cfg.MaxMessageBytes,
		Auth:              authn,
		Logger:            log.With("service", "imap"),
	})

	var services []*service
	add := func(name, addr string, serve func(net.Listener) error) {
		if addr != "" {
			services = append(services, &service{name: name, addr: addr, serve: serve})
		}
	}
	var apiHandler *api.API
	var httpServers []*http.Server
	newHTTP := func(h http.Handler) *http.Server {
		s := &http.Server{
			Handler:           h,
			ReadHeaderTimeout: 15 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    64 << 10,
			ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
		}
		httpServers = append(httpServers, s)
		return s
	}
	// The API handler is created after the listeners are bound so that it
	// can advertise the real ports; the HTTP servers call it indirectly.
	web := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { apiHandler.ServeHTTP(w, r) })
	var plainHTTP http.Handler = web
	if cfg.HTTPSAddr != "" {
		httpsSrv := newHTTP(web)
		httpsSrv.TLSConfig = webTLS
		add("https", cfg.HTTPSAddr, func(ln net.Listener) error { return httpsSrv.ServeTLS(ln, "", "") })
		plainHTTP = httpsRedirect(cfg.PublicURL, web)
	}
	if acmeMgr != nil {
		// Answer HTTP-01 challenges, so certificates can be issued even
		// without an HTTPS listener for the TLS-ALPN-01 challenge.
		plainHTTP = acmeMgr.HTTPHandler(plainHTTP)
	}
	add("http", cfg.HTTPAddr, newHTTP(plainHTTP).Serve)
	add("smtp", cfg.SMTPAddr, smtpSrv.ServeMX)
	add("submission", cfg.SubmissionAddr, smtpSrv.ServeSubmission)
	add("submissions", cfg.SubmissionsAddr, smtpSrv.ServeSubmissions)
	add("imap", cfg.IMAPAddr, imapSrv.Serve)
	add("imaps", cfg.IMAPSAddr, imapSrv.ServeTLS)

	addrs := map[string]string{}
	ports := map[string]int{}
	for _, s := range services {
		ln, err := net.Listen("tcp", s.addr)
		if err != nil {
			for _, o := range services {
				if o.ln != nil {
					o.ln.Close()
				}
			}
			return fmt.Errorf("%s: %w", s.name, err)
		}
		s.ln = ln
		addrs[s.name] = ln.Addr().String()
		if _, p, err := net.SplitHostPort(ln.Addr().String()); err == nil {
			ports[s.name], _ = strconv.Atoi(p)
		}
	}

	apiHandler = api.New(st, d, authn, hub, api.Options{
		Hostname:        cfg.Hostname,
		PublicURL:       cfg.PublicURL,
		Version:         opts.Version,
		SecureCookies:   cfg.SecureCookies || cfg.HTTPSAddr != "",
		TrustProxy:      cfg.TrustProxy,
		MaxUploadBytes:  cfg.MaxUploadBytes,
		MaxMessageBytes: cfg.MaxMessageBytes,
		SetupToken:      setupToken,
		TLS:             mailTLS != nil,
		IMAPPort:        ports["imap"],
		IMAPSPort:       ports["imaps"],
		SubmissionPort:  ports["submission"],
		SubmissionsPort: ports["submissions"],
		SMTPPort:        ports["smtp"],
		Static:          opts.Static,
		Resolver:        opts.Resolver,
		Logger:          log.With("service", "http"),
	})
	for _, s := range httpServers {
		s.RegisterOnShutdown(apiHandler.Shutdown)
	}

	bg, stopBG := context.WithCancel(context.WithoutCancel(ctx))
	var bgWG sync.WaitGroup
	bgWG.Go(func() { d.Run(bg) })
	bgWG.Go(func() { maintain(bg, st, log) })

	errc := make(chan error, len(services))
	var serveWG sync.WaitGroup
	for _, s := range services {
		serveWG.Go(func() {
			err := s.serve(s.ln)
			if err != nil && !isClosed(err) {
				errc <- fmt.Errorf("%s: %w", s.name, err)
			}
		})
		log.Info("listening", "service", s.name, "addr", addrs[s.name])
	}
	if acmeMgr != nil {
		go prefetchCertificate(bg, acmeMgr, cfg.Hostname, log)
	}
	if setupToken != "" {
		log.Warn("no accounts exist yet: open the setup page to create the first administrator",
			"url", cfg.PublicURL+"/setup?token="+setupToken)
	}
	log.Info("TitoMail started", "version", opts.Version, "hostname", cfg.Hostname, "url", cfg.PublicURL)
	if opts.Ready != nil {
		opts.Ready(addrs)
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-errc:
		log.Error("service failed; shutting down", "err", runErr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range httpServers {
		wg.Go(func() {
			if err := s.Shutdown(shutdownCtx); err != nil {
				s.Close()
			}
		})
	}
	wg.Go(func() {
		if err := smtpSrv.Shutdown(shutdownCtx); err != nil {
			smtpSrv.Close()
		}
	})
	wg.Go(func() { imapSrv.Close() })
	wg.Wait()
	for _, s := range services {
		s.ln.Close()
	}
	serveWG.Wait()
	stopBG()
	bgWG.Wait()
	return runErr
}

func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, http.ErrServerClosed) ||
		strings.Contains(err.Error(), "server closed")
}

// bootstrap creates the configured administrator, or returns a one-time
// setup token when the server has no accounts yet.
func bootstrap(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) (string, error) {
	if cfg.AdminEmail != "" {
		addr := email.NormalizeAddress(cfg.AdminEmail)
		if _, err := st.UserByEmail(ctx, addr); err == nil {
			return "", nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		if cfg.AdminPassword == "" {
			return "", errors.New("admin-password is required to create the administrator account")
		}
		if _, err := EnsureDomain(ctx, st, email.DomainOf(addr)); err != nil {
			return "", fmt.Errorf("creating domain for %s: %w", addr, err)
		}
		u, err := st.CreateUser(ctx, store.NewUser{Email: addr, Password: cfg.AdminPassword, IsAdmin: true})
		if err != nil {
			return "", fmt.Errorf("creating administrator %s: %w", addr, err)
		}
		log.Info("created administrator account", "email", u.Email)
		return "", nil
	}
	n, err := st.CountUsers(ctx)
	if err != nil || n > 0 {
		return "", err
	}
	b := make([]byte, 20)
	rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func tlsSetup(cfg config.Config, st *store.Store, log *slog.Logger) (web, mail *tls.Config, mgr *autocert.Manager, err error) {
	var get func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	switch {
	case cfg.TLSCert != "":
		cf := &certFile{cert: cfg.TLSCert, key: cfg.TLSKey, log: log}
		if _, err := cf.load(); err != nil {
			return nil, nil, nil, fmt.Errorf("loading TLS certificate: %w", err)
		}
		get = cf.get
	case cfg.ACME:
		mgr = &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Cache:  autocert.DirCache(filepath.Join(cfg.DataDir, "acme")),
			Email:  cfg.ACMEEmail,
			HostPolicy: func(ctx context.Context, host string) error {
				host = strings.ToLower(host)
				if host == cfg.Hostname {
					return nil
				}
				for _, p := range []string{"autoconfig.", "autodiscover."} {
					if d, ok := strings.CutPrefix(host, p); ok && st.IsLocalDomain(ctx, d) {
						return nil
					}
				}
				return fmt.Errorf("acme: host %q is not served here", host)
			},
		}
		if cfg.ACMEDirectory != "" {
			mgr.Client = &acme.Client{DirectoryURL: cfg.ACMEDirectory}
		}
		get = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if hello.ServerName == "" {
				// Mail clients often omit SNI; use the server's own name.
				h := *hello
				h.ServerName = cfg.Hostname
				hello = &h
			}
			return mgr.GetCertificate(hello)
		}
	default:
		return nil, nil, nil, nil
	}
	mail = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: get}
	web = mail.Clone()
	web.NextProtos = []string{"h2", "http/1.1"}
	if mgr != nil {
		web.NextProtos = append(web.NextProtos, acme.ALPNProto)
	}
	return web, mail, mgr, nil
}

// certFile serves a certificate from disk and reloads it when the files
// change, so renewed certificates are picked up without a restart.
type certFile struct {
	cert, key string
	log       *slog.Logger

	mu      sync.Mutex
	current *tls.Certificate
	checked time.Time
	stamp   string
}

func (c *certFile) fileStamp() string {
	var b strings.Builder
	for _, name := range []string{c.cert, c.key} {
		if fi, err := os.Stat(name); err == nil {
			fmt.Fprintf(&b, "%d/%d;", fi.ModTime().UnixNano(), fi.Size())
		}
	}
	return b.String()
}

func (c *certFile) load() (*tls.Certificate, error) {
	stamp := c.fileStamp()
	crt, err := tls.LoadX509KeyPair(c.cert, c.key)
	if err != nil {
		return nil, err
	}
	c.current, c.stamp = &crt, stamp
	return c.current, nil
}

func (c *certFile) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) < 30*time.Second {
		return c.current, nil
	}
	c.checked = time.Now()
	if c.fileStamp() == c.stamp {
		return c.current, nil
	}
	old := c.current
	if _, err := c.load(); err != nil {
		c.log.Error("reloading TLS certificate failed; keeping the previous one", "err", err)
		c.current = old
		return old, nil
	}
	c.log.Info("reloaded TLS certificate", "file", c.cert)
	return c.current, nil
}

func prefetchCertificate(ctx context.Context, m *autocert.Manager, host string, log *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}
	hello := &tls.ClientHelloInfo{
		ServerName:        host,
		SignatureSchemes:  []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
		SupportedCurves:   []tls.CurveID{tls.CurveP256},
		CipherSuites:      []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		SupportedVersions: []uint16{tls.VersionTLS13, tls.VersionTLS12},
	}
	if _, err := m.GetCertificate(hello); err != nil {
		log.Warn("obtaining ACME certificate failed; will retry on the next TLS connection", "host", host, "err", err)
	}
}

// httpsRedirect redirects plain HTTP to the HTTPS interface, but serves
// client autoconfiguration (fetched over HTTP by some clients) and health
// checks directly.
func httpsRedirect(publicURL string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || p == "/mail/config-v1.1.xml" || strings.HasPrefix(p, "/.well-known/autoconfig/") {
			h.ServeHTTP(w, r)
			return
		}
		code := http.StatusMovedPermanently
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			code = http.StatusPermanentRedirect
		}
		http.Redirect(w, r, publicURL+r.URL.RequestURI(), code)
	})
}

// maintain periodically removes expired data.
func maintain(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := st.Cleanup(ctx); err != nil && ctx.Err() == nil {
			log.Warn("cleanup failed", "err", err)
		}
		cutoff := time.Now().Add(-TrashRetention)
		for _, special := range []string{store.SpecialTrash, store.SpecialJunk} {
			for ctx.Err() == nil {
				n, err := st.PurgeOld(ctx, special, cutoff)
				if err != nil {
					if ctx.Err() == nil {
						log.Warn("purging old messages failed", "mailbox", special, "err", err)
					}
					break
				}
				if n > 0 {
					log.Info("purged old messages", "mailbox", special, "count", n)
				}
				if n < 5000 {
					break
				}
			}
		}
		t.Reset(time.Hour)
	}
}
