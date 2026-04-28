package logstream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LocalSource is the filesystem-backed Source. It reads the legacy
// wizard log layout under <root>/wizards/wizard-<beadID>/... that
// pkg/wizard writes to in local-native mode. Backwards compatibility
// with that layout is mandatory while local-native is the default — see
// design spi-7wzwk2 for the migration path. New artifact-manifest reads
// (when available) layer on top via the gateway-backed source rather
// than mutating the local filesystem layout.
//
// The root is the wizards directory (e.g. "<dolt-global>/wizards"), not
// the per-bead directory; LocalSource resolves "wizard-<beadID>" itself
// so callers can construct it once per board session.
type LocalSource struct {
	wizardsRoot string
}

// NewLocalSource returns a Source backed by wizardsRoot — a directory
// holding wizard log files (e.g. "<dolt-global>/wizards"). The
// directory does not have to exist when the source is constructed; the
// empty state surfaces naturally as "no artifacts" on List.
func NewLocalSource(wizardsRoot string) *LocalSource {
	return &LocalSource{wizardsRoot: wizardsRoot}
}

// List walks the legacy wizard log layout and returns one Artifact per
// log file:
//
//   - wizard-<beadID>.log (or -fix.log fallback) — the operational
//     wizard log, named "wizard"
//   - wizard-<beadID>-<spawn>.log — sibling spawn operational logs,
//     named "<spawn>"
//   - wizard-<beadID>/<provider>/<file> — provider transcripts under
//     the wizard directory, named "<filename-derived-label>"
//   - wizard-<beadID>-<spawn>/<provider>/<file> — provider transcripts
//     under each spawn's directory, named "<spawn>/<filename-derived-label>"
//
// Stderr sidecars (.stderr.log next to a transcript) are loaded into
// the Artifact's StderrContent rather than returned as separate
// artifacts, matching the inspector's per-LogView display contract.
//
// Empty wizardsRoot or a missing wizard directory yields ([], nil) —
// "no artifacts yet" rather than an error.
func (s *LocalSource) List(_ context.Context, beadID string) ([]Artifact, error) {
	if s.wizardsRoot == "" || beadID == "" {
		return nil, nil
	}
	wizardName := "wizard-" + beadID
	logDir := s.wizardsRoot

	var out []Artifact

	// Top-level wizard operational log. Try the canonical name first,
	// then the legacy "<wizard>-fix.log" fallback that some flows used
	// before the runtime moved to a single canonical location.
	for _, candidate := range []string{
		filepath.Join(logDir, wizardName+".log"),
		filepath.Join(logDir, wizardName+"-fix.log"),
	} {
		if content, err := os.ReadFile(candidate); err == nil {
			out = append(out, Artifact{
				Name:    "wizard",
				Path:    candidate,
				Content: string(content),
			})
			break
		}
	}

	// Provider transcripts under the wizard directory. The filename
	// already encodes a label + timestamp; LoadProviderArtifacts maps
	// that to a display string.
	out = append(out, LoadProviderArtifacts(filepath.Join(logDir, wizardName), "")...)

	// Sibling spawn logs (apprentices, sages, clerics) plus their own
	// provider subdirs. The naming convention is wizard-<bead>-<suffix>
	// so spawns are easy to glob; filter out any path already counted
	// as the top-level wizard log.
	knownPaths := make(map[string]bool, len(out))
	for _, a := range out {
		if a.Path != "" {
			knownPaths[filepath.Base(a.Path)] = true
		}
	}
	siblings, err := filepath.Glob(filepath.Join(logDir, wizardName+"-*.log"))
	if err == nil {
		sort.Strings(siblings)
		for _, path := range siblings {
			if knownPaths[filepath.Base(path)] {
				continue
			}
			content, rerr := os.ReadFile(path)
			if rerr != nil {
				continue
			}
			stem := strings.TrimSuffix(filepath.Base(path), ".log")
			name := strings.TrimPrefix(stem, wizardName+"-")
			out = append(out, Artifact{
				Name:    name,
				Path:    path,
				Content: string(content),
			})
			out = append(out, LoadProviderArtifacts(filepath.Join(logDir, stem), name+"/")...)
		}
	}

	return out, nil
}

// LoadProviderArtifacts walks wizardDir/<provider>/ subdirectories and
// returns an Artifact for every transcript file it finds. Mirrors the
// pkg/board legacy reader behaviour — keeping a single implementation
// in pkg/board/logstream means the local source and pkg/board's
// `loadProviderLogViews` (which converts these Artifacts to LogViews
// with adapter-parsed Events) agree on pattern selection, sidecar
// discovery, and display naming.
//
// Pattern selection per provider:
//
//   - "claude" → *.jsonl plus legacy *.log (transcripts captured before
//     the .jsonl convention landed)
//   - any other name → *.jsonl only
//
// Files ending in ".stderr.log" are never returned as transcripts —
// they are sidecars whose contents are loaded into StderrContent /
// StderrPath of the matching Artifact.
//
// namePrefix is prepended to every returned Name (used to mark sibling
// spawn logs with "<spawn-name>/"). Within each provider directory,
// matches are sorted newest-first so the inspector sub-tab strip and
// the CLI both surface the most recent transcript by default.
func LoadProviderArtifacts(wizardDir, namePrefix string) []Artifact {
	entries, err := os.ReadDir(wizardDir)
	if err != nil {
		return nil
	}
	var out []Artifact
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		providerName := e.Name()
		providerDir := filepath.Join(wizardDir, providerName)

		patterns := []string{"*.jsonl"}
		if providerName == "claude" {
			patterns = append(patterns, "*.log")
		}
		seen := map[string]bool{}
		var matches []string
		for _, pat := range patterns {
			paths, perr := filepath.Glob(filepath.Join(providerDir, pat))
			if perr != nil {
				continue
			}
			for _, p := range paths {
				if strings.HasSuffix(p, ".stderr.log") {
					continue
				}
				if seen[p] {
					continue
				}
				seen[p] = true
				matches = append(matches, p)
			}
		}
		// Newest-first: timestamped filenames sort lexicographically,
		// so reverse-string sort is effectively descending by timestamp.
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))

		for _, path := range matches {
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				continue
			}
			content := string(raw)

			var stderrPath, stderrContent string
			candidate := DeriveStderrPath(path)
			if sc, serr := os.ReadFile(candidate); serr == nil {
				stderrPath = candidate
				stderrContent = string(sc)
			}

			name := DeriveProviderLogName(filepath.Base(path))
			if namePrefix != "" {
				name = namePrefix + name
			}
			out = append(out, Artifact{
				Name:          name,
				Provider:      providerName,
				Path:          path,
				Content:       content,
				StderrPath:    stderrPath,
				StderrContent: stderrContent,
			})
		}
	}
	return out
}

// DeriveStderrPath returns the sidecar path for a transcript: strip the
// final extension and append ".stderr.log". For "foo/bar.jsonl" it
// returns "foo/bar.stderr.log"; for "foo/bar.log" it returns the same.
// Exposed so the inspector and substrate code can produce identical
// sidecar paths from a transcript path.
func DeriveStderrPath(transcriptPath string) string {
	ext := filepath.Ext(transcriptPath)
	base := strings.TrimSuffix(transcriptPath, ext)
	return base + ".stderr.log"
}

// DeriveProviderLogName turns a provider transcript filename into the
// display label used in the inspector's sub-tab strip and in
// `spire logs` listings.
//
// Input shape: "<label>-<YYYYMMDD-HHMMSS>.{log,jsonl}" — for example
// "epic-plan-20260417-173412.log" or "implement-20260417-173412.jsonl".
// Output shape: "<label> (HH:MM)" — e.g. "epic-plan (17:34)". When the
// filename does not match the timestamp shape, the extension-stripped
// basename is returned unchanged so unfamiliar filenames still render.
func DeriveProviderLogName(filename string) string {
	base := strings.TrimSuffix(filename, ".log")
	base = strings.TrimSuffix(base, ".jsonl")
	if len(base) < 16 {
		return base
	}
	tsStart := len(base) - 16
	if base[tsStart] != '-' {
		return base
	}
	tsPart := base[tsStart+1:]
	if len(tsPart) != 15 || tsPart[8] != '-' {
		return base
	}
	datePart := tsPart[:8]
	timePart := tsPart[9:]
	for i := 0; i < 8; i++ {
		if datePart[i] < '0' || datePart[i] > '9' {
			return base
		}
	}
	for i := 0; i < 6; i++ {
		if timePart[i] < '0' || timePart[i] > '9' {
			return base
		}
	}
	label := base[:tsStart]
	if label == "" {
		return base
	}
	return fmt.Sprintf("%s (%s:%s)", label, timePart[:2], timePart[2:4])
}
