// kill_audit.c — dyld-interpose libsystem kill(2) so we can identify
// the call site of every signal-send in a target binary. Built as a
// dylib and loaded via DYLD_INSERT_LIBRARIES; this is the only way
// macOS lets us interpose kill() — embedding the interpose table in
// the main executable does not work (verified empirically: dyld's
// interpose machinery only scans dylibs, not the main image).
//
// Build (from repo root):
//   clang -dynamiclib tools/diagnostics/kill_audit.c \
//     -o /tmp/libspire_kill_audit.dylib
//
// Use:
//   DYLD_INSERT_LIBRARIES=/tmp/libspire_kill_audit.dylib go test ...
//
// Output: appends one record per kill() call to
//   /tmp/spire-kill-audit-<pid>.log
//
// Resolve C-backtrace addresses post-mortem with:
//   atos -o <binary> -l <slide> <addr1> <addr2> ...
//
// See spi-kn3e8f for the investigation context (apprentice killed by
// SIGINT from cmd/spire test bypassing every spire-internal audit
// wrapper). This dylib is the catch-all because it intercepts at
// the libsystem boundary, below every Go syscall path.

#include <signal.h>
#include <unistd.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <fcntl.h>
#include <time.h>
#include <pthread.h>
#include <execinfo.h>
#include <dlfcn.h>
#include <sys/types.h>

static int g_log_fd = -1;
static pthread_once_t g_log_once = PTHREAD_ONCE_INIT;
static pthread_mutex_t g_log_mu = PTHREAD_MUTEX_INITIALIZER;

static void init_log(void) {
    char path[1024];
    snprintf(path, sizeof(path), "/tmp/spire-kill-audit-%d.log", getpid());
    g_log_fd = open(path, O_WRONLY | O_CREAT | O_APPEND, 0644);
}

// spire_audited_kill replaces libsystem kill() at dyld load time via the
// interpose table below. It logs the caller, the target PID, and the
// signal — then forwards to the real kill().
int spire_audited_kill(pid_t pid, int sig) {
    pthread_once(&g_log_once, init_log);
    if (g_log_fd >= 0) {
        pthread_mutex_lock(&g_log_mu);

        // C-level backtrace: addresses only. Go runtime frames will
        // appear here; resolve with atos against the test binary.
        void *frames[32];
        int depth = backtrace(frames, 32);

        // Mach thread ID lets us correlate with eslogger / Activity Monitor.
        uint64_t tid = 0;
        pthread_threadid_np(NULL, &tid);

        char buf[4096];
        int n = snprintf(buf, sizeof(buf),
            "[kill-audit] ts=%ld pid=%d sig=%d caller=%d thread=%llu depth=%d\n",
            (long)time(NULL), pid, sig, getpid(), tid, depth);
        if (n > 0) {
            ssize_t _w = write(g_log_fd, buf, (size_t)n);
            (void)_w;
        }
        for (int i = 0; i < depth; i++) {
            // dladdr resolves the closest exported symbol to the address;
            // Go's runtime exports a subset of its functions, so we'll
            // see e.g. runtime.cgocall, syscall.syscall, and the libsystem
            // kill stub. Caller-of-syscall (the Go function that issued
            // the signal) is usually NOT exported and shows up as just
            // an offset into the binary — resolve those with `atos -o
            // <test-binary> <addr>` after the run, or correlate with
            // /tmp/spire-test-trace.log (the active test name at the
            // matching timestamp).
            Dl_info info;
            if (dladdr(frames[i], &info) && info.dli_sname) {
                long off = (long)((const char *)frames[i] - (const char *)info.dli_saddr);
                const char *fname = info.dli_fname ? strrchr(info.dli_fname, '/') : NULL;
                fname = fname ? fname + 1 : (info.dli_fname ? info.dli_fname : "?");
                n = snprintf(buf, sizeof(buf), "  [%2d] %p %s+0x%lx (%s)\n",
                    i, frames[i], info.dli_sname, off, fname);
            } else {
                n = snprintf(buf, sizeof(buf), "  [%2d] %p\n", i, frames[i]);
            }
            if (n > 0) {
                ssize_t _w = write(g_log_fd, buf, (size_t)n);
                (void)_w;
            }
        }
        const char *sep = "----\n";
        ssize_t _w = write(g_log_fd, sep, strlen(sep));
        (void)_w;

        pthread_mutex_unlock(&g_log_mu);
    }
    return kill(pid, sig);
}

typedef struct interpose_s {
    const void *new_func;
    const void *orig_func;
} interpose_t;

__attribute__((used))
static interpose_t spire_kill_interposers[]
    __attribute__((section("__DATA,__interpose"))) = {
    { (const void *)spire_audited_kill, (const void *)kill }
};
