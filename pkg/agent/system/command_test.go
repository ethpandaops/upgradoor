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
)

func TestCommandAllowed(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"dpkg-query", []string{"-W"}, true},
		{"dpkg", []string{"--print-architecture"}, true},
		{"apt-get", []string{"update"}, true},
		{"apt-cache", []string{"policy", "docker-ce"}, true},
		{"uname", []string{"-r"}, true},
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
		require.ErrorIs(t, err, ErrCommandNotAllowed, "%v", cmd)

		var cmdErr *CommandError

		require.ErrorAs(t, err, &cmdErr)
		assert.Equal(t, cmd[0], cmdErr.Name)
	}

	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// uname exists on every Linux, so the real runner can be exercised with it.
func TestExecRunnerRunsUname(t *testing.T) {
	if _, err := lookupCommand(cmdUname); err != nil {
		t.Skip("uname not found")
	}

	out, err := execRunner{}.Run(t.Context(), cmdUname, "-r")
	require.NoError(t, err)

	want, err := os.ReadFile("/proc/sys/kernel/osrelease")
	require.NoError(t, err)
	assert.Equal(t, string(want), string(out))

	t.Run("failure carries exit code and output", func(t *testing.T) {
		_, err := execRunner{}.Run(t.Context(), cmdUname, "--no-such-option")

		var cmdErr *CommandError

		require.ErrorAs(t, err, &cmdErr)
		assert.Positive(t, cmdErr.ExitCode)
		assert.Zero(t, cmdErr.Signal)
		assert.Contains(t, cmdErr.Output, "no-such-option")
		assert.Contains(t, err.Error(), "uname --no-such-option: exit status")
		assert.Contains(t, err.Error(), "output: ")

		var exitErr *exec.ExitError

		require.ErrorAs(t, err, &exitErr)
	})

	t.Run("cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := execRunner{}.Run(ctx, cmdUname, "-r")
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestLookupCommandIgnoresPath(t *testing.T) {
	// A program of the right name early in PATH is not picked up.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(dir+"/uname", []byte("#!/bin/sh\necho hijacked\n"), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	path, err := lookupCommand(cmdUname)
	if err != nil {
		t.Skip("uname not found")
	}

	assert.NotContains(t, path, dir)

	out, err := execRunner{}.Run(t.Context(), cmdUname, "-s")
	require.NoError(t, err)
	assert.Equal(t, "Linux\n", string(out))

	_, err = lookupCommand("no-such-program-anywhere")
	require.ErrorIs(t, err, exec.ErrNotFound)
}

func TestCommandErrorMessage(t *testing.T) {
	err := &CommandError{
		Name: "apt-get", Args: []string{"install", "docker-ce"}, ExitCode: 100,
		Err: errors.New("exit status 100"), Output: "\nE: Broken packages\n",
	}

	assert.Equal(t, "apt-get install docker-ce: exit status 100; output: E: Broken packages",
		err.Error())

	err.Output = ""
	assert.Equal(t, "apt-get install docker-ce: exit status 100", err.Error())
}

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

func TestCommandEnvironmentIsFixed(t *testing.T) {
	assert.Contains(t, commandEnv, "DEBIAN_FRONTEND=noninteractive")
	assert.Contains(t, commandEnv, "LC_ALL=C")

	for _, entry := range commandEnv {
		assert.Contains(t, entry, "=")
	}

	assert.Less(t, commandWaitDelay, time.Minute)
}
