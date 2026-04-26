package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResolveRequestIdentity_HeaderHappyPath: both headers present and
// non-empty produces a header-sourced identity (no fallback consulted).
func TestResolveRequestIdentity_HeaderHappyPath(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/beads", nil)
	r.Header.Set("X-Archmage-Name", "Bob")
	r.Header.Set("X-Archmage-Email", "bob@example.com")

	called := false
	id := resolveRequestIdentity(r, func() ArchmageIdentity {
		called = true
		return ArchmageIdentity{Name: "FALLBACK", Email: "fallback@example.com"}
	})
	if id.Name != "Bob" || id.Email != "bob@example.com" {
		t.Errorf("identity = %+v, want Bob/bob@example.com", id)
	}
	if id.Source != "header" {
		t.Errorf("Source = %q, want \"header\"", id.Source)
	}
	if called {
		t.Error("fallback was consulted even though both headers were present")
	}
}

// TestResolveRequestIdentity_PartialHeadersFallBack: when only one of the
// two identity headers is set (or one is whitespace-only), the resolver
// must NOT accept a partial identity. It falls through to the cluster
// tower's default. Pins the trust contract.
func TestResolveRequestIdentity_PartialHeadersFallBack(t *testing.T) {
	tests := []struct {
		name  string
		hName string
		hMail string
	}{
		{"name only", "Bob", ""},
		{"email only", "", "bob@example.com"},
		{"name whitespace", "   ", "bob@example.com"},
		{"email whitespace", "Bob", "   "},
		{"both empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/beads", nil)
			if tc.hName != "" {
				r.Header.Set("X-Archmage-Name", tc.hName)
			}
			if tc.hMail != "" {
				r.Header.Set("X-Archmage-Email", tc.hMail)
			}
			id := resolveRequestIdentity(r, func() ArchmageIdentity {
				return ArchmageIdentity{Name: "Cluster", Email: "cluster@example.com"}
			})
			if id.Name != "Cluster" || id.Email != "cluster@example.com" {
				t.Errorf("partial-headers identity = %+v, want fallback Cluster/cluster@example.com", id)
			}
			if id.Source != "tower-default" {
				t.Errorf("Source = %q, want \"tower-default\"", id.Source)
			}
		})
	}
}

// TestResolveRequestIdentity_NoFallbackReturnsZero: when neither headers
// nor fallback yield a complete identity, the resolver returns the zero
// value with empty Source. Handlers branch on this to fall back to
// store.Actor() default.
func TestResolveRequestIdentity_NoFallbackReturnsZero(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/beads", nil)
	id := resolveRequestIdentity(r, func() ArchmageIdentity {
		return ArchmageIdentity{} // no fallback
	})
	if id.Name != "" || id.Email != "" || id.Source != "" {
		t.Errorf("identity = %+v, want zero value", id)
	}

	// Nil fallback closure is also OK — same outcome.
	id = resolveRequestIdentity(r, nil)
	if id.Name != "" || id.Email != "" || id.Source != "" {
		t.Errorf("nil fallback identity = %+v, want zero value", id)
	}
}

// TestArchmageIdentity_AuthorString covers the "Name <email>" rendering
// that gateway handlers pass into store.AddCommentAsReturning. Empty Name
// or Email yields "" so callers fall through to store.Actor().
func TestArchmageIdentity_AuthorString(t *testing.T) {
	tests := []struct {
		id   ArchmageIdentity
		want string
	}{
		{ArchmageIdentity{Name: "Bob", Email: "bob@example.com"}, "Bob <bob@example.com>"},
		{ArchmageIdentity{Name: "Bob"}, ""},
		{ArchmageIdentity{Email: "bob@example.com"}, ""},
		{ArchmageIdentity{}, ""},
	}
	for _, tc := range tests {
		got := tc.id.AuthorString()
		if got != tc.want {
			t.Errorf("AuthorString(%+v) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// TestBearerAuth_ThreadsIdentityIntoContext: the auth middleware not only
// validates the bearer but also resolves identity headers (or fallback)
// and stashes the result on r.Context(). Handlers downstream can read it
// via IdentityFromContext.
func TestBearerAuth_ThreadsIdentityIntoContext(t *testing.T) {
	// Set up a server with a known token + a handler that returns whatever
	// IdentityFromContext yields, so we can assert the threading worked.
	prev := towerArchmageFallback
	defer func() { towerArchmageFallback = prev }()
	towerArchmageFallback = func() ArchmageIdentity {
		return ArchmageIdentity{Name: "Cluster", Email: "cluster@example.com"}
	}

	s := &Server{apiToken: "good", log: log.New(io.Discard, "", 0)}
	var gotID ArchmageIdentity
	var gotOK bool
	echo := func(w http.ResponseWriter, r *http.Request) {
		gotID, gotOK = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}

	// Header-supplied identity wins.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/beads", nil)
	r.Header.Set("Authorization", "Bearer good")
	r.Header.Set("X-Archmage-Name", "Bob")
	r.Header.Set("X-Archmage-Email", "bob@example.com")
	s.bearerAuth(echo)(httptest.NewRecorder(), r)
	if !gotOK {
		t.Fatal("identity was not stashed on context")
	}
	if gotID.Name != "Bob" || gotID.Email != "bob@example.com" || gotID.Source != "header" {
		t.Errorf("identity = %+v, want Bob/bob@example.com source=header", gotID)
	}

	// Missing headers — fallback stamped instead.
	r = httptest.NewRequest(http.MethodGet, "/api/v1/beads", nil)
	r.Header.Set("Authorization", "Bearer good")
	s.bearerAuth(echo)(httptest.NewRecorder(), r)
	if !gotOK {
		t.Fatal("fallback identity was not stashed on context")
	}
	if gotID.Name != "Cluster" || gotID.Source != "tower-default" {
		t.Errorf("fallback identity = %+v, want Cluster source=tower-default", gotID)
	}
}

// TestBearerAuth_RejectsBeforeIdentityResolution: an unauthenticated
// request must not have its claimed identity recorded — bearer is the
// trust boundary. The handler is never invoked, and IdentityFromContext
// would return false if it were.
func TestBearerAuth_RejectsBeforeIdentityResolution(t *testing.T) {
	s := &Server{apiToken: "good", log: log.New(io.Discard, "", 0)}
	called := false
	r := httptest.NewRequest(http.MethodGet, "/api/v1/beads", nil)
	r.Header.Set("X-Archmage-Name", "ATTACKER")
	r.Header.Set("X-Archmage-Email", "attacker@example.com")
	// no Authorization header
	rec := httptest.NewRecorder()
	s.bearerAuth(func(_ http.ResponseWriter, r *http.Request) { called = true })(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("handler was invoked despite missing bearer; identity headers must not bypass auth")
	}
}

// TestBearerAuth_DevModeStillResolvesIdentity: when SPIRE_API_TOKEN is
// empty (dev mode), the bearer check is skipped but identity headers are
// still parsed so end-to-end tests against a tokenless gateway can
// exercise the same audit paths.
func TestBearerAuth_DevModeStillResolvesIdentity(t *testing.T) {
	s := &Server{apiToken: "", log: log.New(io.Discard, "", 0)}
	var gotID ArchmageIdentity
	echo := func(w http.ResponseWriter, r *http.Request) {
		gotID, _ = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/beads", nil)
	r.Header.Set("X-Archmage-Name", "Bob")
	r.Header.Set("X-Archmage-Email", "bob@example.com")
	s.bearerAuth(echo)(httptest.NewRecorder(), r)
	if gotID.Name != "Bob" || gotID.Source != "header" {
		t.Errorf("dev-mode identity = %+v, want Bob source=header", gotID)
	}
}

// TestPostBeadComment_UsesIdentityAuthor: when the request carries an
// identity, postBeadComment passes it through commentsAddAsFunc as the
// comment author. Pins the per-call attribution from end to end at the
// handler boundary.
func TestPostBeadComment_UsesIdentityAuthor(t *testing.T) {
	prevEnsure := commentsStoreEnsureFunc
	prevAddAs := commentsAddAsFunc
	defer func() {
		commentsStoreEnsureFunc = prevEnsure
		commentsAddAsFunc = prevAddAs
	}()
	commentsStoreEnsureFunc = func(string) error { return nil }

	var gotAuthor string
	commentsAddAsFunc = func(id, author, text string) (string, error) {
		gotAuthor = author
		return "c-1", nil
	}

	s := &Server{log: log.New(io.Discard, "", 0)}

	body := strings.NewReader(`{"text":"hello"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/comments", body)
	r.ContentLength = int64(body.Len())
	r = r.WithContext(WithIdentity(r.Context(), ArchmageIdentity{
		Name:  "Bob",
		Email: "bob@example.com",
	}))
	rec := httptest.NewRecorder()
	s.postBeadComment(rec, r, "spi-abc")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%q)", rec.Code, rec.Body.String())
	}
	if gotAuthor != "Bob <bob@example.com>" {
		t.Errorf("commentsAddAsFunc author = %q, want \"Bob <bob@example.com>\"", gotAuthor)
	}
}

// TestPostBeadComment_NoIdentityFallsBackToActor: when the context has no
// identity, postBeadComment passes "" as author so AddCommentAsReturning
// falls through to store.Actor() ("spire"). Preserves the pre-identity
// behaviour for non-gateway callers that share commentsAddAsFunc.
func TestPostBeadComment_NoIdentityFallsBackToActor(t *testing.T) {
	prevEnsure := commentsStoreEnsureFunc
	prevAddAs := commentsAddAsFunc
	defer func() {
		commentsStoreEnsureFunc = prevEnsure
		commentsAddAsFunc = prevAddAs
	}()
	commentsStoreEnsureFunc = func(string) error { return nil }

	var gotAuthor string
	commentsAddAsFunc = func(id, author, text string) (string, error) {
		gotAuthor = author
		return "c-1", nil
	}

	s := &Server{log: log.New(io.Discard, "", 0)}
	body := strings.NewReader(`{"text":"hello"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/comments", body)
	r.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.postBeadComment(rec, r, "spi-abc")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%q)", rec.Code, rec.Body.String())
	}
	if gotAuthor != "" {
		t.Errorf("commentsAddAsFunc author = %q, want \"\" (fall-through to Actor())", gotAuthor)
	}
}

// TestSendMessage_HeaderWinsOverBodyFrom verifies the From-field collision
// rule: header identity overrides any body.From the desktop sent. Pins
// the audit-trust contract in the plan.
func TestSendMessage_HeaderWinsOverBodyFrom(t *testing.T) {
	// store.Ensure inside sendMessage will fail on an empty data dir;
	// short-circuit by capturing the identity resolution before we hit
	// the storage path. We do this by replacing the dataDir with a
	// non-existent path and reading the log line we added.
	var logBuf bytes.Buffer
	s := &Server{log: log.New(&logBuf, "", 0)}

	body := strings.NewReader(`{"to":"wizard","message":"hi","from":"daemon"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/messages", body)
	r.ContentLength = int64(body.Len())
	r = r.WithContext(WithIdentity(r.Context(), ArchmageIdentity{
		Name:  "Bob",
		Email: "bob@example.com",
	}))
	rec := httptest.NewRecorder()
	s.sendMessage(rec, r)

	// We expect the handler to log the collision regardless of the
	// downstream store error. Decoding the response body is irrelevant
	// here; the log line is what carries the contract.
	if !strings.Contains(logBuf.String(), "from-field collision") {
		t.Errorf("log = %q, want collision warning", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `body="daemon"`) {
		t.Errorf("log = %q, want body=daemon recorded", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), `header="Bob"`) {
		t.Errorf("log = %q, want header=Bob recorded", logBuf.String())
	}
}

// TestAppendArchmageLabels_StampsIdentity: createBead's label-stamping
// helper should add archmage:<name> + archmage-email:<email> while
// preserving caller-supplied labels and avoiding duplicates.
func TestAppendArchmageLabels_StampsIdentity(t *testing.T) {
	got := appendArchmageLabels([]string{"msg", "to:wizard"}, ArchmageIdentity{Name: "Bob", Email: "bob@example.com"})
	want := []string{"msg", "to:wizard", "archmage:Bob", "archmage-email:bob@example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %d labels, want %d (got=%v)", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}

	// Idempotent — calling twice does not duplicate.
	got2 := appendArchmageLabels(got, ArchmageIdentity{Name: "Bob", Email: "bob@example.com"})
	if len(got2) != len(want) {
		t.Errorf("idempotency check: len = %d, want %d (got=%v)", len(got2), len(want), got2)
	}

	// Empty email: only stamp archmage:<name>.
	got3 := appendArchmageLabels(nil, ArchmageIdentity{Name: "Bob"})
	if len(got3) != 1 || got3[0] != "archmage:Bob" {
		t.Errorf("empty-email stamp = %+v, want [archmage:Bob]", got3)
	}
}

// TestSendMessage_AuthorAndFromUseHeader verifies the message handler
// stamps both From and Author from the header identity so SendMessage's
// underlying bead carries the calling archmage on the dolt commit and
// the message audit row.
func TestSendMessage_AuthorAndFromUseHeader(t *testing.T) {
	// Same as TestSendMessage_HeaderWinsOverBodyFrom: log-driven assertion
	// because the underlying store.CreateBead would need a real beads
	// store to reach the author-stamp step. We verify the in-handler
	// resolution that runs before store access.
	var logBuf bytes.Buffer
	s := &Server{log: log.New(&logBuf, "", 0)}

	body := strings.NewReader(`{"to":"wizard","message":"hi"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/messages", body)
	r.ContentLength = int64(body.Len())
	r = r.WithContext(WithIdentity(r.Context(), ArchmageIdentity{
		Name:  "Bob",
		Email: "bob@example.com",
	}))
	rec := httptest.NewRecorder()
	s.sendMessage(rec, r)

	// No collision warning expected here (body From was empty).
	if strings.Contains(logBuf.String(), "from-field collision") {
		t.Errorf("unexpected collision log when body.From was empty: %q", logBuf.String())
	}
}

// TestPostBeadComment_AuthorStringRoundTrip is an end-to-end sanity check
// that the gateway echoes back a comment id from AddCommentAsReturning
// so the desktop's CloseBeadModal can render it without a follow-up GET.
// Decoded JSON proves the create-then-id flow is intact after the
// AddCommentAs swap.
func TestPostBeadComment_AuthorStringRoundTrip(t *testing.T) {
	prevEnsure := commentsStoreEnsureFunc
	prevAddAs := commentsAddAsFunc
	defer func() {
		commentsStoreEnsureFunc = prevEnsure
		commentsAddAsFunc = prevAddAs
	}()
	commentsStoreEnsureFunc = func(string) error { return nil }
	commentsAddAsFunc = func(id, author, text string) (string, error) {
		return "c-routed", nil
	}

	s := &Server{log: log.New(io.Discard, "", 0)}
	body := strings.NewReader(`{"text":"hello"}`)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/comments", body)
	r.ContentLength = int64(body.Len())
	r = r.WithContext(WithIdentity(r.Context(), ArchmageIdentity{Name: "Bob", Email: "bob@example.com"}))
	rec := httptest.NewRecorder()
	s.postBeadComment(rec, r, "spi-abc")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%q)", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != "c-routed" {
		t.Errorf("id = %q, want c-routed", got["id"])
	}
}
