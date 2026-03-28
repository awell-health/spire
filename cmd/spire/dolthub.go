// dolthub.go provides backward-compatible wrappers delegating to pkg/dolt.
package main

import (
	"github.com/awell-health/spire/pkg/dolt"
)

func normalizeDolthubURL(url string) string { return dolt.NormalizeDolthubURL(url) }
func readBeadsDBName() string               { return dolt.ReadBeadsDBName(realCwd) }
func parseOriginURL(out string) string      { return dolt.ParseOriginURL(out) }

func resolveDataDir() (string, error) {
	return dolt.ResolveDataDir(realCwd)
}
