package main

import (
	"errors"

	"github.com/spf13/cobra"
)

var debugCmd = &cobra.Command{
	Use:   "debug",
	Short: "Archmage-only debugging tooling (hidden)",
	Long: `Archmage-only debugging tooling.

This is the parent for hidden debug subcommands intended for testing
spire internals (e.g. cleric recovery flows) without polluting a
production tower. Subcommands are namespaced by subsystem:

  recovery  Cleric failure-recovery test surface

These commands refuse to operate against a non-debug tower; see
requireDebugTower for the safety policy.`,
	Hidden: true,
}

var debugRecoveryCmd = &cobra.Command{
	Use:   "recovery",
	Short: "Cleric failure-recovery test surface",
	Long: `Cleric failure-recovery test surface.

Subcommands let an archmage author synthetic recovery beads, dispatch
the cleric in the foreground against them, and inspect the resulting
trace — all without touching a production tower.

  new       Author a synthetic recovery bead with controlled failure metadata
  dispatch  Run the cleric in foreground against a recovery bead
  trace     Read a completed recovery's trace (decide branch, action, verdict, learnings)`,
}

var debugRecoveryNewCmd = &cobra.Command{
	Use:   "new",
	Short: "Author a synthetic recovery bead with controlled failure metadata",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdDebugRecoveryNew(args)
	},
}

var debugRecoveryDispatchCmd = &cobra.Command{
	Use:   "dispatch",
	Short: "Run the cleric in foreground against a recovery bead",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdDebugRecoveryDispatch(args)
	},
}

var debugRecoveryTraceCmd = &cobra.Command{
	Use:   "trace",
	Short: "Read a completed recovery's trace (decide branch, action, verdict, learnings)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdDebugRecoveryTrace(args)
	},
}

func init() {
	debugRecoveryCmd.AddCommand(
		debugRecoveryNewCmd,
		debugRecoveryDispatchCmd,
		debugRecoveryTraceCmd,
	)
	debugCmd.AddCommand(debugRecoveryCmd)
}

func cmdDebugRecoveryNew(args []string) error {
	if err := requireDebugTower(); err != nil {
		return err
	}
	return errors.New("spire debug recovery new: not yet implemented")
}

func cmdDebugRecoveryDispatch(args []string) error {
	if err := requireDebugTower(); err != nil {
		return err
	}
	return errors.New("spire debug recovery dispatch: not yet implemented")
}
