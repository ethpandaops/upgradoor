package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
)

func newDeployCmd() *cobra.Command {
	var (
		image   string
		command string
		reset   bool
	)

	cmd := &cobra.Command{
		Use:   "deploy <container>",
		Short: "Write what Ansible deploys a container as (root only)",
		Long: "Writes the container's image and command line into the generated block of\n" +
			"the overrides file; --reset also removes its override entry. A container the\n" +
			"agent rolls and that runs something else is rolled in its group's rollout.\n" +
			"Prints one JSON object: container, exists, managed, changed, roll\n" +
			"(requested or none), ignore (the docker_container comparisons to leave to\n" +
			"the agent) and pull. Needs the running agent; only root may call it.",
		Args: cobra.ExactArgs(1),
	}

	client := socketFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		req := &local.DeployRequest{Container: args[0], Image: image, Reset: reset}

		if cmd.Flags().Changed("command") {
			if err := json.Unmarshal([]byte(command), &req.Command); err != nil || req.Command == nil {
				return errors.New("--command is a JSON array of strings")
			}
		}

		res, err := client().Deploy(cmd.Context(), req)
		if err != nil {
			return fmt.Errorf("deploy %s: %w", args[0], err)
		}

		return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
	}

	cmd.Flags().StringVar(&image, "image", "", "the image reference (required)")
	cmd.Flags().StringVar(&command, "command", "",
		"the command line as a JSON array of strings; without it the container's own")
	cmd.Flags().BoolVar(&reset, "reset", false, "also remove the container's override entry")
	_ = cmd.MarkFlagRequired("image")

	return cmd
}
