// doltserver.go provides backward-compatible wrappers delegating to pkg/dolt.
// cmd/spire callers continue to use unexported names; the real logic lives in
// the dolt package.
package main

import (
	"context"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/process"
)

func doltDataDir() string                                  { return dolt.DataDir() }
func doltGlobalDir() string                                { return dolt.GlobalDir() }
func doltPort() string                                     { return dolt.Port() }
func doltHost() string                                     { return dolt.Host() }
func readPID(path string) int                              { return process.ReadPID(path) }
func writePID(path string, pid int) error                  { return process.WritePID(path, pid) }
func processAlive(pid int) bool                            { return process.ProcessAlive(pid) }
func doltBin() string                                      { return dolt.Bin() }
func doltPIDPath() string                                  { return dolt.DoltPIDPath() }
func daemonPIDPath() string                                { return dolt.DaemonPIDPath() }
func stewardPIDPath() string                               { return dolt.StewardPIDPath() }
func doltIsReachable() bool                                { return dolt.IsReachable() }
func requireDolt() error                                   { return dolt.RequireDolt() }
func doltServerStatus() (int, bool, bool)                  { return dolt.ServerStatus() }
// ensureDoltIdentity, doltWriteConfig, ensureDatabase removed — no callers in cmd/spire.
func doltStart() (int, error)                              { return dolt.Start() }
func doltStop() error                                      { return dolt.Stop() }
func stopProcess(pidPath string) (bool, error)             { return process.StopProcess(pidPath) }

// CLI operation wrappers — used by register_repo.go, tower.go, sync.go, etc.
// These pass context.Background() since CLI callers rely on Ctrl-C, not timeouts.
func doltCLIPush(dataDir string, force bool) error         { return dolt.CLIPush(context.Background(), dataDir, force) }
func doltCLIPull(dataDir string, force bool) error         { return dolt.CLIPull(context.Background(), dataDir, force) }
func doltCLIFetchMerge(dataDir string) (string, error)     { return dolt.CLIFetchMerge(context.Background(), dataDir) }
func setDoltCLIRemote(dataDir, name, url string)           { dolt.SetCLIRemote(dataDir, name, url) }
func ensureDoltHubDB(remoteURL string) error               { return dolt.EnsureDoltHubDB(remoteURL) }
