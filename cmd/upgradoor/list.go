package main

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the managed containers with image, digest, revision, command and overrides",
		Args:  cobra.NoArgs,
	}

	client := socketFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		st, err := client().Status(cmd.Context())
		if err != nil {
			return err
		}

		printContainers(cmd.OutOrStdout(), st)

		return nil
	}

	return cmd
}

// printContainers writes every managed container with what an override
// changed about it marked.
func printContainers(w io.Writer, st *local.Status) {
	if st.OverridesError != "" {
		fmt.Fprintf(w, "! %s\n\n", st.OverridesError)
	}

	for i := range st.Containers {
		c := &st.Containers[i]

		fmt.Fprintf(w, "%s  (%s, %s)\n", c.Name, labelsOf(c), c.State)

		if c.NotRolled != "" {
			fmt.Fprintf(w, "  rolled:   no, %s\n", c.NotRolled)
		}

		image := c.Image
		if c.Override != nil && c.Override.Image != "" && c.Image != c.ConfiguredImage {
			image += "   [override; configured: " + c.ConfiguredImage + "]"
		} else if c.Image != c.ConfiguredImage {
			image += "   [rolling to the configured " + c.ConfiguredImage + "]"
		}

		fmt.Fprintf(w, "  image:    %s\n", image)
		fmt.Fprintf(w, "  digest:   %s\n", orDash(c.Digest))
		fmt.Fprintf(w, "  revision: %s\n", orDash(c.Revision))

		if reg := c.Registry; reg != nil && reg.Digest != "" && reg.Digest != c.Digest {
			fmt.Fprintf(w, "  tag at:   %s (pulled: %t)\n", reg.Digest, reg.Pulled)
		}

		if c.Target != "" {
			fmt.Fprintf(w, "  target:   %s (under the next lease)\n", c.Target)
		}

		printCommand(w, c)
		printOverride(w, c.Override)
		fmt.Fprintln(w)
	}

	if st.OverridesFile == "" {
		fmt.Fprintln(w, "overrides: disabled on this host")
	} else {
		fmt.Fprintf(w, "overrides file: %s\n", st.OverridesFile)
	}
}

// labelsOf is the container's kind and team, or that it has no labels.
func labelsOf(c *local.Container) string {
	switch {
	case c.Kind == "":
		return "unlabelled, impact " + string(c.Impact)
	case c.Team == "":
		return string(c.Kind) + ", no team"
	default:
		return string(c.Kind) + " of " + c.Team
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// printCommand writes the command line, one argument per line; an argument
// an override added is marked "+", one it removed "-".
func printCommand(w io.Writer, c *local.Container) {
	fmt.Fprintln(w, "  command:")

	if c.BaseCommand == nil {
		for _, arg := range c.Command {
			fmt.Fprintf(w, "      %s\n", arg)
		}

		return
	}

	for _, line := range diffArgs(c.BaseCommand, c.Command) {
		fmt.Fprintf(w, "    %s\n", line)
	}
}

// diffArgs lists the arguments of now, marking with "+" what base does not
// have, followed by what only base has, marked "-".
func diffArgs(base, now []string) []string {
	left := make(map[string]int, len(base))
	for _, arg := range base {
		left[arg]++
	}

	out := make([]string, 0, len(base)+len(now))

	for _, arg := range now {
		if left[arg] > 0 {
			left[arg]--
			out = append(out, "  "+arg)
		} else {
			out = append(out, "+ "+arg)
		}
	}

	for _, arg := range base {
		if left[arg] > 0 {
			left[arg]--
			out = append(out, "- "+arg)
		}
	}

	return out
}

func printOverride(w io.Writer, o *protocol.ContainerOverride) {
	if o == nil {
		return
	}

	if o.Restoring {
		fmt.Fprintln(w, "  override: gone; the container is put back under the next lease")

		return
	}

	state := "waiting for a lease"

	switch {
	case o.Error != "":
		state = "NOT APPLIED: " + o.Error
	case o.Applied:
		state = "applied"
	}

	fmt.Fprintf(w, "  override: %s\n", state)

	if o.Image != "" {
		fmt.Fprintf(w, "    image:  %s\n", o.Image)
	}

	if len(o.AddFlags)+len(o.RemoveFlags) > 0 {
		fmt.Fprintln(w, "    command, against the default:")

		for _, arg := range o.AddFlags {
			fmt.Fprintf(w, "      + %s\n", arg)
		}

		for _, arg := range o.RemoveFlags {
			fmt.Fprintf(w, "      - %s\n", arg)
		}
	}

	until := "no end"
	if o.Until != nil {
		until = "until " + o.Until.Local().Format(time.RFC3339)
	}

	fmt.Fprintf(w, "    by %s, %s: %s\n", orDash(o.By), until, orDash(o.Reason))
}
