package identity

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCertificateEncoding(t *testing.T) {
	cert, err := IssueCertificate([]byte(testSeed), CertificateSpec{
		Network: "net", Hostname: "host-1",
		Roles: []string{"b", "a", "b"},
	})
	require.NoError(t, err)

	parts := strings.Split(cert, ".")
	require.Len(t, parts, 3)

	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	assert.Equal(t, `{"alg":"ML-DSA-44","typ":"JWT"}`, string(header))

	host, err := DeriveHostKey([]byte(testSeed), "net", "host-1")
	require.NoError(t, err)

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	assert.Equal(t, `{"iss":"upgradoor-signer","net":"net","sub":"host-1",`+
		`"cnf":{"jwk":{"kty":"AKP","alg":"ML-DSA-44","pub":"`+EncodePublicKey(PublicKey(host))+`"}},`+
		`"roles":["a","b"]}`, string(payload))

	noRoles, err := IssueCertificate([]byte(testSeed), CertificateSpec{
		Network: "net", Hostname: "host-1",
	})
	require.NoError(t, err)

	payload, err = base64.RawURLEncoding.DecodeString(strings.Split(noRoles, ".")[1])
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(string(payload), `"roles":[]}`))
}
