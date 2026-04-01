package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDoltDownloadURL(t *testing.T) {
	url := doltDownloadURL()

	// Must contain the pinned version
	if !strings.Contains(url, doltRequiredVersion) {
		t.Errorf("URL %q does not contain version %s", url, doltRequiredVersion)
	}

	// Must contain current OS and arch
	if !strings.Contains(url, runtime.GOOS) {
		t.Errorf("URL %q does not contain GOOS %s", url, runtime.GOOS)
	}
	if !strings.Contains(url, runtime.GOARCH) {
		t.Errorf("URL %q does not contain GOARCH %s", url, runtime.GOARCH)
	}

	// Must be a tar.gz
	if !strings.HasSuffix(url, ".tar.gz") {
		t.Errorf("URL %q does not end with .tar.gz", url)
	}

	// Must match the expected pattern
	expected := "https://github.com/dolthub/dolt/releases/download/v" +
		doltRequiredVersion + "/dolt-" + runtime.GOOS + "-" + runtime.GOARCH + ".tar.gz"
	if url != expected {
		t.Errorf("URL mismatch:\n  got:  %s\n  want: %s", url, expected)
	}
}

func TestDoltManagedBinPath(t *testing.T) {
	path := doltManagedBinPath()

	// Must end with /bin/dolt
	if !strings.HasSuffix(path, filepath.Join("bin", "dolt")) {
		t.Errorf("managed bin path %q does not end with bin/dolt", path)
	}

	// Must be under the spire global dir
	globalDir := doltGlobalDir()
	if !strings.HasPrefix(path, globalDir) {
		t.Errorf("managed bin path %q is not under global dir %q", path, globalDir)
	}
}

func TestDoltResolvedBinPath(t *testing.T) {
	// If dolt is in PATH, we should get a non-empty result
	// (even if the managed binary doesn't exist)
	result := doltResolvedBinPath()

	// We can't assert much here since the test environment
	// may or may not have dolt installed, but the function
	// should not panic
	if result != "" {
		if _, err := os.Stat(result); err != nil {
			// PATH lookup may return a valid path even if stat fails
			// (e.g., the binary is a symlink). That's fine.
			t.Logf("resolved path %q stat error (may be ok): %v", result, err)
		}
	}
}

func TestDoltInstalledVersion(t *testing.T) {
	// Create a fake dolt binary that prints version info
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "dolt")

	script := "#!/bin/sh\necho 'dolt version 1.46.1'\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	v, err := doltInstalledVersion(fakeBin)
	if err != nil {
		t.Fatalf("doltInstalledVersion: %v", err)
	}
	if v != "1.46.1" {
		t.Errorf("version = %q, want %q", v, "1.46.1")
	}
}

func TestDoltInstalledVersionMultiline(t *testing.T) {
	// Dolt sometimes outputs extra lines
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "dolt")

	script := "#!/bin/sh\necho 'some preamble'\necho 'dolt version 1.44.0'\necho 'extra line'\n"
	if err := os.WriteFile(fakeBin, []byte(script), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	v, err := doltInstalledVersion(fakeBin)
	if err != nil {
		t.Fatalf("doltInstalledVersion: %v", err)
	}
	if v != "1.44.0" {
		t.Errorf("version = %q, want %q", v, "1.44.0")
	}
}

func TestDoltVersionOK(t *testing.T) {
	tmpDir := t.TempDir()

	// Binary with correct version
	goodBin := filepath.Join(tmpDir, "dolt-good")
	script := "#!/bin/sh\necho 'dolt version " + doltRequiredVersion + "'\n"
	if err := os.WriteFile(goodBin, []byte(script), 0755); err != nil {
		t.Fatalf("write good binary: %v", err)
	}
	if !doltVersionOK(goodBin) {
		t.Error("doltVersionOK returned false for correct version")
	}

	// Binary with wrong version
	badBin := filepath.Join(tmpDir, "dolt-bad")
	script = "#!/bin/sh\necho 'dolt version 0.99.0'\n"
	if err := os.WriteFile(badBin, []byte(script), 0755); err != nil {
		t.Fatalf("write bad binary: %v", err)
	}
	if doltVersionOK(badBin) {
		t.Error("doltVersionOK returned true for wrong version")
	}

	// Binary with newer version (should be OK — minimum version check)
	newerBin := filepath.Join(tmpDir, "dolt-newer")
	script = "#!/bin/sh\necho 'dolt version 99.0.0'\n"
	if err := os.WriteFile(newerBin, []byte(script), 0755); err != nil {
		t.Fatalf("write newer binary: %v", err)
	}
	if !doltVersionOK(newerBin) {
		t.Error("doltVersionOK returned false for newer version")
	}

	// Non-existent binary
	if doltVersionOK("/nonexistent/dolt") {
		t.Error("doltVersionOK returned true for non-existent binary")
	}
}

func TestDoltExtractBinary(t *testing.T) {
	// Build a tar.gz in memory that contains dolt-darwin-arm64/bin/dolt
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	fakeContent := []byte("#!/bin/sh\necho 'fake dolt'\n")
	entryName := "dolt-" + runtime.GOOS + "-" + runtime.GOARCH + "/bin/dolt"

	hdr := &tar.Header{
		Name: entryName,
		Mode: 0755,
		Size: int64(len(fakeContent)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(fakeContent); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	tw.Close()
	gw.Close()

	// Point doltManagedDir to a temp directory by setting SPIRE_DOLT_DIR
	tmpDir := t.TempDir()
	oldEnv := os.Getenv("SPIRE_DOLT_DIR")
	os.Setenv("SPIRE_DOLT_DIR", tmpDir)
	defer os.Setenv("SPIRE_DOLT_DIR", oldEnv)

	extracted, err := doltExtractBinary(&buf)
	if err != nil {
		t.Fatalf("doltExtractBinary: %v", err)
	}

	// Verify the binary was written
	if _, err := os.Stat(extracted); err != nil {
		t.Fatalf("extracted binary not found at %s: %v", extracted, err)
	}

	// Verify content
	content, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatalf("read extracted binary: %v", err)
	}
	if !bytes.Equal(content, fakeContent) {
		t.Errorf("extracted content mismatch:\n  got:  %q\n  want: %q", content, fakeContent)
	}
}

func TestDoltExtractBinaryMissing(t *testing.T) {
	// Build a tar.gz without the expected binary
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: "some-other-file.txt",
		Mode: 0644,
		Size: 5,
	}
	tw.WriteHeader(hdr)
	tw.Write([]byte("hello"))
	tw.Close()
	gw.Close()

	tmpDir := t.TempDir()
	oldEnv := os.Getenv("SPIRE_DOLT_DIR")
	os.Setenv("SPIRE_DOLT_DIR", tmpDir)
	defer os.Setenv("SPIRE_DOLT_DIR", oldEnv)

	_, err := doltExtractBinary(&buf)
	if err == nil {
		t.Fatal("expected error when binary not in tarball, got nil")
	}
	if !strings.Contains(err.Error(), "not found in tarball") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDoltBinHelper(t *testing.T) {
	// doltBin() should never return empty string
	result := doltBin()
	if result == "" {
		t.Error("doltBin() returned empty string")
	}
}
