package dolt

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const (
	RequiredVersion = "1.84.0"
	binDirName      = "bin"
)

// ManagedDir returns the directory where spire stores its managed dolt binary.
// Creates the directory if it does not exist.
func ManagedDir() string {
	dir := filepath.Join(GlobalDir(), binDirName)
	os.MkdirAll(dir, 0755)
	return dir
}

// ManagedBinPath returns the full path to the managed dolt binary.
func ManagedBinPath() string {
	return filepath.Join(ManagedDir(), "dolt")
}

var (
	resolvedBinPathOnce  sync.Once
	resolvedBinPathValue string
)

// ResolvedBinPath returns the dolt binary to use.
// Priority: 1) managed binary if version OK, 2) PATH binary if version OK, 3) empty string.
// An outdated managed binary does not shadow a valid system binary.
// The result is memoized for the life of the process — the resolved path
// does not change during a run, and the underlying probe shells out to
// `dolt version`, which is too expensive to repeat on every dolt call.
func ResolvedBinPath() string {
	resolvedBinPathOnce.Do(func() {
		managed := ManagedBinPath()
		if _, err := os.Stat(managed); err == nil && VersionOK(managed) {
			resolvedBinPathValue = managed
			return
		}
		if p, err := exec.LookPath("dolt"); err == nil && VersionOK(p) {
			resolvedBinPathValue = p
			return
		}
	})
	return resolvedBinPathValue
}

// Bin returns the resolved dolt binary path, falling back to "dolt" if
// no managed or PATH binary is found. All dolt command invocations should
// use this instead of a hardcoded "dolt" string.
func Bin() string {
	if p := ResolvedBinPath(); p != "" {
		return p
	}
	return "dolt"
}

// InstalledVersion runs `dolt version` and parses the output.
// Dolt outputs lines like: "dolt version 1.46.1"
func InstalledVersion(binPath string) (string, error) {
	cmd := exec.Command(binPath, "version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run dolt version: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "dolt version ") {
			return strings.TrimPrefix(line, "dolt version "), nil
		}
	}
	return "", fmt.Errorf("could not parse dolt version from output: %s", strings.TrimSpace(string(out)))
}

// VersionOK checks if the dolt binary at binPath meets the minimum required version.
func VersionOK(binPath string) bool {
	v, err := InstalledVersion(binPath)
	if err != nil {
		return false
	}
	return semverAtLeast(v, RequiredVersion)
}

// semverAtLeast returns true if version >= minimum, comparing major.minor.patch numerically.
func semverAtLeast(version, minimum string) bool {
	parse := func(s string) (int, int, int) {
		parts := strings.SplitN(s, ".", 3)
		if len(parts) != 3 {
			return 0, 0, 0
		}
		major, _ := strconv.Atoi(parts[0])
		minor, _ := strconv.Atoi(parts[1])
		patch, _ := strconv.Atoi(parts[2])
		return major, minor, patch
	}
	vMaj, vMin, vPat := parse(version)
	mMaj, mMin, mPat := parse(minimum)
	if vMaj != mMaj {
		return vMaj > mMaj
	}
	if vMin != mMin {
		return vMin > mMin
	}
	return vPat >= mPat
}

// DownloadURL constructs the download URL for the current platform.
func DownloadURL() string {
	return fmt.Sprintf(
		"https://github.com/dolthub/dolt/releases/download/v%s/dolt-%s-%s.tar.gz",
		RequiredVersion, runtime.GOOS, runtime.GOARCH,
	)
}

// Download downloads and extracts the dolt binary for the current platform.
func Download() error {
	url := DownloadURL()
	fmt.Printf("  downloading dolt v%s from %s\n", RequiredVersion, url)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download dolt: %w\n  hint: install dolt manually — https://docs.dolthub.com/introduction/installation", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download dolt: HTTP %d from %s\n  hint: install dolt manually — https://docs.dolthub.com/introduction/installation", resp.StatusCode, url)
	}

	fmt.Print("  extracting... ")
	binPath, err := ExtractBinary(resp.Body)
	if err != nil {
		return err
	}
	fmt.Println("done")

	// Verify the extracted binary works
	v, err := InstalledVersion(binPath)
	if err != nil {
		return fmt.Errorf("verify extracted binary: %w", err)
	}
	fmt.Printf("  verified: dolt version %s\n", v)

	return nil
}

// ExtractBinary reads a tar.gz stream and extracts the dolt binary to the managed directory.
// It looks for the entry matching dolt-{os}-{arch}/bin/dolt inside the tarball.
func ExtractBinary(r io.Reader) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("decompress tarball: %w", err)
	}
	defer gz.Close()

	wantSuffix := "/bin/dolt"
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tarball: %w", err)
		}

		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Match the dolt binary entry: dolt-{os}-{arch}/bin/dolt
		if !strings.HasSuffix(hdr.Name, wantSuffix) {
			continue
		}

		dest := ManagedBinPath()
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return "", fmt.Errorf("create %s: %w", dest, err)
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			os.Remove(dest)
			return "", fmt.Errorf("write dolt binary: %w", err)
		}
		f.Close()
		return dest, nil
	}

	return "", fmt.Errorf("dolt binary not found in tarball (looked for *%s)", wantSuffix)
}

// EnsureBinary ensures a usable dolt binary exists and returns its path.
// It checks the managed binary first, then the PATH, then downloads if needed.
func EnsureBinary() (string, error) {
	// 1. Check managed binary
	managed := ManagedBinPath()
	if _, err := os.Stat(managed); err == nil {
		if VersionOK(managed) {
			return managed, nil
		}
		// Wrong version — will re-download below
		fmt.Printf("managed binary is v%s, need v%s\n", MustVersion(managed), RequiredVersion)
	}

	// 2. Check PATH binary
	if p, err := exec.LookPath("dolt"); err == nil {
		if VersionOK(p) {
			return p, nil
		}
		// Wrong version in PATH — continue to download managed binary
	}

	// 3. Download managed binary
	fmt.Println("downloading...")
	if err := Download(); err != nil {
		return "", err
	}

	return managed, nil
}

// MustVersion returns the version string or "unknown" on error.
func MustVersion(binPath string) string {
	v, err := InstalledVersion(binPath)
	if err != nil {
		return "unknown"
	}
	return v
}
