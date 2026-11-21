package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const ssePing = 25 * time.Second

// events streams change notifications to the web client (Server-Sent
// Events), so that new mail and changes made in other clients show up
// immediately.
func (a *API) events(w http.ResponseWriter, r *http.Request, c *reqCtx) error {
	if a.hub == nil {
		return errStatus(http.StatusServiceUnavailable, "live updates are not available")
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	sub, unsubscribe := a.hub.Subscribe(c.user.ID)
	defer unsubscribe()
	w.WriteHeader(http.StatusOK)
	send := func(s string) bool {
		if _, err := fmt.Fprint(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("retry: 3000\n: connected\n\n") {
		return nil
	}
	ping := time.NewTicker(ssePing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-a.done:
			return nil
		case <-ping.C:
			// End the stream when the session was revoked elsewhere.
			if _, err := a.st.SessionByToken(r.Context(), c.token, sessionTTL); err != nil {
				send("event: logout\ndata: {}\n\n")
				return nil
			}
			if !send(": ping\n\n") {
				return nil
			}
		case <-sub.Ready():
			ev, ok := sub.Take()
			if !ok {
				continue
			}
			b, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			if !send("event: change\ndata: " + string(b) + "\n\n") {
				return nil
			}
		}
	}
}
