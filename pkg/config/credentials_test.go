package config

import (
	"testing"
)

// setupCredTest isolates credential file I/O to a temp dir and unsets any
// real env vars that would leak state across tests.
func setupCredTest(t *testing.T) {
	t.Helper()
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("DOLT_REMOTE_USER", "")
	t.Setenv("DOLT_REMOTE_PASSWORD", "")
	t.Setenv("SPIRE_DOLTHUB_USER", "")
	t.Setenv("SPIRE_DOLTHUB_PASSWORD", "")
}

func TestSetRemotesapiCredentials_RoundTrip(t *testing.T) {
	setupCredTest(t)

	if err := SetRemotesapiCredentials("mytower", "alice", "s3cret"); err != nil {
		t.Fatalf("SetRemotesapiCredentials: %v", err)
	}
	if got := GetRemotesapiUser("mytower"); got != "alice" {
		t.Fatalf("GetRemotesapiUser = %q, want %q", got, "alice")
	}
	if got := GetRemotesapiPassword("mytower"); got != "s3cret" {
		t.Fatalf("GetRemotesapiPassword = %q, want %q", got, "s3cret")
	}
}

func TestSetRemotesapiCredentials_PerTowerIsolation(t *testing.T) {
	setupCredTest(t)

	if err := SetRemotesapiCredentials("dev", "alice", "devpw"); err != nil {
		t.Fatalf("set dev: %v", err)
	}
	if err := SetRemotesapiCredentials("prod", "bob", "prodpw"); err != nil {
		t.Fatalf("set prod: %v", err)
	}

	if got := GetRemotesapiUser("dev"); got != "alice" {
		t.Fatalf("dev user = %q, want alice", got)
	}
	if got := GetRemotesapiUser("prod"); got != "bob" {
		t.Fatalf("prod user = %q, want bob", got)
	}
	if got := GetRemotesapiPassword("dev"); got != "devpw" {
		t.Fatalf("dev password = %q, want devpw", got)
	}
	if got := GetRemotesapiPassword("prod"); got != "prodpw" {
		t.Fatalf("prod password = %q, want prodpw", got)
	}
}

func TestGetRemotesapiUser_EnvPrecedence(t *testing.T) {
	tests := []struct {
		name         string
		fileUser     string
		spireEnv     string
		standardEnv  string
		wantUser     string
	}{
		{"file only", "from-file", "", "", "from-file"},
		{"DOLT_REMOTE_USER overrides file", "from-file", "", "from-env", "from-env"},
		{"SPIRE_DOLTHUB_USER overrides all", "from-file", "from-spire", "from-env", "from-spire"},
		{"no file, env only", "", "", "from-env", "from-env"},
		{"all empty", "", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupCredTest(t)
			if tc.fileUser != "" {
				if err := SetRemotesapiCredentials("t", tc.fileUser, "anypw"); err != nil {
					t.Fatalf("SetRemotesapiCredentials: %v", err)
				}
			}
			t.Setenv("SPIRE_DOLTHUB_USER", tc.spireEnv)
			t.Setenv("DOLT_REMOTE_USER", tc.standardEnv)

			if got := GetRemotesapiUser("t"); got != tc.wantUser {
				t.Fatalf("GetRemotesapiUser = %q, want %q", got, tc.wantUser)
			}
		})
	}
}

func TestGetRemotesapiPassword_EnvPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		filePw      string
		spireEnv    string
		standardEnv string
		wantPw      string
	}{
		{"file only", "from-file", "", "", "from-file"},
		{"DOLT_REMOTE_PASSWORD overrides file", "from-file", "", "from-env", "from-env"},
		{"SPIRE_DOLTHUB_PASSWORD overrides all", "from-file", "from-spire", "from-env", "from-spire"},
		{"no file, env only", "", "", "from-env", "from-env"},
		{"all empty", "", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setupCredTest(t)
			if tc.filePw != "" {
				if err := SetRemotesapiCredentials("t", "anyuser", tc.filePw); err != nil {
					t.Fatalf("SetRemotesapiCredentials: %v", err)
				}
			}
			t.Setenv("SPIRE_DOLTHUB_PASSWORD", tc.spireEnv)
			t.Setenv("DOLT_REMOTE_PASSWORD", tc.standardEnv)

			if got := GetRemotesapiPassword("t"); got != tc.wantPw {
				t.Fatalf("GetRemotesapiPassword = %q, want %q", got, tc.wantPw)
			}
		})
	}
}

func TestDeleteRemotesapiCredentials_RemovesBoth(t *testing.T) {
	setupCredTest(t)

	if err := SetRemotesapiCredentials("tower", "alice", "pw"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := DeleteRemotesapiCredentials("tower"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := GetRemotesapiUser("tower"); got != "" {
		t.Fatalf("GetRemotesapiUser after delete = %q, want empty", got)
	}
	if got := GetRemotesapiPassword("tower"); got != "" {
		t.Fatalf("GetRemotesapiPassword after delete = %q, want empty", got)
	}
}

func TestDeleteRemotesapiCredentials_IdempotentOnMissing(t *testing.T) {
	setupCredTest(t)

	// Deleting creds that never existed should not error.
	if err := DeleteRemotesapiCredentials("never-set"); err != nil {
		t.Fatalf("DeleteRemotesapiCredentials on missing tower: %v", err)
	}
}

func TestRemoteCredentials_NilTower_UsesDolthub(t *testing.T) {
	setupCredTest(t)

	if err := SetCredential(CredKeyDolthubUser, "dh-user"); err != nil {
		t.Fatalf("SetCredential user: %v", err)
	}
	if err := SetCredential(CredKeyDolthubPassword, "dh-pw"); err != nil {
		t.Fatalf("SetCredential pw: %v", err)
	}

	user, pw := RemoteCredentials(nil)
	if user != "dh-user" || pw != "dh-pw" {
		t.Fatalf("RemoteCredentials(nil) = (%q, %q), want (dh-user, dh-pw)", user, pw)
	}
}

func TestRemoteCredentials_DolthubKind(t *testing.T) {
	setupCredTest(t)

	if err := SetCredential(CredKeyDolthubUser, "dh-user"); err != nil {
		t.Fatalf("SetCredential user: %v", err)
	}
	if err := SetCredential(CredKeyDolthubPassword, "dh-pw"); err != nil {
		t.Fatalf("SetCredential pw: %v", err)
	}

	// Explicit and empty (legacy) both resolve to dolthub.
	for _, kind := range []string{"", RemoteKindDoltHub} {
		tower := &TowerConfig{Name: "t", RemoteKind: kind}
		user, pw := RemoteCredentials(tower)
		if user != "dh-user" || pw != "dh-pw" {
			t.Fatalf("RemoteCredentials(kind=%q) = (%q, %q), want (dh-user, dh-pw)", kind, user, pw)
		}
	}
}

func TestRemoteCredentials_RemotesapiKind(t *testing.T) {
	setupCredTest(t)

	// Populate both kinds; make sure remotesapi tower reads remotesapi creds.
	if err := SetCredential(CredKeyDolthubUser, "dh-user"); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	if err := SetCredential(CredKeyDolthubPassword, "dh-pw"); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	if err := SetRemotesapiCredentials("cluster", "cluster-user", "cluster-pw"); err != nil {
		t.Fatalf("SetRemotesapiCredentials: %v", err)
	}

	tower := &TowerConfig{Name: "cluster", RemoteKind: RemoteKindRemotesAPI}
	user, pw := RemoteCredentials(tower)
	if user != "cluster-user" || pw != "cluster-pw" {
		t.Fatalf("RemoteCredentials = (%q, %q), want (cluster-user, cluster-pw)", user, pw)
	}
}

func TestRemoteCredentials_RemotesapiFallsBackToRemoteUser(t *testing.T) {
	setupCredTest(t)

	// File has no remotesapi creds, but tower config records a RemoteUser.
	// The fallback should surface that user even without a stored file entry.
	tower := &TowerConfig{
		Name:       "cluster",
		RemoteKind: RemoteKindRemotesAPI,
		RemoteUser: "fallback-user",
	}
	user, _ := RemoteCredentials(tower)
	if user != "fallback-user" {
		t.Fatalf("RemoteCredentials user = %q, want fallback-user", user)
	}
}

func TestSanitizeTowerCredKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"simple", "simple"},
		{"Mixed-Case", "mixed-case"},
		{"with.dots", "with_dots"},
		{"spaces and stuff", "spaces_and_stuff"},
		{"", "default"},
		{"123abc", "123abc"},
	}
	for _, tc := range tests {
		if got := sanitizeTowerCredKey(tc.in); got != tc.want {
			t.Errorf("sanitizeTowerCredKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
