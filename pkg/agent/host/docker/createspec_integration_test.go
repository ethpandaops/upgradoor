//go:build docker_integration

package docker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ansiblePlaybook mirrors the container task of ethpandaops' client roles.
const ansiblePlaybook = `
- hosts: localhost
  connection: local
  gather_facts: false
  tasks:
    - name: Run container
      community.docker.docker_container:
        name: "{{ c_name }}"
        image: "{{ c_image }}"
        image_name_mismatch: recreate
        state: started
        restart_policy: always
        stop_timeout: "{{ c_stop_timeout }}"
        ports: "{{ c_ports }}"
        volumes: "{{ c_volumes }}"
        env: "{{ c_env }}"
        networks: "{{ c_networks }}"
        entrypoint: "{{ c_entrypoint | default(omit) }}"
        command: "{{ c_command }}"
        user: "{{ c_user }}"
        pull: false
        security_opts: "{{ c_security_opts }}"
        memory: "{{ c_memory | default(omit) }}"
        memory_reservation: "{{ c_memory_reservation | default(omit) }}"
        cpus: "{{ c_cpus | default(omit) }}"
        cpu_shares: "{{ c_cpu_shares | default(omit) }}"
`

var (
	recapChanged = regexp.MustCompile(`\bchanged=(\d+)`)
	recapFailed  = regexp.MustCompile(`\bfailed=(\d+)`)
	recapOK      = regexp.MustCompile(`\bok=(\d+)`)
)

// findAnsible locates ansible-playbook via $UPGRADOOR_ANSIBLE_PLAYBOOK, PATH or ~/.python/bin.
func findAnsible() string {
	if path := os.Getenv("UPGRADOOR_ANSIBLE_PLAYBOOK"); path != "" {
		return path
	}

	if path, err := exec.LookPath("ansible-playbook"); err == nil {
		return path
	}

	if home, err := os.UserHomeDir(); err == nil {
		path := filepath.Join(home, ".python", "bin", "ansible-playbook")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}

	return ""
}

// ansibleRun runs the playbook and returns the number of changed tasks.
type ansibleRun struct {
	binary   string
	dir      string
	playbook string
	vars     string
}

func newAnsibleRun(t *testing.T, binary string, vars map[string]any) *ansibleRun {
	t.Helper()

	dir := t.TempDir()
	run := &ansibleRun{
		binary:   binary,
		dir:      dir,
		playbook: filepath.Join(dir, "playbook.yaml"),
		vars:     filepath.Join(dir, "vars.json"),
	}

	// Ansible's own interpreter has the module's Python dependencies.
	if python := filepath.Join(filepath.Dir(binary), "python3"); fileExists(python) {
		vars["ansible_python_interpreter"] = python
	}

	raw, err := json.Marshal(vars)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(run.vars, raw, 0o600))
	require.NoError(t, os.WriteFile(run.playbook, []byte(ansiblePlaybook), 0o600))

	return run
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func (a *ansibleRun) play(t *testing.T) (changed int, output string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.binary,
		"-i", "localhost,", "--diff", "-v", "-e", "@"+a.vars, a.playbook)
	cmd.Dir = a.dir
	cmd.Env = append(os.Environ(),
		"DOCKER_HOST="+env.host,
		"ANSIBLE_NOCOLOR=1",
		"ANSIBLE_FORCE_COLOR=0",
		"ANSIBLE_RETRY_FILES_ENABLED=0",
		"ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False",
		"ANSIBLE_LOCAL_TEMP="+filepath.Join(a.dir, "local"),
		"ANSIBLE_REMOTE_TEMP="+filepath.Join(a.dir, "remote"),
	)

	out, err := cmd.CombinedOutput()
	output = string(out)
	require.NoError(t, err, "ansible-playbook failed:\n%s", output)

	failed := recapFailed.FindStringSubmatch(output)
	require.NotNil(t, failed, "no play recap:\n%s", output)
	require.Equal(t, "0", failed[1], output)

	ok := recapOK.FindStringSubmatch(output)
	require.NotNil(t, ok, output)
	require.Equal(t, "1", ok[1], "the one task must have run:\n%s", output)

	match := recapChanged.FindStringSubmatch(output)
	require.NotNil(t, match, "no play recap:\n%s", output)

	changed, err = strconv.Atoi(match[1])
	require.NoError(t, err)

	return changed, output
}

// A container Ansible created and Recreate rolled is still unchanged to Ansible.
func TestIntegrationAnsibleSeesNoChange(t *testing.T) {
	binary := findAnsible()
	if binary == "" {
		t.Skip("ansible-playbook not found (PATH, ~/.python/bin or $UPGRADOOR_ANSIBLE_PLAYBOOK): " +
			"the check against community.docker.docker_container is skipped")
	}

	loop := []string{"sh", "-c", loopScript}

	tests := []struct {
		name string
		// vars returns the play variables; subnet is the network's first three octets.
		vars func(t *testing.T, name, dir, subnet string) map[string]any
	}{
		{
			// The geth role's defaults.
			name: "geth-defaults",
			vars: func(t *testing.T, _, dir, _ string) map[string]any {
				t.Helper()

				return map[string]any{
					"c_stop_timeout": "300",
					"c_ports": []string{
						"127.0.0.1:" + freePort(t) + ":8545",
						"127.0.0.1:" + freePort(t) + ":8551",
						"0.0.0.0:" + freePort(t) + ":30303",
						"0.0.0.0:" + freePort(t) + ":30303/udp",
					},
					"c_volumes":       []string{dir + ":/data", filepath.Join(dir, "jwt") + ":/execution-auth.jwt:ro"},
					"c_env":           map[string]string{},
					"c_networks":      []any{},
					"c_command":       append(loop, "--", "--datadir=/data", "--http"),
					"c_user":          "1000",
					"c_security_opts": []string{},
				}
			},
		},
		{
			// Everything the role can set.
			name: "everything",
			vars: func(t *testing.T, name, dir, subnet string) map[string]any {
				t.Helper()

				return map[string]any{
					"c_stop_timeout": "120",
					"c_ports":        []string{"127.0.0.1:" + freePort(t) + ":8545", freePort(t) + ":9000/udp"},
					"c_volumes": []string{
						dir + ":/data",
						name + "-named:/named",
						"/anon",
					},
					"c_env": map[string]string{"OWN": "1", "IMAGE_OVERRIDDEN": "by-ansible"},
					"c_networks": []any{map[string]any{
						"name":         name + "-net",
						"aliases":      []string{"el"},
						"ipv4_address": subnet + ".10",
					}},
					"c_entrypoint":         loop,
					"c_command":            []string{"--", "--datadir=/data"},
					"c_user":               "1000:1000",
					"c_security_opts":      []string{"no-new-privileges:true"},
					"c_memory":             "256M",
					"c_memory_reservation": "128M",
					"c_cpus":               "0.5",
					"c_cpu_shares":         512,
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			short := "ansible-" + tt.name
			env.cleanup(t, short)

			ctx := t.Context()
			r := env.newRuntime(t)
			ref := env.repoRef(short)
			name := env.prefix + short

			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0o777))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "jwt"), []byte("secret"), 0o644))

			subnet := env.createNetwork(t, name+"-net")

			vars := tt.vars(t, name, dir, subnet)
			vars["c_name"] = name
			vars["c_image"] = ref

			digest1 := env.publish(t, ref, variantOne)
			pulled, _, err := r.Pull(ctx, ref, nil)
			require.NoError(t, err)
			require.Equal(t, digest1, pulled)

			ansible := newAnsibleRun(t, binary, vars)

			changed, out := ansible.play(t)
			require.Equal(t, 1, changed, "Ansible creates the container:\n%s", out)

			created := env.inspect(t, name)
			require.True(t, created.State.Running)

			// The playbook must be idempotent for the check below to mean anything.
			changed, out = ansible.play(t)
			require.Zero(t, changed, "the playbook is not idempotent by itself:\n%s", out)
			require.Equal(t, created.ID, env.inspect(t, name).ID)

			// A new image is pushed under the same tag and rolled out.
			digest2 := env.publish(t, ref, variantTwo)
			pulled, _, err = r.Pull(ctx, ref, nil)
			require.NoError(t, err)
			require.Equal(t, digest2, pulled)

			require.NoError(t, r.Recreate(ctx, name, digest2, 10*time.Second, true, nil))

			rolled := env.inspect(t, name)
			require.NotEqual(t, created.ID, rolled.ID)
			require.True(t, rolled.State.Running)

			newImage, err := env.cli.ImageInspect(ctx, ref)
			require.NoError(t, err)
			require.Equal(t, newImage.ID, rolled.Image)
			require.Contains(t, rolled.Config.Env, "IMAGE_DEFAULT=two")

			// The same playbook again: Ansible must find nothing to do.
			changed, out = ansible.play(t)
			assert.Zero(t, changed, "Ansible wants to change the recreated container:\n%s", out)

			after := env.inspect(t, name)
			assert.Equal(t, rolled.ID, after.ID, "Ansible replaced the recreated container")
			assert.Equal(t, rolled.State.StartedAt, after.State.StartedAt, "Ansible restarted the recreated container")

			// A replacement of a replacement is no different.
			env.publish(t, ref, variantOne)
			pulled, _, err = r.Pull(ctx, ref, nil)
			require.NoError(t, err)
			require.NoError(t, r.Recreate(ctx, name, pulled, 10*time.Second, true, nil))

			changed, out = ansible.play(t)
			assert.Zero(t, changed, "Ansible wants to change the container after a second roll:\n%s", out)

			_, err = env.cli.ContainerInspect(ctx, name+replacedSuffix, client.ContainerInspectOptions{})
			require.Error(t, err)

			// Control: a real difference is still found. Ansible leaves the
			// replaced container's anonymous volumes behind; remove them.
			for _, m := range env.inspect(t, name).Mounts {
				if m.Type != mount.TypeVolume {
					continue
				}

				t.Cleanup(func() {
					// Fails for the named volume, which is still in use.
					_, _ = env.cli.VolumeRemove(context.Background(), m.Name, client.VolumeRemoveOptions{})
				})
			}

			vars["c_env"] = map[string]string{"CONTROL": "1"}
			changed, out = newAnsibleRun(t, binary, vars).play(t)
			assert.Equal(t, 1, changed, "Ansible overlooks a changed setting:\n%s", out)
		})
	}
}
