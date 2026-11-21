package views

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// ErrBadImageCursor says that "after" is not the Next of a page.
var ErrBadImageCursor = errors.New("views: after is not the next of a page")

// Image ranks: how well a reference matches the query.
const (
	imageRankExact = iota
	imageRankPrefix
	imageRankOther
)

// Images is the body of GET /api/v1/images: one page of the images the
// containers of the fleet run. It pages like Packages.
type Images struct {
	Query   string       `json:"query"`
	Matches []ImageMatch `json:"matches"`
	// Total is the number of references the query selects. Only the first
	// page has it.
	Total *int `json:"total,omitempty"`
	// Next continues after this page (the "after" parameter); absent on the
	// last page.
	Next string `json:"next,omitempty"`
}

// ImagesRefresh is the body of POST /api/v1/images/refresh.
type ImagesRefresh struct {
	// Tags is the number of tags that are looked up now; Recent the number
	// that were looked up too recently to be looked up again yet.
	Tags   int `json:"tags"`
	Recent int `json:"recent,omitempty"`
	// Next is when the first of the recent ones can be looked up on
	// request again; absent when none is recent.
	Next *time.Time `json:"next,omitempty"`
	// Message says what happened, for people.
	Message string `json:"message"`
}

// NewImagesRefresh is the answer of a refresh that looks tags up now and
// leaves recent ones alone until next; floor is how recent that is.
func NewImagesRefresh(tags, recent int, next time.Time, floor time.Duration) ImagesRefresh {
	out := ImagesRefresh{Tags: tags, Recent: recent}

	switch {
	case tags == 0 && recent == 0:
		out.Message = "no tag is followed"
	case tags > 0:
		out.Message = fmt.Sprintf("%d tag(s) are looked up now", tags)
	default:
		out.Message = fmt.Sprintf("every tag was looked up within the last %s", floor)
	}

	if recent > 0 {
		next = next.UTC()
		out.Next = &next

		if tags > 0 {
			out.Message += fmt.Sprintf("; %d looked up within the last %s are left alone", recent, floor)
		}

		out.Message += "; the next lookup on request is possible at " + next.Format(time.TimeOnly) + " UTC"
	}

	return out
}

// ImageMatch is one image reference, as the hosts write it, with the digests
// it runs at across the fleet.
type ImageMatch struct {
	Name     string         `json:"name"`
	Versions []ImageVersion `json:"versions"`
	// Registry is the newest registry lookup of the tag a host reports;
	// absent for a reference nobody follows.
	Registry *ImageLookup `json:"registry,omitempty"`
}

// ImageLookup is where a tag points by the controller's lookup.
type ImageLookup struct {
	Digest    string    `json:"digest,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
	// Error is why the newest lookup failed; Digest is then the last good one.
	Error string `json:"error,omitempty"`
}

// ImageVersion is one digest of a reference and the hosts with a container
// on it. Digest is empty for an image without a registry digest.
type ImageVersion struct {
	Digest   string   `json:"digest"`
	Revision string   `json:"revision,omitempty"`
	Hosts    []string `json:"hosts"`
}

// NewImages returns one page of the images of every container the hosts
// report, managed or not. Without a query the references are in alphabetical
// order; with one, the exact match comes first, then the references that
// start with it, then the others that contain it. after is the Next of the
// page before.
func NewImages(hosts []*fleet.Host, query, after string, limit int) (Images, error) {
	needle := strings.ToLower(query)

	rank := func(name string) int {
		switch lower := strings.ToLower(name); {
		case needle == "" || lower == needle:
			return imageRankExact
		case strings.HasPrefix(lower, needle):
			return imageRankPrefix
		default:
			return imageRankOther
		}
	}

	cursor := func(name string) string { return string(rune('0'+rank(name))) + name }

	if after != "" && (len(after) < 2 || after[0] < '0'+imageRankExact || after[0] > '0'+imageRankOther) {
		return Images{}, ErrBadImageCursor
	}

	// digest -> hosts, by reference.
	byName := make(map[string]map[string]*ImageVersion, 16)
	lookups := make(map[string]*ImageLookup, 16)

	for _, host := range hosts {
		if host.Status == nil {
			continue
		}

		for i := range host.Status.Containers {
			c := &host.Status.Containers[i]
			if c.Image == "" || !strings.Contains(strings.ToLower(c.Image), needle) {
				continue
			}

			versions := byName[c.Image]
			if versions == nil {
				versions = make(map[string]*ImageVersion, 2)
				byName[c.Image] = versions
			}

			if rc := c.Registry; rc != nil && (lookups[c.Image] == nil || rc.CheckedAt.After(lookups[c.Image].CheckedAt)) {
				lookups[c.Image] = &ImageLookup{Digest: rc.Digest, CheckedAt: rc.CheckedAt, Error: rc.Error}
			}

			v := versions[c.Digest]
			if v == nil {
				v = &ImageVersion{Digest: c.Digest, Hosts: make([]string, 0, 4)}
				versions[c.Digest] = v
			}

			v.Revision = cmp.Or(v.Revision, c.Revision)

			// A host with two containers on one image counts once.
			if !slices.Contains(v.Hosts, host.Name) {
				v.Hosts = append(v.Hosts, host.Name)
			}
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}

	slices.SortFunc(names, func(a, b string) int { return cmp.Compare(cursor(a), cursor(b)) })

	out := Images{Query: query, Matches: make([]ImageMatch, 0, limit)}

	if after == "" {
		total := len(names)
		out.Total = &total
	}

	names = slices.DeleteFunc(names, func(name string) bool { return cursor(name) <= after })

	for i, name := range names {
		if i == limit {
			out.Next = cursor(names[i-1])

			break
		}

		m := ImageMatch{
			Name: name, Versions: make([]ImageVersion, 0, len(byName[name])), Registry: lookups[name],
		}

		for _, v := range byName[name] {
			slices.Sort(v.Hosts)
			m.Versions = append(m.Versions, *v)
		}

		// The digest most hosts run first.
		slices.SortFunc(m.Versions, func(a, b ImageVersion) int {
			return cmp.Or(cmp.Compare(len(b.Hosts), len(a.Hosts)), cmp.Compare(a.Digest, b.Digest))
		})

		out.Matches = append(out.Matches, m)
	}

	return out, nil
}
