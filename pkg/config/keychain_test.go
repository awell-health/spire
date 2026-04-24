package config

import (
	"errors"
	"testing"
)

// installFakeTowerKeychain swaps the towerKeychain{Set,Get,Delete}Fn hooks
// with an in-process map so tests don't shell out to the real OS keychain.
// Restores the originals via t.Cleanup.
func installFakeTowerKeychain(t *testing.T) map[string]string {
	t.Helper()
	store := make(map[string]string)

	origSet := towerKeychainSetFn
	origGet := towerKeychainGetFn
	origDel := towerKeychainDeleteFn

	towerKeychainSetFn = func(account, value string) error {
		store[account] = value
		return nil
	}
	towerKeychainGetFn = func(account string) (string, error) {
		v, ok := store[account]
		if !ok {
			return "", ErrTokenNotFound
		}
		return v, nil
	}
	towerKeychainDeleteFn = func(account string) error {
		delete(store, account)
		return nil
	}

	t.Cleanup(func() {
		towerKeychainSetFn = origSet
		towerKeychainGetFn = origGet
		towerKeychainDeleteFn = origDel
	})
	return store
}

func TestTowerToken_RoundTrip(t *testing.T) {
	installFakeTowerKeychain(t)

	if err := SetTowerToken("mytower", "sekret-token"); err != nil {
		t.Fatalf("SetTowerToken: %v", err)
	}
	got, err := GetTowerToken("mytower")
	if err != nil {
		t.Fatalf("GetTowerToken: %v", err)
	}
	if got != "sekret-token" {
		t.Fatalf("GetTowerToken = %q, want %q", got, "sekret-token")
	}
}

func TestTowerToken_NotFoundSentinel(t *testing.T) {
	installFakeTowerKeychain(t)

	_, err := GetTowerToken("never-set")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("GetTowerToken on missing tower = %v, want ErrTokenNotFound", err)
	}
}

func TestTowerToken_DeleteThenMissing(t *testing.T) {
	installFakeTowerKeychain(t)

	if err := SetTowerToken("tower", "t"); err != nil {
		t.Fatalf("SetTowerToken: %v", err)
	}
	if err := DeleteTowerToken("tower"); err != nil {
		t.Fatalf("DeleteTowerToken: %v", err)
	}
	_, err := GetTowerToken("tower")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("GetTowerToken after delete = %v, want ErrTokenNotFound", err)
	}
}

func TestTowerToken_DeleteIdempotent(t *testing.T) {
	installFakeTowerKeychain(t)

	if err := DeleteTowerToken("never-set"); err != nil {
		t.Fatalf("DeleteTowerToken on missing tower = %v, want nil", err)
	}
}

func TestTowerToken_PerTowerIsolation(t *testing.T) {
	installFakeTowerKeychain(t)

	if err := SetTowerToken("dev", "dev-token"); err != nil {
		t.Fatalf("set dev: %v", err)
	}
	if err := SetTowerToken("prod", "prod-token"); err != nil {
		t.Fatalf("set prod: %v", err)
	}

	devTok, err := GetTowerToken("dev")
	if err != nil || devTok != "dev-token" {
		t.Fatalf("dev token = (%q, %v), want (dev-token, nil)", devTok, err)
	}
	prodTok, err := GetTowerToken("prod")
	if err != nil || prodTok != "prod-token" {
		t.Fatalf("prod token = (%q, %v), want (prod-token, nil)", prodTok, err)
	}

	// Deleting one tower must not affect the other.
	if err := DeleteTowerToken("dev"); err != nil {
		t.Fatalf("delete dev: %v", err)
	}
	if _, err := GetTowerToken("dev"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("dev after delete = %v, want ErrTokenNotFound", err)
	}
	if tok, err := GetTowerToken("prod"); err != nil || tok != "prod-token" {
		t.Fatalf("prod after deleting dev = (%q, %v), want (prod-token, nil)", tok, err)
	}
}

func TestTowerToken_UsesDistinctNamespace(t *testing.T) {
	// Verifies the account name is derived from the tower name and carries
	// the "-token" suffix — callers depend on this namespace staying distinct
	// from other credential keys (e.g. RemotesapiUserKey).
	store := installFakeTowerKeychain(t)

	if err := SetTowerToken("dev", "tok"); err != nil {
		t.Fatalf("SetTowerToken: %v", err)
	}
	wantAccount := "dev-token"
	if _, ok := store[wantAccount]; !ok {
		t.Fatalf("expected account %q in store, got keys: %v", wantAccount, mapKeys(store))
	}
}

func TestTowerTokenAccount_Sanitizes(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"simple", "simple-token"},
		{"Mixed-Case", "mixed-case-token"},
		{"with.dots", "with_dots-token"},
		{"has spaces", "has_spaces-token"},
		{"", "default-token"},
	}
	for _, tc := range tests {
		if got := towerTokenAccount(tc.in); got != tc.want {
			t.Errorf("towerTokenAccount(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
