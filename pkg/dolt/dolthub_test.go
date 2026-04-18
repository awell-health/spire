package dolt

import (
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

func TestClassifyRemoteURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"short org/repo form", "awell/spi", config.RemoteKindDoltHub, false},
		{"dolthub API URL", "https://doltremoteapi.dolthub.com/awell/spi", config.RemoteKindDoltHub, false},
		{"dolthub web URL", "https://www.dolthub.com/repositories/awell/spi", config.RemoteKindDoltHub, false},
		{"dolthub bare host", "https://dolthub.com/awell/spi", config.RemoteKindDoltHub, false},
		{"cluster port-forward http", "http://localhost:50051/spi", config.RemoteKindRemotesAPI, false},
		{"cluster in-cluster http", "http://spire-dolt.spire.svc.cluster.local:50051/spi", config.RemoteKindRemotesAPI, false},
		{"cluster https", "https://dolt.example.com:50051/spi", config.RemoteKindRemotesAPI, false},
		{"dolt:// scheme", "dolt://host:50051/spi", config.RemoteKindRemotesAPI, false},
		{"host:port no scheme should error", "localhost:50051/spi", "", true},
		{"empty input", "", "", true},
		{"unsupported scheme", "ftp://host/db", "", true},
		{"whitespace only", "   ", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyRemoteURL(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ClassifyRemoteURL(%q) err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ClassifyRemoteURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNormalizeRemoteURL(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		kind  string
		want  string
	}{
		{"dolthub short form", "awell/spi", config.RemoteKindDoltHub, "https://doltremoteapi.dolthub.com/awell/spi"},
		{"dolthub full URL passthrough", "https://doltremoteapi.dolthub.com/awell/spi", config.RemoteKindDoltHub, "https://doltremoteapi.dolthub.com/awell/spi"},
		{"remotesapi trims trailing slash", "http://localhost:50051/spi/", config.RemoteKindRemotesAPI, "http://localhost:50051/spi"},
		{"remotesapi preserves dolt scheme", "dolt://host:50051/spi", config.RemoteKindRemotesAPI, "dolt://host:50051/spi"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeRemoteURL(tc.raw, tc.kind)
			if got != tc.want {
				t.Errorf("NormalizeRemoteURL(%q, %q) = %q, want %q", tc.raw, tc.kind, got, tc.want)
			}
		})
	}
}

func TestDatabaseFromRemoteURL(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"http://localhost:50051/spi", "spi"},
		{"http://localhost:50051/spi/", "spi"},
		{"dolt://host:50051/beads_hub", "beads_hub"},
		{"awell/spi", "spi"},
		{"https://doltremoteapi.dolthub.com/awell/spi", "spi"},
		{"", ""},
	}

	for _, tc := range tests {
		got := DatabaseFromRemoteURL(tc.input)
		if got != tc.want {
			t.Errorf("DatabaseFromRemoteURL(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
