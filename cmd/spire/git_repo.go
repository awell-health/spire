package main

// git_repo.go — RepoContext and gitConfigGet have been extracted to pkg/git.
// This file remains as a tombstone to prevent accidental recreation.
// All callers now import "github.com/awell-health/spire/pkg/git".
//
// See: pkg/git/repo.go

import (
	spgit "github.com/awell-health/spire/pkg/git"
)

// gitConfigGet is kept as a thin wrapper for callers within cmd/spire that
// referenced the old unexported function. Delegates to git.ConfigGet.
func gitConfigGet(args ...string) string {
	return spgit.ConfigGet(args...)
}
