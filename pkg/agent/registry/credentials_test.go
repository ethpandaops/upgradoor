package registry

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestLoadLogins(t *testing.T) {
	path := writeAuthFile(t, `{
		"auths": {
			"https://index.docker.io/v1/": {"auth": "`+b64("hub:hub-pass")+`"},
			"GHCR.io": {"auth": "`+b64("gh:pa:ss:word")+`"},
			"http://plain.example:5000/": {"username": "plain", "password": "p"},
			"https://quay.io/v2/some/path": {"username": "quay", "password": ""},
			"empty.example": {},
			"padded.example": {"auth": "`+b64("pad:secret\x00\x00")+`"},
			"both.example": {"auth": "`+b64("from-auth:1")+`", "username": "from-fields", "password": "2"}
		},
		"HttpHeaders": {"User-Agent": "x"}
	}`)

	log, hook := logrustest.NewNullLogger()

	logins, err := LoadLogins(log, path)
	require.NoError(t, err)
	require.Empty(t, hook.AllEntries(), "nothing to warn about")
	require.Equal(t, Logins{
		"docker.io":          {Username: "hub", Password: "hub-pass"},
		"ghcr.io":            {Username: "gh", Password: "pa:ss:word"},
		"plain.example:5000": {Username: "plain", Password: "p"},
		"quay.io":            {Username: "quay", Password: ""},
		"padded.example":     {Username: "pad", Password: "secret"},
		"both.example":       {Username: "from-auth", Password: "1"},
	}, logins)

	require.Equal(t, "Basic "+b64("gh:pa:ss:word"), logins["ghcr.io"].basic())

	user, pass, ok := logins.Lookup("ghcr.io")
	require.True(t, ok)
	require.Equal(t, "gh", user)
	require.Equal(t, "pa:ss:word", pass)

	_, _, ok = logins.Lookup("other.example")
	require.False(t, ok)
}

// What cannot be used is said once, at start, and the registry is then
// accessed anonymously by the lookups and the pulls alike.
func TestLoadLoginsWarnsAboutWhatItCannotUse(t *testing.T) {
	for name, content := range map[string]string{
		"credential store":  `{"credsStore":"desktop"}`,
		"credential helper": `{"credHelpers":{"ghcr.io":"gh"},"auths":{"ghcr.io":{}}}`,
		"identity token":    `{"auths":{"ghcr.io":{"identitytoken":"secret-token"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			log, hook := logrustest.NewNullLogger()

			logins, err := LoadLogins(log, writeAuthFile(t, content))
			require.NoError(t, err)
			require.Empty(t, logins)
			require.Len(t, hook.AllEntries(), 1)
			assert.Equal(t, logrus.WarnLevel, hook.LastEntry().Level)
			assert.Contains(t, hook.LastEntry().Message, "not supported")

			line, err := hook.LastEntry().String()
			require.NoError(t, err)
			assert.NotContains(t, line, "secret-token")
		})
	}
}

func TestLoadLoginsMissingFileIsAnonymous(t *testing.T) {
	logins, err := LoadLogins(testLogger(), filepath.Join(t.TempDir(), "nope", "config.json"))
	require.NoError(t, err)
	require.Empty(t, logins)
}

func TestLoadLoginsErrors(t *testing.T) {
	const secret = "c2VjcmV0LXZhbHVl!"

	cases := []struct {
		name, content, want string
	}{
		{"not json", `{"auths": `, "parse auth file"},
		{"wrong shape", `{"auths": ["x"]}`, "parse auth file"},
		{"not base64", `{"auths":{"ghcr.io":{"auth":"` + secret + `"}}}`, "auth for ghcr.io is not base64"},
		{"no colon", `{"auths":{"ghcr.io":{"auth":"` + b64("justuser") + `"}}}`, "is not user:password"},
		{"empty user", `{"auths":{"ghcr.io":{"auth":"` + b64(":pass") + `"}}}`, "is not user:password"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeAuthFile(t, c.content)

			_, err := LoadLogins(testLogger(), path)
			require.ErrorContains(t, err, c.want)
			require.NotContains(t, err.Error(), secret)
			require.NotContains(t, err.Error(), "justuser")
		})
	}

	// A path that exists but cannot be read as a file.
	_, err := LoadLogins(testLogger(), t.TempDir())
	require.ErrorContains(t, err, "read auth file")
}

// TestLoadLoginsAliasPrecedence covers auth files in which several keys
// name one registry. Go randomizes map iteration, so every case is loaded
// many times: the winner must never change.
func TestLoadLoginsAliasPrecedence(t *testing.T) {
	entry := func(key, user string) string {
		return `"` + key + `": {"auth": "` + b64(user+":pw-"+user) + `"}`
	}

	cases := []struct {
		name    string
		entries []string
		host    string
		want    string
		used    string
		ignored []string
	}{
		{
			"exact host over the legacy index url",
			[]string{entry("https://index.docker.io/v1/", "legacy"), entry("docker.io", "exact")},
			imageref.DockerHubHost, "exact", "docker.io", []string{"https://index.docker.io/v1/"},
		},
		{
			"exact host over a bare alias over a url",
			[]string{
				entry("https://index.docker.io/v1/", "legacy"),
				entry("registry-1.docker.io", "alias"),
				entry("docker.io", "exact"),
			},
			imageref.DockerHubHost, "exact", "docker.io", []string{"registry-1.docker.io", "https://index.docker.io/v1/"},
		},
		{
			"bare alias over a url",
			[]string{entry("https://index.docker.io/v1/", "legacy"), entry("registry-1.docker.io", "alias")},
			imageref.DockerHubHost, "alias", "registry-1.docker.io", []string{"https://index.docker.io/v1/"},
		},
		{
			"equal rank: the key that sorts first",
			[]string{entry("registry-1.docker.io", "api"), entry("index.docker.io", "index")},
			imageref.DockerHubHost, "index", "index.docker.io", []string{"registry-1.docker.io"},
		},
		{
			"urls of one host",
			[]string{entry("https://ghcr.io/v2/", "v2"), entry("http://ghcr.io", "http")},
			"ghcr.io", "http", "http://ghcr.io", []string{"https://ghcr.io/v2/"},
		},
		{
			"an entry without a login does not shadow one with",
			[]string{`"docker.io": {}`, entry("https://index.docker.io/v1/", "legacy")},
			imageref.DockerHubHost, "legacy", "", nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeAuthFile(t, `{"auths": {`+strings.Join(c.entries, ",")+`}}`)

			for range 64 {
				log, hook := logrustest.NewNullLogger()

				logins, err := LoadLogins(log, path)
				require.NoError(t, err)
				require.Equal(t, Logins{c.host: {Username: c.want, Password: "pw-" + c.want}}, logins)

				ignored := make([]string, 0, len(c.ignored))

				for _, e := range hook.AllEntries() {
					assert.Equal(t, logrus.WarnLevel, e.Level)
					assert.Equal(t, c.host, e.Data["registry"])
					assert.Equal(t, c.used, e.Data["used"])

					ignored = append(ignored, fmt.Sprint(e.Data["ignored"]))

					line, err := e.String()
					require.NoError(t, err)
					assert.NotContains(t, line, "pw-", "passwords stay out of the log")
					assert.NotContains(t, line, b64(c.want+":pw-"+c.want))
				}

				if len(c.ignored) == 0 {
					require.Empty(t, ignored)
				} else {
					require.Equal(t, c.ignored, ignored)
				}
			}
		})
	}
}
