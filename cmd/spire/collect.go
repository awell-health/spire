package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

var collectCmd = &cobra.Command{
	Use:   "collect [name]",
	Short: "Check inbox for messages (DB query)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			fullArgs = append(fullArgs, "--json")
		}
		if v, _ := cmd.Flags().GetString("as"); v != "" {
			fullArgs = append(fullArgs, "--as", v)
		}
		fullArgs = append(fullArgs, args...)
		return cmdCollect(fullArgs)
	},
}

func init() {
	collectCmd.Flags().Bool("json", false, "Output as JSON")
	collectCmd.Flags().String("as", "", "Override identity")
}

func cmdCollect(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	var jsonOut bool
	var remaining []string
	for _, arg := range args {
		if arg == "--json" {
			jsonOut = true
			continue
		}
		remaining = append(remaining, arg)
	}
	args = remaining

	asFlag, args := parseAsFlag(args)

	// Name can be positional or detected
	var name string
	if len(args) > 0 {
		name = args[0]
	} else {
		var err error
		name, err = detectIdentity(asFlag)
		if err != nil {
			return err
		}
	}

	messages, err := storeListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"msg", "to:" + name},
		Status:   statusPtr(beads.StatusOpen),
	})
	if err != nil {
		return fmt.Errorf("collect: %w", err)
	}

	if jsonOut {
		data, err := json.MarshalIndent(messages, "", "  ")
		if err != nil {
			return fmt.Errorf("collect: encode json: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	// Print agent context from registration bead if set
	if agentID, err := findAgentBead(name); err == nil && agentID != "" {
		if agentBead, err := storeGetBead(agentID); err == nil {
			if agentBead.Description != "" {
				fmt.Printf("Context: %s\n\n", agentBead.Description)
			}
		}
	}

	if len(messages) == 0 {
		fmt.Println("No messages.")
		return nil
	}

	fmt.Printf("%d message(s):\n\n", len(messages))
	for _, m := range messages {
		from := ""
		for _, l := range m.Labels {
			if strings.HasPrefix(l, "from:") {
				from = l[5:]
				break
			}
		}
		fmt.Printf("  %s  [from:%s]  %s\n", m.ID, from, m.Title)
	}
	fmt.Printf("\nRun `spire focus <id>` to focus on a message, or `spire read <id>` to mark as read.\n")
	return nil
}
