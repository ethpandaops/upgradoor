package needs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/ethpandaops/upgradoor/pkg/agent/config"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// Validators reads the host's validator range for one sync. The marker file is
// read every time: it changes without the agent restarting.
//
//   - A marker file is configured: its content is the range. A marker that
//     does not exist is reported as missing and one that cannot be read or
//     parsed as invalid, never as "no validators": the controller then keeps
//     counting what it last knew of the host.
//   - Otherwise a static range is configured: that range.
//   - Otherwise nil: the host has no validators.
func Validators(c *config.Validators) *protocol.Validators {
	if c.MarkerFile == "" {
		if c.Start == c.End {
			return nil
		}

		return &protocol.Validators{
			Start: c.Start, End: c.End,
			Source: protocol.ValidatorsFromConfig, State: protocol.ValidatorsKnown,
		}
	}

	out := &protocol.Validators{Source: protocol.ValidatorsFromMarker}

	content, err := readKeyRangeFile(c.MarkerFile)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		out.State = protocol.ValidatorsMissing

		return out
	case err != nil:
		out.State = protocol.ValidatorsInvalid
		out.Error = err.Error()

		return out
	}

	start, end, err := protocol.ParseKeyRange(content)
	if err != nil {
		out.State = protocol.ValidatorsInvalid
		out.Error = err.Error()

		return out
	}

	out.Start, out.End, out.State = start, end, protocol.ValidatorsKnown

	return out
}

// readKeyRangeFile reads a marker file, which is one short line. Anything
// longer is not a marker.
func readKeyRangeFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("key range marker: %w", err)
	}

	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, protocol.MaxKeyRangeFile+1))
	if err != nil {
		return "", fmt.Errorf("key range marker: %w", err)
	}

	if len(raw) > protocol.MaxKeyRangeFile {
		return "", errors.New("the key range marker is longer than a key range")
	}

	return string(raw), nil
}
