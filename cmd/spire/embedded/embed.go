// Package embedded provides templates compiled into the spire binary.
// Templates are used by scaffolding commands (repo add, doctor --fix).
// Note: formula embeds live in pkg/formula/embedded to avoid pkg→cmd imports.
package embedded

import _ "embed"

//go:embed SPIRE.md.tmpl
var SpireMDTemplate string
