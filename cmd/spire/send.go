package main

import (
	"fmt"
	"os"
	"strconv"
)

func cmdSend(args []string) error {
	asFlag, args := parseAsFlag(args)

	// Parse flags
	var ref, thread string
	priority := 3
	remaining := []string{}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--ref":
			if i+1 >= len(args) {
				return fmt.Errorf("--ref requires a value")
			}
			i++
			ref = args[i]
		case "--thread":
			if i+1 >= len(args) {
				return fmt.Errorf("--thread requires a value")
			}
			i++
			thread = args[i]
		case "--priority", "-p":
			if i+1 >= len(args) {
				return fmt.Errorf("--priority requires a value")
			}
			i++
			p, err := strconv.Atoi(args[i])
			if err != nil || p < 0 || p > 4 {
				return fmt.Errorf("--priority must be 0-4")
			}
			priority = p
		default:
			remaining = append(remaining, args[i])
		}
	}

	if len(remaining) < 2 {
		return fmt.Errorf("usage: spire send <to> <message> [--ref <id>] [--thread <id>] [--priority <0-4>]")
	}

	to := remaining[0]
	message := remaining[1]

	from, err := detectIdentity(asFlag)
	if err != nil {
		return err
	}

	// Warn if recipient is not registered (but still send)
	existingID, _ := findAgentBead(to)
	if existingID == "" {
		fmt.Fprintf(os.Stderr, "spire: warning: no registered agent %q (message created anyway)\n", to)
	}

	// Build labels
	labels := fmt.Sprintf("msg,to:%s,from:%s", to, from)
	if ref != "" {
		labels += fmt.Sprintf(",ref:%s", ref)
	}

	// Build bd create args
	bdArgs := []string{
		"create",
		"--rig=spi",
		"--type=task",
		"-p", strconv.Itoa(priority),
		"--title", message,
		"--labels", labels,
	}

	if thread != "" {
		bdArgs = append(bdArgs, "--parent", thread)
	}

	id, err := bdSilent(bdArgs...)
	if err != nil {
		return fmt.Errorf("send to %s: %w", to, err)
	}

	fmt.Println(id)
	return nil
}
