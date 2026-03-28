// dolt_lifecycle.go provides backward-compatible wrappers delegating to pkg/dolt.
package main

import (
	"io"

	"github.com/awell-health/spire/pkg/dolt"
)

const doltRequiredVersion = dolt.RequiredVersion

// Keep the unexported constant name for internal references.
const doltBinDirName = "bin"

func doltManagedDir() string                   { return dolt.ManagedDir() }
func doltManagedBinPath() string               { return dolt.ManagedBinPath() }
func doltResolvedBinPath() string              { return dolt.ResolvedBinPath() }
func doltInstalledVersion(bin string) (string, error) { return dolt.InstalledVersion(bin) }
func doltVersionOK(bin string) bool            { return dolt.VersionOK(bin) }
func doltDownloadURL() string                  { return dolt.DownloadURL() }
func doltDownload() error                      { return dolt.Download() }
func doltExtractBinary(r io.Reader) (string, error) { return dolt.ExtractBinary(r) }
func doltEnsureBinary() (string, error)        { return dolt.EnsureBinary() }
func mustVersion(bin string) string            { return dolt.MustVersion(bin) }
