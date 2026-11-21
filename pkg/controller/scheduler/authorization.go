package scheduler

import (
	"context"
	"slices"
	"strings"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
)

// Audit detail keys of an action's authorization.
const (
	keyAuthorizedBy = "authorizedBy"
	keyTeams        = "teams"
)

// Authorization says why the API let a person make a request. The scheduler
// writes it into the audit entries of the action, and asks it one question:
// whether a hold or unhold came through a team feature (setHold).
type Authorization struct {
	// By names what allowed the request: a feature of the token ("roll"), its
	// team twin ("team_roll"), several joined by a comma ("reboot,admin"), or
	// "open" when the API runs without an auth provider.
	By string
	// Teams are the teams of the token.
	Teams []string
}

type authorizationKey struct{}

// WithAuthorization returns a context that carries the authorization of the
// request an action is called for. Every audit entry the action writes
// records it as "authorizedBy" and "teams" in its detail.
func WithAuthorization(ctx context.Context, a Authorization) context.Context {
	return context.WithValue(ctx, authorizationKey{}, a)
}

// AuthorizationFrom returns the authorization a context carries.
func AuthorizationFrom(ctx context.Context) (Authorization, bool) {
	a, ok := ctx.Value(authorizationKey{}).(Authorization)

	return a, ok
}

// stampAuthorization records the authorization carried by ctx in the audit
// entries about to be written. Entries written outside a request (the
// reconcile, an agent's sync) carry none and stay as they are.
func stampAuthorization(ctx context.Context, entries []fleet.AuditEntry) {
	a, ok := AuthorizationFrom(ctx)
	if !ok {
		return
	}

	for i := range entries {
		e := &entries[i]

		detail := make(map[string]any, len(e.Detail)+2)
		for k, v := range e.Detail {
			detail[k] = v
		}

		detail[keyAuthorizedBy] = a.By
		detail[keyTeams] = slices.Clone(a.Teams)

		if a.Teams == nil {
			detail[keyTeams] = []string{}
		}

		e.Detail = detail
	}
}

// teamFeaturePrefix starts the name of a team feature in Authorization.By.
const teamFeaturePrefix = "team_"

// throughTeamFeature reports whether the request of ctx was allowed through a
// team feature and not through the plain one: the first name of
// Authorization.By is what allowed the action itself.
func throughTeamFeature(ctx context.Context) bool {
	a, ok := AuthorizationFrom(ctx)

	return ok && strings.HasPrefix(a.By, teamFeaturePrefix)
}

// teamFeatureOf returns the team feature a request was allowed through
// ("team_hold"), and whether it was allowed through one.
func teamFeatureOf(ctx context.Context) (string, bool) {
	if !throughTeamFeature(ctx) {
		return "", false
	}

	a, _ := AuthorizationFrom(ctx)
	twin, _, _ := strings.Cut(a.By, ",")

	return twin, true
}

// holdByStaff is how an audit entry names a hold that is not a team's.
const holdByStaff = "staff"
