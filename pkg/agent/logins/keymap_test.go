package logins

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fingerprints of the keys of testdata/authorized_keys, by ssh-keygen -lf.
const (
	fpGitHub  = "SHA256:PIVpPQL34ZOi+Stv5JaVVcp1lrNzt/o/mfpzDa/OUig"
	fpComment = "SHA256:FZ4MZgjwAfMUDndwBOzqL8RsJ7K+WPJyrUUxRTFDINc"
	fpPlain   = "SHA256:+fWzx8W2ouNB2hPe/AQjfRCtsjuC+5ratJhC7Mx9bVU"
)

func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	t.Parallel()

	blob, err := base64.StdEncoding.DecodeString(
		"AAAAC3NzaC1lZDI1NTE5AAAAIEYaAj19HR+Rcy0KgQzqQv/pxAxOiMAIBmYEWIPV935u")
	require.NoError(t, err)

	assert.Equal(t, fpComment, Fingerprint(blob))
}

func TestAddAuthorizedKeys(t *testing.T) {
	t.Parallel()

	m := make(KeyMap, 4)
	m.AddAuthorizedKeys("devops", readFixture(t, "authorized_keys"))

	assert.Equal(t, KeyMap{
		fpGitHub:  {Identity: "github:pk910", Source: SourceGitHub, User: "devops"},
		fpComment: {Identity: "ops@laptop", Source: SourceComment, User: "devops"},
	}, m, "a key without a comment outside a GitHub block is unknown")

	identity, source := m.Owner(fpGitHub)
	assert.Equal(t, "github:pk910", identity)
	assert.Equal(t, SourceGitHub, source)

	identity, source = m.Owner(fpPlain)
	assert.Empty(t, identity)
	assert.Equal(t, SourceUnknown, source)

	var none KeyMap

	_, source = none.Owner(fpGitHub)
	assert.Equal(t, SourceUnknown, source, "no key map knows no key")
}

func TestAddAuthorizedKeysBlocks(t *testing.T) {
	t.Parallel()

	const (
		ed25519 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAID3VbaOIc2FFe42xDOrji5KfnEFcZc8KA6VbPj18Cvok"
		other   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEYaAj19HR+Rcy0KgQzqQv/pxAxOiMAIBmYEWIPV935u"
	)

	m := make(KeyMap, 4)
	m.AddAuthorizedKeys("root", []byte("# a comment\n"+ed25519+" root@box\n"))
	m.AddAuthorizedKeys("devops", []byte("# github:pk910\n"+ed25519+"\n# managed by ansible\n"+other+"\n"))

	assert.Equal(t, KeyOwner{Identity: "github:pk910", Source: SourceGitHub, User: "devops"}, m[fpGitHub],
		"a GitHub owner replaces a comment")
	assert.NotContains(t, m, fpComment, "a comment line ends the GitHub block")

	m.AddAuthorizedKeys("other", []byte(ed25519+" someone\n"))
	assert.Equal(t, "github:pk910", m[fpGitHub].Identity, "a comment does not replace a GitHub owner")

	bad := make(KeyMap, 1)
	bad.AddAuthorizedKeys("devops", []byte("# github:-bad-\n"+ed25519+"\nssh-ed25519 notbase64 x\n"))
	assert.Empty(t, bad, "neither an invalid GitHub name nor an invalid key")
}

func TestParseKeyMap(t *testing.T) {
	t.Parallel()

	m, err := ParseKeyMap([]byte(`{"` + fpGitHub + `":{"identity":"github:pk910","source":"github","user":"devops"}}`))
	require.NoError(t, err)
	assert.Equal(t, "github:pk910", m[fpGitHub].Identity)

	_, err = ParseKeyMap([]byte(`[`))
	require.Error(t, err)
}
