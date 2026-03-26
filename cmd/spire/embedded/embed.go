// Package embedded provides default formulas and templates compiled into the spire binary.
// Formulas serve as fallbacks when no on-disk formula override exists.
// Templates are used by scaffolding commands (repo add, doctor --fix).
package embedded

import "embed"

//go:embed formulas/*.formula.toml
var Formulas embed.FS

//go:embed SPIRE.md.tmpl
var SpireMDTemplate string
