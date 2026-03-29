// Package embedded provides default formulas compiled into the spire binary.
// Formulas serve as fallbacks when no on-disk formula override exists.
package embedded

import "embed"

//go:embed formulas/*.formula.toml
var Formulas embed.FS
