package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/upgradoor/pkg/agent/local"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the hold, the open needs, the lease and the rolls requested here",
		Args:  cobra.NoArgs,
	}

	client := socketFlag(cmd)

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		st, err := client().Status(cmd.Context())
		if err != nil {
			return err
		}

		printStatus(cmd.OutOrStdout(), st)

		return nil
	}

	return cmd
}

func printStatus(w io.Writer, st *local.Status) {
	fmt.Fprintf(w, "host %s (%s), agent %s, looked at %s\n",
		st.Host, st.Network, st.Version, st.At.Local().Format(time.TimeOnly))

	if st.SyncError != "" {
		fmt.Fprintf(w, "! %s\n", st.SyncError)
	}

	if !st.Hold.Active() {
		fmt.Fprintln(w, "hold: none")
	}

	for _, h := range st.Hold.Holds {
		fmt.Fprintf(w, "hold: %s by %s, %s (%s)\n", h.Holder, orDash(h.By),
			h.At.Local().Format(time.DateTime), orDash(h.Reason))
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
