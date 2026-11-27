package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// githubURL is the only place heads are read from: the repository names
	// come from image labels.
	githubURL = "https://github.com"

	requestTimeout = 15 * time.Second
	maxRedirects   = 3
	// maxRefsBytes bounds the answer of one repository.
	maxRefsBytes = 1 << 20

	refsHeads = "refs/heads/"
	refsTags  = "refs/tags/"
	// peeledAttr carries the commit an annotated tag points at.
	peeledAttr = "peeled:"
)

// Remote reads the heads of source repositories.
type Remote interface {
	// Heads returns the commit each of refs (branches or tags) of the GitHub
	// repository repo (owner/name) is at; a ref the repository does not have
	// is left out.
	Heads(ctx context.Context, repo string, refs []string) (map[string]string, error)
}

// github reads heads with the ls-refs command of the git protocol (version
// 2) over HTTP: it needs no token and is not counted against the API rate
// limit.
type github struct {
	client *http.Client
	base   string
	agent  string
}

var _ Remote = (*github)(nil)

// NewGitHub returns the Remote of github.com; version is the controller's.
func NewGitHub(version string) Remote {
	return newGitHub(githubURL, version)
}

func newGitHub(base, version string) *github {
	return &github{
		base:  base,
		agent: "upgradoor-controller/" + version,
		client: &http.Client{
			Timeout: requestTimeout,
			// A renamed repository answers with a redirect; it stays on the host.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects || req.URL.Host != via[0].URL.Host {
					return http.ErrUseLastResponse
				}

				return nil
			},
		},
	}
}

// Heads implements Remote.
func (g *github) Heads(ctx context.Context, repo string, refs []string) (map[string]string, error) {
	var body bytes.Buffer

	writePkt(&body, "command=ls-refs\n")
	writePkt(&body, "agent="+strings.ReplaceAll(g.agent, " ", "-")+"\n")
	body.WriteString("0001")
	writePkt(&body, "peel\n")

	for _, ref := range refs {
		writePkt(&body, "ref-prefix "+refsHeads+ref+"\n")
		writePkt(&body, "ref-prefix "+refsTags+ref+"\n")
	}

	body.WriteString("0000")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/"+repo+".git/git-upload-pack", &body)
	if err != nil {
		return nil, fmt.Errorf("upstream: build request: %w", err)
	}

	req.Header.Set("User-Agent", g.agent)
	req.Header.Set("Git-Protocol", "version=2")
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Accept", "application/x-git-upload-pack-result")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: %w", err)
	}

	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRefsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("upstream: read refs of %s: %w", repo, err)
	}

	// GitHub answers 401 for a repository that is private or does not exist.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream: github.com/%s: %d %s", repo, resp.StatusCode,
			http.StatusText(resp.StatusCode))
	}

	if len(data) > maxRefsBytes {
		return nil, fmt.Errorf("upstream: github.com/%s: the answer exceeds the size limit", repo)
	}

	return parseRefs(data, refs)
}

// writePkt writes one pkt-line.
func writePkt(b *bytes.Buffer, line string) {
	fmt.Fprintf(b, "%04x%s", len(line)+4, line)
}

// parseRefs reads an ls-refs answer: "<commit> <ref>[ <attribute>...]" per
// pkt-line. A branch wins over a tag of the same name, and of an annotated
// tag the commit it points at counts.
func parseRefs(data []byte, refs []string) (map[string]string, error) {
	var (
		out      = make(map[string]string, len(refs))
		branches = make(map[string]struct{}, len(refs))
	)

	for len(data) > 0 {
		if len(data) < 4 {
			return nil, errors.New("upstream: truncated refs answer")
		}

		size, err := strconv.ParseUint(string(data[:4]), 16, 16)
		if err != nil {
			return nil, errors.New("upstream: malformed refs answer")
		}

		// 0000 ends the answer; the other lengths below 4 separate sections.
		if size < 4 {
			if size == 0 {
				break
			}

			data = data[4:]

			continue
		}

		if int(size) > len(data) {
			return nil, errors.New("upstream: truncated refs answer")
		}

		fields := strings.Fields(string(data[4:size]))
		data = data[size:]

		if len(fields) < 2 || !isCommit(fields[0]) {
			continue
		}

		commit, name := fields[0], fields[1]

		for _, attr := range fields[2:] {
			if peeled, ok := strings.CutPrefix(attr, peeledAttr); ok && isCommit(peeled) {
				commit = peeled
			}
		}

		if ref, ok := strings.CutPrefix(name, refsHeads); ok {
			out[ref] = commit
			branches[ref] = struct{}{}

			continue
		}

		if ref, ok := strings.CutPrefix(name, refsTags); ok {
			if _, branch := branches[ref]; !branch {
				out[ref] = commit
			}
		}
	}

	// ref-prefix selects by prefix: only what was asked for is kept.
	for ref := range out {
		wanted := false

		for _, want := range refs {
			wanted = wanted || want == ref
		}

		if !wanted {
			delete(out, ref)
		}
	}

	return out, nil
}

// isCommit reports whether s is a full object name in lower-case hex.
func isCommit(s string) bool {
	return (len(s) == 40 || len(s) == 64) && isHex(s)
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return s != ""
}
