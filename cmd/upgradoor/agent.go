package main

import (
	"errors"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func newAgentCmd(_ *logrus.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "agent",
		Short: "Run the per-host agent",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("agent: not wired yet")
		},
	}
}
