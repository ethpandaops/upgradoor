package main

import (
	"errors"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func newControllerCmd(_ *logrus.Logger) *cobra.Command {
	return &cobra.Command{
		Use:   "controller",
		Short: "Run the per-devnet controller",
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("controller: not wired yet")
		},
	}
}
