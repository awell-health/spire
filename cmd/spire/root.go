package main

import (
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "spire",
	Short: "Coordination hub for AI agents",
	Long: `Usage: spire <command> [args]

Global flags:
  --tower <name>        Override active tower for this command

Setup:
  tower create          Create a new tower (--name, --dolthub, --prefix)
  tower attach          Clone a tower from DoltHub (<url> [--name])
  tower list            List configured towers
  tower remove <name>   Remove a tower (--force)
  tower use <name>      Set the active tower
  repo add [path]       Register a repo under a tower (--prefix, --repo-url, --branch)
  repo list             List registered repos (--json)
  repo remove <prefix>  Remove a repo
  config <get|set|list> Read/write config values and credentials
  doctor [--fix]        Health checks and auto-repair

Sync:
  push [url]            Push local database to DoltHub
  pull [url]            Pull from DoltHub (fast-forward; --force to overwrite)
  sync --merge          Three-way merge pull for diverged histories

Lifecycle:
  up                    Start dolt server + daemon (--interval)
  down                  Stop daemon (dolt keeps running)
  shutdown              Stop daemon + dolt server
  status                Show services, agents, and work queue
  logs [name]           Tail agent/system logs (--daemon, --dolt)

Work:
  file <title> [flags]  Create a bead (--prefix, -t type, -p priority)
  design <title>        Create a design bead (brainstorm/exploration artifact)
  spec <title> [flags]  Scaffold a spec and file it (--no-file, --break <id>)
  claim <bead-id>       Pull, verify, claim, push (atomic)
  close <bead-id>       Force-close a bead (remove phase labels, close molecule steps)
  focus <bead-id>       Assemble read-only context for a task
  grok <bead-id>        Focus + live Linear context
  wizard-epic <epic-id>  Execute wizard epic orchestration

Workshop:
  workshop              Interactive formula exploration
  workshop list         List available formulas (--custom, --embedded, --json)
  workshop show <name>  Display formula with phase diagram
  workshop validate <name>  Validate formula syntax and logic
  workshop compose      Interactive formula builder
  workshop dry-run <name>   Simulate formula execution (--json, --bead <id>)
  workshop test <name>  Dry-run with full bead context (--bead <id>)
  workshop publish <name>   Copy formula to tower's .beads/formulas/
  workshop unpublish <name> Remove published formula

Agents:
  summon [n]            Summon wizards (--targets <ids>, --auto)
  resummon <bead-id>    Clear timer + needs-human, re-summon wizard
  dismiss [n]           Dismiss wizards (--all)
  roster                List work by epic and agent status

Messaging:
  send <to> <message>   Send a message (--ref, --thread, --priority)
  collect [name]        Check inbox for messages (DB query)
  inbox [name]          Read local inbox file (--check, --watch, --json)
  read <bead-id>        Mark a message as read

Observability:
  board [flags]         Interactive board TUI (--mine, --ready, --json)
  trace <bead-id>       Execution DAG timeline (--json, --follow)
  watch                 Live-updating activity view
  metrics [flags]       Agent run metrics (--bead, --model, --json)
  alert [bead-id]       Alert on bead state changes

Advanced:
  register <name>       Register an agent identity
  unregister <name>     Unregister an agent identity
  daemon                Run sync daemon (--interval, --once)
  steward               Run work coordinator (--once, --dry-run)
  serve                 Run webhook receiver (--port)
  connect <service>     Connect an integration (linear)
  disconnect <service>  Disconnect an integration

  version               Print version
  help                  Show this help`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

func init() {
	rootCmd.PersistentFlags().String("tower", "", "Override active tower for this command")
	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if t, _ := cmd.Flags().GetString("tower"); t != "" {
			os.Setenv("SPIRE_TOWER", t)
		}
		return nil
	}
}
