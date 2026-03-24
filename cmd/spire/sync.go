package main

import (
	"fmt"
	"os"
	"strings"
)

func cmdSync(args []string) error {
	merge := false

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--merge":
			merge = true
		case args[i] == "--help" || args[i] == "-h":
			fmt.Print(`Usage: spire sync --merge

Three-way merge pull for diverged histories.

Run this when 'spire pull' fails because local and remote histories have
diverged. Unlike 'spire pull --force' (which overwrites local history),
'spire sync --merge' attempts a three-way merge, preserving commits from
both sides.

If the merge produces conflicts, dolt's output is printed verbatim so you
can identify and resolve them manually.

Options:
  --merge        Required. Perform the three-way merge pull.

Auth:
  Credentials are read from spire's credential store (spire config set dolthub-user,
  dolthub-password) or from DOLT_REMOTE_USER / DOLT_REMOTE_PASSWORD env vars.

Examples:
  spire sync --merge      # three-way merge pull from existing remote
`)
			return nil
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire sync --merge", args[i])
		}
	}

	if !merge {
		fmt.Println("Usage: spire sync --merge")
		fmt.Println()
		fmt.Println("  --merge    Three-way merge pull for diverged histories")
		fmt.Println()
		fmt.Println("Run 'spire sync --help' for more information.")
		return nil
	}

	return runSync()
}

func runSync() error {
	if err := requireDolt(); err != nil {
		return err
	}

	// ── Resolve database data directory ───────────────────────────────────────
	dataDir, err := resolveDataDir()
	if err != nil {
		return err
	}

	// ── Remote setup ──────────────────────────────────────────────────────────
	out, _ := bd("dolt", "remote", "list")
	if !strings.Contains(out, "origin") {
		return fmt.Errorf("no remote configured\n  set one with 'spire pull <url>' first, or run: bd dolt remote add origin <url>")
	}
	// Sync SQL remote to CLI config in case it was set via bd but not CLI.
	if url := parseOriginURL(out); url != "" {
		setDoltCLIRemote(dataDir, "origin", url)
	}

	// ── Inject DoltHub credentials ────────────────────────────────────────────
	if user := getCredential(CredKeyDolthubUser); user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
	}
	if pass := getCredential(CredKeyDolthubPassword); pass != "" {
		os.Setenv("DOLT_REMOTE_PASSWORD", pass)
	}

	// ── Three-way merge pull via dolt CLI ─────────────────────────────────────
	// Unlike runPull, we don't intercept divergence errors — we let dolt's
	// output flow through verbatim so the user can inspect conflict details.
	fmt.Println("  Merging from origin (three-way)...")
	if err := doltCLIPull(dataDir, false); err != nil {
		fmt.Println("  Merge failed — dolt output:")
		fmt.Println()
		fmt.Println(err.Error())
		fmt.Println()
		fmt.Println("  Resolve any conflicts manually, then commit with:")
		fmt.Println("    bd vc commit -m 'resolve merge conflicts'")
		return fmt.Errorf("sync --merge failed")
	}

	fmt.Println("  Merge complete.")
	fmt.Println()
	bd("status") //nolint
	return nil
}
