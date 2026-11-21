package api

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

// --- Contacts ---

func (a *API) listContacts(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	q := r.URL.Query()
	list, err := a.st.ListContacts(r.Context(), c.user.ID, q.Get("q"), q.Get("saved") == "1", 1000)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (a *API) saveContact(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var ct store.Contact
	if err := decode(w, r, &ct, 0); err != nil {
		return err
	}
	ct.ID = 0
	if r.Method == http.MethodPut {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		ct.ID = id
	}
	ct.Email = email.NormalizeAddress(ct.Email)
	ct.Name = strings.TrimSpace(ct.Name)
	saved, err := a.st.SaveContact(r.Context(), c.user.ID, &ct)
	if err != nil {
		return err
	}
	return ok(w, saved)
}

func (a *API) deleteContact(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.st.DeleteContact(r.Context(), c.user.ID, id); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// autocomplete suggests recipients from the user's contacts (including
// people they have written to) and the directory of users in their domain.
func (a *API) autocomplete(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	out := []email.Address{}
	if q == "" {
		return ok(w, out)
	}
	ctx := r.Context()
	contacts, err := a.st.ListContacts(ctx, c.user.ID, q, false, 10)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, ct := range contacts {
		key := strings.ToLower(ct.Email)
		if !seen[key] {
			seen[key] = true
			out = append(out, email.Address{Name: ct.Name, Address: ct.Email})
		}
	}
	dir, err := a.st.SearchDirectory(ctx, q, 50)
	if err != nil {
		return err
	}
	domain := strings.ToLower(email.DomainOf(c.user.Email))
	for _, d := range dir {
		key := strings.ToLower(d.Address)
		if seen[key] || strings.ToLower(email.DomainOf(d.Address)) != domain {
			continue
		}
		seen[key] = true
		out = append(out, email.Address{Name: d.Name, Address: d.Address})
	}
	if len(out) > 10 {
		out = out[:10]
	}
	return ok(w, out)
}

// --- Settings and account ---

func (a *API) getSettings(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	return ok(w, c.user.Settings)
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var st store.Settings
	if err := decode(w, r, &st, 256<<10); err != nil {
		return err
	}
	ctx := r.Context()
	saved, err := a.st.UpdateSettings(ctx, c.user.ID, st)
	if err != nil {
		return err
	}
	// Changing the auto-reply starts a new vacation: everybody gets the new
	// message once.
	if saved.Vacation != c.user.Settings.Vacation {
		if err := a.st.ResetVacationLog(ctx, c.user.ID); err != nil {
			return err
		}
	}
	return ok(w, saved)
}

func (a *API) updateAccount(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req struct {
		DisplayName *string `json:"display_name"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if err := a.st.UpdateUser(r.Context(), c.user.ID, store.UserUpdate{DisplayName: &name}); err != nil {
			return err
		}
	}
	u, err := a.st.GetUser(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	return ok(w, u)
}

func (a *API) changePassword(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := a.auth.Login(ctx, a.clientAddr(r), c.user.Email, req.Current); err != nil {
		if errors.Is(err, store.ErrInvalidCredentials) {
			return errStatus(http.StatusForbidden, "your current password is incorrect")
		}
		return err
	}
	if err := a.st.SetPassword(ctx, c.user.ID, req.New); err != nil {
		return err
	}
	// Sign out everywhere else.
	if err := a.st.DeleteUserSessions(ctx, c.user.ID, c.token); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

func (a *API) myAliases(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListAliases(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

// --- Filters ---

func (a *API) listFilters(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListFilters(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	return ok(w, nonNil(list))
}

func (a *API) saveFilter(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var f store.Filter
	if err := decode(w, r, &f, 0); err != nil {
		return err
	}
	f.ID = 0
	if r.Method == http.MethodPut {
		id, err := pathID(r, "id")
		if err != nil {
			return err
		}
		f.ID = id
	}
	saved, err := a.st.SaveFilter(r.Context(), c.user.ID, &f)
	if err != nil {
		return err
	}
	return ok(w, saved)
}

func (a *API) deleteFilter(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := a.st.DeleteFilter(r.Context(), c.user.ID, id); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

// --- Blocked and trusted senders ---

func (a *API) listSenders(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	list, err := a.st.ListSenderPrefs(r.Context(), c.user.ID)
	if err != nil {
		return err
	}
	list = nonNil(list)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Address < list[j].Address })
	return ok(w, list)
}

func (a *API) setSender(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	var req struct {
		Address string `json:"address"`
		Kind    string `json:"kind"`
	}
	if err := decode(w, r, &req, 0); err != nil {
		return err
	}
	if err := a.st.SetSenderPref(r.Context(), c.user.ID, req.Address, req.Kind); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}

func (a *API) deleteSender(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if err := a.st.DeleteSenderPref(r.Context(), c.user.ID, r.URL.Query().Get("address")); err != nil {
		return err
	}
	return ok(w, map[string]bool{"ok": true})
}
