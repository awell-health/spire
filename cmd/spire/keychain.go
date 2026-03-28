// keychain.go provides backward-compatible wrappers delegating to pkg/config.
package main

import "github.com/awell-health/spire/pkg/config"

func keychainSet(key, value string) error {
	return config.KeychainSet(key, value)
}

func keychainGet(key string) (string, error) {
	return config.KeychainGet(key)
}

func keychainDelete(key string) error {
	return config.KeychainDelete(key)
}
