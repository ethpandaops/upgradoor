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
)

// credentials is one registry login.
type credentials struct {
	username string
	password string
}

// basic renders the value of a Basic Authorization header.
func (c credentials) basic() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.username+":"+c.password))
}

// dockerConfig is the part of a Docker config.json this package reads.
type dockerConfig struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
	CredHelpers map[string]string `json:"credHelpers"`
	CredsStore  string            `json:"credsStore"`
}

// loadCredentials reads the "auths" map of a Docker config.json. A missing
// file yields no credentials (anonymous access); an unreadable or malformed
// one is an error. unsupported reports that the file names a credential
// helper or store, which this package cannot use.
//
// Several keys can name one registry (docker.io, registry-1.docker.io and
// https://index.docker.io/v1/ are all Docker Hub). The entry used is then
// always the same one: the key that spells the registry host itself, else a
// bare host alias, else a URL; among equals the key that sorts first. The
// entries passed over are logged, by key only.
func loadCredentials(
	log logrus.FieldLogger, path string,
) (creds map[string]credentials, unsupported bool, err error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]credentials{}, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("registry: read auth file: %w", err)
	}

	var cfg dockerConfig
	if jsonErr := json.Unmarshal(raw, &cfg); jsonErr != nil {
		return nil, false, fmt.Errorf("registry: parse auth file %s: %w", path, jsonErr)
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

	creds = make(map[string]credentials, len(keys))
	usedKey := make(map[string]string, len(keys))

	for _, key := range keys {
		entry := cfg.Auths[key]
		host := authHost(key)

		var login credentials

		switch {
		case entry.Auth != "":
			dec, decErr := base64.StdEncoding.DecodeString(entry.Auth)
			if decErr != nil {
				// The decode error quotes the input, which is a secret.
				return nil, false, fmt.Errorf("registry: auth file %s: auth for %s is not base64", path, host)
			}

			user, pass, ok := strings.Cut(string(dec), ":")
			if !ok || user == "" {
				return nil, false, fmt.Errorf("registry: auth file %s: auth for %s is not user:password", path, host)
			}

			login = credentials{username: user, password: pass}
		case entry.Username != "":
			login = credentials{username: entry.Username, password: entry.Password}
		default:
			continue
		}

		if used, taken := usedKey[host]; taken {
			log.WithFields(logrus.Fields{
				"auth_file": path,
				"registry":  host,
				"used":      used,
				"ignored":   key,
			}).Warn("auth file holds several entries for one registry; using the more specific one")

			continue
		}

		creds[host] = login
		usedKey[host] = key
	}

	return creds, len(cfg.CredHelpers) > 0 || cfg.CredsStore != "", nil
}

// authKeyRank orders the keys of the "auths" map that name one registry:
// 0 for the registry host as Reference.Host spells it, 1 for another bare
// host (a Docker Hub alias, or a different case), 2 for a URL.
func authKeyRank(key string) int {
	switch {
	case key == authHost(key):
		return 0
	case !strings.Contains(key, "/"):
		return 1
	default:
		return 2
	}
}

// authHost maps a key of the "auths" map to the registry host as
// Reference.Host spells it. Docker writes Docker Hub logins under
// https://index.docker.io/v1/.
func authHost(key string) string {
	host := strings.ToLower(key)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host, _, _ = strings.Cut(host, "/")

	if host == dockerHubIndex || host == dockerHubAPIHost {
		return dockerHubHost
	}

	return host
}
