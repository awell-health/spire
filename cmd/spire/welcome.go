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

	reader := bufio.NewReader(os.Stdin)

	// Step 1: Hub initialization
	prefix, _ := bd("config", "get", "issue-prefix")
	hubReady := prefix != "" && !strings.Contains(prefix, "(not set)")

	if hubReady {
		prefix = strings.TrimSpace(prefix)
		fmt.Printf("  ✓ Hub initialized (prefix: %s-)\n", prefix)
	} else {
		fmt.Println("  Step 1: Initialize hub")
		fmt.Println()
		dirName := currentDirName()
		defaultPrefix := dirName
		if len(defaultPrefix) > 3 {
			defaultPrefix = defaultPrefix[:3]
		}
		fmt.Printf("    Pick a prefix for this repo [%s]: ", defaultPrefix)
		input, _ := reader.ReadString('\n')
		prefix = strings.TrimSpace(input)
		if prefix == "" {
			prefix = defaultPrefix
		}

		_, initErr := bd("init", "--prefix", prefix)
		if initErr != nil {
			fmt.Printf("    ⚠ Init failed: %s\n", initErr)
			fmt.Println("    Try: bd init --prefix " + prefix)
			return
		}
		fmt.Printf("    ✓ Beads initialized (prefix: %s-)\n", prefix)

		// Write .envrc
		if _, err := os.Stat(".envrc"); os.IsNotExist(err) {
			os.WriteFile(".envrc", []byte(fmt.Sprintf("export SPIRE_IDENTITY=\"%s\"\n", prefix)), 0644)
		}
	}
	fmt.Println()

	// Step 2: Linear connection
	teamKey, _ := bd("config", "get", "linear.team-key")
	linearReady := teamKey != "" && !strings.Contains(teamKey, "(not set)")

	if linearReady {
		fmt.Printf("  ✓ Linear connected (team: %s)\n", strings.TrimSpace(teamKey))
	} else {
		fmt.Print("  Connect to Linear for epic sync? [y/N] ")
		answer, _ := reader.ReadString('\n')
		if strings.HasPrefix(strings.TrimSpace(strings.ToLower(answer)), "y") {
			err := connectLinear()
			if err != nil {
				fmt.Printf("    ⚠ %s\n", err)
				fmt.Println("    Run later: spire connect linear")
			}
		} else {
			fmt.Println("    Skipped — run later: spire connect linear")
		}
	}
	fmt.Println()

	// Step 3: Agent registration
	var agents []Bead
	_ = bdJSON(&agents, "list", "--rig="+prefix, "--label", "agent", "--status=open")
	registered := len(agents) > 0

	if registered {
		names := make([]string, len(agents))
		for i, a := range agents {
			name := hasLabel(a, "name:")
			if name == "" {
				name = a.Title
			}
			names[i] = name
		}
		fmt.Printf("  ✓ Agents online: %s\n", strings.Join(names, ", "))
	} else {
		fmt.Printf("  Register as an agent? This lets other agents message you. [Y/n] ")
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer == "" || strings.HasPrefix(answer, "y") {
			err := cmdRegister([]string{prefix})
			if err != nil {
				fmt.Printf("    ⚠ %s\n", err)
			} else {
				fmt.Printf("    ✓ Registered as '%s'\n", prefix)
			}
		}
	}
	fmt.Println()

	// Step 4: Try it out
	fmt.Println("  ── Try it out ──")
	fmt.Println()
	fmt.Printf("    spire send %s \"hello world\"   # send yourself a message\n", prefix)
	fmt.Println("    spire collect                   # check your inbox")
	fmt.Println("    spire focus <bead-id>           # dive into a task")
	fmt.Println()
	fmt.Println("    bd create \"My first task\" -p 2 -t task   # create work")
	fmt.Println("    bd list                                   # see all work")
	fmt.Println()

	// Step 5: What's next
	fmt.Println("  ── What's next ──")
	fmt.Println()
	if !linearReady {
		fmt.Println("    spire connect linear             # sync epics to Linear")
	}
	fmt.Println("    spire daemon                     # start background sync")
	fmt.Println("    spire serve --port 8080          # run webhook receiver")
	fmt.Println("    spire help                       # all commands")
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
