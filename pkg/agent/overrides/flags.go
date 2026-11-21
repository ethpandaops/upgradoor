package overrides

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

// A flag override changes a container's command line: the list of arguments
// after the entrypoint. It is two lists, applied in this order:
//
//  1. remove: every entry takes arguments out.
//     - "--name=value" (anything without a blank) removes every argument
//     that equals it exactly.
//     - "--name" (starts with "-", has no "=") also removes every argument
//     of the form "--name=<anything>", and "--name" together with the
//     argument after it when that one does not start with "-" (the
//     two-argument form "--name value").
//     - "--name value" (one blank) removes exactly that pair of adjacent
//     arguments.
//  2. add: every entry is appended at the end, unless it is already there.
//     - "--name=value" or "--name" is one argument.
//     - "--name value" (starts with "-", one blank) is two arguments.
//
// Nothing else is interpreted: no quoting, no merging of values, no
// knowledge of what a client's flags mean. To change the value of a flag,
// remove it by name and add it with the new value.

// applyFlags returns command with the override's flags applied. The result
// is a new slice; applying the same flags to it again changes nothing.
func applyFlags(command, add, remove []string) []string {
	out := slices.Clone(command)

	for _, entry := range remove {
		out = removeFlag(out, entry)
	}

	for _, entry := range add {
		args := flagArgs(entry)
		if !containsRun(out, args) {
			out = append(out, args...)
		}
	}

	return out
}

// flagArgs splits an entry into its arguments: two for "--name value", one
// otherwise.
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
			// The two-argument form: the value goes with its name.
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

// validateFlags checks the flag lists of one override.
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
		case strings.Count(entry, " ") > 0 && len(flagArgs(entry)) == 2 &&
			strings.Contains(flagArgs(entry)[1], " "):
			return fmt.Errorf("flag %q: write the name and one value (\"--name value\") "+
				"or one argument (\"--name=value\")", entry)
		}
	}

	return nil
}
