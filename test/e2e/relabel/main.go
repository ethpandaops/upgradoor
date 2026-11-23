// Command relabel gives containers that something other than Ansible
// created (the Kurtosis devnet of the end-to-end run) the labels Ansible
// sets: the kind and the team. It recreates each container with its own
// configuration and image plus the labels, as Ansible's recreate would, so
// the agent finds it.
//
//	relabel [+<label>=<value> ...] <container>=<kind>[/<team>] ...
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
		return errors.New("usage: relabel [+<label>=<value> ...] <container>=<kind>[/<team>] [more]")
	}

	// +<label>=<value> goes on every container, e.g. the label an agent is
	// limited to (docker.labels).
	extra := make(map[string]string, 1)
	specs := make([]string, 0, len(args))

	for _, arg := range args {
		label, ok := strings.CutPrefix(arg, "+")
		if !ok {
			specs = append(specs, arg)

			continue
		}

		k, v, ok := strings.Cut(label, "=")
		if !ok || k == "" {
			return fmt.Errorf("%q is not +<label>=<value>", arg)
		}

		extra[k] = v
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

	for _, arg := range specs {
		name, spec, ok := strings.Cut(arg, "=")
		kind, team, _ := strings.Cut(spec, "/")

		if !ok || kind == "" {
			return fmt.Errorf("%q is not <container>=<kind>[/<team>]", arg)
		}

		labels := make(map[string]string, 2+len(extra))
		for k, v := range extra {
			labels[k] = v
		}

		labels[protocol.LabelKind] = kind
		if team != "" {
			labels[protocol.LabelTeam] = team
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

		change := &host.Change{Labels: labels}

		if err := machine.Recreate(ctx, name, digest, 30*time.Second, true, change); err != nil {
			return fmt.Errorf("container %s: %w", name, err)
		}
	}

	return nil
}
