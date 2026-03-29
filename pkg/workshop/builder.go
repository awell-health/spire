package workshop

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/awell-health/spire/pkg/formula"
)

// FormulaBuilder accumulates formula configuration. Use programmatically (artificer API)
// or driven by ComposeInteractive (CLI prompts).
type FormulaBuilder struct {
	name         string
	description  string
	beadType     string
	phases       []string
	phaseConfigs map[string]formula.PhaseConfig
	vars         map[string]formula.FormulaVar
}

// NewBuilder creates a new FormulaBuilder with empty phases and vars.
func NewBuilder(name string) *FormulaBuilder {
	return &FormulaBuilder{
		name:         name,
		phaseConfigs: make(map[string]formula.PhaseConfig),
		vars:         make(map[string]formula.FormulaVar),
	}
}

// SetDescription sets the formula description.
func (b *FormulaBuilder) SetDescription(desc string) {
	b.description = desc
}

// SetBeadType sets the target bead type (used for phase defaults).
func (b *FormulaBuilder) SetBeadType(typ string) {
	b.beadType = typ
}

// EnablePhase validates the phase name, appends it to the ordered list,
// and seeds the phase config with defaults from PhaseDefaultsFor.
func (b *FormulaBuilder) EnablePhase(phase string) error {
	if !formula.IsValidPhase(phase) {
		return fmt.Errorf("unknown phase %q", phase)
	}
	// Don't double-add
	for _, p := range b.phases {
		if p == phase {
			return nil
		}
	}
	b.phases = append(b.phases, phase)
	b.phaseConfigs[phase] = defaultToPhaseConfig(PhaseDefaultsFor(phase, b.beadType))
	return nil
}

// DisablePhase removes a phase from the ordered list and config map.
func (b *FormulaBuilder) DisablePhase(phase string) {
	for i, p := range b.phases {
		if p == phase {
			b.phases = append(b.phases[:i], b.phases[i+1:]...)
			break
		}
	}
	delete(b.phaseConfigs, phase)
}

// ConfigurePhase overwrites the config for an enabled phase.
func (b *FormulaBuilder) ConfigurePhase(phase string, cfg formula.PhaseConfig) error {
	if _, ok := b.phaseConfigs[phase]; !ok {
		return fmt.Errorf("phase %q is not enabled", phase)
	}
	b.phaseConfigs[phase] = cfg
	return nil
}

// PhaseConfig returns the current config for an enabled phase.
func (b *FormulaBuilder) PhaseConfig(phase string) (formula.PhaseConfig, bool) {
	cfg, ok := b.phaseConfigs[phase]
	return cfg, ok
}

// Phases returns the ordered list of enabled phases.
func (b *FormulaBuilder) Phases() []string {
	return b.phases
}

// AddVar adds or overwrites a formula variable.
func (b *FormulaBuilder) AddVar(name string, v formula.FormulaVar) {
	b.vars[name] = v
}

// RemoveVar removes a formula variable.
func (b *FormulaBuilder) RemoveVar(name string) {
	delete(b.vars, name)
}

// Build constructs a FormulaV2 from the builder state.
// Returns error if no phases are enabled or config is invalid.
func (b *FormulaBuilder) Build() (*formula.FormulaV2, error) {
	if len(b.phases) == 0 {
		return nil, fmt.Errorf("no phases enabled")
	}

	// Validate: wave dispatch requires staging_branch
	for _, phase := range b.phases {
		cfg := b.phaseConfigs[phase]
		if cfg.GetDispatch() == "wave" && cfg.StagingBranch == "" {
			return nil, fmt.Errorf("phase %q uses wave dispatch but has no staging_branch", phase)
		}
	}

	phases := make(map[string]formula.PhaseConfig, len(b.phases))
	for _, p := range b.phases {
		phases[p] = b.phaseConfigs[p]
	}

	var vars map[string]formula.FormulaVar
	if len(b.vars) > 0 {
		vars = make(map[string]formula.FormulaVar, len(b.vars))
		for k, v := range b.vars {
			vars[k] = v
		}
	}

	return &formula.FormulaV2{
		Name:        b.name,
		Description: b.description,
		Version:     2,
		Phases:      phases,
		Vars:        vars,
	}, nil
}

// MarshalTOML builds the formula and serializes it to ordered TOML bytes.
// Phases are written in ValidPhases order (not random map order).
func (b *FormulaBuilder) MarshalTOML() ([]byte, error) {
	f, err := b.Build()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	// Header
	fmt.Fprintf(&buf, "name = %q\n", f.Name)
	fmt.Fprintf(&buf, "description = %q\n", f.Description)
	fmt.Fprintf(&buf, "version = %d\n", f.Version)

	// Phases in canonical order
	for _, phase := range formula.ValidPhases {
		cfg, ok := f.Phases[phase]
		if !ok {
			continue
		}
		fmt.Fprintf(&buf, "\n[phases.%s]\n", phase)
		writePhaseConfig(&buf, phase, cfg)
	}

	// Vars in sorted order
	if len(f.Vars) > 0 {
		names := make([]string, 0, len(f.Vars))
		for n := range f.Vars {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, name := range names {
			v := f.Vars[name]
			fmt.Fprintf(&buf, "\n[vars.%s]\n", name)
			writeFormulaVar(&buf, v)
		}
	}

	return buf.Bytes(), nil
}

// defaultToPhaseConfig converts a PhaseDefault to a formula.PhaseConfig.
func defaultToPhaseConfig(d PhaseDefault) formula.PhaseConfig {
	return formula.PhaseConfig{
		Role:           d.Role,
		Model:          d.Model,
		Timeout:        d.Timeout,
		Dispatch:       d.Dispatch,
		Worktree:       d.Worktree,
		Apprentice:     d.Apprentice,
		VerdictOnly:    d.VerdictOnly,
		Judgment:       d.Judgment,
		Auto:           d.Auto,
		StagingBranch:  d.StagingBranch,
		MergeStrategy:  d.MergeStrategy,
		Context:        d.Context,
		RevisionPolicy: d.RevisionPolicy,
	}
}

// writePhaseConfig writes PhaseConfig fields in a readable order, skipping zero values.
func writePhaseConfig(buf *bytes.Buffer, phase string, cfg formula.PhaseConfig) {
	if cfg.Role != "" {
		fmt.Fprintf(buf, "role = %q\n", cfg.Role)
	}
	if cfg.Timeout != "" {
		fmt.Fprintf(buf, "timeout = %q\n", cfg.Timeout)
	}
	if cfg.Model != "" {
		fmt.Fprintf(buf, "model = %q\n", cfg.Model)
	}
	if cfg.MaxTurns > 0 {
		fmt.Fprintf(buf, "max_turns = %d\n", cfg.MaxTurns)
	}
	if cfg.Dispatch != "" {
		fmt.Fprintf(buf, "dispatch = %q\n", cfg.Dispatch)
	}
	if cfg.StagingBranch != "" {
		fmt.Fprintf(buf, "staging_branch = %q\n", cfg.StagingBranch)
	}
	if cfg.MergeStrategy != "" {
		fmt.Fprintf(buf, "strategy = %q\n", cfg.MergeStrategy)
	}
	if cfg.Apprentice {
		fmt.Fprintf(buf, "apprentice = true\n")
	}
	if cfg.Worktree {
		fmt.Fprintf(buf, "worktree = true\n")
	}
	if cfg.Auto {
		fmt.Fprintf(buf, "auto = true\n")
	}
	if cfg.VerdictOnly {
		fmt.Fprintf(buf, "verdict_only = true\n")
	}
	if cfg.Judgment {
		fmt.Fprintf(buf, "judgment = true\n")
	}
	if cfg.Behavior != "" {
		fmt.Fprintf(buf, "behavior = %q\n", cfg.Behavior)
	}
	if cfg.Build != "" {
		fmt.Fprintf(buf, "build = %q\n", cfg.Build)
	}
	if cfg.Test != "" {
		fmt.Fprintf(buf, "test = %q\n", cfg.Test)
	}
	if cfg.MaxBuildFixRounds > 0 {
		fmt.Fprintf(buf, "max_build_fix_rounds = %d\n", cfg.MaxBuildFixRounds)
	}
	if cfg.Deploy != "" {
		fmt.Fprintf(buf, "deploy = %q\n", cfg.Deploy)
	}
	if len(cfg.Context) > 0 {
		fmt.Fprintf(buf, "context = [%s]\n", formatStringSlice(cfg.Context))
	}
	if cfg.RevisionPolicy != nil {
		fmt.Fprintf(buf, "\n[phases.%s.revision_policy]\n", phase)
		fmt.Fprintf(buf, "max_rounds = %d\n", cfg.RevisionPolicy.MaxRounds)
		if cfg.RevisionPolicy.ArbiterModel != "" {
			fmt.Fprintf(buf, "arbiter_model = %q\n", cfg.RevisionPolicy.ArbiterModel)
		}
	}
}

// writeFormulaVar writes FormulaVar fields.
func writeFormulaVar(buf *bytes.Buffer, v formula.FormulaVar) {
	if v.Description != "" {
		fmt.Fprintf(buf, "description = %q\n", v.Description)
	}
	if v.Required {
		fmt.Fprintf(buf, "required = true\n")
	}
	if v.Default != "" {
		fmt.Fprintf(buf, "default = %q\n", v.Default)
	}
}

// formatStringSlice formats a string slice as TOML inline array content.
func formatStringSlice(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}
