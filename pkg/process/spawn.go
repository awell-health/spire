package process

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// SpawnOpts configures a background process launched by SpawnBackground.
type SpawnOpts struct {
	Name    string   // descriptive name (used in error messages)
	Bin     string   // binary path
	Args    []string // arguments
	Dir     string   // working directory
	Env     []string // nil = inherit parent env; non-nil = use provided env
	LogDir  string   // directory for stdout/stderr log files
	LogName string   // base name for log files; defaults to Name
	PIDPath string   // where to write the PID file
}

// SpawnBackground starts a detached process (Setsid), redirects stdout/stderr
// to {LogName}.log / {LogName}.error.log in LogDir, writes the PID file, and
// releases the process handle. Returns the PID.
//
// Caller is responsible for post-start verification (port checks, alive checks,
// etc.) since that differs per service.
//
// Log file open errors are silently ignored to match existing behavior — a log
// file failing to open does not prevent the process from starting.
func SpawnBackground(opts SpawnOpts) (int, error) {
	logName := opts.LogName
	if logName == "" {
		logName = opts.Name
	}

	cmd := exec.Command(opts.Bin, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if opts.Env != nil {
		cmd.Env = opts.Env
	}

	// Redirect output to log files. Errors are silently ignored — stdout/stderr
	// will be nil (discarded) if the files cannot be opened.
	var logFile, errFile *os.File
	if opts.LogDir != "" {
		logFile, _ = os.OpenFile(filepath.Join(opts.LogDir, logName+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		errFile, _ = os.OpenFile(filepath.Join(opts.LogDir, logName+".error.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		cmd.Stdout = logFile
		cmd.Stderr = errFile
	}

	if err := cmd.Start(); err != nil {
		closeFiles(logFile, errFile)
		return 0, fmt.Errorf("start %s: %w", opts.Name, err)
	}

	pid := cmd.Process.Pid

	if opts.PIDPath != "" {
		WritePID(opts.PIDPath, pid)
	}

	// Release the process so it continues after we exit.
	cmd.Process.Release()

	// Close log file handles (the child process has its own references).
	closeFiles(logFile, errFile)

	return pid, nil
}

func closeFiles(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			f.Close()
		}
	}
}
