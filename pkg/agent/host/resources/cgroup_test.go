package resources

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCgroupDir(t *testing.T) {
	s, root, _ := testSampler(t)

	for _, tt := range []struct {
		name, content, id, want string
	}{
		{name: "systemd driver", content: "0::/system.slice/docker-abc.scope\n", id: "abc",
			want: "system.slice/docker-abc.scope"},
		{name: "cgroupfs driver, child cgroup", content: "0::/docker/abc/init.scope\n", id: "abc",
			want: "docker/abc"},
		{name: "hybrid hierarchy", content: "12:cpu:/docker/abc\n0::/custom/abc\n", id: "abc",
			want: "custom/abc"},
		{name: "custom parent without the id", content: "0::/ci/job-7\n", id: "abc", want: "ci/job-7"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(root, "proc", "77")
			require.NoError(t, os.MkdirAll(dir, 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "cgroup"), []byte(tt.content), 0o600))

			got, err := s.cgroupDir(77, tt.id)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	_, err := s.cgroupDir(78, "abc")
	assert.Error(t, err, "no such process")
}

func TestStatValue(t *testing.T) {
	raw := []byte("anon 5\ninactive_file 7\n")

	v, ok := statValue(raw, "inactive_file")
	assert.True(t, ok)
	assert.Equal(t, uint64(7), v)

	_, ok = statValue(raw, "file")
	assert.False(t, ok)
}
