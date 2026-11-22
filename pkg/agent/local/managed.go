package local

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/ethpandaops/upgradoor/pkg/agent/plan"
)

// DefaultManagedFile is managed.yaml: what Ansible deployed each container
// as, written only by upgradoor deploy.
const DefaultManagedFile = "/etc/upgradoor/managed.yaml"

const (
	managedHeader = "# written by upgradoor deploy, do not edit"
	// managedMode lets the agent's group (the directory's) read the file.
	managedMode = 0o640
	lockMode    = 0o600
	lockSuffix  = ".lock"
)

type managedYAML struct {
	Containers map[string]*deployedYAML `yaml:"containers"`
}

type deployedYAML struct {
	Image   string    `yaml:"image"`
	Command *[]string `yaml:"command"`
}

// ReadManaged reads managed.yaml; a missing file deploys nothing. The file
// is read only as a regular file not reached through a symbolic link.
func ReadManaged(path string) (map[string]plan.Deployed, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return map[string]plan.Deployed{}, nil
	case errors.Is(err, syscall.ELOOP):
		return nil, errors.New("managed.yaml is a symbolic link")
	case err != nil:
		return nil, err
	}

	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, errors.New("managed.yaml is not a regular file")
	}

	raw, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return nil, err
	}

	if len(raw) > maxFileSize {
		return nil, fmt.Errorf("managed.yaml is larger than %d bytes", maxFileSize)
	}

	return parseManaged(raw)
}

// parseManaged reads managed.yaml strictly.
func parseManaged(raw []byte) (map[string]plan.Deployed, error) {
	var doc managedYAML

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New(oneLine(err.Error()))
	}

	out := make(map[string]plan.Deployed, len(doc.Containers))

	for name, y := range doc.Containers {
		if !containerName.MatchString(name) {
			return nil, fmt.Errorf("%q is not a valid container name", name)
		}

		if y == nil || strings.TrimSpace(y.Image) == "" {
			return nil, fmt.Errorf("container %q names no image", name)
		}

		d := plan.Deployed{Image: strings.TrimSpace(y.Image)}
		if y.Command != nil {
			d.Command, d.HasCommand = append([]string{}, *y.Command...), true
		}

		out[name] = d
	}

	return out, nil
}

// renderManaged writes managed.yaml, containers sorted by name.
func renderManaged(containers map[string]plan.Deployed) []byte {
	out := []string{managedHeader}

	if len(containers) == 0 {
		out = append(out, "containers: {}")
	} else {
		out = append(out, "containers:")
	}

	for _, name := range slices.Sorted(maps.Keys(containers)) {
		d := containers[name]
		out = append(out, "  "+scalar(name)+":", "    image: "+scalar(d.Image))

		if d.HasCommand {
			out = append(out, commandLines("    ", d.Command)...)
		}
	}

	return []byte(strings.Join(out, "\n") + "\n")
}

// WriteManaged writes one container's entry of managed.yaml under an
// exclusive lock of <path>.lock and calls then with whether the file changed
// while it still holds the lock. The file is replaced atomically, mode 0640
// with the group of its directory. A lock file that cannot be opened is an
// fs.ErrPermission for a caller that may not write the directory.
func WriteManaged(path, container string, d plan.Deployed, then func(changed bool) error) error {
	if !containerName.MatchString(container) {
		return fmt.Errorf("%q is not a valid container name", container)
	}

	if err := d.Validate(); err != nil {
		return err
	}

	if !d.HasCommand {
		d.Command = nil
	}

	lock, err := os.OpenFile(path+lockSuffix, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, lockMode)
	if err != nil {
		return err
	}

	defer func() { _ = lock.Close() }()

	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock managed.yaml: %w", err)
	}

	changed, err := writeManagedLocked(path, container, d)
	if err != nil {
		return err
	}

	return then(changed)
}

func writeManagedLocked(path, container string, d plan.Deployed) (bool, error) {
	cur, err := ReadManaged(path)
	if err != nil {
		return false, fmt.Errorf("managed.yaml cannot be read (%w); delete it and deploy again", err)
	}

	cur[container] = d
	data := renderManaged(cur)

	if old, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(old, data) {
		return false, nil
	}

	dir := filepath.Dir(path)

	info, err := os.Stat(dir)
	if err != nil {
		return false, err
	}

	_, gid, _ := ownerOf(info)

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+tempInfix+"*")
	if err != nil {
		return false, fmt.Errorf("write managed.yaml: %w", err)
	}

	name := tmp.Name()

	err = writeTemp(tmp, data, managedMode, os.Geteuid(), gid, gid != os.Getegid())
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(name, path)
	}

	if err != nil {
		_ = os.Remove(name)

		return false, fmt.Errorf("write managed.yaml: %w", err)
	}

	if err := syncDir(dir); err != nil {
		return true, fmt.Errorf("sync managed.yaml's directory: %w", err)
	}

	return true, nil
}
