package tower

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

func TestAttachCluster_RequiresNamespace(t *testing.T) {
	if err := AttachCluster(AttachOptions{}); err == nil {
		t.Fatal("expected error when namespace is empty")
	}
}

func TestAttachCluster_RejectsInClusterWithKubeconfig(t *testing.T) {
	err := AttachCluster(AttachOptions{
		Namespace:  "spire-a",
		InCluster:  true,
		Kubeconfig: "/tmp/kc",
	})
	if err == nil {
		t.Fatal("expected error when --in-cluster is combined with --kubeconfig")
	}
}

func TestAttachCluster_UpsertsByNamespace(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	tower := &config.TowerConfig{
		Name:      "t1",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "t1",
		Database:  "beads_t1",
		CreatedAt: "2026-04-17T10:00:00Z",
	}
	if err := config.SaveTowerConfig(tower); err != nil {
		t.Fatalf("save tower: %v", err)
	}

	// First attach: new entry appended.
	if err := AttachCluster(AttachOptions{
		Tower:     "t1",
		Namespace: "spire-a",
		InCluster: true,
	}); err != nil {
		t.Fatalf("attach (1): %v", err)
	}
	got, err := config.LoadTowerConfig("t1")
	if err != nil {
		t.Fatalf("load tower: %v", err)
	}
	if len(got.Clusters) != 1 {
		t.Fatalf("Clusters len = %d, want 1", len(got.Clusters))
	}
	if got.Clusters[0].Namespace != "spire-a" || !got.Clusters[0].InCluster {
		t.Errorf("unexpected attachment: %+v", got.Clusters[0])
	}

	// Second attach for same namespace: replaces, does not duplicate.
	if err := AttachCluster(AttachOptions{
		Tower:      "t1",
		Namespace:  "spire-a",
		Kubeconfig: "/tmp/kc",
		Context:    "kind-kind",
	}); err != nil {
		t.Fatalf("attach (2): %v", err)
	}
	got, err = config.LoadTowerConfig("t1")
	if err != nil {
		t.Fatalf("load tower (2): %v", err)
	}
	if len(got.Clusters) != 1 {
		t.Fatalf("Clusters len after replace = %d, want 1", len(got.Clusters))
	}
	att := got.Clusters[0]
	if att.InCluster || att.Kubeconfig != "/tmp/kc" || att.Context != "kind-kind" {
		t.Errorf("replace did not apply: %+v", att)
	}

	// Third attach for a different namespace: appended.
	if err := AttachCluster(AttachOptions{
		Tower:     "t1",
		Namespace: "spire-b",
		InCluster: true,
	}); err != nil {
		t.Fatalf("attach (3): %v", err)
	}
	got, err = config.LoadTowerConfig("t1")
	if err != nil {
		t.Fatalf("load tower (3): %v", err)
	}
	if len(got.Clusters) != 2 {
		t.Fatalf("Clusters len after second ns = %d, want 2", len(got.Clusters))
	}
}

func TestAttachCluster_PersistsJSONShape(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	tower := &config.TowerConfig{
		Name: "t2", ProjectID: "11111111-2222-4333-8444-555555555555",
		HubPrefix: "t2", Database: "beads_t2", CreatedAt: "2026-04-17T10:00:00Z",
	}
	if err := config.SaveTowerConfig(tower); err != nil {
		t.Fatalf("save tower: %v", err)
	}
	if err := AttachCluster(AttachOptions{
		Tower:     "t2",
		Namespace: "team-x",
		InCluster: true,
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	path := filepath.Join(tmpDir, ".config", "spire", "towers", "t2.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tower file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse tower file: %v", err)
	}
	clusters, ok := raw["clusters"].([]any)
	if !ok || len(clusters) != 1 {
		t.Fatalf("clusters key missing or wrong length: %v", raw["clusters"])
	}
	c := clusters[0].(map[string]any)
	if c["namespace"] != "team-x" {
		t.Errorf("namespace = %v, want team-x", c["namespace"])
	}
	if c["in_cluster"] != true {
		t.Errorf("in_cluster = %v, want true", c["in_cluster"])
	}
}
