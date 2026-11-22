package system

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Only the closed set of programs runs.
func TestCommandAllowed(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"dpkg-query", []string{"-W"}, true},
		{"dpkg", []string{"--print-architecture"}, true},
		{"apt-get", []string{"update"}, true},
		{"systemctl", []string{"reboot"}, true},

		{"systemctl", nil, false},
		{"systemctl", []string{"poweroff"}, false},
		{"systemctl", []string{"reboot", "--force"}, false},
		{"systemctl", []string{"stop", "docker"}, false},
		{"sh", []string{"-c", "id"}, false},
		{"bash", nil, false},
		{"/usr/bin/apt-get", []string{"update"}, false},
		{"../bin/sh", nil, false},
		{"apt", []string{"install", "x"}, false},
		{"apt-cache", []string{"policy", "docker-ce"}, false},
		{"uname", []string{"-r"}, false},
		{"reboot", nil, false},
		{"", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name+" "+strings.Join(tt.args, " "), func(t *testing.T) {
			assert.Equal(t, tt.want, commandAllowed(tt.name, tt.args))
		})
	}
}

func TestExecRunnerRefusesOtherCommands(t *testing.T) {
	marker := t.TempDir() + "/ran"

	for _, cmd := range [][]string{
		{"sh", "-c", "touch " + marker},
		{"touch", marker},
		{"/bin/touch", marker},
		{"systemctl", "poweroff"},
		{"systemctl", "reboot", "--force"},
	} {
		_, err := execRunner{}.Run(t.Context(), cmd[0], cmd[1:]...)
		require.ErrorIs(t, err, errCommandNotAllowed, "%v", cmd)

		var cmdErr *commandError

		require.ErrorAs(t, err, &cmdErr)
		assert.Equal(t, cmd[0], cmdErr.Name)
	}

	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// Skipped where dpkg does not exist.
func TestExecRunnerRunsDpkg(t *testing.T) {
	if _, err := lookupCommand(cmdDpkg); err != nil {
		t.Skip("dpkg not found")
	}

	out, err := execRunner{}.Run(t.Context(), cmdDpkg, "--print-architecture")
	require.NoError(t, err)
	assert.Regexp(t, `^[a-z0-9]+\n$`, string(out))

	t.Run("failure carries exit code and output", func(t *testing.T) {
		_, err := execRunner{}.Run(t.Context(), cmdDpkg, "--no-such-option")

		var cmdErr *commandError

		require.ErrorAs(t, err, &cmdErr)
		assert.Positive(t, cmdErr.ExitCode)
		assert.Zero(t, cmdErr.Signal)
		assert.Contains(t, cmdErr.Output, "no-such-option")
		assert.Contains(t, err.Error(), "dpkg --no-such-option: exit status")
		assert.Contains(t, err.Error(), "output: ")

		var exitErr *exec.ExitError

		require.ErrorAs(t, err, &exitErr)
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := execRunner{}.Run(ctx, cmdDpkg, "--print-architecture")
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestLookupCommandIgnoresPath(t *testing.T) {
	// A program of the right name early in PATH is not picked up.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(dir+"/dpkg", []byte("#!/bin/sh\necho hijacked\n"), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	path, err := lookupCommand(cmdDpkg)
	if err != nil {
		t.Skip("dpkg not found")
	}

	assert.NotContains(t, path, dir)

	out, err := execRunner{}.Run(t.Context(), cmdDpkg, "--print-architecture")
	require.NoError(t, err)
	assert.NotContains(t, string(out), "hijacked")

	_, err = lookupCommand("no-such-program-anywhere")
	require.ErrorIs(t, err, exec.ErrNotFound)
}

func TestCommandErrorMessage(t *testing.T) {
	err := &commandError{
		Name: "apt-get", Args: []string{"install", "docker-ce"}, ExitCode: 100,
		Err: errors.New("exit status 100"), Output: "\nE: Broken packages\n",
	}

	assert.Equal(t, "apt-get install docker-ce: exit status 100; output: E: Broken packages",
		err.Error())

	err.Output = ""
	assert.Equal(t, "apt-get install docker-ce: exit status 100", err.Error())
}

// Command output is bounded.
func TestOutputBuffers(t *testing.T) {
	tail := &tailBuffer{limit: 8}
	for _, chunk := range []string{"abc", "defgh", "ij", "0123456789abcdef", "Z"} {
		n, err := tail.Write([]byte(chunk))
		require.NoError(t, err)
		assert.Equal(t, len(chunk), n)
	}

	assert.Equal(t, "9abcdefZ", tail.String())

	capped := &cappedBuffer{limit: 5}

	n, err := capped.Write([]byte("abc"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.False(t, capped.truncated)

	n, err = capped.Write([]byte("defg"))
	require.NoError(t, err)
	assert.Equal(t, 4, n, "writes never fail, so the program is not blocked")
	assert.True(t, capped.truncated)

	_, err = capped.Write([]byte("more"))
	require.NoError(t, err)
	assert.Equal(t, "abcde", capped.buf.String())
}

// An action runs without a shell around it, by its argv, until it exits or
// its timeout passes; the result keeps the tail of the combined output.
func TestRunAction(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}

	long := strings.Repeat("x", protocol.MaxActionOutput) + "tail"

	tests := []struct {
		name    string
		argv    []string
		timeout time.Duration
		want    protocol.ActionResult
	}{
		{
			name: "success", argv: []string{sh, "-c", "echo out; echo err >&2"}, timeout: time.Minute,
			want: protocol.ActionResult{ExitCode: 0, Output: "out\nerr\n"},
		},
		{
			name: "exit code", argv: []string{sh, "-c", "echo locked >&2; exit 3"}, timeout: time.Minute,
			want: protocol.ActionResult{ExitCode: 3, Output: "locked\n"},
		},
		{
			name: "timeout", argv: []string{sh, "-c", "echo started; exec sleep 30"}, timeout: 200 * time.Millisecond,
			want: protocol.ActionResult{ExitCode: -1, Output: "started\n", TimedOut: true},
		},
		{
			name: "output tail", argv: []string{sh, "-c", "printf %s " + long}, timeout: time.Minute,
			want: protocol.ActionResult{ExitCode: 0, Output: long[len(long)-protocol.MaxActionOutput:]},
		},
		{
			name: "no shell", argv: []string{sh, "-c", "echo $0", "a;b"}, timeout: time.Minute,
			want: protocol.ActionResult{ExitCode: 0, Output: "a;b\n"},
		},
		{
			name: "relative program", argv: []string{"sh", "-c", "true"}, timeout: time.Minute,
			want: protocol.ActionResult{ExitCode: -1, Output: errCommandNotAllowed.Error()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RunAction(t.Context(), tt.argv, tt.timeout)
			assert.Equal(t, tt.want, got)
		})
	}
}
