package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/spf13/cobra"
)

func init() {
	debugRecoveryNewCmd.Flags().String("origin", "", "Origin bead ID (parent bead OR pinned identity bead). Required.")
	debugRecoveryNewCmd.Flags().String("failure-class", "", "Simulated failure class (e.g. merge-failure). Required.")
	debugRecoveryNewCmd.Flags().String("failed-step", "", "Simulated failed-step name.")
	debugRecoveryNewCmd.Flags().String("labels", "", "Extra interrupted:* labels as comma-separated k=v pairs.")
	debugRecoveryNewCmd.Flags().Bool("wisp", false, "Treat origin as a pinned identity bead (records a synthetic:wisp label).")
}

func cmdDebugRecoveryNew(cmd *cobra.Command, _ []string) error {
	if err := requireDebugTower(); err != nil {
		return err
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	origin, _ := cmd.Flags().GetString("origin")
	class, _ := cmd.Flags().GetString("failure-class")
	failedStep, _ := cmd.Flags().GetString("failed-step")
	labelsRaw, _ := cmd.Flags().GetString("labels")
	wisp, _ := cmd.Flags().GetBool("wisp")

	if origin == "" {
		return fmt.Errorf("--origin is required")
	}
	if class == "" {
		return fmt.Errorf("--failure-class is required")
	}
	if !isKnownFailureClass(class) {
		return fmt.Errorf("--failure-class %q is not a recognized recovery.FailureClass; valid values: %s",
			class, strings.Join(knownFailureClassNames(), ", "))
	}

	extras, err := parseLabelPairs(labelsRaw)
	if err != nil {
		return err
	}

	id, err := recovery.WriteSyntheticRecovery(recovery.SyntheticRecoveryRequest{
		OriginBeadID: origin,
		FailureClass: recovery.FailureClass(class),
		FailedStep:   failedStep,
		ExtraLabels:  extras,
		Wisp:         wisp,
	})
	if err != nil {
		return err
	}

	fmt.Println(id)
	return nil
}

// parseLabelPairs parses a comma-separated k=v list into a map. Empty
// input returns (nil, nil). Malformed pairs produce an error so the user
// can fix the input rather than silently dropping the label.
func parseLabelPairs(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 || eq == len(pair)-1 {
			return nil, fmt.Errorf("invalid --labels pair %q: expected k=v", pair)
		}
		k := strings.TrimSpace(pair[:eq])
		v := strings.TrimSpace(pair[eq+1:])
		if k == "" || v == "" {
			return nil, fmt.Errorf("invalid --labels pair %q: key and value must be non-empty", pair)
		}
		out[k] = v
	}
	return out, nil
}

func knownFailureClasses() []recovery.FailureClass {
	return []recovery.FailureClass{
		recovery.FailEmptyImplement,
		recovery.FailMerge,
		recovery.FailBuild,
		recovery.FailReviewFix,
		recovery.FailRepoResolution,
		recovery.FailArbiter,
		recovery.FailStepFailure,
		recovery.FailUnknown,
	}
}

func knownFailureClassNames() []string {
	cs := knownFailureClasses()
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

func isKnownFailureClass(s string) bool {
	for _, c := range knownFailureClasses() {
		if string(c) == s {
			return true
		}
	}
	return false
}
