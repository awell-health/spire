//go:build darwin || linux

package wizard

/*
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <sys/types.h>

// Path to write sender records. Set by Go before installing the handler.
static char g_log_path[1024] = {0};
static int g_log_fd = -1;

// Original sigactions, so we can chain to Go's runtime handler after we
// finish recording. Go's runtime installs its own handlers later in
// program startup; the kernel calls our handler first and we forward.
static struct sigaction g_prev_sigint;
static struct sigaction g_prev_sigterm;

// async-signal-safe writer: format a fixed-shape record into a stack
// buffer and write(2) to the open fd. No malloc, no fprintf, no time
// formatting that could allocate. We pre-open the log fd so the handler
// itself doesn't need to open(2).
static void sender_handler(int sig, siginfo_t *info, void *ctx) {
    if (g_log_fd >= 0 && info != NULL) {
        char buf[256];
        // si_pid: 0 if signal came from kernel; otherwise sender's PID
        // si_uid: sender's uid
        // si_code: signal code (SI_USER = sent via kill(), SI_KERNEL = kernel)
        int n = snprintf(buf, sizeof(buf),
            "sig=%d sender_pid=%d sender_uid=%d si_code=%d ts=%ld self_pid=%d\n",
            sig, info->si_pid, info->si_uid, info->si_code,
            (long)time(NULL), getpid());
        if (n > 0) {
            ssize_t _ = write(g_log_fd, buf, (size_t)n);
            (void)_;
        }
    }
    // Chain to whatever was installed before us (typically Go's runtime
    // handler, which forwards through to the os/signal channel).
    struct sigaction *prev = (sig == SIGINT) ? &g_prev_sigint : &g_prev_sigterm;
    if (prev->sa_flags & SA_SIGINFO) {
        if (prev->sa_sigaction != NULL && (void*)prev->sa_sigaction != (void*)SIG_IGN && (void*)prev->sa_sigaction != (void*)SIG_DFL) {
            prev->sa_sigaction(sig, info, ctx);
        }
    } else {
        if (prev->sa_handler != NULL && prev->sa_handler != SIG_IGN && prev->sa_handler != SIG_DFL) {
            prev->sa_handler(sig);
        }
    }
}

// install_sender_capture installs the SA_SIGINFO handler for SIGINT and
// SIGTERM. Stores the previous handlers so sender_handler can chain.
// Returns 0 on success, errno on failure.
int install_sender_capture(const char *path) {
    if (path == NULL || strlen(path) == 0 || strlen(path) >= sizeof(g_log_path)) {
        return EINVAL;
    }
    strncpy(g_log_path, path, sizeof(g_log_path)-1);
    g_log_fd = open(g_log_path, O_WRONLY|O_CREAT|O_APPEND, 0644);
    if (g_log_fd < 0) return errno;

    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_sigaction = sender_handler;
    sa.sa_flags = SA_SIGINFO | SA_RESTART;
    sigemptyset(&sa.sa_mask);

    if (sigaction(SIGINT, &sa, &g_prev_sigint) != 0) return errno;
    if (sigaction(SIGTERM, &sa, &g_prev_sigterm) != 0) return errno;
    return 0;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// installSenderCapture wires a C-level SA_SIGINFO handler for SIGINT/SIGTERM
// that writes the sender's PID to logPath before chaining to Go's runtime
// handler. Lets us identify who's signaling our apprentice — Go's
// os/signal package alone does not expose siginfo_t (the struct that
// carries the sender PID).
//
// Must be called BEFORE signal.Notify so that Go's runtime registers
// our handler as its "previous" and chains through. If installed later,
// Go's handler runs first and our chain logic in sender_handler will
// not be reached.
func installSenderCapture(logPath string) error {
	cpath := C.CString(logPath)
	defer C.free(unsafe.Pointer(cpath))
	rc := C.install_sender_capture(cpath)
	if rc != 0 {
		return fmt.Errorf("install_sender_capture: errno=%d", int(rc))
	}
	return nil
}
