package tower

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsBlankDB_ZeroMeansBlank(t *testing.T) {
	exec := func(q string) (string, error) {
		if !strings.Contains(q, "information_schema.tables") {
			t.Fatalf("unexpected query: %q", q)
		}
		if !strings.Contains(q, "table_schema = 'spi'") {
			t.Errorf("query should scope to target database; got %q", q)
		}
		return pipeOutput("0"), nil
	}
	blank, err := IsBlankDB(exec, "spi")
	if err != nil {
		t.Fatalf("IsBlankDB: %v", err)
	}
	if !blank {
		t.Fatal("expected blank=true for COUNT(*)=0")
	}
}

func TestIsBlankDB_NonZeroMeansPopulated(t *testing.T) {
	exec := func(q string) (string, error) {
		return pipeOutput("7"), nil
	}
	blank, err := IsBlankDB(exec, "spi")
	if err != nil {
		t.Fatalf("IsBlankDB: %v", err)
	}
	if blank {
		t.Fatal("expected blank=false for COUNT(*)=7")
	}
}

func TestIsBlankDB_ErrorBubblesUp(t *testing.T) {
	sentinel := errors.New("connection refused")
	exec := func(q string) (string, error) { return "", sentinel }
	_, err := IsBlankDB(exec, "spi")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain should wrap sentinel; got %v", err)
	}
}

func TestIsBlankDB_EmptyDatabaseRejected(t *testing.T) {
	_, err := IsBlankDB(nil, "")
	if err == nil || !strings.Contains(err.Error(), "database is required") {
		t.Fatalf("want 'database is required' error, got %v", err)
	}
}

func TestBootstrapBlank_Validation(t *testing.T) {
	base := BlankBootstrapOpts{
		Database:  "spi",
		Prefix:    "spi",
		DataDir:   "/data",
		RunBdInit: func(database, prefix, runDir string) error { return nil },
	}
	tests := []struct {
		name    string
		mutate  func(o *BlankBootstrapOpts)
		wantErr string
	}{
		{"missing database", func(o *BlankBootstrapOpts) { o.Database = "" }, "Database is required"},
		{"missing prefix", func(o *BlankBootstrapOpts) { o.Prefix = "" }, "Prefix is required"},
		{"missing datadir", func(o *BlankBootstrapOpts) { o.DataDir = "" }, "DataDir is required"},
		{"missing RunBdInit", func(o *BlankBootstrapOpts) { o.RunBdInit = nil }, "RunBdInit is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			tc.mutate(&opts)
			err := BootstrapBlank(nil, opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestBootstrapBlank_Success(t *testing.T) {
	var (
		bdCalls    int
		gotDB      string
		gotPrefix  string
		gotRunDir  string
		typesDir   string
		typesCalls int
	)

	exec := func(q string) (string, error) {
		if strings.Contains(q, "_project_id") {
			return pipeOutput("proj-123"), nil
		}
		return pipeOutput(""), nil
	}

	opts := BlankBootstrapOpts{
		Database: "spi",
		Prefix:   "spi",
		DataDir:  "/data",
		RunBdInit: func(db, prefix, runDir string) error {
			bdCalls++
			gotDB, gotPrefix, gotRunDir = db, prefix, runDir
			return nil
		},
		EnsureCustomTypes: func(dir string) error {
			typesCalls++
			typesDir = dir
			return nil
		},
	}

	if err := BootstrapBlank(exec, opts); err != nil {
		t.Fatalf("BootstrapBlank: %v", err)
	}
	if bdCalls != 1 {
		t.Fatalf("bd init called %d times, want 1", bdCalls)
	}
	if gotDB != "spi" || gotPrefix != "spi" || gotRunDir != "/data" {
		t.Errorf("bd init got (db=%q, prefix=%q, runDir=%q)", gotDB, gotPrefix, gotRunDir)
	}
	if typesCalls != 1 {
		t.Fatalf("EnsureCustomTypes called %d times, want 1", typesCalls)
	}
	if want := filepath.Join("/data", ".beads"); typesDir != want {
		t.Errorf("EnsureCustomTypes got %q, want %q", typesDir, want)
	}
}

func TestBootstrapBlank_BdInitErrorBubblesUp(t *testing.T) {
	sentinel := errors.New("bd exploded")
	opts := BlankBootstrapOpts{
		Database:  "spi",
		Prefix:    "spi",
		DataDir:   "/data",
		RunBdInit: func(db, prefix, runDir string) error { return sentinel },
	}
	err := BootstrapBlank(nil, opts)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want chain wrapping sentinel", err)
	}
}

func TestBootstrapBlank_MissingProjectIDIsFatal(t *testing.T) {
	exec := func(q string) (string, error) {
		// bd init "succeeded" but metadata table is empty
		return "", fmt.Errorf("empty result")
	}
	opts := BlankBootstrapOpts{
		Database:  "spi",
		Prefix:    "spi",
		DataDir:   "/data",
		RunBdInit: func(db, prefix, runDir string) error { return nil },
	}
	err := BootstrapBlank(exec, opts)
	if err == nil {
		t.Fatal("expected error when project_id cannot be read")
	}
	if !strings.Contains(err.Error(), "project_id") {
		t.Errorf("error should mention project_id; got %v", err)
	}
}

func TestBootstrapBlank_EnsureCustomTypesOptional(t *testing.T) {
	exec := func(q string) (string, error) {
		if strings.Contains(q, "_project_id") {
			return pipeOutput("proj-1"), nil
		}
		return "", nil
	}
	opts := BlankBootstrapOpts{
		Database:  "spi",
		Prefix:    "spi",
		DataDir:   "/data",
		RunBdInit: func(db, prefix, runDir string) error { return nil },
		// EnsureCustomTypes intentionally nil.
	}
	if err := BootstrapBlank(exec, opts); err != nil {
		t.Fatalf("BootstrapBlank: %v", err)
	}
}

func TestBootstrapBlank_EnsureCustomTypesErrorPropagates(t *testing.T) {
	sentinel := errors.New("bd type add failed")
	exec := func(q string) (string, error) {
		if strings.Contains(q, "_project_id") {
			return pipeOutput("proj-1"), nil
		}
		return "", nil
	}
	opts := BlankBootstrapOpts{
		Database:          "spi",
		Prefix:            "spi",
		DataDir:           "/data",
		RunBdInit:         func(db, prefix, runDir string) error { return nil },
		EnsureCustomTypes: func(dir string) error { return sentinel },
	}
	err := BootstrapBlank(exec, opts)
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("got %v, want chain wrapping sentinel", err)
	}
}
