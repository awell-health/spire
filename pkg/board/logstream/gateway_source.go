package logstream

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/awell-health/spire/pkg/gatewayclient"
)

// GatewayLogClient is the subset of the gateway client the log Source
// needs. Defined as an interface so tests can inject a fake without
// spinning up an HTTPS server. The production implementation is
// *gatewayclient.Client which already satisfies this surface.
type GatewayLogClient interface {
	ListAllBeadLogs(ctx context.Context, beadID string) ([]gatewayclient.LogArtifactRecord, error)
	FetchBeadLogRaw(ctx context.Context, beadID, artifactID string, asEngineer bool) ([]byte, error)
}

// GatewaySource is a Source backed by the gateway bead-logs API. It is
// the canonical read surface for cluster-attach mode: desktop, board,
// and CLI ask the gateway and the gateway resolves the underlying
// substrate (local filesystem in local-native mode, GCS in cluster
// mode) without exposing those details to clients.
//
// Construct via NewGatewaySource. The zero value is not usable.
type GatewaySource struct {
	client     GatewayLogClient
	asEngineer bool
}

// NewGatewaySource returns a Source that fetches log artifacts through
// the supplied gateway client. asEngineer toggles the X-Spire-Scope
// header on raw fetches; pass true for the local CLI path so the
// gateway returns engineer_only artifacts unredacted (existing
// `spire logs pretty` behavior expected raw transcript bytes), and
// false for desktop / shared callers where redacted output is the
// right answer.
func NewGatewaySource(client GatewayLogClient, asEngineer bool) *GatewaySource {
	return &GatewaySource{client: client, asEngineer: asEngineer}
}

// List walks the gateway manifest list for beadID, fetches the bytes
// for each transcript / stdout artifact, pairs sidecar stderr rows
// with their parent transcript, and returns the result as Artifact
// values shaped identically to the local source.
//
// "No artifacts yet" surfaces as (nil, nil) — distinct from a real
// error. Bytes failures on individual artifacts are downgraded to a
// per-row warning by leaving Content empty: the inspector still shows
// the artifact but renders an "(empty log)" placeholder, which beats
// dropping the whole list because one byte fetch failed.
func (s *GatewaySource) List(ctx context.Context, beadID string) ([]Artifact, error) {
	if s.client == nil || beadID == "" {
		return nil, nil
	}
	records, err := s.client.ListAllBeadLogs(ctx, beadID)
	if err != nil {
		// Treat 404 ("bead missing on the gateway") and similar
		// "nothing here" responses as the empty state. Real errors
		// (network, auth) still propagate so the caller can decide
		// to retry or surface the failure.
		if errors.Is(err, gatewayclient.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("logstream: list gateway logs: %w", err)
	}
	if len(records) == 0 {
		return nil, nil
	}

	// Bucket sidecar (stream=stderr) rows by their parent identity so
	// they pair with the transcript / stdout row they belong to.
	type sidecarKey struct {
		agent, role, phase, provider string
		seq                          int
	}
	stderrByKey := make(map[sidecarKey]gatewayclient.LogArtifactRecord)
	primary := make([]gatewayclient.LogArtifactRecord, 0, len(records))
	for _, r := range records {
		if r.Stream == "stderr" && r.Provider != "" {
			stderrByKey[sidecarKey{
				agent:    r.AgentName,
				role:     r.Role,
				phase:    r.Phase,
				provider: r.Provider,
				seq:      r.Sequence,
			}] = r
			continue
		}
		primary = append(primary, r)
	}

	out := make([]Artifact, 0, len(primary))
	for _, r := range primary {
		// Skip rows whose bytes cannot be served yet; the gateway
		// list endpoint includes writing/failed rows so the board can
		// render manifest-only state, but the artifact in that case
		// has no bytes to load. Inspector renders "(empty log)" for
		// these, matching the local source behaviour for missing
		// content.
		var content []byte
		if r.Status == "finalized" {
			b, err := s.client.FetchBeadLogRaw(ctx, beadID, r.ID, s.asEngineer)
			if err != nil && !errors.Is(err, gatewayclient.ErrNotFound) {
				// A real error on a single artifact must not blow
				// up the whole list — degrade to an empty Content
				// for that artifact so the rest of the bead's logs
				// still render.
				content = nil
			} else {
				content = b
			}
		}

		art := Artifact{
			Name:     gatewayArtifactName(r),
			Provider: r.Provider,
			Content:  string(content),
		}

		if r.Provider != "" {
			key := sidecarKey{
				agent:    r.AgentName,
				role:     r.Role,
				phase:    r.Phase,
				provider: r.Provider,
				seq:      r.Sequence,
			}
			if sc, ok := stderrByKey[key]; ok && sc.Status == "finalized" {
				if scBytes, err := s.client.FetchBeadLogRaw(ctx, beadID, sc.ID, s.asEngineer); err == nil {
					art.StderrContent = string(scBytes)
				}
			}
		}
		out = append(out, art)
	}

	// Order: operational stdout rows first (the "wizard" / "<spawn>"
	// rows), then transcripts grouped under their owning agent. Inside
	// each group, newest-first so the inspector sub-tab strip lands on
	// the most recent activity by default. The gateway already returns
	// rows in (attempt, run, sequence, created_at) order — we adapt
	// that into the inspector's display order with a stable sort.
	sort.SliceStable(out, func(i, j int) bool {
		// Provider-empty (operational) rows sort before provider rows.
		if (out[i].Provider == "") != (out[j].Provider == "") {
			return out[i].Provider == ""
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// gatewayArtifactName synthesises the inspector-display name for a
// manifest row. Local source names look like "wizard" /
// "implement-1" / "implement-1/claude (HH:MM)" — gateway naming has
// to match those shapes so cycle tagging in the inspector applies
// uniformly.
//
// Heuristics, derived from how pkg/wizard names spawns:
//
//   - Wizard role + provider == "" + stream != stderr → "wizard"
//   - Other role + provider == "" → strip "wizard-<bead>-" from
//     AgentName and use the suffix (e.g. "impl-1", "sage-review-2")
//   - Provider != "" → "<spawn>/<provider>" or "<provider>" when the
//     spawn is the wizard itself; followed by " (<phase>)" so multiple
//     transcripts under one spawn remain distinguishable
func gatewayArtifactName(r gatewayclient.LogArtifactRecord) string {
	beadPrefix := "wizard-" + r.BeadID
	spawn := r.AgentName
	if strings.HasPrefix(spawn, beadPrefix+"-") {
		spawn = strings.TrimPrefix(spawn, beadPrefix+"-")
	} else if spawn == beadPrefix {
		spawn = ""
	}

	if r.Provider == "" {
		if spawn == "" {
			return "wizard"
		}
		return spawn
	}

	// Provider transcript. Distinguish chunk sequences when seq > 0 so
	// chunked transcripts surface as separate sub-tabs instead of
	// collapsing onto the same name.
	label := r.Provider
	if r.Phase != "" {
		label = fmt.Sprintf("%s (%s)", r.Provider, r.Phase)
	}
	if r.Sequence > 0 {
		label = fmt.Sprintf("%s.%d", label, r.Sequence)
	}
	if spawn == "" {
		return label
	}
	return spawn + "/" + label
}
