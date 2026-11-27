package upstream

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/imageref"
)

// The labels an image says its source with. The first three are set by
// eth-client-docker-image-builder, the others are the OCI ones.
const (
	labelBuilderRepo   = "ethpandaops.io.repo"
	labelBuilderRef    = "ethpandaops.io.commitRef"
	labelBuilderCommit = "commit"
	labelOCISource     = "org.opencontainers.image.source"
	labelOCIRevision   = "org.opencontainers.image.revision"

	// builderOrg is the organisation eth-client-docker-image-builder
	// publishes under, at Docker Hub and at ghcr.io.
	builderOrg = "ethpandaops/"

	// shortCommit is the length of the commit in the builder's
	// "<tag>-<commit>" tags, and the shortest commit that is compared.
	shortCommit = 7
)

var (
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}/[A-Za-z0-9_.-]{1,100}$`)
	refPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./+@-]{0,199}$`)
)

// builderRepos are the repositories eth-client-docker-image-builder builds
// its images from (DEFAULT_REPOS of its generate_config.py), by image name:
// an image its build script pushes carries no label that says so.
var builderRepos = map[string]string{
	"besu":                    "besu-eth/besu",
	"caplin":                  "erigontech/erigon",
	"erigon":                  "erigontech/erigon",
	"ethlambda":               "lambdaclass/ethlambda",
	"ethrex":                  "lambdaclass/ethrex",
	"geth":                    "ethereum/go-ethereum",
	"grandine":                "grandinetech/grandine",
	"lighthouse":              "sigp/lighthouse",
	"lodestar":                "chainsafe/lodestar",
	"nethermind":              "nethermindeth/nethermind",
	"nimbus-eth1":             "status-im/nimbus-eth1",
	"nimbus-eth2":             "status-im/nimbus-eth2",
	"nimbus-validator-client": "status-im/nimbus-eth2",
	"prysm-beacon-chain":      "offchainlabs/prysm",
	"prysm-validator":         "offchainlabs/prysm",
	"reth":                    "paradigmxyz/reth",
	"silver":                  "gattaca-com/silver",
	"teku":                    "consensys-incorporated/teku",
}

// Source is where an image is built from.
type Source struct {
	// Repo is the GitHub repository, owner/name.
	Repo string
	// Ref is the branch or tag the image follows.
	Ref string
	// Commit is the commit the image says it was built from, in lower case;
	// empty when no label says.
	Commit string
}

// ParseSource reads the value of --image-source, "<image>=<owner>/<name>[@<ref>]":
// the image repository (without a tag) and the GitHub repository its tags
// are built from, each tag from the ref of its name unless one is given.
func ParseSource(value string) (image string, source Source, err error) {
	image, target, ok := strings.Cut(value, "=")
	if !ok {
		return "", Source{}, fmt.Errorf("%q is not <image>=<owner>/<name>[@<ref>]", value)
	}

	parsed, err := imageref.Parse(image)
	if err != nil {
		return "", Source{}, err
	}

	source.Repo, source.Ref, _ = strings.Cut(target, "@")

	if !repoPattern.MatchString(source.Repo) {
		return "", Source{}, fmt.Errorf("%q is not a GitHub repository (owner/name)", source.Repo)
	}

	if source.Ref != "" && !refPattern.MatchString(source.Ref) {
		return "", Source{}, fmt.Errorf("%q is not a branch or tag name", source.Ref)
	}

	return parsed.Repository(), source, nil
}

// sourceOf says where the image of a tag reference is built from, by what
// was configured for its repository, then by its labels, then by the
// builder's table; the ref is the tag when nothing names one. ok is false
// for an image whose repository nothing names.
func sourceOf(ref imageref.Reference, labels map[string]string, configured map[string]Source) (Source, bool) {
	var out Source

	if commit := strings.ToLower(labels[labelBuilderCommit]); isShortCommit(commit) {
		out.Commit = commit
	} else if commit := strings.ToLower(labels[labelOCIRevision]); isShortCommit(commit) {
		out.Commit = commit
	}

	set := configured[ref.Repository()]

	switch {
	case set.Repo != "":
		out.Repo = set.Repo
	case repoPattern.MatchString(labels[labelBuilderRepo]):
		out.Repo = labels[labelBuilderRepo]
	case githubRepo(labels[labelOCISource]) != "":
		out.Repo = githubRepo(labels[labelOCISource])
	default:
		name, builder := strings.CutPrefix(ref.Path, builderOrg)
		if !builder || (ref.Host != imageref.DockerHubHost && ref.Host != "ghcr.io") {
			return out, false
		}

		if out.Repo = builderRepos[name]; out.Repo == "" {
			return out, false
		}
	}

	switch {
	case set.Ref != "":
		out.Ref = set.Ref
	case refPattern.MatchString(labels[labelBuilderRef]):
		out.Ref = labels[labelBuilderRef]
	default:
		out.Ref = ref.Tag
	}

	return out, refPattern.MatchString(out.Ref)
}

// githubRepo returns owner/name of a GitHub repository URL, or "".
func githubRepo(url string) string {
	repo, ok := strings.CutPrefix(url, githubURL+"/")
	if !ok {
		return ""
	}

	repo = strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
	if !repoPattern.MatchString(repo) {
		return ""
	}

	return repo
}

// isShortCommit reports whether s can be compared with a commit: at least
// shortCommit digits of lower-case hex.
func isShortCommit(s string) bool {
	return len(s) >= shortCommit && len(s) <= 64 && isHex(s)
}
