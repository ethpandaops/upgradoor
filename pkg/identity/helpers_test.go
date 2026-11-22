package identity

import (
	"crypto/mldsa"
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
)

const testSeed = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// hostKey returns a fixed host key per name.
func hostKey(t *testing.T, name string) *mldsa.PrivateKey {
	t.Helper()

	sum := sha256.Sum256([]byte("test-host/" + name))

	key, err := mldsa.NewPrivateKey(params(), sum[:])
	require.NoError(t, err)

	return key
}
