package overrides

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

// Policy is the host's own bound on overrides that arrive from the
// controller: one per managed container, in agent.yaml.
type Policy struct {
	// Images are the image repositories an image override may name
	// ("ethpandaops/geth", "ghcr.io/ethereum/*"; "*" stands for any run of
	// characters without a slash). Empty: no image overrides.
	Images []string `yaml:"images"`
	// Flags allows flag overrides.
	Flags bool `yaml:"flags"`
}

// Validate checks the policy as agent.yaml states it.
func (p *Policy) Validate() error {
	for _, pattern := range p.Images {
		if err := validateRepositoryPattern(pattern); err != nil {
			return fmt.Errorf("overrides.images: %w", err)
		}
	}

	return nil
}

// Allows applies the policy of container to an override the controller asks
// for: the opt-in for image overrides with its allowed repositories, the
// opt-in for flag overrides, and an end within MaxDuration.
func (p *Policy) Allows(container string, entry *Entry, now time.Time) error {
	switch {
	case entry.Image != "" && len(p.Images) == 0:
		return fmt.Errorf("this host does not allow image overrides of container %s "+
			"(no overrides.images in its agent.yaml)", container)
	case entry.Image != "" && !imageAllowed(p.Images, entry.Image):
		return fmt.Errorf("image %s is not in a repository this host allows for container %s (%v)",
			entry.Image, container, p.Images)
	case len(entry.Add)+len(entry.Remove) > 0 && !p.Flags:
		return fmt.Errorf("this host does not allow flag overrides of container %s "+
			"(no overrides.flags in its agent.yaml)", container)
	case entry.Until == nil:
		return errors.New("an override from the controller needs an end")
	case !entry.Until.After(now):
		return fmt.Errorf("the override's end %s has passed", entry.Until.UTC().Format(time.RFC3339))
	case entry.Until.After(now.Add(MaxDuration + time.Minute)):
		return fmt.Errorf("the override ends more than %s from now", MaxDuration)
	default:
		return nil
	}
}

// imageAllowed reports whether patterns allow the repository of ref. A
// pattern is a repository ("ethpandaops/geth", "ghcr.io/ethereum/*") in which
// "*" stands for any run of characters without a slash; it is compared with
// the canonical repository of the reference (docker.io/ethpandaops/geth), so
// a pattern without a registry host means Docker Hub, as a reference does.
func imageAllowed(patterns []string, ref string) bool {
	parsed, err := imageref.Parse(ref)
	if err != nil {
		return false
	}

	for _, p := range patterns {
		if ok, err := path.Match(normalizeRepositoryPattern(p), parsed.Repository()); err == nil && ok {
			return true
		}
	}

	return false
}

// normalizeRepositoryPattern adds the registry host (and library/) that a
// reference without one gets.
func normalizeRepositoryPattern(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))

	first, rest, more := strings.Cut(p, "/")
	if !more {
		return imageref.DockerHubHost + "/library/" + p
	}

	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return imageref.RegistryHost(first) + "/" + rest
	}

	return imageref.DockerHubHost + "/" + p
}

// validateRepositoryPattern checks one entry of a container's allowed image
// repositories.
func validateRepositoryPattern(p string) error {
	last := p[strings.LastIndex(p, "/")+1:]

	if p == "" || strings.ContainsAny(p, " \t@") || strings.Contains(last, ":") {
		return fmt.Errorf("%q is not an image repository pattern (a repository, no tag or digest)", p)
	}

	if _, err := path.Match(normalizeRepositoryPattern(p), "x"); err != nil {
		return fmt.Errorf("%q is not a pattern: %w", p, err)
	}

	return nil
}
