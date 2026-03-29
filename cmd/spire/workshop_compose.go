package main

import (
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/awell-health/spire/pkg/workshop"
)

func cmdWorkshopCompose(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("compose requires an interactive terminal")
	}

	var name string
	if len(args) > 0 {
		name = args[0]
	} else {
		fmt.Print("Formula name: ")
		fmt.Scanln(&name)
		if name == "" {
			return fmt.Errorf("formula name is required")
		}
	}

	_, _, err := workshop.ComposeInteractive(name, os.Stdin, os.Stdout)
	return err
}
