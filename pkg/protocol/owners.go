package protocol

import (
	"regexp"
	"slices"
)

// MaxOwners bounds a list of team names.
const MaxOwners = 8

// ownerPattern is the normal form of a team name, the same as the
// authenticatoor's: lowercase, 1 to 63 characters, starting with a letter or
// a digit.
var ownerPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidOwner reports whether name is a team name in normal form, byte for
// byte. Nothing is trimmed or folded.
func ValidOwner(name string) bool { return ownerPattern.MatchString(name) }

// CleanOwners returns the valid names among owners, sorted and without
// duplicates, cut to MaxOwners; nil when none is left. The controller applies
// it to what an agent reports, so a name that could not be a team in a token
// never becomes an owner.
func CleanOwners(owners []string) []string {
	out := make([]string, 0, len(owners))

	for _, name := range owners {
		if ValidOwner(name) {
			out = append(out, name)
		}
	}

	slices.Sort(out)
	out = slices.Compact(out)

	if len(out) > MaxOwners {
		out = out[:MaxOwners]
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// ValidClient reports whether name is a client name in normal form: the same
// form as a team name, so "geth" and "lighthouse" are and "Geth" is not.
func ValidClient(name string) bool { return ownerPattern.MatchString(name) }

// ValidKind reports whether kind is a container kind in normal form (the
// form of a team name).
func ValidKind(kind ContainerKind) bool { return ownerPattern.MatchString(string(kind)) }
