package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		// If .beads exists, show status; otherwise run init
		if _, err := os.Stat(".beads"); err == nil {
			if err := cmdStatus([]string{}); err != nil {
				fmt.Fprintf(os.Stderr, "spire: %s\n", err)
				os.Exit(1)
			}
		} else {
			if err := cmdInit([]string{}); err != nil {
				fmt.Fprintf(os.Stderr, "spire: %s\n", err)
				os.Exit(1)
			}
		}
		return
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "register":
		err = cmdRegister(args)
	case "unregister":
		err = cmdUnregister(args)
	case "send":
		err = cmdSend(args)
	case "collect":
		err = cmdCollect(args)
	case "focus":
		err = cmdFocus(args)
	case "grok":
		err = cmdGrok(args)
	case "read":
		err = cmdRead(args)
	case "connect":
		err = cmdConnect(args)
	case "disconnect":
		err = cmdDisconnect(args)
	case "serve":
		err = cmdServe(args)
	case "daemon":
		err = cmdDaemon(args)
	case "steward":
		err = cmdSteward(args)
	case "file":
		err = cmdFile(args)
	case "spec":
		err = cmdSpec(args)
	case "claim":
		err = cmdClaim(args)
	case "init":
		err = cmdInit(args)
	case "config":
		err = cmdConfig(args)
	case "sync":
		err = cmdSync(args)
	case "push":
		err = cmdPush(args)
	case "register-repo":
		err = cmdRegisterRepo(args)
	case "repo":
		err = cmdRepo(args)
	case "worktree":
		err = cmdWorktree(args)
	case "up":
		err = cmdUp(args)
	case "down":
		err = cmdDown(args)
	case "shutdown":
		err = cmdShutdown(args)
	case "board":
		err = cmdBoard(args)
	case "roster":
		err = cmdRoster(args)
	case "summon":
		err = cmdSummon(args)
	case "dismiss":
		err = cmdDismiss(args)
	case "watch":
		err = cmdWatch(args)
	case "alert":
		err = cmdAlert(args)
	case "status":
		err = cmdStatus(args)
	case "metrics":
		err = cmdMetrics(args)
	case "tower":
		err = cmdTower(args)
	case "doctor":
		err = cmdDoctor(args)
	case "version":
		fmt.Println("spire", version)
		return
	case "help", "--help", "-h":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "spire: unknown command %q\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "spire: %s\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: spire <command> [args]

Overview:
  board [flags]         Unified work queue view (--mine, --ready, --json)

Setup:
  tower create          Create a new tower (--name, --dolthub, --prefix)
  tower attach          Clone a tower from DoltHub (<url> [--name])
  tower list            List configured towers
  init                  Initialize repo (--prefix, --hub, --standalone, --satellite=<hub>)
  config <get|set|list|repo> Read/write config values; repo prints resolved spire.yaml
  sync [--hard] [url]   Pull from a DoltHub remote (handles divergent histories)
  push [url]            Push local database to DoltHub (creates remote db if needed)
  register-repo         Register a repo under a tower (--prefix, --repo-url, --branch, --database)
  repo list             List all init'd repos (--json)
  repo remove <prefix>  Remove a repo from config (all paths)
  worktree remove       Unregister this directory only, leave other worktrees intact

Lifecycle:
  up                    Start dolt server + daemon (--interval)
  down                  Stop daemon (dolt keeps running)
  shutdown              Stop daemon + dolt server
  status                Show running state of dolt + daemon
  doctor [--fix]        Verify CLAUDE.md, hooks, SPIRE.md, skills are current
  metrics [flags]       Agent run metrics (--bead <id>, --model, --json)

Work:
  file <title> [flags]  Create a bead (--prefix required if not in a repo dir)
  spec <title> [flags]  Scaffold a spec and file it (--no-file, --break <id>)
  claim <bead-id>       Pull, verify, claim, push (atomic)
  focus <bead-id>       Focus on a task (bonds workflow on first focus)
  grok <bead-id>        Focus + live Linear context (requires LINEAR_API_KEY)

Messaging:
  register <name>       Register an agent
  unregister <name>     Unregister an agent
  send <to> <message>   Send a message (--ref, --thread, --priority)
  collect [name]        Check inbox for messages
  read <bead-id>        Mark a message as read

Integrations:
  connect <service>     Connect an integration (linear)
  disconnect <service>  Disconnect an integration
  serve                 Run webhook receiver (--port)
  daemon                Run sync daemon (--interval, --once)
  steward               Run work coordinator (--once, --dry-run, --interval, --agents)

  version               Print version
  help                  Show this help`)
}
