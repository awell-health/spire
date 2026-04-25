package wizard

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// configuredAuth returns an AuthConfig with both slots populated.
// Tests that want a missing slot override the returned config in place.
func configuredAuth() *config.AuthConfig {
	return &config.AuthConfig{
		Default:          config.AuthSlotSubscription,
		AutoPromoteOn429: true,
		Subscription: &config.AuthCredential{
			Slot:   config.AuthSlotSubscription,
			Secret: "sk-ant-sub-xxx",
		},
		APIKey: &config.AuthCredential{
			Slot:   config.AuthSlotAPIKey,
			Secret: "sk-ant-api-yyy",
		},
	}
}

// TestSelectAuth_Table covers the full selection matrix (happy paths) so any
// re-ordering of the hardcoded 6-step rule chain trips a failing test rather
// than shipping a silent behavior change.
func TestSelectAuth_Table(t *testing.T) {
	type expect struct {
		slot      string
		secret    string
		ephemeral bool
	}
	tests := []struct {
		name     string
		cfg      *config.AuthConfig
		priority int
		flags    SelectFlags
		want     expect
	}{
		{
			name:     "rule 1: -H x-anthropic-api-key wins over every later rule",
			cfg:      configuredAuth(),
			priority: 0, // would normally pick api-key via P0 rule — header still wins
			flags: SelectFlags{
				HeaderAPIKey: "sk-ant-inline",
				AuthSlot:     config.AuthSlotSubscription, // would route to subscription without the header
			},
			want: expect{
				slot:      config.AuthSlotAPIKey,
				secret:    "sk-ant-inline",
				ephemeral: true,
			},
		},
		{
			name:     "rule 2: -H x-anthropic-token wins over --auth=api-key flag",
			cfg:      configuredAuth(),
			priority: 3,
			flags: SelectFlags{
				HeaderToken: "sk-ant-oauth-inline",
				AuthSlot:    config.AuthSlotAPIKey, // would route to api-key without the header
			},
			want: expect{
				slot:      config.AuthSlotSubscription,
				secret:    "sk-ant-oauth-inline",
				ephemeral: true,
			},
		},
		{
			name:     "rule 2 beats rule 1 when both headers present — token header sorts first? no, api-key wins (rule 1)",
			cfg:      configuredAuth(),
			priority: 2,
			flags: SelectFlags{
				HeaderAPIKey: "sk-ant-inline-key",
				HeaderToken:  "sk-ant-inline-tok",
			},
			want: expect{
				slot:      config.AuthSlotAPIKey,
				secret:    "sk-ant-inline-key",
				ephemeral: true,
			},
		},
		{
			name:     "rule 3: --turbo resolves to configured api-key",
			cfg:      configuredAuth(),
			priority: 2,
			flags:    SelectFlags{Turbo: true},
			want: expect{
				slot:   config.AuthSlotAPIKey,
				secret: "sk-ant-api-yyy",
			},
		},
		{
			name:     "rule 3: --auth=api-key resolves to configured api-key (equivalent to --turbo)",
			cfg:      configuredAuth(),
			priority: 2,
			flags:    SelectFlags{AuthSlot: config.AuthSlotAPIKey},
			want: expect{
				slot:   config.AuthSlotAPIKey,
				secret: "sk-ant-api-yyy",
			},
		},
		{
			name:     "rule 4: --auth=subscription resolves to configured subscription",
			cfg:      configuredAuth(),
			priority: 2,
			flags:    SelectFlags{AuthSlot: config.AuthSlotSubscription},
			want: expect{
				slot:   config.AuthSlotSubscription,
				secret: "sk-ant-sub-xxx",
			},
		},
		{
			name:     "rule 5: priority-0 with no flags resolves to api-key (hardcoded P0 rule)",
			cfg:      configuredAuth(),
			priority: 0,
			flags:    SelectFlags{},
			want: expect{
				slot:   config.AuthSlotAPIKey,
				secret: "sk-ant-api-yyy",
			},
		},
		{
			name:     "rule 6: default slot wins when no flags and non-P0 bead (default=subscription)",
			cfg:      configuredAuth(),
			priority: 3,
			flags:    SelectFlags{},
			want: expect{
				slot:   config.AuthSlotSubscription,
				secret: "sk-ant-sub-xxx",
			},
		},
		{
			name: "rule 6: default=api-key resolves to configured api-key",
			cfg: func() *config.AuthConfig {
				c := configuredAuth()
				c.Default = config.AuthSlotAPIKey
				return c
			}(),
			priority: 3,
			flags:    SelectFlags{},
			want: expect{
				slot:   config.AuthSlotAPIKey,
				secret: "sk-ant-api-yyy",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectAuth(tc.cfg, tc.priority, tc.flags)
			if err != nil {
				t.Fatalf("SelectAuth err = %v, want nil", err)
			}
			if got.Active == nil {
				t.Fatalf("Active is nil")
			}
			if got.Active.Slot != tc.want.slot {
				t.Errorf("Active.Slot = %q, want %q", got.Active.Slot, tc.want.slot)
			}
			if got.Active.Secret != tc.want.secret {
				t.Errorf("Active.Secret = %q, want %q", got.Active.Secret, tc.want.secret)
			}
			if got.Ephemeral != tc.want.ephemeral {
				t.Errorf("Ephemeral = %v, want %v", got.Ephemeral, tc.want.ephemeral)
			}
			// APIKey fallback should always be populated regardless of which slot is Active.
			// This is load-bearing for the 429 auto-promote path.
			if got.APIKey == nil || got.APIKey.Secret != "sk-ant-api-yyy" {
				t.Errorf("APIKey fallback not populated: %+v", got.APIKey)
			}
			if got.AutoPromoteOn429 != true {
				t.Errorf("AutoPromoteOn429 = %v, want true", got.AutoPromoteOn429)
			}
		})
	}
}

// TestSelectAuth_UnconfiguredSlot covers every branch that can fail because
// the selected slot isn't configured. Each case checks that the error names
// the slot and references `spire config auth set …` so operators know how
// to fix the problem without digging into source.
func TestSelectAuth_UnconfiguredSlot(t *testing.T) {
	cases := []struct {
		name     string
		cfg      *config.AuthConfig
		priority int
		flags    SelectFlags
		wantInMsg []string
	}{
		{
			name: "rule 3: --turbo with api-key unconfigured",
			cfg: &config.AuthConfig{
				AutoPromoteOn429: true,
				Subscription:     &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "t"},
			},
			priority: 2,
			flags:    SelectFlags{Turbo: true},
			wantInMsg: []string{"api-key", "--turbo", "spire config auth set api-key"},
		},
		{
			name: "rule 3: --auth=api-key with api-key unconfigured",
			cfg: &config.AuthConfig{
				AutoPromoteOn429: true,
				Subscription:     &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "t"},
			},
			priority: 2,
			flags:    SelectFlags{AuthSlot: config.AuthSlotAPIKey},
			wantInMsg: []string{"api-key", "--auth=api-key", "spire config auth set api-key"},
		},
		{
			name: "rule 4: --auth=subscription with subscription unconfigured",
			cfg: &config.AuthConfig{
				AutoPromoteOn429: true,
				APIKey:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "k"},
			},
			priority: 2,
			flags:    SelectFlags{AuthSlot: config.AuthSlotSubscription},
			wantInMsg: []string{"subscription", "--auth=subscription", "spire config auth set subscription"},
		},
		{
			name: "rule 5: priority-0 with api-key unconfigured — error must mention P0 rule",
			cfg: &config.AuthConfig{
				AutoPromoteOn429: true,
				Subscription:     &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "t"},
			},
			priority:  0,
			flags:     SelectFlags{},
			wantInMsg: []string{"api-key", "priority-0", "P0", "spire config auth set api-key"},
		},
		{
			name: "rule 6: default=subscription unconfigured",
			cfg: &config.AuthConfig{
				Default:          config.AuthSlotSubscription,
				AutoPromoteOn429: true,
			},
			priority:  3,
			flags:     SelectFlags{},
			wantInMsg: []string{"subscription", "[auth] default", "spire config auth set subscription"},
		},
		{
			name: "rule 6: default=api-key unconfigured",
			cfg: &config.AuthConfig{
				Default:          config.AuthSlotAPIKey,
				AutoPromoteOn429: true,
			},
			priority:  3,
			flags:     SelectFlags{},
			wantInMsg: []string{"api-key", "[auth] default", "spire config auth set api-key"},
		},
		{
			name: "rule 6: default empty — guidance to set default or pass flag",
			cfg: &config.AuthConfig{
				AutoPromoteOn429: true,
			},
			priority:  3,
			flags:     SelectFlags{},
			wantInMsg: []string{"no auth slot selected", "spire config auth default", "--auth"},
		},
		{
			name: "rule 6: default is a bogus value",
			cfg: &config.AuthConfig{
				Default:          "turbo-mode",
				AutoPromoteOn429: true,
			},
			priority:  3,
			flags:     SelectFlags{},
			wantInMsg: []string{"turbo-mode", "subscription", "api-key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectAuth(tc.cfg, tc.priority, tc.flags)
			if err == nil {
				t.Fatalf("SelectAuth err = nil, ctx = %+v", got)
			}
			for _, want := range tc.wantInMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q:\n%s", want, err.Error())
				}
			}
		})
	}
}

// TestSelectAuth_FlagConflicts covers mutually-exclusive flag combinations.
// --turbo is an alias for --auth=api-key, so any --auth value that isn't
// api-key combined with --turbo is a user error (the operator is telling us
// two different things).
func TestSelectAuth_FlagConflicts(t *testing.T) {
	cfg := configuredAuth()
	cases := []struct {
		name      string
		flags     SelectFlags
		wantInMsg []string
	}{
		{
			name:      "--turbo with --auth=subscription",
			flags:     SelectFlags{Turbo: true, AuthSlot: config.AuthSlotSubscription},
			wantInMsg: []string{"--turbo", "--auth=subscription"},
		},
		{
			name:      "--auth with garbage value",
			flags:     SelectFlags{AuthSlot: "enterprise"},
			wantInMsg: []string{"--auth=", "subscription", "api-key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SelectAuth(cfg, 2, tc.flags)
			if err == nil {
				t.Fatalf("SelectAuth err = nil, want flag-conflict error")
			}
			for _, want := range tc.wantInMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q:\n%s", want, err.Error())
				}
			}
		})
	}
}

// TestSelectAuth_TurboEquivalentToAuthAPIKey verifies that --turbo and
// --auth=api-key produce the exact same AuthContext. If future maintenance
// drifts one path, this test fails loudly.
func TestSelectAuth_TurboEquivalentToAuthAPIKey(t *testing.T) {
	cfg := configuredAuth()
	turbo, err := SelectAuth(cfg, 2, SelectFlags{Turbo: true})
	if err != nil {
		t.Fatalf("turbo SelectAuth err = %v", err)
	}
	flag, err := SelectAuth(cfg, 2, SelectFlags{AuthSlot: config.AuthSlotAPIKey})
	if err != nil {
		t.Fatalf("--auth=api-key SelectAuth err = %v", err)
	}
	if turbo.Active.Slot != flag.Active.Slot ||
		turbo.Active.Secret != flag.Active.Secret ||
		turbo.Ephemeral != flag.Ephemeral ||
		turbo.AutoPromoteOn429 != flag.AutoPromoteOn429 {
		t.Errorf("--turbo and --auth=api-key produced different contexts:\n  turbo=%+v\n  flag=%+v", turbo, flag)
	}
}

// TestSelectAuth_HeaderBeatsLaterRules is the rule-ordering canary. A
// header must override the priority-0 P0 rule, the default slot, and the
// --auth flag — every rule that fires later than 1/2. ValidateFlags
// still runs first (a truly conflicting flag combination like
// `--turbo --auth=subscription` is still a user error), so this test
// uses consistent flags.
func TestSelectAuth_HeaderBeatsLaterRules(t *testing.T) {
	cfg := configuredAuth()
	got, err := SelectAuth(cfg, 0, SelectFlags{
		HeaderAPIKey: "sk-ant-inline",
		AuthSlot:     config.AuthSlotAPIKey, // consistent with the header — just proves header is the chosen secret
	})
	if err != nil {
		t.Fatalf("SelectAuth err = %v", err)
	}
	if got.Active.Secret != "sk-ant-inline" {
		t.Errorf("header did not win: got secret %q (want sk-ant-inline)", got.Active.Secret)
	}
	if !got.Ephemeral {
		t.Error("header-supplied context must be Ephemeral=true")
	}
}

// TestSelectAuth_NilConfigErrors ensures the nil-config case surfaces a
// clear error rather than panicking. The caller (summon) should always
// pass a real AuthConfig, but we defend the contract.
func TestSelectAuth_NilConfigErrors(t *testing.T) {
	_, err := SelectAuth(nil, 2, SelectFlags{Turbo: true})
	if err == nil || !strings.Contains(err.Error(), "nil AuthConfig") {
		t.Fatalf("SelectAuth(nil, ...) err = %v, want nil-config error", err)
	}
}

// TestParseSummonHeaders_HappyPath covers the two supported header names
// in both canonical and upper-case forms (HTTP header names are
// case-insensitive; operators shouldn't have to match case exactly).
func TestParseSummonHeaders_HappyPath(t *testing.T) {
	got, err := ParseSummonHeaders([]string{
		"x-anthropic-api-key: sk-ant-api",
		"X-Anthropic-Token: sk-ant-tok",
	})
	if err != nil {
		t.Fatalf("ParseSummonHeaders err = %v", err)
	}
	if got.HeaderAPIKey != "sk-ant-api" {
		t.Errorf("HeaderAPIKey = %q, want sk-ant-api", got.HeaderAPIKey)
	}
	if got.HeaderToken != "sk-ant-tok" {
		t.Errorf("HeaderToken = %q, want sk-ant-tok", got.HeaderToken)
	}
}

// TestParseSummonHeaders_UnsupportedRejected is the critical guard: any
// other header name (typos, unknown Anthropic headers, arbitrary junk)
// must be rejected with a clear error rather than silently passing
// through. Silent passthrough was explicitly called out in the epic as
// something we do NOT want.
func TestParseSummonHeaders_UnsupportedRejected(t *testing.T) {
	cases := []string{
		"authorization: Bearer foo",       // common HTTP auth header
		"anthropic-api-key: sk-ant-typo",  // missing x- prefix
		"x-anthropic-apikey: sk-ant-typo", // collapsed hyphen
		"user-agent: curl/8",              // arbitrary
	}
	for _, h := range cases {
		t.Run(h, func(t *testing.T) {
			_, err := ParseSummonHeaders([]string{h})
			if err == nil {
				t.Fatalf("ParseSummonHeaders(%q) err = nil, want rejection", h)
			}
			if !strings.Contains(err.Error(), "supported") {
				t.Errorf("rejection error missing 'supported' hint:\n%s", err.Error())
			}
		})
	}
}

// TestParseSummonHeaders_MalformedRejected checks that headers missing the
// colon separator (the classic user error) are rejected with a clear
// message rather than silently swallowed.
func TestParseSummonHeaders_MalformedRejected(t *testing.T) {
	cases := []string{
		"x-anthropic-api-key sk-ant-missing-colon",
		": value-without-name",
		"",
	}
	for _, h := range cases {
		t.Run(h, func(t *testing.T) {
			_, err := ParseSummonHeaders([]string{h})
			if err == nil {
				t.Fatalf("ParseSummonHeaders(%q) err = nil, want malformed error", h)
			}
		})
	}
}

// TestParseSummonHeaders_LastWins mirrors curl's semantics for repeated
// -H: the last value for a given header name wins. Useful for scripted
// invocations that template-splice a header default and then override it.
func TestParseSummonHeaders_LastWins(t *testing.T) {
	got, err := ParseSummonHeaders([]string{
		"x-anthropic-api-key: first",
		"x-anthropic-api-key: second",
	})
	if err != nil {
		t.Fatalf("ParseSummonHeaders err = %v", err)
	}
	if got.HeaderAPIKey != "second" {
		t.Errorf("HeaderAPIKey = %q, want second (last-wins)", got.HeaderAPIKey)
	}
}
