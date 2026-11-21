package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

type sessionInfo struct {
	Authenticated bool            `json:"authenticated"`
	SetupRequired bool            `json:"setup_required"`
	User          *store.User     `json:"user,omitempty"`
	Settings      *store.Settings `json:"settings,omitempty"`
	CSRFToken     string          `json:"csrf_token,omitempty"`
	Addresses     []string        `json:"addresses,omitempty"`
	Hostname      string          `json:"hostname"`
	Version       string          `json:"version"`
	MaxUpload     int64           `json:"max_upload_bytes"`
}

func (a *API) sessionInfo(ctx context.Context, u *store.User, csrf string) (*sessionInfo, error) {
	used, err := a.st.UsedBytes(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	u.UsedBytes = used
	addrs, err := a.st.SenderAddresses(ctx, u)
	if err != nil {
		return nil, err
	}
	settings := u.Settings
	return &sessionInfo{Authenticated: true, User: u, Settings: &settings, CSRFToken: csrf, Addresses: addrs,
		Hostname: a.opts.Hostname, Version: a.opts.Version, MaxUpload: a.opts.MaxUploadBytes}, nil
}

func (a *API) setupRequired(ctx context.Context) bool {
	if a.opts.SetupToken == "" {
		return false
	}
	n, err := a.st.CountUsers(ctx)
	return err == nil && n == 0
}

func (a *API) getSession(w http.ResponseWriter, r *http.Request) error {
	c, err := a.authenticate(r)
	if err != nil {
		return ok(w, &sessionInfo{SetupRequired: a.setupRequired(r.Context()), Hostname: a.opts.Hostname, Version: a.opts.Version})
	}
	info, err := a.sessionInfo(r.Context(), c.user, c.sess.CSRFToken)
	if err != nil {
		return err
	}
	return ok(w, info)
}

// checkOrigin protects the unauthenticated state-changing endpoints against
// cross-site requests. JSON bodies already force a CORS preflight, which
// this server never approves; the Origin check is defense in depth.
func checkOrigin(r *http.Request) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errStatus(http.StatusUnsupportedMediaType, "expected a JSON request body")
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return errStatus(http.StatusForbidden, "cross-origin request refused")
		}
	}
	return nil
}

func (a *API) startSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	token, csrf, err := a.st.CreateSession(r.Context(), u.ID, sessionTTL, a.clientAddr(r), r.UserAgent())
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: a.opts.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
	info, err := a.sessionInfo(r.Context(), u, csrf)
	if err != nil {
		return err
	}
	return ok(w, info)
}

func (a *API) login(w http.ResponseWriter, r *http.Request) error {
	if err := checkOrigin(r); err != nil {
		return err
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	u, err := a.auth.Login(r.Context(), a.clientAddr(r), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, store.ErrInvalidCredentials) {
			a.log.Info("web login failed", "remote", a.clientAddr(r), "user", req.Email)
		}
		return err
	}
	return a.startSession(w, r, u)
}

func (a *API) logout(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := a.st.DeleteSession(r.Context(), c.token); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: a.opts.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	return ok(w, map[string]bool{"ok": true})
}

// setup creates the first domain and administrator. It requires the setup
// token printed in the server log so that nobody else can claim a fresh
// installation.
func (a *API) setup(w http.ResponseWriter, r *http.Request) error {
	if err := checkOrigin(r); err != nil {
		return err
	}
	var req struct {
		Token       string `json:"token"`
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	ctx := r.Context()
	if !a.setupRequired(ctx) {
		return errStatus(http.StatusConflict, "this server has already been set up")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(a.opts.SetupToken)) != 1 {
		return errStatus(http.StatusForbidden, "invalid setup token; copy it from the server log")
	}
	addr := email.NormalizeAddress(req.Email)
	if !email.ValidAddress(addr) {
		return badRequest("enter a valid email address")
	}
	if !a.st.IsLocalDomain(ctx, email.DomainOf(addr)) {
		if _, err := a.st.CreateDomain(ctx, email.DomainOf(addr)); err != nil {
			return err
		}
	}
	u, err := a.st.CreateUser(ctx, store.NewUser{Email: addr, Password: req.Password, DisplayName: strings.TrimSpace(req.DisplayName), IsAdmin: true})
	if err != nil {
		return err
	}
	a.log.Info("initial setup completed", "admin", u.Email)
	return a.startSession(w, r, u)
}
