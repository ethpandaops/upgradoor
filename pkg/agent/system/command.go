package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The closed set of programs this package runs. There is no other way to
// execute anything: every call site names one of these constants and the
// default runner refuses everything else.
const (
	cmdDpkgQuery = "dpkg-query"
	cmdDpkg      = "dpkg"
	cmdAptGet    = "apt-get"
	cmdSystemctl = "systemctl"
)

const (
	// maxCommandStdout bounds the captured standard output of one command,
	// far above what any of them prints: a dpkg-query listing of a large
	// host is well below 1 MiB.
	maxCommandStdout = 64 << 20
	// maxCommandErrorOutput is how much of a failed command's combined
	// output ends up in the error.
	maxCommandErrorOutput = 8 << 10
	// commandWaitDelay is how long a cancelled command gets to exit after
	// SIGTERM before it is killed.
	commandWaitDelay = 10 * time.Second
)

// commandDirs are searched, in order, for the programs. The agent's own PATH
// is not consulted.
var commandDirs = []string{"/usr/bin", "/usr/sbin", "/bin", "/sbin"}

// commandEnv is the whole environment of every command.
var commandEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"LC_ALL=C",
	"LANG=C",
	"DEBIAN_FRONTEND=noninteractive",
	"APT_LISTCHANGES_FRONTEND=none",
}

// ErrCommandNotAllowed is returned for a command outside the closed set.
var ErrCommandNotAllowed = errors.New("system: command not allowed")

// CommandRunner executes one of the fixed programs with an argument vector
// and returns its standard output. Implementations must not interpret the
// arguments through a shell. A program that could not be run or exited
// non-zero is reported as a *CommandError.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// CommandError describes a command that failed.
type CommandError struct {
	Name string
	Args []string
	// ExitCode is the exit status, or -1 when the program did not exit by
	// itself (not started, killed by a signal).
	ExitCode int
	// Signal is the signal that ended the program, if any.
	Signal syscall.Signal
	// Output is the tail of the combined standard output and error.
	Output string
	Err    error
}

// Error implements error.
func (e *CommandError) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s: %v", strings.Join(append([]string{e.Name}, e.Args...), " "), e.Err)

	if out := strings.TrimSpace(e.Output); out != "" {
		b.WriteString("; output: ")
		b.WriteString(out)
	}

	return b.String()
}

// Unwrap returns the underlying error.
func (e *CommandError) Unwrap() error {
	return e.Err
}

// commandAllowed reports whether name with args is inside the closed set.
// systemctl is limited to the single invocation "systemctl reboot".
func commandAllowed(name string, args []string) bool {
	switch name {
	case cmdDpkgQuery, cmdDpkg, cmdAptGet:
		return true
	case cmdSystemctl:
		return slices.Equal(args, []string{"reboot"})
	default:
		return false
	}
}

// execRunner is the CommandRunner used outside tests.
type execRunner struct{}

var _ CommandRunner = execRunner{}

// Run implements CommandRunner.
func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	fail := func(err error) *CommandError {
		return &CommandError{Name: name, Args: args, ExitCode: -1, Err: err}
	}

	if !commandAllowed(name, args) {
		return nil, fail(ErrCommandNotAllowed)
	}

	path, err := lookupCommand(name)
	if err != nil {
		return nil, fail(err)
	}

	if err := ctx.Err(); err != nil {
		return nil, fail(err)
	}

	var (
		stdout   = &cappedBuffer{limit: maxCommandStdout}
		combined = &tailBuffer{limit: maxCommandErrorOutput}
	)

	// path is one of four fixed programs resolved in fixed directories and
	// args is an argument vector; nothing goes through a shell.
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = commandEnv
	cmd.Stdout = &teeWriter{stdout, combined}
	cmd.Stderr = combined
	cmd.WaitDelay = commandWaitDelay
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}

	if err := cmd.Run(); err != nil {
		cerr := fail(err)
		cerr.Output = combined.String()

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			cerr.ExitCode = exitErr.ExitCode()

			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				cerr.Signal = ws.Signal()
			}
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			cerr.Err = fmt.Errorf("%w (%w)", ctxErr, err)
		}

		return nil, cerr
	}

	if stdout.truncated {
		return nil, fail(fmt.Errorf("output exceeds %d bytes", maxCommandStdout))
	}

	return stdout.buf.Bytes(), nil
}

// lookupCommand finds one of the fixed programs in the fixed directories.
func lookupCommand(name string) (string, error) {
	for _, dir := range commandDirs {
		path := filepath.Join(dir, name)

		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}

		return path, nil
	}

	return "", fmt.Errorf("%w in %s", exec.ErrNotFound, strings.Join(commandDirs, ":"))
}

// cappedBuffer stores up to limit bytes and remembers whether more arrived.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room < len(p) {
		b.truncated = true
		b.buf.Write(p[:max(room, 0)])
	} else {
		b.buf.Write(p)
	}

	return len(p), nil
}

// tailBuffer keeps the last limit bytes written to it. It is safe for
// concurrent use because stdout and stderr are copied by separate goroutines.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}

	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.buf)
}

// teeWriter writes to both of its writers, which never fail.
type teeWriter struct {
	first  *cappedBuffer
	second *tailBuffer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	if _, err := t.first.Write(p); err != nil {
		return 0, err
	}

	return t.second.Write(p)
}
