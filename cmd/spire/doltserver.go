// doltserver.go provides backward-compatible wrappers delegating to pkg/dolt.
// cmd/spire callers continue to use unexported names; the real logic lives in
// the dolt package.
package main

import (
	"github.com/awell-health/spire/pkg/dolt"
)

func doltDataDir() string                                  { return dolt.DataDir() }
func doltGlobalDir() string                                { return dolt.GlobalDir() }
func doltPort() string                                     { return dolt.Port() }
func doltHost() string                                     { return dolt.Host() }
func readPID(path string) int                              { return dolt.ReadPID(path) }
func writePID(path string, pid int) error                  { return dolt.WritePID(path, pid) }
func processAlive(pid int) bool                            { return dolt.ProcessAlive(pid) }
func doltBin() string                                      { return dolt.Bin() }
func doltPIDPath() string                                  { return dolt.DoltPIDPath() }
func daemonPIDPath() string                                { return dolt.DaemonPIDPath() }
func stewardPIDPath() string                               { return dolt.StewardPIDPath() }
func doltIsReachable() bool                                { return dolt.IsReachable() }
func requireDolt() error                                   { return dolt.RequireDolt() }
func doltServerStatus() (int, bool, bool)                  { return dolt.ServerStatus() }
func ensureDoltIdentity()                                  { dolt.EnsureIdentity() }
func doltWriteConfig() (string, error)                     { return dolt.WriteConfig() }
func doltStart() (int, error)                              { return dolt.Start() }
func doltStop() error                                      { return dolt.Stop() }
func ensureDatabase(name string) error                     { return dolt.EnsureDatabase(name) }
func stopProcess(pidPath string) (bool, error)             { return dolt.StopProcess(pidPath) }

// CLI operation wrappers — used by daemon.go, register_repo.go, tower.go.
func doltCLIPush(dataDir string, force bool) error         { return dolt.CLIPush(dataDir, force) }
func doltCLIPull(dataDir string, force bool) error         { return dolt.CLIPull(dataDir, force) }
func doltCLIFetchMerge(dataDir string) (string, error)     { return dolt.CLIFetchMerge(dataDir) }
func setDoltCLIRemote(dataDir, name, url string)           { dolt.SetCLIRemote(dataDir, name, url) }
func ensureDoltHubDB(remoteURL string) error               { return dolt.EnsureDoltHubDB(remoteURL) }
