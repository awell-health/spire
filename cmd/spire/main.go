package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
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

Commands:
  register <name>       Register an agent
  unregister <name>     Unregister an agent
  send <to> <message>   Send a message (--ref, --thread, --priority)
  collect [name]        Check inbox for messages
  focus <bead-id>       Focus on a task (bonds workflow on first focus)
  grok <bead-id>        Focus + live Linear context (requires LINEAR_API_KEY)
  read <bead-id>        Mark a message as read
  connect <service>    Connect an integration (linear)
  disconnect <service> Disconnect an integration
  serve               Run webhook receiver (--port)
  daemon              Run sync daemon (--interval, --once)
  version               Print version
  help                  Show this help`)
}
