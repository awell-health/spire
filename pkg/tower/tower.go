// Package tower contains shared logic for attaching to and reading from a
// DoltHub-backed Spire tower. Both the interactive CLI (`spire tower attach`)
// and the cluster bootstrap path (`spire tower attach-cluster`) use it so
// "what counts as a tower attach" has a single source of truth.
//
// Out of scope for this package: starting/stopping dolt servers, interactive
// prompts, saving global tower config, registering custom bead types. Those
// are caller concerns.
package tower
