// Summon-time credential selection.
//
// `SelectAuth` implements the hardcoded 6-step rule chain that `spire summon`
// uses to pick which configured auth slot (subscription vs api-key) a wizard
// run should use, or synthesize an ephemeral context when the caller passes
// an inline `-H` header. The rule order is fixed; do not make it configurable
// and do not add other rules — see the epic (spi-gsmvr4) for why.
//
// The returned `*config.AuthContext` always carries the configured api-key
// credential in its `APIKey` field (if one is configured) so the 429
// auto-promote handler has a fallback to swap to, regardless of which slot
// is Active. `Ephemeral` is set true when the context was synthesized from
// a `-H` header; the 429 handler must not swap an ephemeral context (an
// inline header is an explicit one-shot override from the caller).
package wizard

import (
	"errors"
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// SelectFlags captures the summon-time auth flags parsed from the CLI.
// See `SelectAuth` for selection order.
type SelectFlags struct {
	// AuthSlot is the value of `--auth=<subscription|api-key>` ("" when
	// unset).
	AuthSlot string
	// Turbo is true when `--turbo` was passed. Turbo is a strict alias for
	// `--auth=api-key`; combining the two with a non-api-key `--auth` value
	// is rejected as a user error.
	Turbo bool
	// HeaderAPIKey holds the value of `-H x-anthropic-api-key: <v>`, or ""
	// when that header wasn't passed.
	HeaderAPIKey string
	// HeaderToken holds the value of `-H x-anthropic-token: <v>`, or ""
	// when that header wasn't passed.
	HeaderToken string
}

// Allowed `-H` header names. Any other name is rejected with a clear
// error — no silent passthrough, the caller's typo should not be
// reinterpreted as an unknown Anthropic header.
const (
	HeaderNameAPIKey = "x-anthropic-api-key"
	HeaderNameToken  = "x-anthropic-token"
)

// ParseSummonHeaders parses repeated `-H <name>: <value>` args. Names are
// lower-cased for comparison (HTTP header names are case-insensitive) and
// whitespace around the name/value is trimmed. The returned SelectFlags
// has HeaderAPIKey and HeaderToken populated; other fields are left zero.
// A duplicate of the same header replaces the previous value — last one
// wins, matching how curl treats repeated `-H`.
func ParseSummonHeaders(headers []string) (SelectFlags, error) {
	out := SelectFlags{}
	for _, h := range headers {
		name, value, ok := splitHeader(h)
		if !ok {
			return SelectFlags{}, fmt.Errorf("invalid header %q (expected \"name: value\")", h)
		}
		switch strings.ToLower(name) {
		case HeaderNameAPIKey:
			out.HeaderAPIKey = value
		case HeaderNameToken:
			out.HeaderToken = value
		default:
			return SelectFlags{}, fmt.Errorf("unsupported header %q (supported: %s, %s)", name, HeaderNameAPIKey, HeaderNameToken)
		}
	}
	return out, nil
}

func splitHeader(h string) (name, value string, ok bool) {
	idx := strings.Index(h, ":")
	if idx < 0 {
		return "", "", false
	}
	name = strings.TrimSpace(h[:idx])
	value = strings.TrimSpace(h[idx+1:])
	if name == "" {
		return "", "", false
	}
	return name, value, true
}

// ValidateFlags returns an error for mutually exclusive flag combinations
// before SelectAuth runs. Separated so the CLI layer can surface a precise
// "flag combination" error distinct from slot-not-configured errors.
func ValidateFlags(flags SelectFlags) error {
	if flags.Turbo && flags.AuthSlot != "" && flags.AuthSlot != config.AuthSlotAPIKey {
		return fmt.Errorf("--turbo conflicts with --auth=%s (--turbo is an alias for --auth=api-key)", flags.AuthSlot)
	}
	if flags.AuthSlot != "" && flags.AuthSlot != config.AuthSlotSubscription && flags.AuthSlot != config.AuthSlotAPIKey {
		return fmt.Errorf("--auth=%q is invalid (want %q or %q)", flags.AuthSlot, config.AuthSlotSubscription, config.AuthSlotAPIKey)
	}
	return nil
}

// SelectAuth chooses which auth slot a wizard should run with. The rules
// are hardcoded (see epic spi-gsmvr4); do not make them configurable and
// do not add new cases:
//
//  1. `-H x-anthropic-api-key: <v>` → synthesize ephemeral api-key.
//  2. `-H x-anthropic-token: <v>` → synthesize ephemeral subscription.
//  3. `--turbo` or `--auth=api-key` → configured `[auth.api-key]`.
//  4. `--auth=subscription` → configured `[auth.subscription]`.
//  5. beadPriority == 0 → configured `[auth.api-key]` (hardcoded P0 rule).
//  6. Default → slot named in `[auth] default`.
//
// At every rule-3-through-6 branch, if the required slot isn't configured
// the function returns an actionable error referencing the exact
// `spire config auth set …` invocation the operator needs. No silent
// fallthrough; callers must fix their config or pass an explicit flag.
//
// The returned context always has APIKey populated from cfg (if
// configured), AutoPromoteOn429 copied from cfg, and Active set to the
// selected credential. Ephemeral=true only for rules 1 and 2.
func SelectAuth(cfg *config.AuthConfig, beadPriority int, flags SelectFlags) (*config.AuthContext, error) {
	if cfg == nil {
		return nil, errors.New("nil AuthConfig")
	}
	if err := ValidateFlags(flags); err != nil {
		return nil, err
	}

	// APIKey fallback and auto-promote flag ride along with every context
	// so the 429 handler has what it needs regardless of Active slot.
	base := &config.AuthContext{
		APIKey:           cfg.APIKey,
		AutoPromoteOn429: cfg.AutoPromoteOn429,
	}

	// Rule 1: -H x-anthropic-api-key
	if flags.HeaderAPIKey != "" {
		base.Active = &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: flags.HeaderAPIKey}
		base.Ephemeral = true
		return base, nil
	}
	// Rule 2: -H x-anthropic-token
	if flags.HeaderToken != "" {
		base.Active = &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: flags.HeaderToken}
		base.Ephemeral = true
		return base, nil
	}
	// Rule 3: --turbo or --auth=api-key
	if flags.Turbo || flags.AuthSlot == config.AuthSlotAPIKey {
		if cfg.APIKey == nil {
			return nil, unconfiguredSlotError(config.AuthSlotAPIKey, "--auth=api-key / --turbo", "")
		}
		base.Active = cfg.APIKey
		return base, nil
	}
	// Rule 4: --auth=subscription
	if flags.AuthSlot == config.AuthSlotSubscription {
		if cfg.Subscription == nil {
			return nil, unconfiguredSlotError(config.AuthSlotSubscription, "--auth=subscription", "")
		}
		base.Active = cfg.Subscription
		return base, nil
	}
	// Rule 5: P0 rule (hardcoded)
	if beadPriority == 0 {
		if cfg.APIKey == nil {
			return nil, unconfiguredSlotError(config.AuthSlotAPIKey, "priority-0 bead", "P0 beads always use the api-key slot")
		}
		base.Active = cfg.APIKey
		return base, nil
	}
	// Rule 6: default slot
	switch cfg.Default {
	case config.AuthSlotSubscription:
		if cfg.Subscription == nil {
			return nil, unconfiguredSlotError(config.AuthSlotSubscription, "[auth] default", "set via `spire config auth default`")
		}
		base.Active = cfg.Subscription
		return base, nil
	case config.AuthSlotAPIKey:
		if cfg.APIKey == nil {
			return nil, unconfiguredSlotError(config.AuthSlotAPIKey, "[auth] default", "set via `spire config auth default`")
		}
		base.Active = cfg.APIKey
		return base, nil
	case "":
		return nil, errors.New("no auth slot selected and `[auth] default` is unset — run `spire config auth default <subscription|api-key>` or pass `--auth=<slot>`")
	default:
		return nil, fmt.Errorf("`[auth] default` is %q but only %q or %q are valid", cfg.Default, config.AuthSlotSubscription, config.AuthSlotAPIKey)
	}
}

// unconfiguredSlotError formats a consistent "slot not configured" message
// that names (a) the slot, (b) which rule triggered selection, and (c) the
// set command the operator needs. The extra string is appended when the
// caller wants to explain why the rule fired (P0 rule, default slot, etc.).
func unconfiguredSlotError(slot, triggeredBy, extra string) error {
	var setCmd string
	switch slot {
	case config.AuthSlotAPIKey:
		setCmd = "spire config auth set api-key --key-stdin"
	case config.AuthSlotSubscription:
		setCmd = "spire config auth set subscription --token-stdin"
	default:
		setCmd = "spire config auth set <slot> …"
	}
	msg := fmt.Sprintf("%s slot required (%s) but not configured — run `%s`", slot, triggeredBy, setCmd)
	if extra != "" {
		msg = msg + " (" + extra + ")"
	}
	return errors.New(msg)
}
