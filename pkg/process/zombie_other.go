//go:build !linux && !darwin

package process

// isZombie is a no-op on platforms without a zombie state probe. The
// underlying ProcessAlive existence check (kill -0) is the best we can
// do; ProcessAlive's behavior here is unchanged from the pre-fix version.
func isZombie(pid int) bool {
	return false
}
