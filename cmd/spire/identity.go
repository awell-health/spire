// identity.go provides backward-compatible wrappers delegating to pkg/config.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/awell-health/spire/pkg/config"
)

func detectIdentity(asFlag string) (string, error) {
	return config.DetectIdentity(asFlag)
}

func parseAsFlag(args []string) (string, []string) {
	return config.ParseAsFlag(args)
}

// detectDBName delegates to pkg/config.DetectDBName.
func detectDBName() (string, error) {
	return config.DetectDBName()
}

// Bead is now defined in pkg/store — the type alias is in store_bridge.go.

// parseBead parses a bead from bd show --json output (which returns an array).
// This stays in cmd/spire because it uses the Bead type alias from store_bridge.
func parseBead(data []byte) (Bead, error) {
	var beads []Bead
	if err := json.Unmarshal(data, &beads); err != nil {
		return Bead{}, err
	}
	if len(beads) == 0 {
		return Bead{}, fmt.Errorf("no bead found")
	}
	return beads[0], nil
}
