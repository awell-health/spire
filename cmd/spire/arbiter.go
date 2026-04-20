package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

var arbiterCmd = &cobra.Command{
	Use:   "arbiter",
	Short: "Arbiter lifecycle commands (dispute resolution)",
	Long: `Arbiter-side commands.

An arbiter resolves disputes between sages and apprentices by issuing a
binding verdict. "spire arbiter decide" records that verdict on the task
bead's metadata with a source="arbiter" marker that distinguishes it from
sage verdicts. Once recorded the verdict is binding: subsequent sage
verdicts on the same review round are rejected by downstream writers.`,
}

var arbiterDecideCmd = &cobra.Command{
	Use:   "decide <bead>",
	Short: "Record a binding arbiter verdict on a bead",
	Long: `Record a binding arbiter verdict on <bead>.

The verdict is written to the task bead's metadata under the
"arbiter_verdict" key as a JSON payload that carries source="arbiter".
If the task has an active attempt bead (the current dispute round), the
attempt is closed with result "arbiter-resolved". Downstream sage verdict
writers check the arbiter marker and refuse to overwrite it.`,
	Args: cobra.ExactArgs(1),
	RunE: runArbiterDecide,
}

func init() {
	arbiterDecideCmd.Flags().String("verdict", "", "Arbiter verdict: accept | reject | custom")
	arbiterDecideCmd.Flags().String("note", "", "Optional verdict reasoning/note")
	_ = arbiterDecideCmd.MarkFlagRequired("verdict")
	arbiterCmd.AddCommand(arbiterDecideCmd)
	rootCmd.AddCommand(arbiterCmd)
}

// --- Test-replaceable seams ---

var arbiterGetBeadFunc = storeGetBead
var arbiterSetBeadMetadataFunc = store.SetBeadMetadata
var arbiterAddCommentFunc = storeAddComment
var arbiterGetActiveAttemptFunc = storeGetActiveAttempt
var arbiterCloseAttemptBeadFunc = storeCloseAttemptBead
var arbiterNowFunc = func() time.Time { return time.Now().UTC() }

// arbiterVerdictPayload is the JSON structure written to the task bead under
// the "arbiter_verdict" metadata key. The Source field is the load-bearing
// marker that distinguishes arbiter verdicts from sage verdicts; readers
// treat an arbiter-source verdict as binding and refuse to overwrite it.
type arbiterVerdictPayload struct {
	Source    string `json:"source"`
	Verdict   string `json:"verdict"`
	Note      string `json:"note,omitempty"`
	DecidedAt string `json:"decided_at"`
}

const (
	arbiterVerdictMetaKey  = "arbiter_verdict"
	arbiterVerdictSource   = "arbiter"
	arbiterAttemptResult   = "arbiter-resolved"
)

var allowedArbiterVerdicts = map[string]bool{
	"accept": true,
	"reject": true,
	"custom": true,
}

func runArbiterDecide(cmd *cobra.Command, args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	beadID := args[0]
	verdict, _ := cmd.Flags().GetString("verdict")
	note, _ := cmd.Flags().GetString("note")

	if !allowedArbiterVerdicts[verdict] {
		return fmt.Errorf("invalid verdict %q: must be one of accept, reject, custom", verdict)
	}

	if _, err := arbiterGetBeadFunc(beadID); err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	payload := arbiterVerdictPayload{
		Source:    arbiterVerdictSource,
		Verdict:   verdict,
		Note:      note,
		DecidedAt: arbiterNowFunc().Format(time.RFC3339),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal arbiter verdict: %w", err)
	}
	if err := arbiterSetBeadMetadataFunc(beadID, arbiterVerdictMetaKey, string(raw)); err != nil {
		return fmt.Errorf("write arbiter verdict metadata: %w", err)
	}

	// Close the current dispute round attempt if one is open. A failure to
	// close is surfaced to stderr but does not unwind the verdict write —
	// the verdict is already durable and binding.
	if attempt, err := arbiterGetActiveAttemptFunc(beadID); err == nil && attempt != nil {
		if cerr := arbiterCloseAttemptBeadFunc(attempt.ID, arbiterAttemptResult); cerr != nil {
			fmt.Fprintf(os.Stderr, "  (note: could not close active attempt %s: %s)\n", attempt.ID, cerr)
		}
	}

	summary := fmt.Sprintf("arbiter verdict: %s", verdict)
	if note != "" {
		summary += " — " + note
	}
	if err := arbiterAddCommentFunc(beadID, summary); err != nil {
		fmt.Fprintf(os.Stderr, "  (note: could not add arbiter comment to %s: %s)\n", beadID, err)
	}

	fmt.Printf("arbiter verdict %q recorded on %s\n", verdict, beadID)
	return nil
}
