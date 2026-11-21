package system

import (
	"fmt"
	"path/filepath"
)

// Commands are what an agent that is not root runs to change the system:
// argument vectors with an absolute program, such as
// [/usr/bin/sudo, -n, /usr/local/lib/upgradoor/upgrade-packages].
type Commands struct {
	// UpgradePackages upgrades the packages it gets as further arguments:
	// installed packages of the blocked set. It is neither interrupted nor
	// timed out. Empty: apt-get.
	UpgradePackages []string `yaml:"upgradePackages"`
	// Reboot reboots the host. Empty: systemctl reboot.
	Reboot []string `yaml:"reboot"`
}

// Validate checks that a set command names an absolute program.
func (c *Commands) Validate() error {
	for name, argv := range map[string][]string{"upgradePackages": c.UpgradePackages, "reboot": c.Reboot} {
		if len(argv) == 0 {
			continue
		}

		if !filepath.IsAbs(argv[0]) {
			return fmt.Errorf("commands: %s: %q is not an absolute path", name, argv[0])
		}
	}

	return nil
}
