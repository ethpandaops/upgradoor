package system

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJournalAllowed(t *testing.T) {
	t.Parallel()

	since := time.Unix(1795000000, 0)
	until := since.Add(time.Hour)
	cursor := "s=f798acee590a4f18;i=13a2af35;b=5f0f11f5;m=551f3afc;t=6609609ab090a;x=ab6bb9990e4c85ea"

	assert.True(t, commandAllowed(cmdJournalctl, journalArgs("", since, until)))
	assert.True(t, commandAllowed(cmdJournalctl, journalArgs(cursor, since, until)))

	fixed := slices.Clone(journalFixedArgs)

	for name, args := range map[string][]string{
		"nothing":            nil,
		"no until":           append(slices.Clone(fixed), "--since=@1"),
		"another identifier": append(append(slices.Clone(fixed[:4]), "SYSLOG_IDENTIFIER=sudo", fixed[5]), "--since=@1", "--until=@2"),
		"another field":      append(slices.Clone(fixed), "--since=@1", "--until=@2", "--output-fields=MESSAGE"),
		"follow":             append(slices.Clone(fixed), "--follow", "--until=@2"),
		"a cursor that is no cursor": append(slices.Clone(fixed),
			"--after-cursor=a b", "--until=@2"),
		"a time that is no time": append(slices.Clone(fixed), "--since=yesterday", "--until=@2"),
		"a negative time":        append(slices.Clone(fixed), "--since=@-1", "--until=@2"),
		"the whole journal":      {"--no-pager", "--output=json"},
		"flush":                  {"--flush"},
	} {
		assert.False(t, commandAllowed(cmdJournalctl, args), name)
	}
}

func TestSSHJournal(t *testing.T) {
	t.Parallel()

	since := time.Unix(1795000000, 0)
	until := since.Add(time.Hour)
	runner := &fakeRunner{responses: map[string]fakeResponse{}}
	key := "journalctl " + strings.Join(journalArgs("", since, until), " ")
	runner.set(key, []byte("{}\n"), nil)

	s := newOSSystem(logrus.New(), nil, t.TempDir(), runner)

	out, err := s.SSHJournal(context.Background(), "", since, until)
	require.NoError(t, err)
	assert.Equal(t, "{}\n", string(out))
	assert.Equal(t, []string{key}, runner.recorded())
	assert.Contains(t, key, "--since=@1795000000 --until=@1795003600")

	_, err = s.SSHJournal(context.Background(), "x;y z", since, until)
	require.ErrorContains(t, err, "not a cursor")

	_, err = s.SSHJournal(context.Background(), "s=1;i=2", since, until)
	require.Error(t, err, "the runner fails")
	assert.Contains(t, runner.recorded()[1], "--after-cursor=s=1;i=2 --until=@1795003600")
}

func TestSSHAlive(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, root, "/proc/100/comm", "sshd\n")
	writeFile(t, root, "/proc/101/comm", "sshd-session\n")
	writeFile(t, root, "/proc/102/comm", "bash\n")

	s := newOSSystem(logrus.New(), nil, root, &fakeRunner{})

	assert.True(t, s.SSHAlive(100))
	assert.True(t, s.SSHAlive(101))
	assert.False(t, s.SSHAlive(102), "the PID is another program's now")
	assert.False(t, s.SSHAlive(103), "gone")
	assert.False(t, s.SSHAlive(0))
}
