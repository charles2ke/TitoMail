package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/charles2ke/TitoMail/internal/store"
)

func (a *API) adminOverview(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	ctx := r.Context()
	users, err := a.st.ListUsers(ctx)
	if err != nil {
		return err
	}
	domains, err := a.st.ListDomains(ctx)
	if err != nil {
		return err
	}
	queue, err := a.st.ListQueue(ctx, 0)
	if err != nil {
		return err
	}
	var storage int64
	for _, u := range users {
		storage += u.UsedBytes
	}
	pending, deferred := 0, 0
	for _, e := range queue {
		for _, rc := range e.Rcpts {
			if rc.Status != store.RcptPending {
				continue
			}
			if rc.Attempts > 0 {
				deferred++
			} else {
				pending++
			}
		}
	}
	return ok(w, map[string]any{
		"hostname": a.opts.Hostname, "version": a.opts.Version, "users": len(users), "domains": len(domains),
		"storage_bytes": storage, "queue_pending": pending, "queue_deferred": deferred,
		"ports": map[string]int{"smtp": a.opts.SMTPPort, "submission": a.opts.SubmissionPort,
			"submissions": a.opts.SubmissionsPort, "imap": a.opts.IMAPPort, "imaps": a.opts.IMAPSPort},
	})
}

// --- Domains ---

func (a *API) adminDomains(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListDomains(r.Context())
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (a *API) adminCreateDomain(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	d, err := a.st.CreateDomain(r.Context(), req.Name)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, d)
}

func (a *API) adminDeleteDomain(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.st.DeleteDomain(r.Context(), id); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// DNSRecord is a DNS record a domain needs, with the result of checking it.
type DNSRecord struct {
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Value    string   `json:"value"`
	Purpose  string   `json:"purpose"`
	Required bool     `json:"required"`
	Status   string   `json:"status"` // ok, missing, mismatch, error or unchecked
	Found    []string `json:"found"`
}

// ExpectedDNS returns the records needed to host mail for d on hostname.
// SRV records are only suggested for the implicit-TLS ports that are
// offered (0 = not offered).
func ExpectedDNS(d *store.Domain, hostname string, imapsPort, submissionsPort int) ([]DNSRecord, error) {
	pub, err := d.DKIMPublicKey()
	if err != nil {
		return nil, err
	}
	h := strings.TrimSuffix(hostname, ".")
	recs := []DNSRecord{
		{Type: "A/AAAA", Name: h, Value: "<public IP address of this server>", Purpose: "Mail server address", Required: true},
		{Type: "PTR", Name: "<public IP address of this server>", Value: h, Purpose: "Reverse DNS (set by your hosting provider); many receivers reject mail without it", Required: true},
		{Type: "MX", Name: d.Name, Value: "10 " + h + ".", Purpose: "Receive mail for the domain", Required: true},
		{Type: "TXT", Name: d.Name, Value: "v=spf1 mx -all", Purpose: "SPF: authorize this server to send for the domain", Required: true},
		{Type: "TXT", Name: d.DKIMSelector + "._domainkey." + d.Name, Value: "v=DKIM1; k=rsa; p=" + pub, Purpose: "DKIM signature verification", Required: true},
		{Type: "TXT", Name: "_dmarc." + d.Name, Value: "v=DMARC1; p=quarantine; adkim=r; aspf=r", Purpose: "DMARC policy", Required: true},
		{Type: "CNAME", Name: "autoconfig." + d.Name, Value: h + ".", Purpose: "Automatic setup in Thunderbird and other clients"},
	}
	if imapsPort > 0 {
		recs = append(recs, DNSRecord{Type: "SRV", Name: "_imaps._tcp." + d.Name, Value: fmt.Sprintf("0 1 %d %s.", imapsPort, h), Purpose: "IMAP discovery (RFC 6186)"})
	}
	if submissionsPort > 0 {
		recs = append(recs, DNSRecord{Type: "SRV", Name: "_submissions._tcp." + d.Name, Value: fmt.Sprintf("0 1 %d %s.", submissionsPort, h), Purpose: "SMTP submission discovery (RFC 8314)"})
	}
	for i := range recs {
		recs[i].Status = "unchecked"
		recs[i].Found = []string{}
	}
	return recs, nil
}

func (a *API) adminDomainDNS(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	d, err := a.st.GetDomain(r.Context(), id)
	if err != nil {
		return err
	}
	recs, err := ExpectedDNS(d, a.opts.Hostname, a.opts.IMAPSPort, a.opts.SubmissionsPort)
	if err != nil {
		return err
	}
	if r.URL.Query().Get("check") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		a.checkDNS(ctx, recs)
	}
	return ok(w, map[string]any{"domain": d, "hostname": a.opts.Hostname, "records": recs})
}

func normHost(s string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), ".")) }

func lookupErr(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return "missing"
	}
	return "error"
}

// checkDNS looks up the records and fills in their status.
func (a *API) checkDNS(ctx context.Context, recs []DNSRecord) {
	res := a.opts.Resolver
	host := normHost(a.opts.Hostname)
	var ips []net.IPAddr
	txt := func(rec *DNSRecord, prefix string, match func(string) bool) {
		vals, err := res.LookupTXT(ctx, rec.Name)
		if err != nil {
			rec.Status = lookupErr(err)
			return
		}
		rec.Status = "missing"
		for _, v := range vals {
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), prefix) {
				continue
			}
			rec.Found = append(rec.Found, v)
			if match(v) {
				rec.Status = "ok"
			} else if rec.Status != "ok" {
				rec.Status = "mismatch"
			}
		}
	}
	for i := range recs {
		rec := &recs[i]
		switch {
		case rec.Type == "A/AAAA":
			var err error
			ips, err = res.LookupIPAddr(ctx, host)
			if err != nil {
				rec.Status = lookupErr(err)
				continue
			}
			for _, ip := range ips {
				rec.Found = append(rec.Found, ip.IP.String())
			}
			rec.Status = "ok"
		case rec.Type == "PTR":
			if len(ips) == 0 {
				rec.Status = "missing"
				continue
			}
			rec.Status = "ok"
			for _, ip := range ips {
				names, err := res.LookupAddr(ctx, ip.IP.String())
				match := false
				for _, n := range names {
					rec.Found = append(rec.Found, ip.IP.String()+" → "+normHost(n))
					match = match || normHost(n) == host
				}
				if err != nil || !match {
					rec.Status = "mismatch"
				}
			}
		case rec.Type == "MX":
			mxs, err := res.LookupMX(ctx, rec.Name)
			if err != nil {
				rec.Status = lookupErr(err)
				continue
			}
			rec.Status = "mismatch"
			for _, mx := range mxs {
				rec.Found = append(rec.Found, normHost(mx.Host))
				if normHost(mx.Host) == host {
					rec.Status = "ok"
				}
			}
			if len(mxs) == 0 {
				rec.Status = "missing"
			}
		case rec.Type == "TXT" && strings.HasPrefix(rec.Value, "v=spf1"):
			txt(rec, "v=spf1", func(v string) bool {
				l := " " + strings.ToLower(v) + " "
				return strings.Contains(l, " mx ") || strings.Contains(l, " +mx ") || strings.Contains(l, ":"+host+" ") ||
					strings.Contains(l, " mx:") || strings.Contains(l, " a ")
			})
		case rec.Type == "TXT" && strings.HasPrefix(rec.Value, "v=DKIM1"):
			want := rec.Value[strings.Index(rec.Value, "p="):]
			txt(rec, "v=dkim1", func(v string) bool {
				return strings.Contains(strings.ReplaceAll(v, " ", ""), want)
			})
		case rec.Type == "TXT" && strings.HasPrefix(rec.Value, "v=DMARC1"):
			txt(rec, "v=dmarc1", func(string) bool { return true })
		}
	}
}

// --- Users ---

func (a *API) adminUsers(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListUsers(r.Context())
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (a *API) adminCreateUser(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		IsAdmin     bool   `json:"is_admin"`
		QuotaBytes  int64  `json:"quota_bytes"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	u, err := a.st.CreateUser(r.Context(), store.NewUser{Email: req.Email, Password: req.Password,
		DisplayName: strings.TrimSpace(req.DisplayName), IsAdmin: req.IsAdmin, QuotaBytes: req.QuotaBytes})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, u)
}

func (a *API) adminUpdateUser(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var req struct {
		DisplayName *string `json:"display_name"`
		IsAdmin     *bool   `json:"is_admin"`
		Disabled    *bool   `json:"disabled"`
		QuotaBytes  *int64  `json:"quota_bytes"`
		Password    *string `json:"password"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	if id == c.user.ID && ((req.IsAdmin != nil && !*req.IsAdmin) || (req.Disabled != nil && *req.Disabled)) {
		return badRequest("you cannot remove your own administrator access or disable your own account")
	}
	ctx := r.Context()
	if req.Password != nil {
		if err := a.st.SetPassword(ctx, id, *req.Password); err != nil {
			return err
		}
		if err := a.st.DeleteUserSessions(ctx, id, ""); err != nil {
			return err
		}
	}
	if err := a.st.UpdateUser(ctx, id, store.UserUpdate{DisplayName: req.DisplayName, IsAdmin: req.IsAdmin,
		Disabled: req.Disabled, QuotaBytes: req.QuotaBytes}); err != nil {
		return err
	}
	if req.Disabled != nil && *req.Disabled {
		if err := a.st.DeleteUserSessions(ctx, id, ""); err != nil {
			return err
		}
	}
	u, err := a.st.GetUser(ctx, id)
	if err != nil {
		return err
	}
	return ok(w, u)
}

func (a *API) adminDeleteUser(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if id == c.user.ID {
		return badRequest("you cannot delete your own account")
	}
	if err := a.st.DeleteUser(r.Context(), id); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// --- Aliases ---

func (a *API) adminAliases(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListAliases(r.Context(), 0)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (a *API) adminCreateAlias(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req struct {
		Address string `json:"address"`
		UserID  int64  `json:"user_id"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	al, err := a.st.CreateAlias(r.Context(), req.Address, req.UserID)
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusCreated, al)
}

func (a *API) adminDeleteAlias(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.st.DeleteAlias(r.Context(), id); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// --- Queue ---

func (a *API) adminQueue(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListQueue(r.Context(), 0)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (a *API) adminRetryQueue(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.st.RetryQueueEntry(r.Context(), id); err != nil {
		return err
	}
	a.d.Kick()
	return ok(w, map[string]bool{"ok": true})
}

func (a *API) adminDeleteQueue(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.st.DeleteQueueEntry(r.Context(), id); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}
