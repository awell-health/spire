package workshop

import (
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/formula/embedded"
)

// Publish copies a formula TOML file to the tower's .beads/formulas/ directory.
// The formula must pass validation before publishing. Returns the destination path.
// beadsDir is resolved via config.ResolveBeadsDir() by the caller.
func Publish(name string, beadsDir string) (string, error) {
	if beadsDir == "" {
		return "", fmt.Errorf("beadsDir is empty — no tower configured")
	}

	// Resolve source: try on-disk first, then embedded
	var data []byte
	if path, err := formula.FindFormula(name); err == nil {
		data, err = os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read formula %q: %w", name, err)
		}
	} else {
		// Try embedded
		filename := "formulas/" + name + ".formula.toml"
		var readErr error
		data, readErr = embedded.Formulas.ReadFile(filename)
		if readErr != nil {
			return "", fmt.Errorf("formula %q not found on disk or in embedded defaults", name)
		}
	}

	// Detect version and validate with the correct parser
	var hdr struct {
		Version int `toml:"version"`
	}
	if err := toml.Unmarshal(data, &hdr); err != nil {
		return "", fmt.Errorf("formula %q: invalid TOML: %w", name, err)
	}
	switch hdr.Version {
	case 3:
		if _, err := formula.ParseFormulaStepGraph(data); err != nil {
			return "", fmt.Errorf("formula %q is invalid: %w", name, err)
		}
	default:
		return "", fmt.Errorf("formula %q: unsupported version %d", name, hdr.Version)
	}

	// Ensure target directory exists
	formulasDir := filepath.Join(beadsDir, "formulas")
	if err := os.MkdirAll(formulasDir, 0755); err != nil {
		return "", fmt.Errorf("create formulas directory: %w", err)
	}

	// Write to destination
	dest := filepath.Join(formulasDir, name+".formula.toml")
	if err := os.WriteFile(dest, data, 0644); err != nil {
		return "", fmt.Errorf("write formula to %s: %w", dest, err)
	}

	return dest, nil
}

// Unpublish removes a published formula from .beads/formulas/.
// Only removes formulas that exist on disk; returns error if not found.
func Unpublish(name string, beadsDir string) error {
	if beadsDir == "" {
		return fmt.Errorf("beadsDir is empty — no tower configured")
	}

	path := PublishedPath(name, beadsDir)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("formula %q is not published at %s", name, path)
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove published formula %q: %w", name, err)
	}

	return nil
}

// IsPublished checks if a formula exists in .beads/formulas/.
func IsPublished(name string, beadsDir string) bool {
	path := PublishedPath(name, beadsDir)
	_, err := os.Stat(path)
	return err == nil
}

// PublishedPath returns the path where a formula would be published.
func PublishedPath(name string, beadsDir string) string {
	return filepath.Join(beadsDir, "formulas", name+".formula.toml")
}
