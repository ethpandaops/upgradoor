// Package registryauth reads registry credentials from a Docker config.json.
package registryauth

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

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

// Login is the credential of one registry host.
type Login struct {
	Username string
	Password string
}

// Basic renders the value of a Basic Authorization header.
func (l Login) Basic() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(l.Username+":"+l.Password))
}

// Logins are the registry credentials by imageref.RegistryHost; lookups and
// pulls share them so both authenticate alike.
type Logins map[string]Login

// Lookup returns the credentials of a registry host (host.Login).
func (l Logins) Lookup(registryHost string) (username, password string, ok bool) {
	login, ok := l[registryHost]

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

// LoadLogins reads the "auths" of a Docker config.json; a missing file is no
// logins. Credential helpers, stores and identity tokens are not supported.
// When several keys name one registry, the key spelling the registry host
// wins, then a bare host alias, then a URL, then the key that sorts first.
func LoadLogins(log logrus.FieldLogger, path string) (Logins, error) {
	log = log.WithFields(logrus.Fields{"package": "registryauth", "auth_file": path})

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
		host := imageref.RegistryHost(key)
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

// authKeyRank ranks an "auths" key: 0 the registry host itself, 1 another
// bare host, 2 a URL.
func authKeyRank(key string) int {
	switch {
	case key == imageref.RegistryHost(key):
		return 0
	case !strings.Contains(key, "/"):
		return 1
	default:
		return 2
	}
}
