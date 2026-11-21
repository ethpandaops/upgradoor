// Package authz decides what a user of the controller may do: the features
// of the login and the team rule over the hosts and containers a request
// touches.
package authz

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
)

// The features of this service, as they are written in a grant of the
// authenticatoor ("upgradoor:roll", "upgradoor:team_*", "upgradoor:all").
// Every one is checked default-deny: a token has it only when its "services"
// claim grants it by name, by prefix or by "all". Reading needs none of
// them: every token the verifier accepts has basic access, which covers all
// authenticated GET routes.
//
// A feature with a team twin ("team_" + name) can be held in two ways. The
// plain feature allows the action on every target. The twin allows it only
// on targets that one of the token's teams owns.
//
// A prefix grant covers every feature whose name starts with it, so no
// feature name starts with another one (TestFeatureNames): a grant of
// "upgradoor:roll*" is the feature roll and nothing else.
const (
	// FeatureRoll covers the action roll. Twin: team_roll, for containers a
	// team of the token owns.
	FeatureRoll = "roll"
	// FeatureReboot covers the action reboot. Twin: team_reboot, for hosts
	// on which a team of the token owns a container.
	FeatureReboot = "reboot"
	// FeatureHold covers the actions hold, unhold, mark-offline and
	// unmark-offline. Twin: team_hold, for
	// hosts on which a team of the token owns a container, and only for
	// holds that were set through the twin.
	FeatureHold = "hold"
	// FeatureRecover covers what gets a stuck host moving again: the actions
	// skip, retry with hosts (of a rollout that is not aborted) and
	// end-lease. Twin: team_recover, for hosts on which a team of the token
	// owns a container.
	FeatureRecover = "recover"
	// FeatureSupervise covers the actions on one rollout as a whole: pause,
	// resume and abort with a rollout, retry without hosts, and every retry
	// of an aborted rollout, which reopens its cause for all hosts. No twin.
	FeatureSupervise = "supervise"
	// FeatureImageOverride covers the action image-override: setting or
	// taking away the image override of one container on one host. Twin:
	// team_image_override, for a container a team of the token owns.
	FeatureImageOverride = "image_override"
	// FeatureFlagOverride covers the action flag-override: the same for the
	// flags of a container. Twin: team_flag_override.
	FeatureFlagOverride = "flag_override"
	// FeatureClientPolicy covers the action client-policy: the image policy
	// (auto or manual) of one client. Twin: team_client_policy, for a client
	// whose reported containers are all owned by a team of the token.
	FeatureClientPolicy = "client_policy"
	// FeatureAdmin covers the action forget, pause and resume without a
	// rollout (the global switch) and PUT /api/v1/settings, and it is needed
	// on top of an action's own feature for the request flags force and
	// ignoreBudget and for the selector all. It does not imply any other
	// feature. No twin.
	FeatureAdmin = "admin"

	// teamFeaturePrefix turns a feature into its team twin.
	TeamFeaturePrefix = "team_"

	// authorizedByOpen is what the audit log records as the authorization of
	// an action when the API runs open.
	AuthorizedByOpen = "open"

	// holdByStaff is HostSummary.HoldBy for a hold that was not set through
	// a team feature.
	HoldByStaff = "staff"

	// maxTokenTeams bounds the teams of a token. A token with more is not
	// accepted: its teams would be decided with and written into every audit
	// entry of its actions.
	MaxTokenTeams = 64

	// maxEchoedName bounds a name that a refusal repeats.
	maxEchoedName = 128
)

// authServicePattern is what a service name may look like: a name that can
// stand in a directive of the "services" claim.
var AuthServicePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// holdChange says what a request does to the controller hold of its hosts.
type holdChange int

const (
	HoldUntouched holdChange = iota
	// holdSet: the request puts its hosts on hold.
	HoldSet
	// holdLift: the request lifts the hold of its hosts.
	HoldLift
)

// requirement is what one request needs to be allowed. Each action declares
// it once, where the action is registered (actions).
type Requirement struct {
	// feature is the feature that allows the request on any target.
	Feature string
	// team is what the request acts on, for the team twin of feature: a token
	// with "team_"+feature is allowed when a team of the token owns every
	// one of these targets. nil means the feature has no twin.
	Team *Targets
	// admin, when not empty, says why the request needs FeatureAdmin on top
	// ("force", "ignoreBudget", "all").
	Admin string
	// reopens names the rollout of a retry with hosts. When that rollout is
	// aborted the request is not about its hosts: it reopens the cause for
	// every host, and needs FeatureSupervise instead, which has no twin.
	Reopens string
	// teamAdmin, when not empty, names what the request asks for that the
	// team twin alone does not give ("now" for a roll or reboot that is not
	// staggered). A login with FeatureAdmin next to the twin gets it, and the
	// audit log records admin for it; for a login without, teamOnly is called
	// and takes it out of the request.
	TeamAdmin string
	TeamOnly  func()
}

// targets names what a request acts on.
type Targets struct {
	// hosts are the hosts named by the request; all selects every host the
	// controller knows instead.
	hosts []string
	all   bool
	// lease names an open lease; the target is the lease's host.
	lease string
	// perContainer selects the rule. False is the any-owner rule of
	// host-level actions: a host is covered when a team of the token owns at
	// least one of its managed containers. True is the rule of a roll: every
	// container the roll touches must be owned by a team of the token; these
	// are the named containers, or every managed container of the host when
	// none is named.
	perContainer bool
	containers   []string
	// client names a client instead of hosts: the target is every managed
	// container that any host reports for that client, and the rule is that
	// of a roll (every one of them owned by a team of the token). A client
	// no host reports is owned by nobody.
	Client string
	// hold says that the request sets or lifts the hold of its hosts. The
	// twin does neither to a hold that was not set through the twin.
	hold holdChange
}

// hostTargets are the targets of a host-level action.
func HostTargets(hosts []string, all bool) *Targets {
	return &Targets{hosts: hosts, all: all}
}

// holdTargets are the targets of hold and unhold.
func HoldTargets(hosts []string, change holdChange) *Targets {
	return &Targets{hosts: hosts, hold: change}
}

// containerTargets are the targets of a roll.
func ContainerTargets(hosts []string, all bool, containers []string) *Targets {
	return &Targets{hosts: hosts, all: all, containers: containers, perContainer: true}
}

// leaseTarget is the target of an action on one lease.
func LeaseTarget(lease string) *Targets {
	return &Targets{lease: lease}
}

// adminReason names the first of the request flags that needs FeatureAdmin;
// empty when none is set.
func AdminReason(force, ignoreBudget, all bool) string {
	switch {
	case force:
		return "force"
	case ignoreBudget:
		return "ignoreBudget"
	case all:
		return "all"
	default:
		return ""
	}
}

// forbiddenError refuses a request that is authenticated and not allowed.
// Its text is the answer.
type forbiddenError struct {
	msg string
}

func (e *forbiddenError) Error() string { return e.msg }

func forbiddenf(format string, args ...any) error {
	return &forbiddenError{msg: "forbidden: " + fmt.Sprintf(format, args...)}
}

// echo makes a name safe to repeat in a refusal: names of hosts, containers,
// leases and rollouts come from the request or from an agent. Anything that
// is not printable ASCII becomes "?", and the name is cut to maxEchoedName
// characters.
func Echo(name string) string {
	out := make([]byte, 0, min(len(name), maxEchoedName))

	for i := 0; i < len(name) && len(out) < maxEchoedName; i++ {
		c := name[i]
		if c < 0x20 || c > 0x7e {
			c = '?'
		}

		out = append(out, c)
	}

	return string(out)
}

// HasFeature reports whether the user's token grants a feature of this
// service. Default-deny: a token without the "services" claim has none.
// user is who makes an authenticated request, and what the token says they
// may do.
type User struct {
	// actor is the audit name: the token's email, falling back to its
	// subject, or "anonymous" in open mode.
	Actor string
	// open is true when no auth provider is configured: everything is
	// allowed.
	Open bool
	// teams are the token's teams.
	Teams []string
	// services is the token's "services" claim, the directive list the
	// feature checks read.
	Services string
	// expires is when the token stops being valid; zero in open mode. A
	// request is over long before that, an event stream is not: it ends
	// then, and is not opened for a token that does not say.
	Expires time.Time
}

// Authorizer decides what a user may do: the features of the login and the
// team rule over the hosts and containers a request touches.
type Authorizer interface {
	// HasFeature reports whether the login of u carries feature.
	HasFeature(u *User, feature string) bool
	// Allow authorizes need for u. It answers a refusal itself and returns
	// false; on success the context carries the authorization.
	Allow(w http.ResponseWriter, r *http.Request, u *User, need *Requirement) (context.Context, bool)
	// Refuse answers 403 with msg and logs it.
	Refuse(w http.ResponseWriter, r *http.Request, u *User, msg string)
}

var _ Authorizer = (*authorizer)(nil)

type authorizer struct {
	log         logrus.FieldLogger
	authService string
	store       Store
}

// Store is what the authorizer reads to apply the team rule.
type Store interface {
	GetRollout(ctx context.Context, id string) (*fleet.Rollout, error)
	GetLease(ctx context.Context, id string) (*fleet.Lease, error)
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
}

// New returns the authorizer of the login features named after authService.
func New(log logrus.FieldLogger, authService string, st Store) Authorizer {
	return &authorizer{log: log, authService: authService, store: st}
}

func (s *authorizer) HasFeature(u *User, feature string) bool {
	return auth.FeatureAllowed(u.Services, s.authService, feature, auth.FeatureDefaultDeny)
}

// authorize decides whether u may make a request with the given requirement.
// It returns the features that allowed it, as the audit log records them:
// the plain feature ("roll") or its team twin ("team_roll"), followed by
// ",admin" when the request needed that as well, or "open" when the API runs
// without an auth provider.
//
// The rule: the request is allowed when the token has the plain feature, or
// it has the team twin and a team of the token owns every target. When the
// request needs FeatureAdmin as well, the token must have that too. A refusal
// is a *forbiddenError. authorize reads the store and changes nothing.
func (s *authorizer) authorize(ctx context.Context, u *User, need *Requirement) (string, error) {
	if u.Open {
		return AuthorizedByOpen, nil
	}

	feature, team := need.Feature, need.Team
	unknownRollout := false

	if need.Reopens != "" {
		rollout, err := s.store.GetRollout(ctx, need.Reopens)

		switch {
		case errors.Is(err, store.ErrNotFound):
			// With the plain feature the scheduler answers.
			unknownRollout = true
		case err != nil:
			return "", fmt.Errorf("authorize: get rollout %s: %w", Echo(need.Reopens), err)
		case rollout.State == fleet.RolloutAborted:
			feature, team = FeatureSupervise, nil
		}
	}

	by := feature
	viaTeam := false

	if !s.HasFeature(u, feature) {
		twin := TeamFeaturePrefix + feature

		if team == nil || !s.HasFeature(u, twin) {
			return "", forbiddenf("this login lacks the feature %s:%s", s.authService, feature)
		}

		if unknownRollout {
			return "", forbiddenf("%s:%s does not cover rollout %s (owners: none)",
				s.authService, twin, Echo(need.Reopens))
		}

		uncovered, err := s.uncovered(ctx, u.Teams, team)
		if err != nil {
			return "", err
		}

		if uncovered != "" {
			return "", forbiddenf("%s:%s %s", s.authService, twin, uncovered)
		}

		by, viaTeam = twin, true
	}

	admin := s.HasFeature(u, FeatureAdmin)

	if need.Admin != "" {
		if !admin {
			return "", forbiddenf("this login lacks the feature %s:%s (needed for %s)",
				s.authService, FeatureAdmin, need.Admin)
		}

		by += "," + FeatureAdmin
	}

	// What the twin alone does not give: admin permits it, and is named for
	// it; without admin it is taken out of the request.
	if viaTeam && need.TeamAdmin != "" {
		switch {
		case !admin:
			need.TeamOnly()
		case need.Admin == "":
			by += "," + FeatureAdmin
		}
	}

	return by, nil
}

// uncovered says what keeps the teams from covering the targets, naming the
// first target none of them owns: "does not cover container geth on host
// lighthouse-geth-1 (owners: geth)". It returns the empty string when every
// target is covered. A request that names no target is covered by nobody.
//
// Ownership is what each host's agent last reported for its managed
// containers. A host the controller does not know, a host that never synced
// and a container the host does not manage have no owners.
func (s *authorizer) uncovered(ctx context.Context, teams []string, t *Targets) (string, error) {
	names := slices.Clone(t.hosts)

	if t.lease != "" {
		lease, err := s.store.GetLease(ctx, t.lease)

		switch {
		case errors.Is(err, store.ErrNotFound):
			return fmt.Sprintf("does not cover lease %s (owners: none)", Echo(t.lease)), nil
		case err != nil:
			return "", fmt.Errorf("authorize: get lease %s: %w", Echo(t.lease), err)
		}

		names = []string{lease.Host}
	}

	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return "", fmt.Errorf("authorize: list hosts: %w", err)
	}

	known := OwnershipOf(hosts)

	if t.all {
		for _, host := range hosts {
			names = append(names, host.Name)
		}
	}

	if t.Client != "" {
		return UncoveredClient(teams, t.Client, hosts), nil
	}

	if len(names) == 0 {
		return "needs a target that one of your teams owns, and the request names none", nil
	}

	for _, name := range names {
		own := known[name]
		if own == nil {
			own = &hostOwnership{}
		}

		if reason := UncoveredOn(teams, name, own, t); reason != "" {
			return reason, nil
		}
	}

	return "", nil
}

// ownershipOf is what the team rule reads of each host, by host name.
func OwnershipOf(hosts []*fleet.Host) map[string]*hostOwnership {
	known := make(map[string]*hostOwnership, len(hosts))

	for _, host := range hosts {
		own := &hostOwnership{held: host.HoldWanted, heldByTeam: host.HoldBy == fleet.HoldByTeam}

		if host.Status != nil {
			own.containers = make([]containerOwners, 0, len(host.Status.Containers))

			for i := range host.Status.Containers {
				if c := &host.Status.Containers[i]; c.Managed {
					own.containers = append(own.containers, containerOwners{
						name: c.Name, owners: c.Owners, notRolled: c.NotRolled != "",
					})
				}
			}
		}

		known[host.Name] = own
	}

	return known
}

// hostOwnership is what the team rule reads of one host.
type hostOwnership struct {
	// containers are the managed containers with their owners.
	containers []containerOwners
	// held: the controller hold is wanted; heldByTeam: it was set through a
	// team feature.
	held       bool
	heldByTeam bool
}

// containerOwners is one managed container of a host with its owners.
type containerOwners struct {
	name   string
	owners []string
	// notRolled: the host rolls the container to nothing (pinned, linked, or
	// of the default group on a host that does not roll it), so a roll of
	// the whole host does not touch it.
	notRolled bool
}

// ownedBy reports whether one of the teams is among the owners. Names are
// compared byte for byte; no teams, or no owners, own nothing.
func ownedBy(teams, owners []string) bool {
	return slices.ContainsFunc(owners, func(o string) bool { return slices.Contains(teams, o) })
}

// uncoveredOn applies the target rule to one host. It returns the empty
// string when the host is covered.
func UncoveredOn(teams []string, host string, own *hostOwnership, t *Targets) string {
	all := make([]string, 0, len(own.containers))
	for _, c := range own.containers {
		all = append(all, c.owners...)
	}

	slices.Sort(all)
	all = slices.Compact(all)

	onHost := fmt.Sprintf("does not cover host %s (owners: %s)", Echo(host), ownerList(all))

	if !t.perContainer {
		// The any-owner rule.
		if !ownedBy(teams, all) {
			return onHost
		}

		// A hold that was not set through the twin is not the twin's to
		// lift, nor to set again as its own.
		if t.hold != HoldUntouched && own.held && !own.heldByTeam {
			verb := "lift"
			if t.hold == HoldSet {
				verb = "change"
			}

			return fmt.Sprintf("does not %s the hold on host %s (set by staff)", verb, Echo(host))
		}

		return ""
	}

	if len(t.containers) == 0 {
		// A roll of the whole host touches every managed container.
		if len(own.containers) == 0 {
			return onHost
		}

		for _, c := range own.containers {
			if !c.notRolled && !ownedBy(teams, c.owners) {
				return uncoveredContainer(c.name, host, c.owners)
			}
		}

		return ""
	}

	for _, name := range t.containers {
		idx := slices.IndexFunc(own.containers, func(c containerOwners) bool { return c.name == name })
		if idx < 0 {
			// Not a managed container of the host: nobody owns it. The answer
			// does not tell whether it exists.
			return uncoveredContainer(name, host, nil)
		}

		if !ownedBy(teams, own.containers[idx].owners) {
			return uncoveredContainer(name, host, own.containers[idx].owners)
		}
	}

	return ""
}

// uncoveredClient applies the team rule to a client: every managed container
// that a host reports for it is owned by a team of the token, and there is at
// least one.
func UncoveredClient(teams []string, client string, hosts []*fleet.Host) string {
	found := false

	for _, host := range hosts {
		if host.Status == nil {
			continue
		}

		for i := range host.Status.Containers {
			c := &host.Status.Containers[i]
			if !c.Managed || c.Client != client {
				continue
			}

			found = true

			if !ownedBy(teams, c.Owners) {
				return fmt.Sprintf("does not cover client %s: container %s on host %s (owners: %s)",
					Echo(client), Echo(c.Name), Echo(host.Name), ownerList(c.Owners))
			}
		}
	}

	if !found {
		return fmt.Sprintf("does not cover client %s (owners: none)", Echo(client))
	}

	return ""
}

func uncoveredContainer(container, host string, owners []string) string {
	return fmt.Sprintf("does not cover container %s on host %s (owners: %s)",
		Echo(container), Echo(host), ownerList(owners))
}

// ownerList writes owners for a message.
func ownerList(owners []string) string {
	if len(owners) == 0 {
		return "none"
	}

	return Echo(strings.Join(owners, ", "))
}

// Refuse answers a request the login may not make, and logs it at debug
// level.
func (s *authorizer) Refuse(w http.ResponseWriter, r *http.Request, u *User, msg string) {
	s.log.WithFields(logrus.Fields{
		"path": r.URL.Path, "actor": u.Actor, "reason": msg,
	}).Debug("Refused user request")
	httpjson.Error(s.log, w, http.StatusForbidden, msg)
}

// Allow authorizes a request and answers a refusal. On success it returns
// the request's context carrying who authorized it, for the scheduler's audit
// entry. A refused request changes nothing and is not audited; it is logged
// at debug level.
func (s *authorizer) Allow(
	w http.ResponseWriter, r *http.Request, u *User, need *Requirement,
) (context.Context, bool) {
	by, err := s.authorize(r.Context(), u, need)

	var forbidden *forbiddenError

	switch {
	case err == nil:
		return scheduler.WithAuthorization(r.Context(), scheduler.Authorization{
			By: by, Teams: u.Teams,
		}), true
	case errors.As(err, &forbidden):
		s.Refuse(w, r, u, forbidden.msg)
	default:
		httpjson.InternalError(s.log, w, r, err)
	}

	return nil, false
}
