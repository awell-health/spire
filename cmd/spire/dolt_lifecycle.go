package main

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
	"strings"
)

const (
	doltRequiredVersion = "1.84.0"
	doltBinDirName      = "bin"
)

// doltManagedDir returns the directory where spire stores its managed dolt binary.
// Creates the directory if it does not exist.
func doltManagedDir() string {
	dir := filepath.Join(doltGlobalDir(), doltBinDirName)
	os.MkdirAll(dir, 0755)
	return dir
}

// doltManagedBinPath returns the full path to the managed dolt binary.
func doltManagedBinPath() string {
	return filepath.Join(doltManagedDir(), "dolt")
}

// doltResolvedBinPath returns the dolt binary to use.
// Priority: 1) managed binary if it exists, 2) PATH lookup, 3) empty string.
func doltResolvedBinPath() string {
	managed := doltManagedBinPath()
	if _, err := os.Stat(managed); err == nil {
		return managed
	}
	if p, err := exec.LookPath("dolt"); err == nil {
		return p
	}
	return ""
}

// doltInstalledVersion runs `dolt version` and parses the output.
// Dolt outputs lines like: "dolt version 1.46.1"
func doltInstalledVersion(binPath string) (string, error) {
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

// doltVersionOK checks if the dolt binary at binPath matches the required version.
func doltVersionOK(binPath string) bool {
	v, err := doltInstalledVersion(binPath)
	if err != nil {
		return false
	}
	return v == doltRequiredVersion
}

// doltDownloadURL constructs the download URL for the current platform.
func doltDownloadURL() string {
	return fmt.Sprintf(
		"https://github.com/dolthub/dolt/releases/download/v%s/dolt-%s-%s.tar.gz",
		doltRequiredVersion, runtime.GOOS, runtime.GOARCH,
	)
}

// doltDownload downloads and extracts the dolt binary for the current platform.
func doltDownload() error {
	url := doltDownloadURL()
	fmt.Printf("  downloading dolt v%s from %s\n", doltRequiredVersion, url)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download dolt: %w\n  hint: install dolt manually — https://docs.dolthub.com/introduction/installation", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download dolt: HTTP %d from %s\n  hint: install dolt manually — https://docs.dolthub.com/introduction/installation", resp.StatusCode, url)
	}

	fmt.Print("  extracting... ")
	binPath, err := doltExtractBinary(resp.Body)
	if err != nil {
		return err
	}
	fmt.Println("done")

	// Verify the extracted binary works
	v, err := doltInstalledVersion(binPath)
	if err != nil {
		return fmt.Errorf("verify extracted binary: %w", err)
	}
	fmt.Printf("  verified: dolt version %s\n", v)

	return nil
}

// doltExtractBinary reads a tar.gz stream and extracts the dolt binary to the managed directory.
// It looks for the entry matching dolt-{os}-{arch}/bin/dolt inside the tarball.
func doltExtractBinary(r io.Reader) (string, error) {
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

		dest := doltManagedBinPath()
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

// doltEnsureBinary ensures a usable dolt binary exists and returns its path.
// It checks the managed binary first, then the PATH, then downloads if needed.
func doltEnsureBinary() (string, error) {
	// 1. Check managed binary
	managed := doltManagedBinPath()
	if _, err := os.Stat(managed); err == nil {
		if doltVersionOK(managed) {
			return managed, nil
		}
		// Wrong version — will re-download below
		fmt.Printf("managed binary is v%s, need v%s\n", mustVersion(managed), doltRequiredVersion)
	}

	// 2. Check PATH binary
	if p, err := exec.LookPath("dolt"); err == nil {
		if doltVersionOK(p) {
			return p, nil
		}
		// Wrong version in PATH — continue to download managed binary
	}

	// 3. Download managed binary
	fmt.Println("downloading...")
	if err := doltDownload(); err != nil {
		return "", err
	}

	return managed, nil
}

// mustVersion returns the version string or "unknown" on error.
func mustVersion(binPath string) string {
	v, err := doltInstalledVersion(binPath)
	if err != nil {
		return "unknown"
	}
	return v
}
