package fakehost

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Scenario describes a fake host. Example:
//
//	bootId: boot
//	os: {id: debian, versionId: "13", prettyName: Debian GNU/Linux 13, arch: amd64}
//	runningKernel: 6.12.41-amd64
//	newestKernel: 6.12.48-amd64
//	rebootRequired: true
//	containers:
//	  - {name: geth, image: "ethpandaops/geth:master", digest: "sha256:aaa"}
//	  - {name: lighthouse, image: "sigp/lighthouse:latest", digest: "sha256:bbb"}
//	registry:
//	  "ethpandaops/geth:master": {digest: "sha256:ccc", revision: 1a2b3c}
//	packages:
//	  blocked: [docker-ce]
//	  installed:
//	    - {name: docker-ce, version: "28.0", arch: amd64}
//	  upgradable:
//	    - {name: docker-ce, arch: amd64, current: "28.0", candidate: "28.1"}
//	control: 127.0.0.1:7777
type Scenario struct {
	// BootID is the boot id before the first simulated reboot (default
	// "boot"); every reboot appends a counter.
	BootID string `yaml:"bootId"`

	OS            OS     `yaml:"os"`
	RunningKernel string `yaml:"runningKernel"`
	NewestKernel  string `yaml:"newestKernel"`
	// RebootRequired is the /var/run/reboot-required flag.
	RebootRequired bool `yaml:"rebootRequired"`

	Containers []Container `yaml:"containers"`
	// Registry maps a tag reference to what the registry says about it. A
	// container's tag that is not listed points at the container's digest.
	Registry map[string]Tag `yaml:"registry"`
	Packages Packages       `yaml:"packages"`

	// Control is the loopback address of the control endpoint (optional;
	// see Host.ServeControl).
	Control string `yaml:"control"`
}

// OS is the fake /etc/os-release.
type OS struct {
	ID         string `yaml:"id"`
	VersionID  string `yaml:"versionId"`
	PrettyName string `yaml:"prettyName"`
	Arch       string `yaml:"arch"`
}

// Container is one fake container.
type Container struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
	// Digest is the digest of the image the container runs. The local
	// image of the container's tag starts out as this digest.
	Digest   string `yaml:"digest"`
	Revision string `yaml:"revision"`
	// State is the Docker state (default "running").
	State string `yaml:"state"`
}

// Tag is the registry's answer for one tag.
type Tag struct {
	Digest   string `yaml:"digest" json:"digest"`
	Revision string `yaml:"revision" json:"revision,omitempty"`
}

// Packages is the fake package state.
type Packages struct {
	// Blocked is the blocked set: the only packages UpgradePackages accepts.
	Blocked    []string              `yaml:"blocked"`
	Installed  []protocol.Package    `yaml:"installed"`
	Upgradable []protocol.Upgradable `yaml:"upgradable"`
}

// LoadScenario reads a scenario file.
func LoadScenario(path string) (*Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario: %w", err)
	}

	var sc Scenario

	if err := yaml.Unmarshal(raw, &sc); err != nil {
		return nil, fmt.Errorf("parse scenario %s: %w", path, err)
	}

	if err := sc.validate(); err != nil {
		return nil, fmt.Errorf("scenario %s: %w", path, err)
	}

	return &sc, nil
}

func (sc *Scenario) validate() error {
	seen := make(map[string]struct{}, len(sc.Containers))

	for _, c := range sc.Containers {
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
