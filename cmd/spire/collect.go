package main

import (
	"fmt"
	"strings"
)

func cmdCollect(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

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

	var messages []Bead
	err := bdJSON(&messages, "list", "--rig=spi", "--label", fmt.Sprintf("msg,to:%s", name), "--status=open")
	if err != nil {
		return fmt.Errorf("collect: %w", err)
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
