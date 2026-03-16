package main

import (
	"bufio"
	_ "embed"
	"fmt"
	"os"
	"strings"
)

//go:embed spire.txt
var spireLogo string

func runWelcome() {
	fmt.Println(spireLogo)
	fmt.Println("  Welcome to Spire — a coordination hub for AI agents across repos.")
	fmt.Println()

	// Check if already initialized
	_, err := bd("config", "get", "issue-prefix")
	if err == nil {
		// Already initialized — show status
		identity, _ := detectIdentity("")
		fmt.Printf("  Hub: %s\n", identity)

		teamKey, _ := bd("config", "get", "linear.team-key")
		if teamKey != "" && !strings.Contains(teamKey, "(not set)") {
			fmt.Printf("  Linear: connected (%s)\n", strings.TrimSpace(teamKey))
		}
		fmt.Println()
		printUsage()
		return
	}

	// Not initialized — run onboarding
	fmt.Println("  It looks like you haven't set up a hub yet. Let's get started.")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	// Step 1: Initialize
	fmt.Println("  Step 1: Initialize hub")
	dirName := currentDirName()
	defaultPrefix := dirName
	if len(defaultPrefix) > 3 {
		defaultPrefix = defaultPrefix[:3]
	}
	fmt.Printf("  Pick a prefix for this repo [%s]: ", defaultPrefix)
	prefix, _ := reader.ReadString('\n')
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		prefix = defaultPrefix
	}

	_, initErr := bd("init", "--prefix", prefix)
	if initErr != nil {
		fmt.Printf("  ⚠ Init failed: %s\n", initErr)
		fmt.Println("  Try running: bd init --prefix " + prefix)
		return
	}
	fmt.Printf("  ✓ Beads initialized (prefix: %s-)\n", prefix)
	fmt.Println()

	// Write .envrc
	envrcPath := ".envrc"
	if _, err := os.Stat(envrcPath); os.IsNotExist(err) {
		os.WriteFile(envrcPath, []byte(fmt.Sprintf("export SPIRE_IDENTITY=\"%s\"\n", prefix)), 0644)
	}

	// Step 2: Remote sync
	fmt.Println("  Step 2: Remote sync (optional)")
	fmt.Println("  Set up DoltHub for cross-machine sync?")
	fmt.Print("  DoltHub database URL (enter to skip): ")
	remoteURL, _ := reader.ReadString('\n')
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL != "" {
		_, remoteErr := bd("dolt", "remote", "add", "origin", remoteURL)
		if remoteErr != nil {
			fmt.Printf("  ⚠ Remote setup failed: %s\n", remoteErr)
		} else {
			fmt.Printf("  ✓ Remote configured: %s\n", remoteURL)
		}
	} else {
		fmt.Println("  Skipped — you can add a remote later with: bd dolt remote add origin <url>")
	}
	fmt.Println()

	// Step 3: Connect Linear
	fmt.Println("  Step 3: Connect Linear (optional)")
	fmt.Print("  Connect to Linear for epic sync? [y/N] ")
	answer, _ := reader.ReadString('\n')
	if strings.HasPrefix(strings.TrimSpace(strings.ToLower(answer)), "y") {
		err := connectLinear()
		if err != nil {
			fmt.Printf("  ⚠ %s\n", err)
			fmt.Println("  You can connect later with: spire connect linear")
		}
	} else {
		fmt.Println("  Skipped — connect later with: spire connect linear")
	}
	fmt.Println()

	// Done
	fmt.Println("  ✓ Spire is ready!")
	fmt.Println()
	fmt.Println("  Next steps:")
	fmt.Printf("    spire register %s       # register as an agent\n", prefix)
	fmt.Println("    spire daemon             # start sync loop")
	fmt.Println("    spire collect            # check inbox")
	fmt.Println()
}

func currentDirName() string {
	dir, err := os.Getwd()
	if err != nil {
		return "hub"
	}
	parts := strings.Split(dir, string(os.PathSeparator))
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "hub"
}
