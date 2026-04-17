package main

import (
	"strings"
	"testing"
)

func stubDesignDeps(t *testing.T) func() {
	t.Helper()
	origCreate := designCreateBeadFunc
	origAddLabel := designAddLabelFunc
	origResolvePrefix := designResolvePrefixFunc
	origRequireApproval := designRequireApprovalFunc

	designResolvePrefixFunc = func(explicit string) (string, error) {
		if explicit != "" {
			return explicit, nil
		}
		return "spi", nil
	}

	return func() {
		designCreateBeadFunc = origCreate
		designAddLabelFunc = origAddLabel
		designResolvePrefixFunc = origResolvePrefix
		designRequireApprovalFunc = origRequireApproval
	}
}

func TestDesign_CreatesBeadWithTypeDesign(t *testing.T) {
	cleanup := stubDesignDeps(t)
	defer cleanup()

	var createdOpts createOpts
	designCreateBeadFunc = func(opts createOpts) (string, error) {
		createdOpts = opts
		return "spi-des01", nil
	}
	designAddLabelFunc = func(id, label string) error { return nil }
	designRequireApprovalFunc = func() bool { return true }

	if err := cmdDesign([]string{"My design", "--prefix", "spi"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if createdOpts.Title != "My design" {
		t.Errorf("expected title 'My design', got %q", createdOpts.Title)
	}
	if createdOpts.Type != parseIssueType("design") {
		t.Errorf("expected type design, got %v", createdOpts.Type)
	}
}

func TestDesign_RequireApprovalTrue_AddsNeedsHuman(t *testing.T) {
	cleanup := stubDesignDeps(t)
	defer cleanup()

	designCreateBeadFunc = func(opts createOpts) (string, error) {
		return "spi-des02", nil
	}
	designRequireApprovalFunc = func() bool { return true }

	var labeledID, labeledName string
	designAddLabelFunc = func(id, label string) error {
		labeledID = id
		labeledName = label
		return nil
	}

	if err := cmdDesign([]string{"My design", "--prefix", "spi"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if labeledID != "spi-des02" {
		t.Errorf("expected label on spi-des02, got %q", labeledID)
	}
	if labeledName != "needs-human" {
		t.Errorf("expected needs-human label, got %q", labeledName)
	}
}

func TestDesign_RequireApprovalFalse_NoLabel(t *testing.T) {
	cleanup := stubDesignDeps(t)
	defer cleanup()

	designCreateBeadFunc = func(opts createOpts) (string, error) {
		return "spi-des03", nil
	}
	designRequireApprovalFunc = func() bool { return false }

	labelCalled := false
	designAddLabelFunc = func(id, label string) error {
		labelCalled = true
		return nil
	}

	if err := cmdDesign([]string{"My design", "--prefix", "spi"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if labelCalled {
		t.Error("expected no label to be added when require_approval is false")
	}
}

func TestDesign_NoStatusUpdate(t *testing.T) {
	cleanup := stubDesignDeps(t)
	defer cleanup()

	designCreateBeadFunc = func(opts createOpts) (string, error) {
		return "spi-des04", nil
	}
	designRequireApprovalFunc = func() bool { return true }
	designAddLabelFunc = func(id, label string) error { return nil }

	if err := cmdDesign([]string{"My design", "--prefix", "spi"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDesign_RequiresTitle(t *testing.T) {
	cleanup := stubDesignDeps(t)
	defer cleanup()

	err := cmdDesign([]string{"--prefix", "spi"})
	if err == nil {
		t.Fatal("expected error for missing title")
	}
	if !strings.Contains(err.Error(), "title is required") {
		t.Errorf("expected 'title is required' in error, got: %v", err)
	}
}
