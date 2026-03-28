// steward_local.go provides backward-compatible wrappers delegating to pkg/steward.
// Business logic lives in pkg/steward.
package main

import (
	"github.com/awell-health/spire/pkg/steward"
)

// --- Type aliases so existing cmd/spire code compiles unchanged ---

type LocalStewardConfig = steward.LocalConfig

// --- Wrapper functions delegating to pkg/steward ---

func isInK8s() bool                             { return steward.IsInK8s() }
func loadLocalStewardConfig() *LocalStewardConfig { return steward.LoadLocalConfig() }
func wizardPIDPath(name string) string           { return steward.WizardPIDPath(name) }
func isWizardRunning(name string) bool           { return steward.IsWizardRunning(name) }
func recordWizardPID(name string, pid int)       { steward.RecordWizardPID(name, pid) }
func clearWizardPID(name string)                 { steward.ClearWizardPID(name) }
