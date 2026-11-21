// Command relabel gives containers that something other than Ansible
// created (the Kurtosis devnet of the end-to-end run) the labels Ansible
// sets: the roll group and the kind. It recreates each container with its
// own configuration and image plus the two labels, as Ansible's recreate
// would, so the agent finds it.
//
//	relabel <container>=<group>/<kind> ...
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/agent/host"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "relabel:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: relabel <container>=<group>/<kind> [<container>=<group>/<kind>]")
	}

	log := logrus.New()
	log.SetLevel(logrus.WarnLevel)

	cfg := &host.Config{}
	cfg.Docker.Host = os.Getenv("DOCKER_HOST")
	cfg.ApplyDefaults()

	machine, err := host.New(log, cfg, nil, nil)
	if err != nil {
		return err
	}

	defer func() { _ = machine.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	list, err := machine.Containers(ctx)
	if err != nil {
		return err
	}

	for _, arg := range args {
		name, spec, ok := strings.Cut(arg, "=")
		group, kind, ok2 := strings.Cut(spec, "/")

		if !ok || !ok2 {
			return fmt.Errorf("%q is not <container>=<group>/<kind>", arg)
		}

		var digest string

		for i := range list {
			if list[i].Name == name {
				digest = list[i].Digest
			}
		}

		if digest == "" {
			return fmt.Errorf("container %s: not found or its image has no registry digest", name)
		}

		change := &host.Change{Group: group, Kind: protocol.ContainerKind(kind)}

		if err := machine.Recreate(ctx, name, digest, 30*time.Second, true, change); err != nil {
			return fmt.Errorf("container %s: %w", name, err)
		}
	}

	return nil
}
