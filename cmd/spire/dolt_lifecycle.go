// dolt_lifecycle.go provides backward-compatible wrappers delegating to pkg/dolt.
// Dead wrappers removed: doltManagedDir, mustVersion, doltBinDirName — no callers.
package main

import (
	"io"

	"github.com/awell-health/spire/pkg/dolt"
)

const doltRequiredVersion = dolt.RequiredVersion

func doltManagedBinPath() string                      { return dolt.ManagedBinPath() }
func doltResolvedBinPath() string                     { return dolt.ResolvedBinPath() }
func doltInstalledVersion(bin string) (string, error)  { return dolt.InstalledVersion(bin) }
func doltVersionOK(bin string) bool                   { return dolt.VersionOK(bin) }
func doltDownloadURL() string                         { return dolt.DownloadURL() }
func doltDownload() error                             { return dolt.Download() }
func doltExtractBinary(r io.Reader) (string, error)   { return dolt.ExtractBinary(r) }
func doltEnsureBinary() (string, error)               { return dolt.EnsureBinary() }
