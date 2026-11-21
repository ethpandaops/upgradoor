package plan

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Bounds of the flags of one override.
const (
	maxFlags   = 64
	maxFlagLen = 2048
)

// applyFlags returns a copy of command with remove, then add applied; it is
// idempotent. Remove: "--name=value" drops equal arguments; "--name" also
// drops "--name=*" and "--name <value>"; "--name value" drops that adjacent
// pair. Add appends an entry ("--name value" as two arguments) unless it is
// already there. Nothing else is interpreted.
func applyFlags(command, add, remove []string) []string {
	out := slices.Clone(command)

	for _, entry := range remove {
		out = removeFlag(out, entry)
	}

	for _, entry := range add {
		if args := flagArgs(entry); !containsRun(out, args) {
			out = append(out, args...)
		}
	}

	return out
}

func flagArgs(entry string) []string {
	if name, value, ok := strings.Cut(entry, " "); ok && strings.HasPrefix(name, "-") {
		return []string{name, value}
	}

	return []string{entry}
}

// containsRun reports whether args occur in command next to each other.
func containsRun(command, args []string) bool {
	for i := 0; i+len(args) <= len(command); i++ {
		if slices.Equal(command[i:i+len(args)], args) {
			return true
		}
	}

	return false
}

func removeFlag(command []string, entry string) []string {
	args := flagArgs(entry)
	byName := len(args) == 1 && strings.HasPrefix(entry, "-") && !strings.Contains(entry, "=")
	out := make([]string, 0, len(command))

	for i := 0; i < len(command); i++ {
		arg := command[i]

		switch {
		case len(args) == 2:
			if arg == args[0] && i+1 < len(command) && command[i+1] == args[1] {
				i++

				continue
			}
		case arg == entry:
			// "--name value": the value goes with its name.
			if byName && i+1 < len(command) && !strings.HasPrefix(command[i+1], "-") {
				i++
			}

			continue
		case byName && strings.HasPrefix(arg, entry+"="):
			continue
		}

		out = append(out, arg)
	}

	return out
}

func validateFlags(add, remove []string) error {
	if len(add)+len(remove) > maxFlags {
		return fmt.Errorf("more than %d flags", maxFlags)
	}

	for _, entry := range slices.Concat(add, remove) {
		switch {
		case entry == "" || strings.TrimSpace(entry) != entry:
			return fmt.Errorf("flag %q is empty or has blanks around it", entry)
		case len(entry) > maxFlagLen:
			return errors.New("a flag is too long")
		case strings.ContainsAny(entry, "\x00\r\n"):
			return fmt.Errorf("flag %q has a control character", entry)
		case len(flagArgs(entry)) == 2 && strings.Contains(flagArgs(entry)[1], " "):
			return fmt.Errorf("flag %q: write the name and one value (\"--name value\") "+
				"or one argument (\"--name=value\")", entry)
		}
	}

	return nil
}
