// credentials.go provides backward-compatible wrappers delegating to pkg/config.
package main

import (
	"github.com/awell-health/spire/pkg/config"
)

// Re-export credential key constants so existing cmd/spire code compiles unchanged.
const (
	CredKeyAnthropicKey    = config.CredKeyAnthropicKey
	CredKeyGithubToken     = config.CredKeyGithubToken
	CredKeyDolthubUser     = config.CredKeyDolthubUser
	CredKeyDolthubPassword = config.CredKeyDolthubPassword
)

// Re-export credential env var maps.
var credEnvVars = config.CredEnvVars
var credSpireEnvVars = config.CredSpireEnvVars

func credentialsPath() (string, error) {
	return config.CredentialsPath()
}

func isCredentialKey(key string) bool {
	return config.IsCredentialKey(key)
}

func validCredentialKeys() []string {
	return config.ValidCredentialKeys()
}

func loadCredentials() (map[string]string, error) {
	return config.LoadCredentials()
}

func loadCredentialsFrom(path string) (map[string]string, error) {
	return config.LoadCredentialsFrom(path)
}

func saveCredentials(creds map[string]string) error {
	return config.SaveCredentials(creds)
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

func deleteCredential(key string) error {
	return config.DeleteCredential(key)
}

func deleteCredentialFrom(path, key string) error {
	return config.DeleteCredentialFrom(path, key)
}

func listCredentials() (map[string]string, error) {
	return config.ListCredentials()
}

func maskValue(value string) string {
	return config.MaskValue(value)
}
