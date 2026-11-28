package logins

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
)

// KeyMap maps key fingerprints to who the keys belong to: the file
// "upgradoor ssh-keymap" writes and the agent reads.
type KeyMap map[string]KeyOwner

// KeyOwner is who a key belongs to.
type KeyOwner struct {
	// Identity is "github:<name>" or the key's comment.
	Identity string `json:"identity"`
	// Source is SourceGitHub or SourceComment.
	Source string `json:"source"`
	// User is the login user whose authorized_keys holds the key.
	User string `json:"user"`
}

// Key sources.
const (
	SourceGitHub  = "github"
	SourceComment = "comment"
	SourceUnknown = "unknown"
)

// githubMarker starts the comment line before the keys of a GitHub user.
const githubMarker = "github:"

// maxIdentity bounds an identity taken from a comment.
const maxIdentity = 128

// ParseKeyMap decodes a key map file.
func ParseKeyMap(raw []byte) (KeyMap, error) {
	var m KeyMap
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("key map: %w", err)
	}

	return m, nil
}

// Owner returns who the key with a fingerprint belongs to: its identity and
// source, or "" and SourceUnknown.
func (m KeyMap) Owner(fingerprint string) (identity, source string) {
	if o, ok := m[fingerprint]; ok && o.Identity != "" {
		return o.Identity, o.Source
	}

	return "", SourceUnknown
}

// AddAuthorizedKeys adds the keys of a login user's authorized_keys. A key
// after a "# github:<name>" line, up to the next comment or blank line, is
// github:<name>'s; any other key is its comment's; a key without a comment
// is left out. A fingerprint already in the map keeps its owner, unless the
// new one is a GitHub user and the old one is not.
func (m KeyMap) AddAuthorizedKeys(user string, content []byte) {
	github := ""

	sc := bufio.NewScanner(strings.NewReader(string(content)))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		switch {
		case line == "":
			github = ""

			continue
		case strings.HasPrefix(line, "#"):
			github = ""

			name, ok := strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(line, "#")), githubMarker)
			if ok && validGitHubName(name) {
				github = name
			}

			continue
		}

		key, ok := parseAuthorizedKey(line)
		if !ok {
			continue
		}

		owner := KeyOwner{User: user}

		switch {
		case github != "":
			owner.Identity, owner.Source = githubMarker+github, SourceGitHub
		case key.comment != "":
			owner.Identity, owner.Source = clip(key.comment, maxIdentity), SourceComment
		default:
			continue
		}

		if old, ok := m[key.fingerprint]; ok && (old.Source == SourceGitHub || owner.Source != SourceGitHub) {
			continue
		}

		m[key.fingerprint] = owner
	}
}

// validGitHubName reports whether name is a GitHub login: letters, digits
// and single hyphens, at most 39 characters.
func validGitHubName(name string) bool {
	if name == "" || len(name) > 39 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}

	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}

	return !strings.Contains(name, "--")
}

// authorizedKey is one key of an authorized_keys file.
type authorizedKey struct {
	keyType     string
	fingerprint string
	comment     string
}

// parseAuthorizedKey reads one authorized_keys line: [options] type base64
// [comment]. The key is the first field that decodes to a blob of the type
// named by the field before it.
func parseAuthorizedKey(line string) (authorizedKey, bool) {
	fields := splitFields(line)

	for i := 0; i+1 < len(fields); i++ {
		blob, err := base64.StdEncoding.DecodeString(fields[i+1])
		if err != nil || blobType(blob) != fields[i] {
			continue
		}

		return authorizedKey{
			keyType:     fields[i],
			fingerprint: Fingerprint(blob),
			comment:     strings.Join(fields[i+2:], " "),
		}, true
	}

	return authorizedKey{}, false
}

// splitFields splits at blanks outside double quotes (options may quote
// blanks).
func splitFields(line string) []string {
	out := make([]string, 0, 4)

	var (
		cur    strings.Builder
		quoted bool
	)

	for _, r := range line {
		switch {
		case r == '"':
			quoted = !quoted

			cur.WriteRune(r)
		case (r == ' ' || r == '\t') && !quoted:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}

	if cur.Len() > 0 {
		out = append(out, cur.String())
	}

	return out
}

// blobType is the type name a public key blob starts with.
func blobType(blob []byte) string {
	if len(blob) < 4 {
		return ""
	}

	n := binary.BigEndian.Uint32(blob)
	if n == 0 || uint64(n) > uint64(len(blob)-4) {
		return ""
	}

	return string(blob[4 : 4+n])
}

// Fingerprint is ssh-keygen's SHA256 fingerprint of a public key blob.
func Fingerprint(blob []byte) string {
	sum := sha256.Sum256(blob)

	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return strings.ToValidUTF8(s[:n], "")
}
