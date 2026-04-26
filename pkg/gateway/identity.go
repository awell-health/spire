package gateway

import (
	"context"
	"net/http"
	"strings"
)

// ArchmageIdentity carries the per-call archmage attribution the desktop CLI
// supplies via X-Archmage-Name / X-Archmage-Email request headers. The
// Source field records where the gateway resolved the identity from
// ("header" when both headers were present and accepted, "tower-default"
// when the cluster tower's static archmage was used as a fallback). Empty
// Source means no identity was resolved (logs / debug paths only — handlers
// see a populated Source whenever the request reached them through
// withIdentity).
type ArchmageIdentity struct {
	Name   string
	Email  string
	Source string
}

// archmageHeaderName / archmageHeaderEmail are the canonical request headers
// the gateway parses for per-call archmage attribution. Clients (the
// gatewayclient package) emit both or neither.
const (
	archmageHeaderName  = "X-Archmage-Name"
	archmageHeaderEmail = "X-Archmage-Email"
)

// identityCtxKey is the context key under which a resolved ArchmageIdentity
// is stored. Use IdentityFromContext to read; production callers never use
// the raw key directly.
type identityCtxKey struct{}

// WithIdentity returns ctx with id stashed under identityCtxKey. Test
// helpers and middleware use this to attach a resolved identity to the
// request context handlers downstream read.
func WithIdentity(ctx context.Context, id ArchmageIdentity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFromContext returns the ArchmageIdentity threaded into ctx by the
// identity middleware. The second return value is false when no identity was
// stashed — handlers should treat this the same as an empty Source identity
// and fall back to the underlying store's Actor() default.
func IdentityFromContext(ctx context.Context) (ArchmageIdentity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(ArchmageIdentity)
	return id, ok
}

// AuthorString renders the identity as the standard "Name <email>" git
// commit author format the bead store accepts. Returns an empty string when
// either field is missing so callers can branch on "use default Actor()".
func (id ArchmageIdentity) AuthorString() string {
	if id.Name == "" || id.Email == "" {
		return ""
	}
	return id.Name + " <" + id.Email + ">"
}

// resolveRequestIdentity parses X-Archmage-Name / X-Archmage-Email from r,
// applying the trust rules documented on this package:
//
//   - Both headers must be present and non-empty (after Trim) to count as a
//     valid header-supplied identity. Partial identity (just name OR just
//     email) is rejected as worse than no identity for audit attribution.
//   - When both are missing/empty and fallback yields a valid Name+Email,
//     the cluster tower's static archmage is returned with Source
//     "tower-default" so the existing "GET /api/v1/tower" archmage stays
//     honoured.
//   - When neither path produces a complete identity, the zero value is
//     returned with Source "". Handlers fall back to store.Actor() in this
//     case.
//
// The fallback closure is the package indirection that handleRoster /
// handleTower already use (config.ResolveTowerConfig); injection keeps
// resolveRequestIdentity unit-testable without touching the real config dir.
func resolveRequestIdentity(r *http.Request, fallback func() ArchmageIdentity) ArchmageIdentity {
	name := strings.TrimSpace(r.Header.Get(archmageHeaderName))
	email := strings.TrimSpace(r.Header.Get(archmageHeaderEmail))
	if name != "" && email != "" {
		return ArchmageIdentity{Name: name, Email: email, Source: "header"}
	}
	if fallback != nil {
		fb := fallback()
		if fb.Name != "" && fb.Email != "" {
			fb.Source = "tower-default"
			return fb
		}
	}
	return ArchmageIdentity{}
}
