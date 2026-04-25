package executor

// cluster_dispatch.go — cluster-native child-dispatch seam.
//
// In cluster-native mode, executor-driven child work (step/implement/fix
// dispatch from graph_actions.go and the wave/sequential/direct
// dispatchers in action_dispatch.go, plus the wizard's review-fix
// re-entry) MUST NOT call backend.Spawn directly. The operator owns
// child-pod materialization through the WorkloadIntent contract
// introduced in spi-5bzu9r.1; this file defines the seam executor and
// wizard call sites consume.
//
// In local-native mode, this seam stays nil and dispatch follows the
// existing Spawner.Spawn path unchanged.

import (
	"context"

	"github.com/awell-health/spire/pkg/steward/intent"
)

// ClusterChildDispatcher is the cluster-native seam executor and wizard
// review-fix call sites use to publish a child-run WorkloadIntent
// instead of spawning a process locally. The operator's intent
// reconciler picks up the intent and materializes the apprentice/sage
// pod from Runtime.Image / Command / Env per the spi-5bzu9r.1 contract.
//
// Implementations adapt intent.IntentPublisher (the .1-introduced
// transport seam) and any executor-side correlation work (assigning a
// monotonic DispatchSeq, validating the (Role, Phase) pair through
// intent.Validate) under one interface so call sites do not need to
// reach into pkg/store or pkg/steward/intent themselves.
//
// Nil is the local-native default. Action handlers MUST go through
// (*Executor).useClusterChildDispatch() rather than re-inspecting
// tower config — that helper centralizes the mode + nil-dispatcher
// check so cluster-native semantics fail closed (no fallback to
// Spawner.Spawn) and local-native paths stay direct-spawn.
type ClusterChildDispatcher interface {
	// Dispatch publishes a child-run WorkloadIntent through the
	// cluster intent plane. Implementations are responsible for
	// assigning a fresh DispatchSeq when the caller leaves it zero
	// and for invoking intent.Validate before publishing — call
	// sites pass a partially-populated intent (TaskID, Role, Phase,
	// Runtime, RepoIdentity, Reason, HandoffMode) and trust the
	// dispatcher to finalize it.
	Dispatch(ctx context.Context, wi intent.WorkloadIntent) error
}
