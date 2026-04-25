package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// authStdinReader is the stdin source for `--token-stdin` / `--key-stdin`.
// Swap in tests to feed a canned secret without attaching to the real TTY.
var authStdinReader io.Reader = os.Stdin

// authStdoutWriter is the output sink for auth CLI commands. Swap in tests
// to capture human-formatted output without redirecting os.Stdout.
var authStdoutWriter io.Writer = os.Stdout

// cmdConfigAuth dispatches `spire config auth <verb> ...`. args is the
// slice after the `auth` token, e.g. ["show"] or ["set", "api-key",
// "--key", "sk-..."].
func cmdConfigAuth(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	if len(args) == 0 {
		return errors.New(authUsage())
	}

	switch args[0] {
	case "set":
		return cmdConfigAuthSet(args[1:])
	case "default":
		return cmdConfigAuthDefault(args[1:])
	case "show":
		return cmdConfigAuthShow(args[1:])
	case "remove":
		return cmdConfigAuthRemove(args[1:])
	default:
		return fmt.Errorf("unknown auth subcommand: %q\n%s", args[0], authUsage())
	}
}

func authUsage() string {
	return "usage: spire config auth <set|default|show|remove> ...\n" +
		"  spire config auth set subscription --token <t> [--token-stdin]\n" +
		"  spire config auth set api-key --key <k> [--key-stdin]\n" +
		"  spire config auth default <subscription|api-key>\n" +
		"  spire config auth show\n" +
		"  spire config auth remove <subscription|api-key>"
}

// cmdConfigAuthSet: spire config auth set <slot> [--token|--key] <v> | [--token-stdin|--key-stdin]
func cmdConfigAuthSet(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: spire config auth set <subscription|api-key> (--token <t>|--token-stdin|--key <k>|--key-stdin)")
	}
	slot := args[0]
	if slot != config.AuthSlotSubscription && slot != config.AuthSlotAPIKey {
		return fmt.Errorf("unknown auth slot: %q (must be %q or %q)",
			slot, config.AuthSlotSubscription, config.AuthSlotAPIKey)
	}

	var tokenVal, keyVal string
	var tokenStdin, keyStdin bool
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch a {
		case "--token":
			if i+1 >= len(rest) {
				return errors.New("--token requires a value")
			}
			tokenVal = rest[i+1]
			i++
		case "--token-stdin":
			tokenStdin = true
		case "--key":
			if i+1 >= len(rest) {
				return errors.New("--key requires a value")
			}
			keyVal = rest[i+1]
			i++
		case "--key-stdin":
			keyStdin = true
		default:
			return fmt.Errorf("unknown flag: %q", a)
		}
	}

	// Map slot to the correct flag pair; reject mismatched flags (e.g.
	// --key used with subscription slot) so scripts don't silently fail.
	var flagName, stdinFlag string
	var literal string
	var useStdin bool
	switch slot {
	case config.AuthSlotSubscription:
		if keyVal != "" || keyStdin {
			return fmt.Errorf("subscription slot uses --token / --token-stdin (got --key)")
		}
		flagName, stdinFlag = "--token", "--token-stdin"
		literal, useStdin = tokenVal, tokenStdin
	case config.AuthSlotAPIKey:
		if tokenVal != "" || tokenStdin {
			return fmt.Errorf("api-key slot uses --key / --key-stdin (got --token)")
		}
		flagName, stdinFlag = "--key", "--key-stdin"
		literal, useStdin = keyVal, keyStdin
	}

	secret, err := readSecretFlag(flagName, stdinFlag, literal, useStdin)
	if err != nil {
		return err
	}

	cfg, err := config.ReadAuthConfig()
	if err != nil {
		return fmt.Errorf("read auth config: %w", err)
	}
	switch slot {
	case config.AuthSlotSubscription:
		cfg.Subscription = &config.AuthCredential{Slot: slot, Secret: secret}
	case config.AuthSlotAPIKey:
		cfg.APIKey = &config.AuthCredential{Slot: slot, Secret: secret}
	}
	// First configured slot becomes the default so `spire config auth show`
	// isn't ambiguous on a fresh install.
	if cfg.Default == "" {
		cfg.Default = slot
	}
	if err := config.WriteAuthConfig(cfg); err != nil {
		return fmt.Errorf("write auth config: %w", err)
	}

	fmt.Fprintf(authStdoutWriter, "%s = %s (saved)\n", slot, config.MaskSecret(secret))
	return nil
}

// readSecretFlag resolves a secret from either a literal flag value or a
// --stdin variant. Returns an error if both or neither are set, or if the
// resolved secret is empty.
func readSecretFlag(flagName, stdinFlag, literal string, useStdin bool) (string, error) {
	if literal != "" && useStdin {
		return "", fmt.Errorf("specify only one of %s or %s", flagName, stdinFlag)
	}
	if literal == "" && !useStdin {
		return "", fmt.Errorf("%s <value> or %s is required", flagName, stdinFlag)
	}
	if useStdin {
		data, err := io.ReadAll(authStdinReader)
		if err != nil {
			return "", fmt.Errorf("read secret from stdin: %w", err)
		}
		// Trim a single trailing newline (\r\n or \n) — typical from
		// `echo value | spire ...`. Don't strip leading whitespace; a
		// secret starting with spaces is an unusual but valid value.
		s := strings.TrimRight(string(data), "\r\n")
		if s == "" {
			return "", fmt.Errorf("secret from stdin is empty")
		}
		return s, nil
	}
	return literal, nil
}

// cmdConfigAuthDefault: spire config auth default <subscription|api-key>
func cmdConfigAuthDefault(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: spire config auth default <subscription|api-key>")
	}
	slot := args[0]
	if slot != config.AuthSlotSubscription && slot != config.AuthSlotAPIKey {
		return fmt.Errorf("unknown auth slot: %q (must be %q or %q)",
			slot, config.AuthSlotSubscription, config.AuthSlotAPIKey)
	}

	cfg, err := config.ReadAuthConfig()
	if err != nil {
		return fmt.Errorf("read auth config: %w", err)
	}
	var configured bool
	switch slot {
	case config.AuthSlotSubscription:
		configured = cfg.Subscription != nil && cfg.Subscription.Secret != ""
	case config.AuthSlotAPIKey:
		configured = cfg.APIKey != nil && cfg.APIKey.Secret != ""
	}
	if !configured {
		return fmt.Errorf("slot %q not configured — set it with %q",
			slot, setHintFor(slot))
	}

	cfg.Default = slot
	if err := config.WriteAuthConfig(cfg); err != nil {
		return fmt.Errorf("write auth config: %w", err)
	}
	fmt.Fprintf(authStdoutWriter, "default = %s\n", slot)
	return nil
}

// setHintFor returns the "spire config auth set …" hint line for a slot,
// used in actionable error messages.
func setHintFor(slot string) string {
	switch slot {
	case config.AuthSlotSubscription:
		return "spire config auth set subscription --token <t>"
	case config.AuthSlotAPIKey:
		return "spire config auth set api-key --key <k>"
	}
	return "spire config auth set <slot> ..."
}

// cmdConfigAuthShow: spire config auth show
func cmdConfigAuthShow(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: spire config auth show")
	}
	cfg, err := config.ReadAuthConfig()
	if err != nil {
		return fmt.Errorf("read auth config: %w", err)
	}
	path, _ := config.AuthConfigPath()

	w := authStdoutWriter
	fmt.Fprintf(w, "Auth credentials (%s)\n", path)
	fmt.Fprintln(w)

	printSlot(w, config.AuthSlotSubscription, cfg.Subscription, cfg.Default)
	printSlot(w, config.AuthSlotAPIKey, cfg.APIKey, cfg.Default)

	fmt.Fprintln(w)
	if cfg.Default == "" {
		fmt.Fprintln(w, "default              = (none)")
	} else {
		fmt.Fprintf(w, "default              = %s\n", cfg.Default)
	}
	fmt.Fprintf(w, "auto_promote_on_429  = %s\n", onOff(cfg.AutoPromoteOn429))

	// Recent-runs footer (spi-uvqe3r): surface the auth_profile
	// observability column as a quick sanity check on what each slot
	// has been doing lately. A reader error is non-fatal so `show` stays
	// useful on towers without agent_runs populated (fresh install, no
	// bd in PATH, etc.) — we just print a short note instead.
	if err := renderRecentRunsPerSlot(w, authObsReader, 10); err != nil {
		fmt.Fprintf(w, "\n(recent runs unavailable: %v)\n", err)
	}
	return nil
}

// printSlot writes one line for a slot in the show output. `*` prefixes
// the default; masked secret is shown for configured slots; a `(not
// configured)` marker replaces the secret otherwise.
func printSlot(w io.Writer, slot string, cred *config.AuthCredential, defaultSlot string) {
	marker := "  "
	if slot == defaultSlot {
		marker = "* "
	}
	label := slot
	// Pad slot names to a consistent column width so the secret column
	// lines up regardless of which slot is longer.
	const col = 14
	if n := col - len(label); n > 0 {
		label += strings.Repeat(" ", n)
	}
	if cred != nil && cred.Secret != "" {
		fmt.Fprintf(w, "%s%s %s\n", marker, label, config.MaskSecret(cred.Secret))
	} else {
		fmt.Fprintf(w, "%s%s (not configured)\n", marker, label)
	}
}

func onOff(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

// cmdConfigAuthRemove: spire config auth remove <subscription|api-key>
func cmdConfigAuthRemove(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: spire config auth remove <subscription|api-key>")
	}
	slot := args[0]
	if slot != config.AuthSlotSubscription && slot != config.AuthSlotAPIKey {
		return fmt.Errorf("unknown auth slot: %q (must be %q or %q)",
			slot, config.AuthSlotSubscription, config.AuthSlotAPIKey)
	}

	cfg, err := config.ReadAuthConfig()
	if err != nil {
		return fmt.Errorf("read auth config: %w", err)
	}

	if cfg.Default == slot {
		other := config.AuthSlotAPIKey
		if slot == config.AuthSlotAPIKey {
			other = config.AuthSlotSubscription
		}
		return fmt.Errorf("cannot remove %q while it is the default — switch first with %q",
			slot, "spire config auth default "+other)
	}

	var wasConfigured bool
	switch slot {
	case config.AuthSlotSubscription:
		wasConfigured = cfg.Subscription != nil
		cfg.Subscription = nil
	case config.AuthSlotAPIKey:
		wasConfigured = cfg.APIKey != nil
		cfg.APIKey = nil
	}
	if !wasConfigured {
		return fmt.Errorf("slot %q not configured", slot)
	}

	if err := config.WriteAuthConfig(cfg); err != nil {
		return fmt.Errorf("write auth config: %w", err)
	}
	fmt.Fprintf(authStdoutWriter, "removed %s\n", slot)
	return nil
}
