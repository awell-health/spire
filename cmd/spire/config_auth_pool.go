package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/auth/pool"
	"github.com/awell-health/spire/pkg/config"
)

// authProbeHTTPClient is the HTTP client used by `spire config auth probe`.
// Tests swap it for a transport that returns canned responses without
// hitting the live Anthropic API.
var authProbeHTTPClient = &http.Client{Timeout: 10 * time.Second}

// authProbeEndpoint is the URL probed for each slot. Overridable in tests.
// We use `count_tokens` because it accepts the same auth headers as
// `/v1/messages` and returns the same `rate_limit_event` signal in the
// response body, but does no completion work and is the cheapest route
// the API exposes for a liveness/rate-limit check.
var authProbeEndpoint = "https://api.anthropic.com/v1/messages/count_tokens"

// authPoolDirs returns the per-tower auth.toml and slot-state-cache
// directories for the currently selected tower. The auth pool config and
// state cache live under the tower's data directory (XDG_DATA_HOME-based,
// the same root used by OLAPPath); slot state JSONs go in an `auth-state/`
// subdirectory so they don't collide with future per-tower data files.
func authPoolDirs() (towerDir, stateDir string, err error) {
	tc, err := config.ResolveTowerConfig()
	if err != nil {
		return "", "", fmt.Errorf("resolve tower: %w", err)
	}
	slug := authTowerSlug(tc.Name)
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, hErr := os.UserHomeDir()
		if hErr != nil {
			return "", "", fmt.Errorf("home dir: %w", hErr)
		}
		base = filepath.Join(home, ".local", "share")
	}
	towerDir = filepath.Join(base, "spire", slug)
	stateDir = filepath.Join(towerDir, "auth-state")
	return towerDir, stateDir, nil
}

// authTowerSlug mirrors TowerConfig.OLAPPath's slug rule so the auth.toml
// and slot-state cache land in the same per-tower directory used for
// other tower-scoped data.
var authSlugSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

func authTowerSlug(name string) string {
	slug := strings.ToLower(authSlugSanitizer.ReplaceAllString(name, "-"))
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "default"
	}
	return slug
}

// poolUsage is the help text printed when `pool` is invoked without a
// recognized subverb.
func poolUsage() string {
	return "usage: spire config auth pool <add|remove|set> ...\n" +
		"  spire config auth pool add <subscription|api-key> <name> --max-concurrent N (--token-stdin|--key-stdin)\n" +
		"  spire config auth pool remove <subscription|api-key> <name>\n" +
		"  spire config auth pool set <name> --max-concurrent N"
}

// cmdConfigAuthPool dispatches `spire config auth pool <verb> ...`.
func cmdConfigAuthPool(args []string) error {
	if len(args) == 0 {
		return errors.New(poolUsage())
	}
	switch args[0] {
	case "add":
		return cmdConfigAuthPoolAdd(args[1:])
	case "remove":
		return cmdConfigAuthPoolRemove(args[1:])
	case "set":
		return cmdConfigAuthPoolSet(args[1:])
	default:
		return fmt.Errorf("unknown pool subcommand: %q\n%s", args[0], poolUsage())
	}
}

// loadOrEmptyConfig wraps pool.LoadConfig so a missing auth.toml (with no
// legacy credentials.toml in the same dir) is treated as a fresh, empty
// Config rather than an error — `pool add` is the natural way to create
// the file in the first place.
func loadOrEmptyConfig(towerDir string) (*pool.Config, error) {
	cfg, err := pool.LoadConfig(towerDir)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return &pool.Config{}, nil
	}
	return nil, err
}

// cmdConfigAuthPoolAdd: spire config auth pool add <subscription|api-key>
// <name> --max-concurrent N (--token-stdin|--key-stdin)
//
// The secret is read from stdin only — accepting it on argv would expose
// it in shell history.
func cmdConfigAuthPoolAdd(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: spire config auth pool add <subscription|api-key> <name> --max-concurrent N (--token-stdin|--key-stdin)")
	}
	kind, name := args[0], args[1]
	if kind != pool.PoolNameSubscription && kind != pool.PoolNameAPIKey {
		return fmt.Errorf("unknown pool kind: %q (must be %q or %q)",
			kind, pool.PoolNameSubscription, pool.PoolNameAPIKey)
	}
	if name == "" {
		return errors.New("slot name is required")
	}

	var maxConcurrent int
	var tokenStdin, keyStdin bool
	rest := args[2:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch a {
		case "--max-concurrent":
			if i+1 >= len(rest) {
				return errors.New("--max-concurrent requires a value")
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil {
				return fmt.Errorf("--max-concurrent: %w", err)
			}
			maxConcurrent = n
			i++
		case "--token-stdin":
			tokenStdin = true
		case "--key-stdin":
			keyStdin = true
		default:
			return fmt.Errorf("unknown flag: %q", a)
		}
	}
	if maxConcurrent < 1 {
		return errors.New("--max-concurrent must be >= 1")
	}

	switch kind {
	case pool.PoolNameSubscription:
		if keyStdin {
			return errors.New("subscription pool uses --token-stdin (got --key-stdin)")
		}
		if !tokenStdin {
			return errors.New("subscription pool requires --token-stdin (secret must come from stdin, not argv)")
		}
	case pool.PoolNameAPIKey:
		if tokenStdin {
			return errors.New("api-key pool uses --key-stdin (got --token-stdin)")
		}
		if !keyStdin {
			return errors.New("api-key pool requires --key-stdin (secret must come from stdin, not argv)")
		}
	}

	secret, err := readPoolSecretFromStdin()
	if err != nil {
		return err
	}

	towerDir, _, err := authPoolDirs()
	if err != nil {
		return err
	}
	cfg, err := loadOrEmptyConfig(towerDir)
	if err != nil {
		return fmt.Errorf("load auth pool config: %w", err)
	}

	switch kind {
	case pool.PoolNameSubscription:
		for _, s := range cfg.Subscription {
			if s.Name == name {
				return fmt.Errorf("subscription pool already has slot %q", name)
			}
		}
		cfg.Subscription = append(cfg.Subscription, pool.SlotConfig{
			Name:          name,
			Token:         secret,
			MaxConcurrent: maxConcurrent,
		})
	case pool.PoolNameAPIKey:
		for _, s := range cfg.APIKey {
			if s.Name == name {
				return fmt.Errorf("api-key pool already has slot %q", name)
			}
		}
		cfg.APIKey = append(cfg.APIKey, pool.SlotConfig{
			Name:          name,
			Key:           secret,
			MaxConcurrent: maxConcurrent,
		})
	}
	if cfg.DefaultPool == "" {
		cfg.DefaultPool = kind
	}

	if err := pool.WriteConfig(towerDir, cfg); err != nil {
		return fmt.Errorf("write auth pool config: %w", err)
	}
	fmt.Fprintf(authStdoutWriter, "added %s slot %q (max_concurrent=%d, secret=%s)\n",
		kind, name, maxConcurrent, config.MaskSecret(secret))
	return nil
}

// readPoolSecretFromStdin drains stdin, trims a single trailing newline, and
// rejects an empty result. Mirrors readSecretFlag's stdin handling.
func readPoolSecretFromStdin() (string, error) {
	data, err := io.ReadAll(authStdinReader)
	if err != nil {
		return "", fmt.Errorf("read secret from stdin: %w", err)
	}
	s := strings.TrimRight(string(data), "\r\n")
	if s == "" {
		return "", errors.New("secret from stdin is empty")
	}
	return s, nil
}

// cmdConfigAuthPoolRemove: spire config auth pool remove
// <subscription|api-key> <name>
func cmdConfigAuthPoolRemove(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: spire config auth pool remove <subscription|api-key> <name>")
	}
	kind, name := args[0], args[1]
	if kind != pool.PoolNameSubscription && kind != pool.PoolNameAPIKey {
		return fmt.Errorf("unknown pool kind: %q (must be %q or %q)",
			kind, pool.PoolNameSubscription, pool.PoolNameAPIKey)
	}

	towerDir, _, err := authPoolDirs()
	if err != nil {
		return err
	}
	cfg, err := pool.LoadConfig(towerDir)
	if err != nil {
		return fmt.Errorf("load auth pool config: %w", err)
	}

	switch kind {
	case pool.PoolNameSubscription:
		idx := indexBySlotName(cfg.Subscription, name)
		if idx < 0 {
			return fmt.Errorf("subscription pool has no slot %q", name)
		}
		cfg.Subscription = append(cfg.Subscription[:idx], cfg.Subscription[idx+1:]...)
	case pool.PoolNameAPIKey:
		idx := indexBySlotName(cfg.APIKey, name)
		if idx < 0 {
			return fmt.Errorf("api-key pool has no slot %q", name)
		}
		cfg.APIKey = append(cfg.APIKey[:idx], cfg.APIKey[idx+1:]...)
	}

	// If we just emptied the default pool but the other pool has slots,
	// switch DefaultPool so the resulting config validates. If both pools
	// are now empty, clear DefaultPool — validation requires the named
	// pool to have slots, and the operator can re-add later.
	cfg.DefaultPool = repointDefaultPool(cfg)
	if cfg.FallbackPool != "" && !poolHasSlots(cfg, cfg.FallbackPool) {
		cfg.FallbackPool = ""
	}

	if err := pool.WriteConfig(towerDir, cfg); err != nil {
		return fmt.Errorf("write auth pool config: %w", err)
	}
	fmt.Fprintf(authStdoutWriter, "removed %s slot %q\n", kind, name)
	return nil
}

func indexBySlotName(slots []pool.SlotConfig, name string) int {
	for i, s := range slots {
		if s.Name == name {
			return i
		}
	}
	return -1
}

func poolHasSlots(cfg *pool.Config, kind string) bool {
	switch kind {
	case pool.PoolNameSubscription:
		return len(cfg.Subscription) > 0
	case pool.PoolNameAPIKey:
		return len(cfg.APIKey) > 0
	}
	return false
}

// repointDefaultPool keeps the existing DefaultPool when it still has
// slots, otherwise falls back to whichever pool does. Returns "" when
// neither pool has slots so writes don't fail validation.
func repointDefaultPool(cfg *pool.Config) string {
	if cfg.DefaultPool != "" && poolHasSlots(cfg, cfg.DefaultPool) {
		return cfg.DefaultPool
	}
	if len(cfg.Subscription) > 0 {
		return pool.PoolNameSubscription
	}
	if len(cfg.APIKey) > 0 {
		return pool.PoolNameAPIKey
	}
	return ""
}

// cmdConfigAuthPoolSet: spire config auth pool set <name> --max-concurrent N
//
// The slot is searched in both pools by name; the design allows the same
// name in both subscription and api-key pools, so we update every match.
// In practice, name collisions across pools are rare; if they happen, the
// operator's intent is "update both", not "ambiguous".
func cmdConfigAuthPoolSet(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: spire config auth pool set <name> --max-concurrent N")
	}
	name := args[0]
	if name == "" {
		return errors.New("slot name is required")
	}

	var maxConcurrent int
	var maxSet bool
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch a {
		case "--max-concurrent":
			if i+1 >= len(rest) {
				return errors.New("--max-concurrent requires a value")
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil {
				return fmt.Errorf("--max-concurrent: %w", err)
			}
			maxConcurrent = n
			maxSet = true
			i++
		default:
			return fmt.Errorf("unknown flag: %q", a)
		}
	}
	if !maxSet {
		return errors.New("--max-concurrent is required")
	}
	if maxConcurrent < 1 {
		return errors.New("--max-concurrent must be >= 1")
	}

	towerDir, _, err := authPoolDirs()
	if err != nil {
		return err
	}
	cfg, err := pool.LoadConfig(towerDir)
	if err != nil {
		return fmt.Errorf("load auth pool config: %w", err)
	}

	updated := 0
	for i := range cfg.Subscription {
		if cfg.Subscription[i].Name == name {
			cfg.Subscription[i].MaxConcurrent = maxConcurrent
			updated++
		}
	}
	for i := range cfg.APIKey {
		if cfg.APIKey[i].Name == name {
			cfg.APIKey[i].MaxConcurrent = maxConcurrent
			updated++
		}
	}
	if updated == 0 {
		return fmt.Errorf("no slot named %q in either pool", name)
	}

	if err := pool.WriteConfig(towerDir, cfg); err != nil {
		return fmt.Errorf("write auth pool config: %w", err)
	}
	fmt.Fprintf(authStdoutWriter, "set max_concurrent=%d on %d slot(s) named %q\n",
		maxConcurrent, updated, name)
	return nil
}

// cmdConfigAuthProbe: spire config auth probe
//
// Fires one tiny `count_tokens` request per configured slot and applies
// any rate-limit signal to the slot's cached state. Per-slot failures
// are reported but don't abort the whole run; one bad slot shouldn't hide
// the status of the rest.
func cmdConfigAuthProbe(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: spire config auth probe")
	}
	towerDir, stateDir, err := authPoolDirs()
	if err != nil {
		return err
	}
	cfg, err := pool.LoadConfig(towerDir)
	if err != nil {
		return fmt.Errorf("load auth pool config: %w", err)
	}

	w := authStdoutWriter
	fmt.Fprintln(w, "Probing auth pool slots...")

	probeSlot(w, stateDir, pool.PoolNameSubscription, cfg.Subscription)
	probeSlot(w, stateDir, pool.PoolNameAPIKey, cfg.APIKey)
	return nil
}

// probeSlot iterates a single pool's slots, hits the probe endpoint, and
// reports the outcome. Continues on per-slot errors.
func probeSlot(w io.Writer, stateDir, kind string, slots []pool.SlotConfig) {
	for _, s := range slots {
		secret := s.Token
		if secret == "" {
			secret = s.Key
		}
		status, applied, err := probeOnce(stateDir, kind, s.Name, secret)
		switch {
		case err != nil:
			fmt.Fprintf(w, "  %-13s %-20s status=ERR        %v\n", kind, s.Name, err)
		case applied:
			fmt.Fprintf(w, "  %-13s %-20s status=%-3d        rate-limit signal applied\n", kind, s.Name, status)
		default:
			fmt.Fprintf(w, "  %-13s %-20s status=%-3d        no rate_limit_event in response\n", kind, s.Name, status)
		}
	}
}

// probeOnce builds one count_tokens request, applies any embedded
// rate_limit_event to the slot's cached state, and returns the HTTP
// status. The body is intentionally minimal: we only care about the
// rate-limit signal in the response, not the token count itself.
func probeOnce(stateDir, kind, slotName, secret string) (status int, applied bool, err error) {
	body := []byte(`{"model":"claude-haiku-4-5-20251001","messages":[{"role":"user","content":"."}]}`)
	req, err := http.NewRequest(http.MethodPost, authProbeEndpoint, bytes.NewReader(body))
	if err != nil {
		return 0, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	switch kind {
	case pool.PoolNameSubscription:
		req.Header.Set("Authorization", "Bearer "+secret)
	case pool.PoolNameAPIKey:
		req.Header.Set("x-api-key", secret)
	}

	resp, err := authProbeHTTPClient.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, false, fmt.Errorf("read response: %w", err)
	}

	event, ok, parseErr := pool.ParseRateLimitEvent(respBody)
	if !ok {
		// Many responses (success or error) won't be rate_limit_events;
		// no signal to apply, but the probe itself succeeded.
		return resp.StatusCode, false, nil
	}
	if parseErr != nil {
		return resp.StatusCode, false, fmt.Errorf("parse rate_limit_event: %w", parseErr)
	}

	mErr := pool.MutateSlotState(stateDir, slotName, func(state *pool.SlotState) error {
		event.ApplyTo(state, time.Now())
		return nil
	})
	if mErr != nil {
		return resp.StatusCode, false, fmt.Errorf("apply state: %w", mErr)
	}
	return resp.StatusCode, true, nil
}

// cmdConfigAuthMigrate: spire config auth migrate-from-credentials
//
// Idempotent: refuses to overwrite an existing auth.toml (prints a
// no-op message and returns nil), and exits cleanly with a friendly
// message when there's nothing to migrate. Leaves the legacy
// credentials.toml in place so the operator can verify the new file
// before deleting the old one.
func cmdConfigAuthMigrate(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: spire config auth migrate-from-credentials")
	}

	towerDir, _, err := authPoolDirs()
	if err != nil {
		return err
	}
	authPath := pool.Path(towerDir)
	if _, err := os.Stat(authPath); err == nil {
		fmt.Fprintf(authStdoutWriter, "auth.toml already exists at %s — nothing to migrate\n", authPath)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", authPath, err)
	}

	legacy, err := config.ReadAuthConfig()
	if err != nil {
		return fmt.Errorf("read legacy credentials: %w", err)
	}
	hasSub := legacy.Subscription != nil && legacy.Subscription.Secret != ""
	hasKey := legacy.APIKey != nil && legacy.APIKey.Secret != ""
	if !hasSub && !hasKey {
		fmt.Fprintln(authStdoutWriter, "no legacy credentials configured — nothing to migrate")
		return nil
	}

	cfg := &pool.Config{}
	if hasSub {
		cfg.Subscription = []pool.SlotConfig{{
			Name:          "default",
			Token:         legacy.Subscription.Secret,
			MaxConcurrent: 1,
		}}
	}
	if hasKey {
		cfg.APIKey = []pool.SlotConfig{{
			Name:          "default",
			Key:           legacy.APIKey.Secret,
			MaxConcurrent: 1,
		}}
	}
	switch {
	case legacy.Default == config.AuthSlotSubscription && hasSub:
		cfg.DefaultPool = pool.PoolNameSubscription
	case legacy.Default == config.AuthSlotAPIKey && hasKey:
		cfg.DefaultPool = pool.PoolNameAPIKey
	case hasSub:
		cfg.DefaultPool = pool.PoolNameSubscription
	default:
		cfg.DefaultPool = pool.PoolNameAPIKey
	}

	if err := pool.WriteConfig(towerDir, cfg); err != nil {
		return fmt.Errorf("write auth pool config: %w", err)
	}
	legacyPath, _ := config.AuthConfigPath()
	fmt.Fprintf(authStdoutWriter,
		"wrote %s\nlegacy credentials at %s left in place — remove manually after verifying the new config\n",
		authPath, legacyPath)
	return nil
}

// cmdConfigAuthShowPool renders the pool-aware show table when an
// auth.toml is present. Falls back to the legacy single-slot show output
// when the pool config doesn't exist (or fails to load with a benign
// missing-file error).
func cmdConfigAuthShowPool(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: spire config auth show")
	}

	towerDir, stateDir, err := authPoolDirs()
	if err != nil {
		// If we can't even resolve a tower, defer to the legacy show
		// (which uses the global config dir and works without a tower).
		return cmdConfigAuthShow(args)
	}
	cfg, err := pool.LoadConfig(towerDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cmdConfigAuthShow(args)
		}
		return fmt.Errorf("load auth pool config: %w", err)
	}

	states, err := pool.ListSlotStates(stateDir)
	if err != nil {
		// Cache failures shouldn't block the table; render with empty
		// state and continue.
		states = map[string]*pool.SlotState{}
	}

	w := authStdoutWriter
	fmt.Fprintf(w, "Auth pool (%s)\n\n", pool.Path(towerDir))

	header := fmt.Sprintf("  %-13s %-20s %-10s %-14s %s\n",
		"POOL", "SLOT", "STATUS", "RESETS-IN", "IN-FLIGHT")
	fmt.Fprint(w, header)

	now := time.Now()
	renderPoolRows(w, pool.PoolNameSubscription, cfg.Subscription, states, now)
	renderPoolRows(w, pool.PoolNameAPIKey, cfg.APIKey, states, now)

	fmt.Fprintln(w)
	if cfg.Selection != "" {
		fmt.Fprintf(w, "selection_policy = %s\n", cfg.Selection)
	} else {
		fmt.Fprintln(w, "selection_policy = (default)")
	}
	if cfg.DefaultPool == "" {
		fmt.Fprintln(w, "default_pool     = (none)")
	} else {
		fmt.Fprintf(w, "default_pool     = %s\n", cfg.DefaultPool)
	}
	if cfg.FallbackPool == "" {
		fmt.Fprintln(w, "fallback_pool    = (none)")
	} else {
		fmt.Fprintf(w, "fallback_pool    = %s\n", cfg.FallbackPool)
	}

	// Waiting-wizards heuristic: count slot state files where InFlight is
	// fully saturated (len == MaxConcurrent of the matching SlotConfig).
	// A saturated slot is the precondition for a wizard to be parked
	// waiting for release, so the count is a useful upper bound on
	// blocked dispatches without having to scan every flock holder.
	waiting := countWaitingWizards(cfg, states)
	fmt.Fprintf(w, "waiting_wizards  = %d (saturated slots)\n", waiting)
	return nil
}

// renderPoolRows writes one table row per slot. Missing state is rendered
// as a healthy zero-row so the operator sees the slot exists even before
// the first claim writes a state file.
func renderPoolRows(w io.Writer, kind string, slots []pool.SlotConfig, states map[string]*pool.SlotState, now time.Time) {
	// Stable iteration order — slots are read from TOML which preserves
	// array order, but defensively sort by name in case state-only
	// entries (orphans) get rendered too.
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].Name < slots[j].Name })
	for _, s := range slots {
		state := states[s.Name]
		status := slotStatus(state)
		resets := slotResetsIn(state, now)
		inflight := fmt.Sprintf("%d/%d", inFlightLen(state), s.MaxConcurrent)
		fmt.Fprintf(w, "  %-13s %-20s %-10s %-14s %s\n", kind, s.Name, status, resets, inflight)
	}
}

func inFlightLen(state *pool.SlotState) int {
	if state == nil {
		return 0
	}
	return len(state.InFlight)
}

// slotStatus picks a single human-readable verdict from the slot's
// rate-limit info. We treat the worse of the two windows as authoritative
// since either a five_hour or overage rejection makes the slot
// ineligible, and `allowed_warning` on either window is a softer signal.
func slotStatus(state *pool.SlotState) string {
	if state == nil {
		return "healthy"
	}
	worst := worstStatus(state.RateLimit.FiveHour.Status, state.RateLimit.Overage.Status)
	switch worst {
	case pool.RateLimitStatusRejected:
		return "limited"
	case pool.RateLimitStatusAllowedWarning:
		return "warning"
	default:
		return "healthy"
	}
}

func worstStatus(a, b pool.RateLimitStatus) pool.RateLimitStatus {
	rank := func(s pool.RateLimitStatus) int {
		switch s {
		case pool.RateLimitStatusRejected:
			return 2
		case pool.RateLimitStatusAllowedWarning:
			return 1
		default:
			return 0
		}
	}
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

// slotResetsIn returns the human-readable "resets in" value for the
// nearest non-zero ResetsAt across both rate-limit windows. We take the
// soonest reset so the operator sees the most actionable timer.
func slotResetsIn(state *pool.SlotState, now time.Time) string {
	if state == nil {
		return "—"
	}
	candidates := []time.Time{
		state.RateLimit.FiveHour.ResetsAt,
		state.RateLimit.Overage.ResetsAt,
	}
	var soonest time.Time
	for _, t := range candidates {
		if t.IsZero() {
			continue
		}
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}
	if soonest.IsZero() {
		return "—"
	}
	d := soonest.Sub(now)
	if d <= 0 {
		return "now"
	}
	return humanizeDuration(d)
}

// humanizeDuration formats a positive duration as a short, operator-
// friendly string. We round to whole seconds for sub-minute values and
// otherwise emit the largest two units (e.g. "1h23m", "5m12s").
func humanizeDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
	}
	if d < time.Hour {
		mins := int(d / time.Minute)
		secs := int((d % time.Minute) / time.Second)
		if secs == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dm%ds", mins, secs)
	}
	hours := int(d / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	if mins == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, mins)
}

// countWaitingWizards counts slots that are currently saturated (in-flight
// claims == configured MaxConcurrent). This is a proxy for "wizards
// blocked on this slot" — the actual flock holders aren't visible from
// here, but a saturated slot is the precondition for a wizard to be
// parked waiting on it.
func countWaitingWizards(cfg *pool.Config, states map[string]*pool.SlotState) int {
	n := 0
	for _, slots := range [][]pool.SlotConfig{cfg.Subscription, cfg.APIKey} {
		for _, s := range slots {
			st, ok := states[s.Name]
			if !ok {
				continue
			}
			if len(st.InFlight) >= s.MaxConcurrent {
				n++
			}
		}
	}
	return n
}

