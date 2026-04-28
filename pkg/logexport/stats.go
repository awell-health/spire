package logexport

import "sync/atomic"

// atomicStats holds the running counters surfaced by Exporter.Stats. All
// fields are int64 atomics so the tailer goroutine can update them
// without holding a lock the test goroutine reads through Snapshot.
type atomicStats struct {
	files     atomic.Int64
	bytes     atomic.Int64
	lines     atomic.Int64
	finalized atomic.Int64
	failed    atomic.Int64
	retries   atomic.Int64
}

func (s *atomicStats) addFiles(d int64)     { s.files.Add(d) }
func (s *atomicStats) addBytes(n int)       { s.bytes.Add(int64(n)) }
func (s *atomicStats) incLines()            { s.lines.Add(1) }
func (s *atomicStats) incFinalized()        { s.finalized.Add(1) }
func (s *atomicStats) incFailed()           { s.failed.Add(1) }
func (s *atomicStats) addRetries(n int)     { s.retries.Add(int64(n)) }

// Snapshot returns a value-copy of the counters.
func (s *atomicStats) Snapshot() Stats {
	return Stats{
		FilesTracked:       s.files.Load(),
		BytesEmitted:       s.bytes.Load(),
		LinesEmitted:       s.lines.Load(),
		ArtifactsFinalized: s.finalized.Load(),
		ArtifactsFailed:    s.failed.Load(),
		ManifestRetries:    s.retries.Load(),
	}
}
