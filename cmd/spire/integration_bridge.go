// integration_bridge.go wires pkg/integration callbacks and provides thin CLI
// adapters for commands that delegate to the package.
package main

import (
	"fmt"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/integration"
	"github.com/awell-health/spire/pkg/steward"
)

func init() {
	// Wire store callbacks
	integration.StoreListBeads = storeListBeads
	integration.StoreGetBead = storeGetBead
	integration.StoreCreateBead = storeCreateBead
	integration.StoreUpdateBead = storeUpdateBead
	integration.StoreCloseBead = storeCloseBead
	integration.StoreAddLabel = storeAddLabel
	integration.StoreAddComment = storeAddComment
	integration.StoreGetConfig = storeGetConfig
	integration.StoreSetConfig = storeSetConfig
	integration.StoreDeleteConfig = storeDeleteConfig
	integration.StoreGetActiveAttempt = storeGetActiveAttempt
	integration.StoreEnsure = ensureStore

	// Wire keychain callbacks
	integration.KeychainGet = keychainGet
	integration.KeychainSet = keychainSet
	integration.KeychainDelete = keychainDelete

	// Wire dolt SQL callback
	integration.DoltSQL = doltSQL

	// Wire send callback
	integration.CmdSendFunc = cmdSend

	// Wire requireDolt callback
	integration.RequireDoltFunc = requireDolt
}

// --- Thin CLI adapters ---

func cmdConnect(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire connect <service>\n\nAvailable services:\n  linear    Connect to Linear for epic sync and webhooks")
	}

	switch args[0] {
	case "linear":
		return integration.ConnectLinear()
	default:
		return fmt.Errorf("unknown service: %q\n\nAvailable services:\n  linear", args[0])
	}
}

func cmdDisconnect(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire disconnect <service>")
	}

	switch args[0] {
	case "linear":
		return integration.DisconnectLinear()
	default:
		return fmt.Errorf("unknown service: %q", args[0])
	}
}

func cmdServe(args []string) error {
	port := "8080"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 >= len(args) {
				return fmt.Errorf("--port requires a value")
			}
			i++
			port = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire serve [--port 8080]", args[i])
		}
	}

	return integration.ServeWebhooks(port)
}

// --- doltSQL wrapper (was in webhook.go, needs daemonDB + detectDBName from cmd/spire) ---

// doltSQL runs a SQL query against the Dolt server and returns the output.
// Delegates to pkg/dolt.SQL with the ambient daemonDB and detectDBName fallback.
func doltSQL(query string, jsonOutput bool) (string, error) {
	db := steward.DaemonDB
	if db == "" {
		db = daemonDB
	}
	return dolt.SQL(query, jsonOutput, db, detectDBName)
}

// --- Delegation wrappers for daemon.go ---

// syncEpicsToLinear delegates to pkg/integration.
func syncEpicsToLinear() int {
	return integration.SyncEpicsToLinear()
}

// processWebhookQueue delegates to pkg/integration.
func processWebhookQueue() (int, int) {
	return integration.ProcessWebhookQueue()
}

// processWebhookEvent delegates to pkg/integration.
func processWebhookEvent(eventBead Bead) error {
	return integration.ProcessWebhookEvent(eventBead)
}
