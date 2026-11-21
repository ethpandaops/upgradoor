package docker

import (
	"encoding/json"

	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Override labels record what a container was before an override, so a
// restore needs no other state; without them it is as Ansible deployed it.
const (
	// labelOverrideImage holds the deployed image under an image override.
	labelOverrideImage = "io.ethpandaops.upgradoor.override.image"
	// labelOverrideCommand holds the pre-override command line as a JSON array.
	labelOverrideCommand = "io.ethpandaops.upgradoor.override.command"
)

// baseFromLabels reads the override labels; nil when there are none.
func baseFromLabels(labels map[string]string) *protocol.OverrideBase {
	image := labels[labelOverrideImage]
	raw, hasCommand := labels[labelOverrideCommand]

	if image == "" && !hasCommand {
		return nil
	}

	base := &protocol.OverrideBase{Image: image}

	if hasCommand {
		// A label that does not read as a list of strings is no record.
		if err := json.Unmarshal([]byte(raw), &base.Command); err == nil {
			base.HasCommand = true
		}
	}

	if base.Image == "" && !base.HasCommand {
		return nil
	}

	return base
}

// setBaseLabels writes base into the override labels; a nil base removes both.
func setBaseLabels(labels map[string]string, base *protocol.OverrideBase) {
	delete(labels, labelOverrideImage)
	delete(labels, labelOverrideCommand)

	if base == nil {
		return
	}

	if base.Image != "" {
		labels[labelOverrideImage] = base.Image
	}

	if base.HasCommand {
		command := base.Command
		if command == nil {
			// An empty command line is a record too: "[]", never "null".
			command = []string{}
		}

		// Encoding a list of strings cannot fail.
		raw, _ := json.Marshal(command)
		labels[labelOverrideCommand] = string(raw)
	}
}
