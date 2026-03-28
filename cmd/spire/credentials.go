// credentials.go provides backward-compatible wrappers delegating to pkg/config.
// Only wrappers that are actually called by production or test code in cmd/spire
// are kept. Dead wrappers (credentialsPath, loadCredentials, saveCredentials,
// listCredentials, deleteCredential, credEnvVars, credSpireEnvVars) were removed.
package main

import (
	"github.com/awell-health/spire/pkg/config"
)

// Re-export credential key constants actually used in cmd/spire.
// Dead aliases removed: CredKeyAnthropicKey, CredKeyGithubToken.
const (
	CredKeyDolthubUser     = config.CredKeyDolthubUser
	CredKeyDolthubPassword = config.CredKeyDolthubPassword
)

func isCredentialKey(key string) bool {
	return config.IsCredentialKey(key)
}

func validCredentialKeys() []string {
	return config.ValidCredentialKeys()
}

// --- Path-parameterized variants (used by tests) ---

func loadCredentialsFrom(path string) (map[string]string, error) {
	return config.LoadCredentialsFrom(path)
}

func saveCredentialsTo(path string, creds map[string]string) error {
	return config.SaveCredentialsTo(path, creds)
}

func getCredential(key string) string {
	return config.GetCredential(key)
}

func getCredentialFrom(path, key string) string {
	return config.GetCredentialFrom(path, key)
}

func credentialSource(key string) string {
	return config.CredentialSource(key)
}

func setCredential(key, value string) error {
	return config.SetCredential(key, value)
}

func setCredentialTo(path, key, value string) error {
	return config.SetCredentialTo(path, key, value)
}

func deleteCredentialFrom(path, key string) error {
	return config.DeleteCredentialFrom(path, key)
}

func maskValue(value string) string {
	return config.MaskValue(value)
}
