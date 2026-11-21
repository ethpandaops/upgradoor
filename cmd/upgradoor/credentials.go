package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/ethpandaops/upgradoor/pkg/identity"
)

// loadCredentials reads the host key and certificate.
func loadCredentials(keyFile, certFile string) (*identity.Credentials, error) {
	rawKey, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read host key: %w", err)
	}

	key, err := identity.DecodePrivateKey(string(rawKey))
	if err != nil {
		return nil, fmt.Errorf("host key %s: %w", keyFile, err)
	}

	rawCert, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("read host certificate: %w", err)
	}

	creds, err := identity.NewCredentials(key, string(bytes.TrimSpace(rawCert)))
	if err != nil {
		return nil, fmt.Errorf("host certificate %s: %w", certFile, err)
	}

	return creds, nil
}
