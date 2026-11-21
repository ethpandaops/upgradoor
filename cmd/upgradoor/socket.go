package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
)

// envSocket names the agent's socket for the on-host commands.
const envSocket = "UPGRADOOR_SOCKET"

// socketFlag adds --socket to cmd (and its sub-commands) and returns the
// client for the socket it names.
func socketFlag(cmd *cobra.Command) func() local.Client {
	def := os.Getenv(envSocket)
	if def == "" {
		def = local.DefaultSocket
	}

	var socket string

	cmd.PersistentFlags().StringVar(&socket, "socket", def, "the agent's socket ($"+envSocket+")")

	return func() local.Client { return local.NewClient(socket) }
}

// printAnswer prints the message of the agent's answer to call.
func printAnswer(
	cmd *cobra.Command, call func(ctx context.Context) (*local.Answer, error),
) error {
	answer, err := call(cmd.Context())
	if err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout(), answer.Message)

	return nil
}
