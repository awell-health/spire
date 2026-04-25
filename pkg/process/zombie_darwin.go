package process

import "golang.org/x/sys/unix"

// szomb mirrors the macOS kernel SZOMB constant (sys/proc.h): a process
// awaiting collection by its parent. KinfoProc.Proc.P_stat carries this
// value for zombie processes.
const szomb = 5

// isZombie returns true when the process is in state SZOMB (defunct,
// awaiting reap). Uses sysctl(kern.proc.pid) — works for any PID, not
// just children of the caller.
func isZombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil {
		return false
	}
	return kp.Proc.P_stat == szomb
}
