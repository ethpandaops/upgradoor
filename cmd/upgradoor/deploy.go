package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

func newDeployCmd() *cobra.Command {
	var (
		image   string
		command string
		reset   bool
		managed string
	)

	cmd := &cobra.Command{
		Use:   "deploy <container>",
		Short: "Write what Ansible deploys a container as (root only)",
		Long: "Writes the container's image and command line into managed.yaml and has the\n" +
			"agent read it again; --reset also clears its override. A container the agent\n" +
			"rolls and that runs something else is rolled in its group's rollout.\n" +
			"Prints one JSON object: container, exists, managed, changed, roll\n" +
			"(requested or none), ignore (the docker_container comparisons to leave to\n" +
			"the agent) and pull. Needs the running agent; only root may call it.",
		Args: cobra.ExactArgs(1),
	}

	client := socketFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		d := plan.Deployed{Image: image}

		if cmd.Flags().Changed("command") {
			if err := json.Unmarshal([]byte(command), &d.Command); err != nil || d.Command == nil {
				return errors.New("--command is a JSON array of strings")
			}

			d.HasCommand = true
		}

		var res *local.DeployResult

		err := local.WriteManaged(managed, args[0], d, func(changed bool) error {
			var err error

			res, err = client().Deploy(cmd.Context(), &local.DeployRequest{Container: args[0], Reset: reset})
			if err != nil {
				return err
			}

			res.Changed = res.Changed || changed

			return nil
		})

		switch {
		case errors.Is(err, fs.ErrPermission):
			return fmt.Errorf("deploy is for root only: cannot write %s", filepath.Dir(managed))
		case err != nil:
			return fmt.Errorf("deploy %s: %w", args[0], err)
		}

		return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
	}

	cmd.Flags().StringVar(&image, "image", "", "the image reference (required)")
	cmd.Flags().StringVar(&command, "command", "",
		"the command line as a JSON array of strings; without it the container's own")
	cmd.Flags().BoolVar(&reset, "reset", false, "also clear the container's override")
	cmd.Flags().StringVar(&managed, "managed-file", local.DefaultManagedFile, "managed.yaml")
	_ = cmd.MarkFlagRequired("image")

	return cmd
}
