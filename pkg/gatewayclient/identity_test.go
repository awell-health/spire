package gatewayclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNewClientWithIdentity_StampsHeadersOnEveryRequest verifies the
// gatewayclient injects X-Archmage-Name / X-Archmage-Email on every call
// when the constructor was given a complete Identity. Pins the contract
// the gateway middleware on the other side reads from.
func TestNewClientWithIdentity_StampsHeadersOnEveryRequest(t *testing.T) {
	var gotName, gotEmail string
	var hits int
	// The handler picks a payload shape based on the request path so each
	// gatewayclient method's own JSON decoder is happy. The intent of the
	// test is the headers, not the payload.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotName = r.Header.Get("X-Archmage-Name")
		gotEmail = r.Header.Get("X-Archmage-Email")
		hits++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/beads":
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	c := NewClientWithIdentity(srv.URL, "tok", Identity{Name: "Bob", Email: "bob@example.com"})

	// Each method that issues a request must stamp the headers — exercise
	// a representative subset across files so a future endpoint added
	// without going through doJSON would still be flagged.
	if _, err := c.GetTower(context.Background()); err != nil {
		t.Fatalf("GetTower: %v", err)
	}
	if gotName != "Bob" || gotEmail != "bob@example.com" {
		t.Fatalf("GetTower headers = (%q, %q), want (Bob, bob@example.com)", gotName, gotEmail)
	}

	if _, err := c.ListBeads(context.Background(), ListBeadsFilter{}); err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if gotName != "Bob" || gotEmail != "bob@example.com" {
		t.Fatalf("ListBeads headers = (%q, %q), want (Bob, bob@example.com)", gotName, gotEmail)
	}

	if _, err := c.SendMessage(context.Background(), SendMessageInput{To: "wizard", Message: "hi"}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if gotName != "Bob" || gotEmail != "bob@example.com" {
		t.Fatalf("SendMessage headers = (%q, %q), want (Bob, bob@example.com)", gotName, gotEmail)
	}

	if hits != 3 {
		t.Errorf("server hit %d times, want 3", hits)
	}
}

// TestNewClient_OmitsIdentityHeaders verifies the bare NewClient form
// (no identity) emits no X-Archmage-* headers. Required so the
// attach-cluster pre-handshake (which has no archmage to attribute yet)
// stays compatible with older gateways that ignore the headers.
func TestNewClient_OmitsIdentityHeaders(t *testing.T) {
	var hasName, hasEmail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasName = r.Header[http.CanonicalHeaderKey("X-Archmage-Name")]
		_, hasEmail = r.Header[http.CanonicalHeaderKey("X-Archmage-Email")]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.GetTower(context.Background()); err != nil {
		t.Fatalf("GetTower: %v", err)
	}
	if hasName || hasEmail {
		t.Errorf("bare NewClient must not emit identity headers (name=%v email=%v)", hasName, hasEmail)
	}
}

// TestNewClientWithIdentity_PartialIdentitySuppressesBoth verifies the
// "all-or-nothing" rule: if either field is empty, neither header is
// emitted. The gateway treats partial identity as worse than no identity
// for audit attribution; the client-side enforcement keeps the contract
// symmetric so a half-configured desktop falls back cleanly to the
// cluster tower's default archmage.
func TestNewClientWithIdentity_PartialIdentitySuppressesBoth(t *testing.T) {
	cases := []struct {
		name string
		id   Identity
	}{
		{"name only", Identity{Name: "Bob"}},
		{"email only", Identity{Email: "bob@example.com"}},
		{"both empty", Identity{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hasName, hasEmail bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, hasName = r.Header[http.CanonicalHeaderKey("X-Archmage-Name")]
				_, hasEmail = r.Header[http.CanonicalHeaderKey("X-Archmage-Email")]
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			c := NewClientWithIdentity(srv.URL, "tok", tc.id)
			if _, err := c.GetTower(context.Background()); err != nil {
				t.Fatalf("GetTower: %v", err)
			}
			if hasName || hasEmail {
				t.Errorf("partial identity %+v emitted headers (name=%v email=%v) — want neither",
					tc.id, hasName, hasEmail)
			}
		})
	}
}

// TestClient_IdentityAccessor returns the identity passed at
// construction so callers (e.g. dispatch wiring) can check whether a
// given client was built with a usable identity without having to parse
// it back out of the header.
func TestClient_IdentityAccessor(t *testing.T) {
	want := Identity{Name: "Bob", Email: "bob@example.com"}
	c := NewClientWithIdentity("http://example.com", "tok", want)
	if got := c.Identity(); got != want {
		t.Errorf("Identity() = %+v, want %+v", got, want)
	}

	// Bare NewClient: zero Identity.
	c2 := NewClient("http://example.com", "tok")
	if got := c2.Identity(); got != (Identity{}) {
		t.Errorf("bare NewClient Identity() = %+v, want zero", got)
	}
}
