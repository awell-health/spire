package recovery

// DepBead is the minimal bead projection needed by recovery.
// Callers map from their store.Bead to this type.
type DepBead struct {
	ID     string
	Title  string
	Status string
	Labels []string
	Parent string
}

// DepDependent represents a dependent bead with its dependency type.
type DepDependent struct {
	ID             string
	Title          string
	Status         string
	Labels         []string
	DependencyType string
}

// Deps abstracts all external dependencies so the recovery package can be
// tested without a real store, registry, or filesystem. Modeled after
// pkg/executor.Deps and pkg/wizard.Deps.
type Deps struct {
	// Store operations
	GetBead     func(id string) (DepBead, error)
	GetChildren func(parentID string) ([]DepBead, error)

	// Dependents (reverse deps) — returns beads that depend on the given ID.
	GetDependentsWithMeta func(id string) ([]DepDependent, error)

	// Executor state — returns nil if no state file exists.
	LoadExecutorState func(agentName string) (*RuntimeState, error)

	// Git checks
	CheckBranchExists  func(repoPath, branch string) bool
	CheckWorktreeExists func(dir string) bool
	CheckWorktreeDirty  func(dir string) bool

	// Mutations
	AddComment func(id, text string) error
	CloseBead  func(id string) error

	// Wizard registry
	LookupRegistry func(beadID string) (name string, pid int, alive bool, err error)

	// Repo resolution — returns (repoPath, baseBranch, err).
	ResolveRepo func(beadID string) (string, string, error)
}
