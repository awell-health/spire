// Package alerts is the exclusive owner of alert bead and archmage-message
// bead creation in Spire. All callers must go through Raise — no direct
// store.CreateBead(..., Labels: []string{"alert:..."}) calls are permitted
// outside this package (sole exception: tower-level messages with no source
// bead, documented at the one remaining call site in pkg/steward/steward.go).
package alerts

import (
	"fmt"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Class is the kind of alert bead to create.
type Class string

const (
	// ClassAlert creates a per-failure alert bead surfaced on the board.
	// Labels: []string{"alert:<subclass>"}. Dep: caused-by → sourceBeadID.
	ClassAlert Class = "alert"

	// ClassArchmageMsg creates a direct message to the archmage.
	// Labels: []string{"msg", "to:archmage", "from:<agent>"}. Dep: related → sourceBeadID.
	ClassArchmageMsg Class = "msg"
)

// BeadOps is the narrow interface callers must satisfy. It is intentionally
// kept small to avoid pulling in all of executor.Deps or store.* directly.
type BeadOps interface {
	CreateBead(opts store.CreateOpts) (string, error)
	AddDepTyped(from, to, depType string) error
}

// Option modifies how Raise builds the bead.
type Option func(*raiseConfig)

type raiseConfig struct {
	title       string
	subclass    string
	from        string
	priority    *int
	extraLabels []string
}

// WithTitle sets the bead title. Required for ClassAlert; ClassArchmageMsg
// uses the message parameter as the title if this is not provided.
func WithTitle(t string) Option {
	return func(c *raiseConfig) { c.title = t }
}

// WithSubclass appends "alert:<subclass>" to the bead's labels.
// For example WithSubclass("merge-failure") produces label "alert:merge-failure".
func WithSubclass(s string) Option {
	return func(c *raiseConfig) { c.subclass = s }
}

// WithFrom stamps a "from:<agent>" label on the bead.
func WithFrom(agent string) Option {
	return func(c *raiseConfig) { c.from = agent }
}

// WithPriority overrides the default priority (P0 for ClassAlert, P1 for ClassArchmageMsg).
func WithPriority(p int) Option {
	return func(c *raiseConfig) { c.priority = &p }
}

// WithExtraLabels appends additional labels beyond the class-default set.
func WithExtraLabels(labels ...string) Option {
	return func(c *raiseConfig) { c.extraLabels = append(c.extraLabels, labels...) }
}

// Raise creates an alert or message bead attributed to sourceBeadID.
//
// It always:
//   - derives the new bead's prefix from sourceBeadID via store.PrefixFromID
//   - creates a dep from the new bead to sourceBeadID (caused-by for ClassAlert,
//     related for ClassArchmageMsg)
//   - stamps appropriate labels for the class
//   - returns the new bead's ID for the caller to reference
//
// Errors if sourceBeadID is empty or its prefix cannot be derived, or if the
// underlying CreateBead call fails. On AddDepTyped failure the bead is already
// created — the ID is returned along with the error so the caller can decide
// whether to retry the dep link.
func Raise(ops BeadOps, sourceBeadID string, class Class, message string, opts ...Option) (string, error) {
	if sourceBeadID == "" {
		return "", fmt.Errorf("alerts.Raise: sourceBeadID must not be empty")
	}
	prefix := store.PrefixFromID(sourceBeadID)
	if prefix == "" {
		return "", fmt.Errorf("alerts.Raise: cannot derive prefix from sourceBeadID %q", sourceBeadID)
	}

	cfg := &raiseConfig{}
	for _, o := range opts {
		o(cfg)
	}

	var (
		labels    []string
		priority  int
		depType   string
		beadTitle string
	)

	switch class {
	case ClassAlert:
		priority = 0
		depType = "caused-by"
		if cfg.subclass != "" {
			labels = append(labels, "alert:"+cfg.subclass)
		} else {
			labels = append(labels, "alert")
		}
		beadTitle = cfg.title
		if beadTitle == "" {
			beadTitle = message
		}

	case ClassArchmageMsg:
		priority = 1
		depType = "related"
		labels = append(labels, "msg", "to:archmage")
		if cfg.from != "" {
			labels = append(labels, "from:"+cfg.from)
		}
		beadTitle = message

	default:
		return "", fmt.Errorf("alerts.Raise: unknown class %q", class)
	}

	// Apply caller overrides.
	if cfg.priority != nil {
		priority = *cfg.priority
	}
	labels = append(labels, cfg.extraLabels...)

	// Truncate very long titles to avoid store limits (consistent with call sites).
	if len(beadTitle) > 200 {
		beadTitle = beadTitle[:200]
	}

	alertID, err := ops.CreateBead(store.CreateOpts{
		Title:    beadTitle,
		Priority: priority,
		Type:     beads.TypeTask,
		Labels:   labels,
		Prefix:   prefix,
	})
	if err != nil {
		return "", fmt.Errorf("alerts.Raise: create bead: %w", err)
	}

	// Link to source bead. On failure the bead exists — return its ID with the error.
	if derr := ops.AddDepTyped(alertID, sourceBeadID, depType); derr != nil {
		return alertID, fmt.Errorf("alerts.Raise: add %s dep %s→%s: %w", depType, alertID, sourceBeadID, derr)
	}

	return alertID, nil
}
