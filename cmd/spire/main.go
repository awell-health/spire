package main

import (
	"fmt"
	"os"
	"strings"
)

var version = "dev"

func main() {
	// Extract global --tower flag before dispatching to subcommands.
	extractTowerFlag()

	if len(os.Args) < 2 {
		printUsage()
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
	case "config":
		err = cmdConfig(args)
	case "push":
		err = cmdPush(args)
	case "pull":
		err = cmdPull(args)
	case "sync":
		err = cmdSync(args)
	case "repo":
		err = cmdRepo(args)
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
	case "wizard-run":
		err = cmdWizardRun(args)
	case "wizard-review":
		err = cmdWizardReview(args)
	case "wizard-merge":
		err = cmdWizardMerge(args)
	case "doctor":
		err = cmdDoctor(args)
	case "version":
		fmt.Println("spire", version)
		binPath := doltResolvedBinPath()
		if binPath == "" {
			fmt.Println("dolt  not installed")
		} else {
			v, err := doltInstalledVersion(binPath)
			if err != nil {
				fmt.Printf("dolt  (unknown version) (%s)\n", binPath)
			} else {
				fmt.Printf("dolt  v%s (%s)\n", v, binPath)
			}
		}
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

// extractTowerFlag removes --tower <name> from os.Args and sets SPIRE_TOWER env.
// This allows any spire command to target a specific tower regardless of CWD or ActiveTower config.
func extractTowerFlag() {
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--tower" && i+1 < len(os.Args) {
			os.Setenv("SPIRE_TOWER", os.Args[i+1])
			os.Args = append(os.Args[:i], os.Args[i+2:]...)
			return
		}
		if strings.HasPrefix(os.Args[i], "--tower=") {
			os.Setenv("SPIRE_TOWER", strings.TrimPrefix(os.Args[i], "--tower="))
			os.Args = append(os.Args[:i], os.Args[i+1:]...)
			return
		}
	}
}

func printUsage() {
	fmt.Println(`Usage: spire <command> [args]

Global flags:
  --tower <name>        Override active tower for this command

Setup:
  tower create          Create a new tower (--name, --dolthub, --prefix)
  tower attach          Clone a tower from DoltHub (<url> [--name])
  tower list            List configured towers
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
  status                Show running state of dolt + daemon

Work:
  file <title> [flags]  Create a bead (--prefix, -t type, -p priority)
  spec <title> [flags]  Scaffold a spec and file it (--no-file, --break <id>)
  claim <bead-id>       Pull, verify, claim, push (atomic)
  focus <bead-id>       Focus on a task (bonds workflow on first focus)
  grok <bead-id>        Focus + live Linear context

Agents:
  summon [n]            Summon wizards (--for <epic-id>)
  dismiss [n]           Dismiss wizards (--all)
  roster                List agents and their status

Messaging:
  send <to> <message>   Send a message (--ref, --thread, --priority)
  collect [name]        Check inbox for messages
  read <bead-id>        Mark a message as read

Observability:
  board [flags]         Work queue view (--mine, --ready, --json)
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
  help                  Show this help`)
}
