package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

func newOverrideCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "override",
		Short: "Set or clear the override of a container (the agent writes the overrides file)",
	}

	client := socketFlag(cmd)

	var (
		req   local.OverrideRequest
		until string
	)

	set := &cobra.Command{
		Use:   "set <container>",
		Short: "Override the image, the command line or both of a container for a while",
		Long: "Writes the container's entry in the overrides file; an entry that is there is\n" +
			"replaced as a whole. The container is recreated under the next lease.\n\n" +
			"Flags change the deployed command line, and the result is written as the\n" +
			"entry's whole command: --remove-flag takes arguments out first (\"--name\"\n" +
			"removes the flag with any value, \"--name=value\" or \"--name value\" exactly\n" +
			"that), then --add-flag appends (\"--name=value\", \"--switch\", or\n" +
			"\"--name value\" as two arguments) what is not there yet.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Container = args[0]

			end, err := parseUntil(until, time.Now())
			if err != nil {
				return err
			}

			req.Until = end

			return printAnswer(cmd, func(ctx context.Context) (*local.Answer, error) {
				return client().Override(ctx, &req)
			})
		},
	}

	set.Flags().StringVar(&req.Image, "image", "", "image reference to run instead (repository:tag)")
	set.Flags().StringArrayVar(&req.AddFlags, "add-flag", nil, "argument to add (repeatable)")
	set.Flags().StringArrayVar(&req.RemoveFlags, "remove-flag", nil, "argument to remove (repeatable)")
	set.Flags().StringVar(&until, "until", plan.DefaultOverride.String(),
		"end of the override: a duration (24h), a time (RFC 3339) or \"never\"")
	set.Flags().StringVar(&req.Reason, "reason", "", "why (written into the file and the audit log)")

	var reason string

	unset := &cobra.Command{
		Use:   "clear <container>",
		Short: "Remove the override of a container; it is put back under the next lease",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return printAnswer(cmd, func(ctx context.Context) (*local.Answer, error) {
				return client().Override(ctx, &local.OverrideRequest{
					Container: args[0], Clear: true, Reason: reason,
				})
			})
		},
	}

	unset.Flags().StringVar(&reason, "reason", "", "why (written into the audit log)")

	cmd.AddCommand(set, unset)

	return cmd
}

// parseUntil reads the end of an override: a duration from now, a time, or
// "never" for none.
func parseUntil(value string, now time.Time) (*time.Time, error) {
	if value == "never" || value == "none" {
		return nil, nil //nolint:nilnil // no end and no error is an answer.
	}

	if d, err := time.ParseDuration(value); err == nil {
		if d <= 0 {
			return nil, errors.New("--until must lie in the future")
		}

		end := now.Add(d).UTC().Truncate(time.Second)

		return &end, nil
	}

	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("--until %q is neither a duration (24h), a time "+
			"(2030-01-31T12:00:00Z) nor \"never\"", value)
	}

	if !at.After(now) {
		return nil, errors.New("--until must lie in the future")
	}

	at = at.UTC()

	return &at, nil
}

func newInitOverridesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init-overrides",
		Short: "Lay out the overrides file now (the agent also does it by itself)",
		Long: "Creates the overrides file when it is missing and writes an entry with the\n" +
			"default image and command line of each container that can be overridden.\n" +
			"Every other line is kept.",
		Args: cobra.NoArgs,
	}

	client := socketFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return printAnswer(cmd, func(ctx context.Context) (*local.Answer, error) {
			return client().InitOverrides(ctx)
		})
	}

	return cmd
}
