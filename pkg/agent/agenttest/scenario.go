package agenttest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Scenario describes an in-memory host; its YAML form is the scenario file.
// Example:
//
//	bootId: boot
//	os: {id: debian, versionId: "13", prettyName: Debian GNU/Linux 13, arch: amd64}
//	runningKernel: 6.12.41-amd64
//	newestKernel: 6.12.48-amd64
//	rebootRequired: true
//	containers:
//	  - {name: geth, image: "ethpandaops/geth:master", digest: "sha256:aaa",
//	     group: execution, kind: el}
//	registry:
//	  "ethpandaops/geth:master": {digest: "sha256:ccc", revision: 1a2b3c}
//	packages:
//	  blocked: [docker-ce]
//	  installed:
//	    - {name: docker-ce, version: "28.0", arch: amd64}
//	  upgradable:
//	    - {name: docker-ce, arch: amd64, current: "28.0", candidate: "28.1"}
type Scenario struct {
	// BootID is the boot id before the first reboot; every reboot appends a counter.
	BootID string `yaml:"bootId"`

	OS            OS     `yaml:"os"`
	RunningKernel string `yaml:"runningKernel"`
	NewestKernel  string `yaml:"newestKernel"`
	// RebootRequired is the reboot-required flag.
	RebootRequired bool `yaml:"rebootRequired"`

	Containers []Container `yaml:"containers"`
	// Registry maps a tag reference to its image; an unlisted container tag
	// points at the container's digest.
	Registry map[string]Tag `yaml:"registry"`
	Packages Packages       `yaml:"packages"`
}

// OS is the fake /etc/os-release.
type OS struct {
	ID         string `yaml:"id"`
	VersionID  string `yaml:"versionId"`
	PrettyName string `yaml:"prettyName"`
	Arch       string `yaml:"arch"`
}

// Container is one container of the scenario.
type Container struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
	// Digest is the image the container runs and the initial local image of its tag.
	Digest   string `yaml:"digest"`
	Revision string `yaml:"revision"`
	// State is the Docker state (default running).
	State   string   `yaml:"state"`
	Command []string `yaml:"command"`
	// Group and Kind are the labels Ansible sets.
	Group string `yaml:"group"`
	Kind  string `yaml:"kind"`
	// Links plays a container created with --link.
	Links bool `yaml:"links"`
}

// Tag is the image the registry holds for one tag.
type Tag struct {
	Digest   string `yaml:"digest" json:"digest"`
	Revision string `yaml:"revision" json:"revision,omitempty"`
}

// Packages is the fake package state.
type Packages struct {
	// Blocked is the only set UpgradePackages accepts.
	Blocked    []string              `yaml:"blocked"`
	Installed  []protocol.Package    `yaml:"installed"`
	Upgradable []protocol.Upgradable `yaml:"upgradable"`
}

// The architecture and the blocked packages of BaseScenario.
const (
	arch       = "amd64"
	docker     = "docker-ce"
	containerd = "containerd.io"
	dockerV1   = "28.0"
)

// The images, digests and groups of BaseScenario.
const (
	GroupExecution = "execution"
	GroupConsensus = "consensus"

	GethImage = "ethpandaops/geth:master"
	TekuImage = "consensys/teku:develop"
	VCImage   = "sigp/lighthouse:stable"

	GethV1 = "sha256:geth-1"
	GethV2 = "sha256:geth-2"
	GethV3 = "sha256:geth-3"
	TekuV1 = "sha256:teku-1"
	TekuV2 = "sha256:teku-2"
	VCV1   = "sha256:vc-1"
	VCV2   = "sha256:vc-2"
)

// The override image of the override tests and its digests; put it in the
// registry with Host.SetRegistry.
const (
	FixImage = "ethpandaops/geth:fix"
	FixV1    = "sha256:fix-1"
	FixV2    = "sha256:fix-2"
)

// BaseScenario is an up-to-date Debian 13 host with geth, teku and a
// validator in managed groups, an unmanaged dora, and the blocked packages
// docker-ce and containerd.io installed.
func BaseScenario() *Scenario {
	return &Scenario{
		BootID:        "boot",
		OS:            OS{ID: "debian", VersionID: "13", PrettyName: "Debian 13", Arch: arch},
		RunningKernel: "6.12.41-amd64",
		NewestKernel:  "6.12.41-amd64",
		Containers: []Container{
			{Name: "validator", Image: VCImage, Digest: VCV1, Group: GroupConsensus, Kind: "vc"},
			{Name: "geth", Image: GethImage, Digest: GethV1, Group: GroupExecution, Kind: "el"},
			{Name: "teku", Image: TekuImage, Digest: TekuV1, Group: GroupConsensus, Kind: "cl"},
			{
				Name: "dora", Image: "ethpandaops/dora:latest", Digest: "sha256:dora-1",
				Group: protocol.GroupNone,
			},
		},
		Packages: Packages{
			Blocked: []string{docker, containerd},
			Installed: []protocol.Package{
				{Name: docker, Version: dockerV1, Arch: arch},
				{Name: containerd, Version: "1.7", Arch: arch},
				{Name: "openssl", Version: "3.5.1", Arch: arch},
			},
		},
	}
}

// DockerUpgrade is an upgradable docker-ce (blocked) and openssl (security)
// for Host.SetUpgradable.
func DockerUpgrade() []protocol.Upgradable {
	return []protocol.Upgradable{
		{Name: docker, Arch: arch, Current: dockerV1, Candidate: "28.1"},
		{Name: "openssl", Arch: arch, Current: "3.5.1", Candidate: "3.5.2", Security: true},
	}
}

// LoadScenario reads a scenario file; unknown keys are errors.
func LoadScenario(path string) (*Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	var sc Scenario

	if err := dec.Decode(&sc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse scenario %s: %w", path, err)
	}

	// A second document would be ignored silently: refuse it.
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse scenario %s: more than one YAML document", path)
	}

	return &sc, nil
}

// validate checks what NewHost needs of a scenario.
func (sc *Scenario) validate() error {
	seen := make(map[string]struct{}, len(sc.Containers))

	for i := range sc.Containers {
		c := &sc.Containers[i]
		if c.Name == "" || c.Image == "" {
			return errors.New("containers need a name and an image")
		}

		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("container %q listed twice", c.Name)
		}

		seen[c.Name] = struct{}{}
	}

	for ref, tag := range sc.Registry {
		if ref == "" || tag.Digest == "" {
			return errors.New("registry entries need a reference and a digest")
		}
	}

	return nil
}
