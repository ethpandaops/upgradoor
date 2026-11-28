package system

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// cmdJournalctl reads the sshd entries of the journal, with journalArgs only.
const cmdJournalctl = "journalctl"

// journalFixedArgs start every journalctl call: JSON output of the sshd
// entries (sshd-session is the per-session process from OpenSSH 9.8 on).
var journalFixedArgs = []string{
	"--no-pager", "--quiet", "--output=json",
	"--output-fields=MESSAGE,_PID,SYSLOG_PID,_BOOT_ID",
	"SYSLOG_IDENTIFIER=sshd", "SYSLOG_IDENTIFIER=sshd-session",
}

// journalCursor is the form of a journal cursor.
var journalCursor = regexp.MustCompile(`^[0-9A-Za-z=;]{1,512}$`)

// journalArgs are the arguments of a read after cursor, or from since when
// cursor is empty, up to until.
func journalArgs(cursor string, since, until time.Time) []string {
	args := slices.Clone(journalFixedArgs)

	if cursor != "" {
		args = append(args, "--after-cursor="+cursor)
	} else {
		args = append(args, "--since=@"+strconv.FormatInt(since.Unix(), 10))
	}

	return append(args, "--until=@"+strconv.FormatInt(until.Unix(), 10))
}

// journalAllowed reports whether args are journalArgs of some read.
func journalAllowed(args []string) bool {
	n := len(journalFixedArgs)
	if len(args) != n+2 || !slices.Equal(args[:n], journalFixedArgs) {
		return false
	}

	start, until := args[n], args[n+1]

	if c, ok := strings.CutPrefix(start, "--after-cursor="); ok {
		if !journalCursor.MatchString(c) {
			return false
		}
	} else if !unixArg(start, "--since=@") {
		return false
	}

	return unixArg(until, "--until=@")
}

// unixArg reports whether arg is prefix and a Unix time.
func unixArg(arg, prefix string) bool {
	v, ok := strings.CutPrefix(arg, prefix)
	if !ok {
		return false
	}

	_, err := strconv.ParseUint(v, 10, 63)

	return err == nil
}

// SSHJournal implements System.
func (s *osSystem) SSHJournal(ctx context.Context, cursor string, since, until time.Time) ([]byte, error) {
	if cursor != "" && !journalCursor.MatchString(cursor) {
		return nil, fmt.Errorf("journal: not a cursor: %q", cursor)
	}

	out, err := s.runner.Run(ctx, cmdJournalctl, journalArgs(cursor, since, until)...)
	if err != nil {
		return nil, fmt.Errorf("journal: %w", err)
	}

	return out, nil
}

// sshdNames are the process names of an sshd session.
var sshdNames = []string{"sshd", "sshd-session"}

// SSHAlive implements System.
func (s *osSystem) SSHAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	raw, err := os.ReadFile(s.path("/proc/" + strconv.Itoa(pid) + "/comm"))
	if err != nil {
		return false
	}

	return slices.Contains(sshdNames, strings.TrimSpace(string(raw)))
}
