// Package api implements the JSON API used by the TitoMail web client, the
// mail client auto-configuration endpoints and the embedded web UI.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charles2ke/TitoMail/internal/auth"
	"github.com/charles2ke/TitoMail/internal/delivery"
	"github.com/charles2ke/TitoMail/internal/events"
	"github.com/charles2ke/TitoMail/internal/store"
)

const (
	sessionCookie = "tm_session"
	sessionTTL    = 30 * 24 * time.Hour
	csrfHeader    = "X-CSRF-Token"
	maxJSONBytes  = 1 << 20
)

// Options configures the API.
type Options struct {
	Hostname  string
	PublicURL string
	Version   string
	// SecureCookies marks the session cookie Secure (HTTPS only).
	SecureCookies bool
	// TrustProxy uses X-Forwarded-For to determine the client address.
	TrustProxy      bool
	MaxUploadBytes  int64
	MaxMessageBytes int64
	// SetupToken enables first-run setup when no users exist yet.
	SetupToken string
	// TLS reports whether the mail ports offer TLS.
	TLS bool
	// Ports advertised to mail clients (0 = not offered).
	IMAPPort, IMAPSPort, SubmissionPort, SubmissionsPort, SMTPPort int
	// Static is the built web UI. Nil serves a placeholder page.
	Static fs.FS
	// Resolver is used for the DNS checks on the admin pages.
	Resolver delivery.Resolver
	Logger   *slog.Logger
}

// API serves HTTP requests.
type API struct {
	st   *store.Store
	d    *delivery.Deliverer
	auth *auth.Authenticator
	hub  *events.Hub
	opts Options
	log  *slog.Logger
	mux  *http.ServeMux
	now  func() time.Time

	done      chan struct{}
	closeOnce sync.Once
}

// New returns an API.
func New(st *store.Store, d *delivery.Deliverer, a *auth.Authenticator, hub *events.Hub, opts Options) *API {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.MaxUploadBytes <= 0 {
		opts.MaxUploadBytes = 25 << 20
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = 50 << 20
	}
	if opts.Resolver == nil {
		opts.Resolver = d.Resolver()
	}
	a2 := &API{st: st, d: d, auth: a, hub: hub, opts: opts, log: opts.Logger, mux: http.NewServeMux(), now: time.Now,
		done: make(chan struct{})}
	a2.routes()
	return a2
}

// Shutdown ends long-lived requests (event streams) so that the HTTP server
// can shut down gracefully. Register it with http.Server.RegisterOnShutdown.
func (a *API) Shutdown() { a.closeOnce.Do(func() { close(a.done) }) }

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			a.log.Error("panic serving request", "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
			writeError(w, http.StatusInternalServerError, "internal error")
		}
	}()
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
		"img-src 'self' data: blob: https: http:; font-src 'self' data: https:; connect-src 'self'; "+
		"frame-src 'self' about: data: blob:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	if r.TLS != nil || (a.opts.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https") {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
	}
	a.mux.ServeHTTP(w, r)
}

// reqCtx is the authenticated context of an API request.
type reqCtx struct {
	user  *store.User
	sess  *store.Session
	token string
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, c *reqCtx) error

func (a *API) handle(pattern string, h handlerFunc) {
	a.mux.HandleFunc(pattern, a.withUser(h, false))
}

func (a *API) handleAdmin(pattern string, h handlerFunc) {
	a.mux.HandleFunc(pattern, a.withUser(h, true))
}

// handlePublic registers an endpoint that does not require a session.
func (a *API) handlePublic(pattern string, h func(w http.ResponseWriter, r *http.Request) error) {
	a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			a.fail(w, r, err)
		}
	})
}

func (a *API) withUser(h handlerFunc, admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := a.authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "please sign in")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if tok := r.Header.Get(csrfHeader); tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(c.sess.CSRFToken)) != 1 {
				writeError(w, http.StatusForbidden, "invalid CSRF token, please reload the page")
				return
			}
		}
		if admin && !c.user.IsAdmin {
			writeError(w, http.StatusForbidden, "administrator access required")
			return
		}
		if err := h(w, r, c); err != nil {
			a.fail(w, r, err)
		}
	}
}

func (a *API) authenticate(r *http.Request) (*reqCtx, error) {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, err
	}
	sess, err := a.st.SessionByToken(r.Context(), ck.Value, sessionTTL)
	if err != nil {
		return nil, err
	}
	u, err := a.st.GetUser(r.Context(), sess.UserID)
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, store.ErrInvalidCredentials
	}
	return &reqCtx{user: u, sess: sess, token: ck.Value}, nil
}

// clientAddr returns the address of the client, honoring X-Forwarded-For
// (the entry added by the closest proxy) when the proxy is trusted.
func (a *API) clientAddr(r *http.Request) string {
	if a.opts.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	return r.RemoteAddr
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func errStatus(status int, format string, args ...any) error {
	return &httpError{status: status, msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) error {
	return errStatus(http.StatusBadRequest, format, args...)
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var he *httpError
	var ve *store.ValidationError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &he):
		writeError(w, he.status, he.msg)
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Msg)
	case errors.As(err, &mbe):
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request is too large (limit %d MB)", mbe.Limit>>20))
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "already exists")
	case errors.Is(err, store.ErrQuotaExceeded):
		writeError(w, http.StatusInsufficientStorage, "your mailbox is full; delete some messages and try again")
	case errors.Is(err, store.ErrTooLate):
		writeError(w, http.StatusConflict, "the message has already been sent")
	case errors.Is(err, store.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, auth.ErrThrottled):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, delivery.ErrNoRecipients):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func ok(w http.ResponseWriter, v any) error { return writeJSON(w, http.StatusOK, v) }

func writeError(w http.ResponseWriter, status int, msg string) {
	_ = writeJSON(w, status, map[string]string{"error": msg})
}

// decode reads a JSON request body of at most limit bytes.
func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	if limit <= 0 {
		limit = maxJSONBytes
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return badRequest("invalid request body: %v", err)
	}
	return nil
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, store.ErrNotFound
	}
	return id, nil
}

func queryInt(r *http.Request, name string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil {
		return v
	}
	return def
}

func (a *API) routes() {
	// Session.
	a.handlePublic("GET /api/session", a.getSession)
	a.handlePublic("POST /api/login", a.login)
	a.handlePublic("POST /api/setup", a.setup)
	a.handle("POST /api/logout", a.logout)

	// Mailboxes and labels.
	a.handle("GET /api/mailboxes", a.listMailboxes)
	a.handle("POST /api/mailboxes", a.createMailbox)
	a.handle("PATCH /api/mailboxes/{id}", a.renameMailbox)
	a.handle("DELETE /api/mailboxes/{id}", a.deleteMailbox)
	a.handle("POST /api/mailboxes/{id}/empty", a.emptyMailbox)
	a.handle("GET /api/labels", a.listLabels)
	a.handle("POST /api/labels", a.createLabel)
	a.handle("PATCH /api/labels/{id}", a.updateLabel)
	a.handle("DELETE /api/labels/{id}", a.deleteLabel)

	// Reading.
	a.handle("GET /api/conversations", a.listConversations)
	a.handle("GET /api/threads/{id}", a.getThread)
	a.handle("GET /api/messages/{id}", a.getMessage)
	a.handle("GET /api/messages/{id}/raw", a.getRaw)
	a.handle("GET /api/messages/{id}/attachments/{part}", a.getAttachment)
	a.handle("GET /api/messages/{id}/compose", a.composeFrom)
	a.handle("POST /api/messages/actions", a.messageActions)
	a.handle("POST /api/threads/actions", a.threadActions)

	// Composing.
	a.handle("POST /api/uploads", a.upload)
	a.handle("GET /api/drafts/{id}", a.getDraft)
	a.handle("POST /api/drafts", a.saveDraft)
	a.handle("POST /api/send", a.send)
	a.handle("GET /api/outbox", a.listOutbox)
	a.handle("POST /api/outbox/{id}/cancel", a.cancelSend)

	// Contacts.
	a.handle("GET /api/contacts", a.listContacts)
	a.handle("POST /api/contacts", a.saveContact)
	a.handle("PUT /api/contacts/{id}", a.saveContact)
	a.handle("DELETE /api/contacts/{id}", a.deleteContact)
	a.handle("GET /api/autocomplete", a.autocomplete)

	// Account and settings.
	a.handle("GET /api/settings", a.getSettings)
	a.handle("PUT /api/settings", a.putSettings)
	a.handle("PATCH /api/account", a.updateAccount)
	a.handle("POST /api/account/password", a.changePassword)
	a.handle("GET /api/account/aliases", a.myAliases)
	a.handle("GET /api/filters", a.listFilters)
	a.handle("POST /api/filters", a.saveFilter)
	a.handle("PUT /api/filters/{id}", a.saveFilter)
	a.handle("DELETE /api/filters/{id}", a.deleteFilter)
	a.handle("GET /api/senders", a.listSenders)
	a.handle("POST /api/senders", a.setSender)
	a.handle("DELETE /api/senders", a.deleteSender)

	// Live updates.
	a.handle("GET /api/events", a.events)

	// Administration.
	a.handleAdmin("GET /api/admin/overview", a.adminOverview)
	a.handleAdmin("GET /api/admin/domains", a.adminDomains)
	a.handleAdmin("POST /api/admin/domains", a.adminCreateDomain)
	a.handleAdmin("DELETE /api/admin/domains/{id}", a.adminDeleteDomain)
	a.handleAdmin("GET /api/admin/domains/{id}/dns", a.adminDomainDNS)
	a.handleAdmin("GET /api/admin/users", a.adminUsers)
	a.handleAdmin("POST /api/admin/users", a.adminCreateUser)
	a.handleAdmin("PATCH /api/admin/users/{id}", a.adminUpdateUser)
	a.handleAdmin("DELETE /api/admin/users/{id}", a.adminDeleteUser)
	a.handleAdmin("GET /api/admin/aliases", a.adminAliases)
	a.handleAdmin("POST /api/admin/aliases", a.adminCreateAlias)
	a.handleAdmin("DELETE /api/admin/aliases/{id}", a.adminDeleteAlias)
	a.handleAdmin("GET /api/admin/queue", a.adminQueue)
	a.handleAdmin("POST /api/admin/queue/{id}/retry", a.adminRetryQueue)
	a.handleAdmin("DELETE /api/admin/queue/{id}", a.adminDeleteQueue)

	a.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "no such API endpoint")
	})

	// Mail client auto-configuration.
	a.mux.HandleFunc("GET /.well-known/autoconfig/mail/config-v1.1.xml", a.autoconfig)
	a.mux.HandleFunc("GET /mail/config-v1.1.xml", a.autoconfig)
	a.mux.HandleFunc("POST /autodiscover/autodiscover.xml", a.autodiscover)
	a.mux.HandleFunc("POST /Autodiscover/Autodiscover.xml", a.autodiscover)
	a.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})

	a.mux.Handle("/", a.static())
}
