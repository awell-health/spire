//go:build !linux

package pool

import "errors"

// newInotifyPoolWake is a stub on non-linux platforms. Returning an
// error here causes NewPoolWake to fall back to LocalPoolWake even
// when SPIRE_POOL_WAKE=inotify is set.
func newInotifyPoolWake(stateDir string) (PoolWake, error) {
	return nil, errors.New("inotify pool wake: not supported on this platform")
}
