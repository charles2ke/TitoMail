// Package events fans out store changes to connected web clients.
package events

import (
	"slices"
	"sync"

	"github.com/charles2ke/TitoMail/internal/store"
)

// Event tells a web client which data to refresh. Events are coalesced, so
// one Event may summarize many changes.
type Event struct {
	// Mailboxes is set when the mailbox list or counts may have changed.
	Mailboxes bool `json:"mailboxes"`
	// Messages is set when any message data changed.
	Messages bool `json:"messages"`
	// Added lists mailboxes that received new messages.
	Added []int64 `json:"added,omitempty"`
}

// Hub distributes changes to subscribers by user.
type Hub struct {
	mu    sync.RWMutex
	subs  map[int64]map[*Sub]struct{}
	unsub func()
}

// Sub receives the events of one user.
type Sub struct {
	ready chan struct{}

	mu      sync.Mutex
	pending Event
	has     bool
}

// New subscribes a Hub to st. Call Close to unsubscribe.
func New(st *store.Store) *Hub {
	h := &Hub{subs: map[int64]map[*Sub]struct{}{}}
	h.unsub = st.Subscribe(h.handle)
	return h
}

// Close stops receiving store changes.
func (h *Hub) Close() { h.unsub() }

// handle runs synchronously inside store writes, so it must never block.
func (h *Hub) handle(c store.Change) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs[c.UserID] {
		s.add(c)
	}
}

// Subscribe registers a subscriber for a user's events. The returned
// function unregisters it.
func (h *Hub) Subscribe(userID int64) (*Sub, func()) {
	s := &Sub{ready: make(chan struct{}, 1)}
	h.mu.Lock()
	if h.subs[userID] == nil {
		h.subs[userID] = map[*Sub]struct{}{}
	}
	h.subs[userID][s] = struct{}{}
	h.mu.Unlock()
	return s, func() {
		h.mu.Lock()
		delete(h.subs[userID], s)
		if len(h.subs[userID]) == 0 {
			delete(h.subs, userID)
		}
		h.mu.Unlock()
	}
}

func (s *Sub) add(c store.Change) {
	s.mu.Lock()
	switch c.Kind {
	case store.MailboxesChanged:
		s.pending.Mailboxes = true
	case store.MessagesAdded:
		s.pending.Mailboxes, s.pending.Messages = true, true
		if !slices.Contains(s.pending.Added, c.MailboxID) {
			s.pending.Added = append(s.pending.Added, c.MailboxID)
		}
	case store.MessagesRemoved, store.FlagsChanged:
		s.pending.Mailboxes, s.pending.Messages = true, true
	default:
		s.pending.Messages = true
	}
	s.has = true
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// Ready is signaled when events are pending.
func (s *Sub) Ready() <-chan struct{} { return s.ready }

// Take returns and clears the pending events.
func (s *Sub) Take() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev, ok := s.pending, s.has
	s.pending, s.has = Event{}, false
	return ev, ok
}
