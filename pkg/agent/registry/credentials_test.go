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
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestLoadCredentials(t *testing.T) {
	path := writeAuthFile(t, `{
		"auths": {
			"https://index.docker.io/v1/": {"auth": "`+b64("hub:hub-pass")+`"},
			"GHCR.io": {"auth": "`+b64("gh:pa:ss:word")+`"},
			"http://plain.example:5000/": {"username": "plain", "password": "p"},
			"https://quay.io/v2/some/path": {"username": "quay", "password": ""},
			"empty.example": {},
			"identity.example": {"identitytoken": "ignored"},
			"both.example": {"auth": "`+b64("from-auth:1")+`", "username": "from-fields", "password": "2"}
		},
		"HttpHeaders": {"User-Agent": "x"}
	}`)

	creds, unsupported, err := loadCredentials(testLogger(), path)
	require.NoError(t, err)
	require.False(t, unsupported)
	require.Equal(t, map[string]credentials{
		"docker.io":          {username: "hub", password: "hub-pass"},
		"ghcr.io":            {username: "gh", password: "pa:ss:word"},
		"plain.example:5000": {username: "plain", password: "p"},
		"quay.io":            {username: "quay", password: ""},
		"both.example":       {username: "from-auth", password: "1"},
	}, creds)

	require.Equal(t, "Basic "+b64("gh:pa:ss:word"), creds["ghcr.io"].basic())
}

func TestAuthHost(t *testing.T) {
	cases := map[string]string{
		"https://index.docker.io/v1/": dockerHubHost,
		"index.docker.io":             dockerHubHost,
		"registry-1.docker.io":        dockerHubHost,
		"docker.io":                   dockerHubHost,
		"https://Docker.IO":           dockerHubHost,
		"ghcr.io":                     "ghcr.io",
		"http://localhost:5000/v2/":   "localhost:5000",
		"":                            "",
	}

	for in, want := range cases {
		require.Equal(t, want, authHost(in), in)
	}
}

func TestLoadCredentialsMissingFileIsAnonymous(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope", "config.json")

	creds, unsupported, err := loadCredentials(testLogger(), missing)
	require.NoError(t, err)
	require.False(t, unsupported)
	require.Empty(t, creds)

	r, err := New(testLogger(), Options{AuthFile: missing})
	require.NoError(t, err)
	require.Empty(t, r.creds)
}

func TestLoadCredentialsErrors(t *testing.T) {
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

			_, _, err := loadCredentials(testLogger(), path)
			require.ErrorContains(t, err, c.want)
			require.NotContains(t, err.Error(), secret)
			require.NotContains(t, err.Error(), "justuser")

			_, err = New(testLogger(), Options{AuthFile: path})
			require.ErrorContains(t, err, c.want)
		})
	}

	// A path that exists but cannot be read as a file.
	_, _, err := loadCredentials(testLogger(), t.TempDir())
	require.ErrorContains(t, err, "read auth file")
}

// TestLoadCredentialsAliasPrecedence covers auth files in which several keys
// name one registry. Go randomizes map iteration, so every case is loaded
// many times: the winner must never change.
func TestLoadCredentialsAliasPrecedence(t *testing.T) {
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
			dockerHubHost, "exact", "docker.io", []string{"https://index.docker.io/v1/"},
		},
		{
			"exact host over a bare alias over a url",
			[]string{
				entry("https://index.docker.io/v1/", "legacy"),
				entry("registry-1.docker.io", "alias"),
				entry("docker.io", "exact"),
			},
			dockerHubHost, "exact", "docker.io", []string{"registry-1.docker.io", "https://index.docker.io/v1/"},
		},
		{
			"bare alias over a url",
			[]string{entry("https://index.docker.io/v1/", "legacy"), entry("registry-1.docker.io", "alias")},
			dockerHubHost, "alias", "registry-1.docker.io", []string{"https://index.docker.io/v1/"},
		},
		{
			"equal rank: the key that sorts first",
			[]string{entry("registry-1.docker.io", "api"), entry("index.docker.io", "index")},
			dockerHubHost, "index", "index.docker.io", []string{"registry-1.docker.io"},
		},
		{
			"urls of one host",
			[]string{entry("https://ghcr.io/v2/", "v2"), entry("http://ghcr.io", "http")},
			"ghcr.io", "http", "http://ghcr.io", []string{"https://ghcr.io/v2/"},
		},
		{
			"an entry without a login does not shadow one with",
			[]string{`"docker.io": {}`, entry("https://index.docker.io/v1/", "legacy")},
			dockerHubHost, "legacy", "", nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := writeAuthFile(t, `{"auths": {`+strings.Join(c.entries, ",")+`}}`)

			for range 64 {
				log, hook := logrustest.NewNullLogger()

				creds, _, err := loadCredentials(log, path)
				require.NoError(t, err)
				require.Equal(t, map[string]credentials{
					c.host: {username: c.want, password: "pw-" + c.want},
				}, creds)

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

func TestAuthKeyRank(t *testing.T) {
	cases := map[string]int{
		"docker.io":                   0,
		"ghcr.io":                     0,
		"localhost:5000":              0,
		"registry-1.docker.io":        1,
		"index.docker.io":             1,
		"GHCR.io":                     1,
		"https://index.docker.io/v1/": 2,
		"http://ghcr.io":              2,
		"ghcr.io/v2/":                 2,
	}

	for key, want := range cases {
		require.Equal(t, want, authKeyRank(key), key)
	}
}
