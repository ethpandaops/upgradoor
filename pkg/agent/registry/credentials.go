package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent"
)

// Login is the login of one registry.
type Login struct {
	Username string
	Password string
}

// basic renders the value of a Basic Authorization header.
func (l Login) basic() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(l.Username+":"+l.Password))
}

// Logins are registry logins by registry host, as agent.Reference.Host
// spells it. The registry lookups and the image pulls both take theirs from
// here, so that a poll and a pull of one image authenticate alike.
type Logins map[string]Login

// Lookup returns the login for a registry host.
func (l Logins) Lookup(host string) (username, password string, ok bool) {
	login, ok := l[host]

	return login.Username, login.Password, ok
}

// dockerConfig is the part of a Docker config.json this package reads.
type dockerConfig struct {
	Auths map[string]struct {
		Auth          string `json:"auth"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		IdentityToken string `json:"identitytoken"`
	} `json:"auths"`
	CredHelpers map[string]string `json:"credHelpers"`
	CredsStore  string            `json:"credsStore"`
}

// LoadLogins reads the "auths" map of a Docker config.json (an "auth" value
// or username and password per registry). The file is read once: a changed
// login takes effect when the agent is restarted. A missing file yields no
// logins (anonymous access); an unreadable or malformed one is an error.
//
// Credential helpers ("credHelpers", "credsStore") and identity tokens are
// not supported: such a registry is accessed anonymously, and a warning says
// so.
//
// Several keys can name one registry (docker.io, registry-1.docker.io and
// https://index.docker.io/v1/ are all Docker Hub). The entry used is then
// always the same one: the key that spells the registry host itself, else a
// bare host alias, else a URL; among equals the key that sorts first. The
// entries passed over are logged, by key only.
func LoadLogins(log logrus.FieldLogger, path string) (Logins, error) {
	log = log.WithFields(logrus.Fields{"package": "registry", "auth_file": path})

	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Logins{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("registry: read auth file: %w", err)
	}

	var cfg dockerConfig
	if jsonErr := json.Unmarshal(raw, &cfg); jsonErr != nil {
		return nil, fmt.Errorf("registry: parse auth file %s: %w", path, jsonErr)
	}

	if len(cfg.CredHelpers) > 0 || cfg.CredsStore != "" {
		log.Warn("auth file names a credential helper or store, which is not supported; " +
			"only its \"auths\" entries are used")
	}

	keys := make([]string, 0, len(cfg.Auths))
	for key := range cfg.Auths {
		keys = append(keys, key)
	}

	// Most specific spelling first, so that the first entry for a host wins.
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := authKeyRank(keys[i]), authKeyRank(keys[j])
		if ri != rj {
			return ri < rj
		}

		return keys[i] < keys[j]
	})

	logins := make(Logins, len(keys))
	usedKey := make(map[string]string, len(keys))

	for _, key := range keys {
		entry := cfg.Auths[key]
		host := agent.RegistryHost(key)
		login := Login{Username: entry.Username, Password: entry.Password}

		switch {
		case entry.Auth != "":
			dec, decErr := base64.StdEncoding.DecodeString(entry.Auth)
			if decErr != nil {
				// The decode error quotes the input, which is a secret.
				return nil, fmt.Errorf("registry: auth file %s: auth for %s is not base64", path, host)
			}

			user, pass, ok := strings.Cut(string(dec), ":")
			if !ok || user == "" {
				return nil, fmt.Errorf("registry: auth file %s: auth for %s is not user:password", path, host)
			}

			// Docker pads some stored passwords with NUL bytes.
			login = Login{Username: user, Password: strings.TrimRight(pass, "\x00")}
		case entry.Username != "":
		case entry.IdentityToken != "":
			log.WithField("registry", host).Warn(
				"auth file entry holds an identity token, which is not supported; it is ignored")

			continue
		default:
			continue
		}

		if used, taken := usedKey[host]; taken {
			log.WithFields(logrus.Fields{"registry": host, "used": used, "ignored": key}).
				Warn("auth file holds several entries for one registry; using the more specific one")

			continue
		}

		logins[host] = login
		usedKey[host] = key
	}

	return logins, nil
}

// authKeyRank orders the keys of the "auths" map that name one registry:
// 0 for the registry host as agent.Reference.Host spells it, 1 for another
// bare host (a Docker Hub alias, or a different case), 2 for a URL.
func authKeyRank(key string) int {
	switch {
	case key == agent.RegistryHost(key):
		return 0
	case !strings.Contains(key, "/"):
		return 1
	default:
		return 2
	}
}
