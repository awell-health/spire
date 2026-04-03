// Package workshop provides read-only formula exploration: listing, showing,
// and validating formulas (both embedded and custom on-disk overrides).
package workshop

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/pkg/formula/embedded"
	"github.com/awell-health/spire/pkg/formula"
)

// FormulaInfo holds lightweight metadata about a formula.
type FormulaInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Version     int      `json:"version"`
	Source      string   `json:"source"`               // "embedded" or "custom"
	Phases      []string `json:"phases"`                // v2: enabled phases; v3: step names
	Workspaces  []string `json:"workspaces,omitempty"`  // v3: declared workspace names
}

// formulaHeader is used for lightweight TOML parsing of just the header fields.
type formulaHeader struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`
	Version     int    `toml:"version"`
}

// ListFormulas enumerates all available formulas from embedded defaults and
// on-disk custom formulas. Disk formulas with the same name as an embedded
// formula override it (marked source="custom"). Returns sorted by name.
func ListFormulas() ([]FormulaInfo, error) {
	byName := make(map[string]FormulaInfo)

	// 1. Walk embedded formulas
	entries, err := fs.ReadDir(embedded.Formulas, "formulas")
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".formula.toml") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".formula.toml")
			data, err := embedded.Formulas.ReadFile("formulas/" + e.Name())
			if err != nil {
				continue
			}
			info := parseFormulaInfo(name, data, "embedded")
			byName[name] = info
		}
	}

	// 2. Walk disk formulas (same paths as formula.FindFormula)
	for _, dir := range diskFormulaDirs() {
		matches, err := filepath.Glob(filepath.Join(dir, "*.formula.toml"))
		if err != nil {
			continue
		}
		for _, path := range matches {
			base := filepath.Base(path)
			name := strings.TrimSuffix(base, ".formula.toml")
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			info := parseFormulaInfo(name, data, "custom")
			byName[name] = info
		}
	}

	// 3. Collect and sort
	result := make([]FormulaInfo, 0, len(byName))
	for _, info := range byName {
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result, nil
}

// diskFormulaDirs returns the directories to scan for custom formulas,
// mirroring the resolution logic in formula.FindFormula.
func diskFormulaDirs() []string {
	var dirs []string
	if bd := os.Getenv("BEADS_DIR"); bd != "" {
		dirs = append(dirs, filepath.Join(bd, "formulas"))
	}
	dirs = append(dirs, ".beads/formulas")
	if home := os.Getenv("HOME"); home != "" {
		dirs = append(dirs, filepath.Join(home, ".beads/formulas"))
	}
	return dirs
}

// parseFormulaInfo does a lightweight parse of formula TOML bytes to extract
// header metadata and phase/step names.
func parseFormulaInfo(name string, data []byte, source string) FormulaInfo {
	var hdr formulaHeader
	_ = toml.Unmarshal(data, &hdr)

	info := FormulaInfo{
		Name:        name,
		Description: hdr.Description,
		Version:     hdr.Version,
		Source:      source,
	}

	switch hdr.Version {
	case 3:
		if f, err := formula.ParseFormulaStepGraph(data); err == nil {
			steps := make([]string, 0, len(f.Steps))
			for s := range f.Steps {
				steps = append(steps, s)
			}
			sort.Strings(steps)
			info.Phases = steps

			if len(f.Workspaces) > 0 {
				ws := make([]string, 0, len(f.Workspaces))
				for w := range f.Workspaces {
					ws = append(ws, w)
				}
				sort.Strings(ws)
				info.Workspaces = ws
			}
		}
	}

	return info
}
