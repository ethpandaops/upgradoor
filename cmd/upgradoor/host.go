package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent"
	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/agent/overrides"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// envSocket names the agent's socket for the host commands.
const envSocket = "UPGRADOOR_SOCKET"

// newHostCmd is 'upgradoor host': what a person on the node asks of the
// running agent, over its local socket.
func newHostCmd() *cobra.Command {
	var socket string

	cmd := &cobra.Command{
		Use:   "host",
		Short: "Talk to the agent on this host: containers, overrides, hold, rolls",
		Long: "The host commands talk to the running agent over its local socket (root and\n" +
			"the entry user's group). Nothing they ask for happens at once: an override and\n" +
			"a roll are served under a lease of the controller, like every other change.",
	}

	def := os.Getenv(envSocket)
	if def == "" {
		def = agent.DefaultSocketFile
	}

	cmd.PersistentFlags().StringVar(&socket, "socket", def, "the agent's socket ($"+envSocket+")")

	client := func() *local.Client { return local.NewClient(socket) }

	cmd.AddCommand(
		newHostListCmd(client),
		newHostStatusCmd(client),
		newHostOverrideCmd(client),
		newHostRollCmd(client),
		newHostHoldCmd(client),
		newHostReleaseCmd(client),
		newHostInitCmd(client),
	)

	return cmd
}

func newHostListCmd(client func() *local.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the managed containers with image, digest, revision, flags and overrides",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := client().Status(cmd.Context())
			if err != nil {
				return err
			}

			printContainers(cmd.OutOrStdout(), st)

			return nil
		},
	}
}

// printContainers writes every managed container with what an override
// changed about it marked.
func printContainers(w io.Writer, st *local.Status) {
	if st.OverridesError != "" {
		fmt.Fprintf(w, "! the overrides file %s cannot be read: %s\n\n", st.OverridesFile, st.OverridesError)
	}

	for i := range st.Containers {
		c := &st.Containers[i]

		fmt.Fprintf(w, "%s  (%s%s, %s)\n", c.Name, c.Kind, clientOf(c), c.State)

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

		printFlags(w, c)
		printOverride(w, c.Override)
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "overrides file: %s\n", st.OverridesFile)
}

func clientOf(c *local.Container) string {
	if c.Client == "" {
		return ""
	}

	return " " + c.Client
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// printFlags writes the command line, one argument per line; an argument an
// override added is marked "+", one it removed "-".
func printFlags(w io.Writer, c *local.Container) {
	fmt.Fprintln(w, "  flags:")

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

	if len(o.AddFlags) > 0 {
		fmt.Fprintf(w, "    add:    %s\n", strings.Join(o.AddFlags, "  "))
	}

	if len(o.RemoveFlags) > 0 {
		fmt.Fprintf(w, "    remove: %s\n", strings.Join(o.RemoveFlags, "  "))
	}

	until := "no end"
	if o.Until != nil {
		until = "until " + o.Until.Local().Format(time.RFC3339)
	}

	fmt.Fprintf(w, "    by %s, %s: %s\n", orDash(o.By), until, orDash(o.Reason))
}

func newHostStatusCmd(client func() *local.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the hold, the open needs, the lease and the rolls requested here",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := client().Status(cmd.Context())
			if err != nil {
				return err
			}

			printStatus(cmd.OutOrStdout(), st)

			return nil
		},
	}
}

func printStatus(w io.Writer, st *local.Status) {
	fmt.Fprintf(w, "host %s (%s), agent %s, looked at %s\n",
		st.Host, st.Network, st.Version, st.At.Local().Format(time.TimeOnly))

	if st.SyncError != "" {
		fmt.Fprintf(w, "! %s\n", st.SyncError)
	}

	switch {
	case st.Hold.Local:
		fmt.Fprintf(w, "hold: local (%s)\n", orDash(st.Hold.LocalReason))
	case st.Hold.Controller:
		fmt.Fprintln(w, "hold: by the controller")
	default:
		fmt.Fprintln(w, "hold: none")
	}

	if len(st.Needs) == 0 {
		fmt.Fprintln(w, "needs: none")
	}

	for i := range st.Needs {
		fmt.Fprintf(w, "need: %s\n", describeNeed(&st.Needs[i]))
	}

	switch {
	case st.Operation != nil && st.Lease != "":
		fmt.Fprintf(w, "lease: %s, %s\n", st.Lease, orDash(string(st.Operation.Phase)))
	case st.Queue != nil:
		fmt.Fprintf(w, "queue: position %d, %s\n", st.Queue.Position, st.Queue.Reason)
	}

	if len(st.Rolls) == 0 {
		return
	}

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nROLL\tCONTAINER\tBY\tSTATE\tDETAIL")

	for i := range st.Rolls {
		r := &st.Rolls[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Container, r.RequestedBy, r.State, rollDetail(r))
	}

	_ = tw.Flush()
}

func describeNeed(n *protocol.Need) string {
	switch {
	case n.Override != nil && n.Override.Restore:
		return "put " + n.Override.Container + " back on " + n.Override.Image
	case n.Override != nil:
		return "override " + n.Override.Container + " (" + n.Override.Image + ")"
	case n.Image != nil:
		return "roll " + n.Image.Container + " to " + n.Image.ToDigest
	case n.Packages != nil:
		return fmt.Sprintf("upgrade %d packages", len(n.Packages.Packages))
	case n.Reboot != nil:
		return "reboot (" + strings.Join(n.Reboot.Reasons, ", ") + ")"
	default:
		return n.ID
	}
}

func rollDetail(r *local.Roll) string {
	if r.State == local.RollQueued && r.Position > 0 {
		return fmt.Sprintf("position %d: %s", r.Position, r.Detail)
	}

	return r.Detail
}

func newHostOverrideCmd(client func() *local.Client) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "override",
		Short: "Set or clear the override of a container (the agent writes the overrides file)",
	}

	var (
		req   local.OverrideRequest
		until string
	)

	set := &cobra.Command{
		Use:   "set <container>",
		Short: "overrides.Entry the image, the flags or both of a container for a while",
		Long: "Writes the container's entry in the overrides file; an entry that is there is\n" +
			"replaced as a whole. The container is recreated under the next lease.\n\n" +
			"Flags: --remove-flag takes arguments out first (\"--name\" removes the flag\n" +
			"with any value, \"--name=value\" or \"--name value\" exactly that), then\n" +
			"--add-flag appends (\"--name=value\", \"--switch\", or \"--name value\" as two\n" +
			"arguments) what is not there yet.",
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
	set.Flags().StringVar(&until, "until", overrides.DefaultDuration.String(),
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

func newHostHoldCmd(client func() *local.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "hold [reason]",
		Short: "Hold the host: the agent reports no needs and refuses all work",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return printAnswer(cmd, func(ctx context.Context) (*local.Answer, error) {
				return client().Hold(ctx, &local.HoldRequest{Reason: strings.Join(args, " ")})
			})
		},
	}
}

func newHostReleaseCmd(client func() *local.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "release",
		Short: "Release the local hold (a hold of the controller is lifted on the page)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printAnswer(cmd, func(ctx context.Context) (*local.Answer, error) {
				return client().Hold(ctx, &local.HoldRequest{Release: true})
			})
		},
	}
}

func newHostInitCmd(client func() *local.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "init-overrides",
		Short: "Write the generated block of the overrides file (where Ansible does not)",
		Long: "Writes the block at the top of the overrides file: the how-to, these commands\n" +
			"and what the agent's configuration deploys on this host. Entries below the\n" +
			"block are kept. The Ansible role writes the same block on every run.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printAnswer(cmd, func(ctx context.Context) (*local.Answer, error) {
				return client().InitOverrides(ctx)
			})
		},
	}
}

func newHostRollCmd(client func() *local.Client) *cobra.Command {
	var noWait bool

	cmd := &cobra.Command{
		Use:   "roll <container>...",
		Short: "Request a roll of containers and follow it until it is done",
		Long: "Asks the agent to recreate the containers on the image their tag points at\n" +
			"now. The roll waits for a slot of the controller like every other; the\n" +
			"command prints how far it is and ends with it. --no-wait returns at once;\n" +
			"'upgradoor host status' shows the roll later.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
		},
	}

	cmd.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for the roll")

	return cmd
}

// followRolls prints every change of the named rolls until all are final.
// It fails when one of them did.
func followRolls(ctx context.Context, out io.Writer, c *local.Client, ids []string) error {
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
			if _, mine := indexOf(ids, r.ID); !mine {
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

func indexOf(list []string, v string) (int, bool) {
	for i := range list {
		if list[i] == v {
			return i, true
		}
	}

	return 0, false
}
