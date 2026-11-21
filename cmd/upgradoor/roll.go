package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
)

func newRollCmd() *cobra.Command {
	var noWait bool

	cmd := &cobra.Command{
		Use:   "roll <container>...",
		Short: "Request a roll of containers and follow it until it is done",
		Long: "Asks the agent to recreate the containers on the image their tag points at\n" +
			"now. The roll waits for a slot of the controller like every other; the\n" +
			"command prints how far it is and ends with it. --no-wait returns at once;\n" +
			"'upgradoor status' shows the roll later.",
		Args: cobra.MinimumNArgs(1),
	}

	client := socketFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c := client()
		out := cmd.OutOrStdout()
		ids := make([]string, 0, len(args))

		for _, name := range args {
			answer, err := c.Roll(cmd.Context(), name)
			if err != nil {
				return fmt.Errorf("roll %s: %w", name, err)
			}

			fmt.Fprintf(out, "%s: requested as %s\n", name, answer.Roll)
			ids = append(ids, answer.Roll)
		}

		if noWait {
			return nil
		}

		return followRolls(cmd.Context(), out, c, ids)
	}

	cmd.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for the roll")

	return cmd
}

// followRolls prints every change of the named rolls until all are final.
// It fails when one of them did.
func followRolls(ctx context.Context, out io.Writer, c local.Client, ids []string) error {
	last := make(map[string]string, len(ids))
	failed := make([]string, 0, len(ids))

	for {
		st, err := c.Status(ctx)
		if err != nil {
			return err
		}

		open := 0

		for i := range st.Rolls {
			r := &st.Rolls[i]
			if !slices.Contains(ids, r.ID) {
				continue
			}

			line := r.State
			if detail := rollDetail(r); detail != "" {
				line += ": " + detail
			}

			if last[r.ID] != line {
				last[r.ID] = line
				fmt.Fprintf(out, "%s  %s  %s\n", time.Now().Format(time.TimeOnly), r.Container, line)

				if r.Final() && r.State != local.RollDone {
					failed = append(failed, r.Container+" "+r.State)
				}
			}

			if !r.Final() {
				open++
			}
		}

		if open == 0 {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	if len(failed) > 0 {
		return errors.New(strings.Join(failed, ", "))
	}

	return nil
}
