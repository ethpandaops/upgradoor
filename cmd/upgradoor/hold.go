package main

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
)

func newHoldCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hold [reason]",
		Short: "Hold the host: the agent refuses all work",
		Args:  cobra.ArbitraryArgs,
	}

	client := socketFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return printAnswer(cmd, func(ctx context.Context) (*local.Answer, error) {
			return client().Hold(ctx, &local.HoldRequest{Reason: strings.Join(args, " ")})
		})
	}

	return cmd
}

func newReleaseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Release the local hold (a hold of the controller is lifted on the page)",
		Args:  cobra.NoArgs,
	}

	client := socketFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return printAnswer(cmd, func(ctx context.Context) (*local.Answer, error) {
			return client().Hold(ctx, &local.HoldRequest{Release: true})
		})
	}

	return cmd
}
