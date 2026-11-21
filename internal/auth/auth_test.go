package auth

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/charles2ke/TitoMail/internal/store"
)

// newTestAuth returns an Authenticator for a store with the user
// alice@example.test, whose password hash has the given bcrypt cost.
func newTestAuth(t *testing.T, cost int) *Authenticator {
	t.Helper()
	old := store.PasswordCost
	store.PasswordCost = cost
	t.Cleanup(func() { store.PasswordCost = old })
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if _, err := st.CreateDomain(ctx, "example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, store.NewUser{Email: "alice@example.test", Password: "correct horse"}); err != nil {
		t.Fatal(err)
	}
	return New(st)
}

func TestUsernameSpellingsShareAccountLimit(t *testing.T) {
	a := newTestAuth(t, 4)
	ctx := context.Background()
	spellings := []string{"alice@example.test", "<alice@example.test>", "ALICE@Example.test", " <<alice@example.test", "< <alice@example.test>>"}
	for _, s := range spellings {
		if _, err := a.Login(ctx, "192.0.2.1:1234", s, "correct horse"); err != nil {
			t.Fatalf("login as %q: %v", s, err)
		}
	}
	// Each attempt comes from another network, so only the account limit applies.
	for i := range 20 {
		_, err := a.Login(ctx, fmt.Sprintf("198.51.100.%d:1234", i+1), spellings[i%len(spellings)], "guess")
		if !errors.Is(err, store.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if _, err := a.Login(ctx, "203.0.113.1:1234", "alice@example.test", "correct horse"); !errors.Is(err, ErrThrottled) {
		t.Fatalf("login after 20 failures = %v, want ErrThrottled", err)
	}
}

func TestConcurrentAttemptsCannotExceedLimit(t *testing.T) {
	a := newTestAuth(t, 8)
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed int
	)
	for range 30 {
		wg.Go(func() {
			_, err := a.Login(context.Background(), "192.0.2.7:1234", "alice@example.test", "guess")
			if errors.Is(err, store.ErrInvalidCredentials) {
				mu.Lock()
				failed++
				mu.Unlock()
			} else if !errors.Is(err, ErrThrottled) {
				t.Errorf("login: %v", err)
			}
		})
	}
	wg.Wait()
	if failed != 10 {
		t.Fatalf("%d passwords were checked, want 10", failed)
	}
}
