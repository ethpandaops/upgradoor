// Package authz decides what a user of the controller may do: the features
// of the login and the team rule over the hosts and containers a request
// touches.
package authz

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ethpandaops/service-authenticatoor/pkg/auth"
	"github.com/sirupsen/logrus"

	"github.com/ethpandaops/upgradoor/pkg/controller/fleet"
	"github.com/ethpandaops/upgradoor/pkg/controller/httpjson"
	"github.com/ethpandaops/upgradoor/pkg/controller/scheduler"
	"github.com/ethpandaops/upgradoor/pkg/controller/store"
	"github.com/ethpandaops/upgradoor/pkg/protocol"
)

// The features of this service, as a grant of the authenticatoor spells them
// ("upgradoor:manage", "upgradoor:team_*", "upgradoor:all"). Each is checked
// default-deny: a token has it only when its "services" claim grants it by
// name, by prefix or by "all". Every token the verifier accepts reads the
// reduced view without any of them.
//
// A feature with a team twin ("team_" + name) allows a request on every
// target; the twin only on targets that one of the token's teams owns, and
// only ever acts on the teams' own containers.
//
// What a team T owns, one rule per kind of target:
//   - a container: its team label is T;
//   - a client: every container of it has team T;
//   - a rollout: its Team is T (a group rollout of T, a manual roll of T's
//     containers only);
//   - a rollout member or a lease: everything it does rolls containers of T;
//     a reboot, packages, a host action, another team's or teamless work
//     along with it makes it staff's;
//   - a host: T owns a managed container on it; only for T's own hold and
//     team_details.
//
// Implications (Implied): admin includes details, manage, manage_image and
// manage_flags; a plain feature includes its twin. Nothing else.
//
// A prefix grant covers every feature whose name starts with it: "manage*"
// is manage, manage_image and manage_flags (TestFeatureNames).
const (
	// FeatureDetails is what a login reads of the hosts beyond the reduced
	// view (views.Access): OS, kernel, packages, unmanaged containers,
	// container internals, addresses, host actions. Twin: team_details, the
	// hosts on which a team of the token owns a managed container. GET
	// /api/v1/packages needs the plain feature.
	FeatureDetails = "details"
	// FeatureManage covers roll, reboot, hold, unhold, mark-offline,
	// unmark-offline, skip, retry, end-lease, client-policy, pause, resume,
	// ignore-gate, ignore-budget, approve-lease and abort of one rollout, POST
	// /api/v1/images/refresh, the flags force and ignoreBudget, the selector
	// all and a roll that is not staggered. It is the feature of a host action
	// that names none. A host has one hold per holder: the plain feature sets
	// and lifts staff's hold and lifts any hold it names.
	//
	// Twin: team_manage, under the rule of each target: a roll of the teams'
	// containers, a hold or unhold of the holds of the token's teams that own
	// a managed container of the host, skip, retry and end-lease of members
	// and leases that only roll the teams' containers, the decisions on a
	// rollout of a team, the policy of a team's client. Never reboot,
	// mark-offline, unmark-offline or the image refresh.
	FeatureManage = "manage"
	// FeatureManageImage covers image-override and the image part of
	// client-override. Twin: team_manage_image, for a container a team of the
	// token owns.
	FeatureManageImage = "manage_image"
	// FeatureManageFlags covers flag-override and the flags part of
	// client-override. Twin: team_manage_flags.
	FeatureManageFlags = "manage_flags"
	// FeatureAdmin covers forget, pause and resume without a rollout (the
	// global switch), GET and PUT /api/v1/settings, the data sources and
	// their refresh, and the audit log in full (views.Access). It includes
	// every other feature. No twin.
	FeatureAdmin = "admin"

	// teamFeaturePrefix turns a feature into its team twin.
	TeamFeaturePrefix = "team_"

	// authorizedByOpen is what the audit log records as the authorization of
	// an action when the API runs open.
	AuthorizedByOpen = "open"

	// maxTokenTeams bounds the teams of a token. A token with more is not
	// accepted: its teams would be decided with and written into every audit
	// entry of its actions.
	MaxTokenTeams = 64

	// maxEchoedName bounds a name that a refusal repeats.
	maxEchoedName = 128

	// refusalWindow is how often a refusal of one actor on one path is
	// audited at most; maxRefusalKeys bounds the remembered pairs.
	refusalWindow  = time.Minute
	maxRefusalKeys = 4096
	// auditTimeout bounds the audit write of a refusal.
	auditTimeout = 5 * time.Second
)

// authServicePattern is what a service name may look like: a name that can
// stand in a directive of the "services" claim.
var AuthServicePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// requirement is what one request needs to be allowed. Each action declares
// it once, where the action is registered (actions).
type Requirement struct {
	// feature is the feature that allows the request on any target.
	Feature string
	// team is what the request acts on, for the team twin of feature: a token
	// with "team_"+feature is allowed when a team of the token owns every
	// one of these targets. nil: the twin does not allow the request.
	Team *Targets
}

// targets names what a request acts on.
type Targets struct {
	// hosts are the hosts named by the request; all selects every host the
	// controller knows instead.
	hosts []string
	all   bool
	// lease names a lease, for the rule of a recovery (recover.go).
	lease string
	// recover selects the rule of a recovery: lease, or the hosts as
	// members of rollout.
	recover bool
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
	// rollout names a rollout instead: it is owned by its team. A rollout
	// for no team is owned by nobody.
	rollout string
	// holders are the holds an unhold names: each must be the hold of a team
	// of the token that owns a managed container of the host.
	holders []string
}

// hostTargets are the targets of a host-level action.
func HostTargets(hosts []string, all bool) *Targets {
	return &Targets{hosts: hosts, all: all}
}

// HoldTargets are the targets of hold and unhold: the hosts, and the holds
// an unhold names.
func HoldTargets(hosts, holders []string) *Targets {
	return &Targets{hosts: hosts, holders: holders}
}

// containerTargets are the targets of a roll.
func ContainerTargets(hosts []string, all bool, containers []string) *Targets {
	return &Targets{hosts: hosts, all: all, containers: containers, perContainer: true}
}

// RolloutTarget is the target of an action on one rollout.
func RolloutTarget(rollout string) *Targets {
	return &Targets{rollout: rollout}
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
	// Refuse answers 403 with msg, logs it and audits it.
	Refuse(w http.ResponseWriter, r *http.Request, u *User, msg string)
	// ActionBasis says on what u may start or approve a host action as the
	// host reports it: ActionByOpen, FeatureAdmin, ActionByFeature or
	// ActionByTeam; empty when u may not.
	ActionBasis(u *User, a *protocol.ActionInfo) string
}

// What allows a person to start a host action (ActionBasis), next to
// FeatureAdmin.
const (
	// ActionByOpen: the API runs without an auth provider.
	ActionByOpen = AuthorizedByOpen
	// ActionByFeature: the login has the action's feature.
	ActionByFeature = "feature"
	// ActionByTeam: the login is in one of the action's teams.
	ActionByTeam = "team"
)

var _ Authorizer = (*authorizer)(nil)

type authorizer struct {
	log         logrus.FieldLogger
	authService string
	store       Store
	now         func() time.Time

	// refusedMu guards refused: when a refusal was last audited, by actor
	// and path.
	refusedMu sync.Mutex
	refused   map[string]time.Time
}

// Store is what the authorizer reads to apply the team rule, and where it
// audits refusals.
type Store interface {
	GetRollout(ctx context.Context, id string) (*fleet.Rollout, error)
	GetLease(ctx context.Context, id string) (*fleet.Lease, error)
	ListHosts(ctx context.Context) ([]*fleet.Host, error)
	ListDirectives(ctx context.Context, filter store.DirectiveFilter) ([]*fleet.Directive, error)
	AppendAudit(ctx context.Context, entries ...fleet.AuditEntry) error
}

// New returns the authorizer of the login features named after authService.
// now is the clock; time.Now when nil.
func New(log logrus.FieldLogger, authService string, st Store, now func() time.Time) Authorizer {
	if now == nil {
		now = time.Now
	}

	return &authorizer{
		log: log, authService: authService, store: st, now: now,
		refused: make(map[string]time.Time, 16),
	}
}

// HasFeature implements Authorizer: the feature is granted, or implied by
// one that is (Implied).
func (s *authorizer) HasFeature(u *User, feature string) bool {
	return Implied(feature, func(f string) bool {
		return auth.FeatureAllowed(u.Services, s.authService, f, auth.FeatureDefaultDeny)
	})
}

// Implied reports whether granted gives feature: by name, through admin
// (which includes every other feature) or through its plain form (a plain
// feature includes its team twin).
func Implied(feature string, granted func(string) bool) bool {
	if granted(feature) || granted(FeatureAdmin) {
		return true
	}

	plain, twin := strings.CutPrefix(feature, TeamFeaturePrefix)

	return twin && granted(plain)
}

// ActionBasis implements Authorizer. The action's feature is one of this
// service, protocol.DefaultActionFeature when it names none; an unknown one
// allows nobody.
func (s *authorizer) ActionBasis(u *User, a *protocol.ActionInfo) string {
	switch {
	case u.Open:
		return ActionByOpen
	case s.HasFeature(u, FeatureAdmin):
		return FeatureAdmin
	}

	if feature := cmp.Or(a.Feature, protocol.DefaultActionFeature); protocol.ValidActionFeature(feature) &&
		s.HasFeature(u, feature) {
		return ActionByFeature
	}

	if ownedBy(u.Teams, a.Teams) {
		return ActionByTeam
	}

	return ""
}

// authorize decides whether u may make a request with the given requirement.
// It returns the feature that allowed it, as the audit log records it: the
// plain feature ("manage") or its team twin ("team_manage"), or "open" when
// the API runs without an auth provider.
//
// The rule: the request is allowed when the token has the plain feature, or
// it has the team twin and a team of the token owns every target. A refusal
// is a *forbiddenError. authorize reads the store and changes nothing.
func (s *authorizer) authorize(ctx context.Context, u *User, need *Requirement) (string, error) {
	if u.Open {
		return AuthorizedByOpen, nil
	}

	feature, team := need.Feature, need.Team
	if s.HasFeature(u, feature) {
		return feature, nil
	}

	twin := TeamFeaturePrefix + feature

	if team == nil || !s.HasFeature(u, twin) {
		return "", forbiddenf("this login lacks the feature %s:%s", s.authService, feature)
	}

	uncovered, err := s.uncovered(ctx, u.Teams, team)
	if err != nil {
		return "", err
	}

	if uncovered != "" {
		return "", forbiddenf("%s:%s %s", s.authService, twin, uncovered)
	}

	return twin, nil
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
	if t.recover {
		return s.uncoveredRecover(ctx, teams, t)
	}

	names := slices.Clone(t.hosts)
	client := t.Client

	if t.rollout != "" {
		r, err := s.store.GetRollout(ctx, t.rollout)

		switch {
		case errors.Is(err, store.ErrNotFound):
			return fmt.Sprintf("does not cover rollout %s (owners: none)", Echo(t.rollout)), nil
		case err != nil:
			return "", fmt.Errorf("authorize: get rollout %s: %w", Echo(t.rollout), err)
		case r.Team == "":
			return fmt.Sprintf("does not cover rollout %s: it is for no team", Echo(t.rollout)), nil
		case !ownedBy(teams, []string{r.Team}):
			return fmt.Sprintf("does not cover rollout %s (owners: %s)", Echo(t.rollout), Echo(r.Team)), nil
		}

		return "", nil
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

	if client != "" {
		return UncoveredClient(teams, client, hosts), nil
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
		own := &hostOwnership{}

		if host.Status != nil {
			own.containers = make([]containerOwners, 0, len(host.Status.Containers))

			for i := range host.Status.Containers {
				if c := &host.Status.Containers[i]; c.Managed {
					own.containers = append(own.containers, containerOwners{
						name: c.Name, owners: teamOf(c), notRolled: c.NotRolled != "",
					})
				}
			}
		}

		known[host.Name] = own
	}

	return known
}

// teamOf is the owners of a container: its team, if it has one.
func teamOf(c *protocol.Container) []string {
	if c.Team == "" {
		return nil
	}

	return []string{c.Team}
}

// hostOwnership is what the team rule reads of one host.
type hostOwnership struct {
	// containers are the managed containers with their owners.
	containers []containerOwners
}

// containerOwners is one managed container of a host with its owners (its
// team; none for a container without one, which is staff's).
type containerOwners struct {
	name   string
	owners []string
	// notRolled: the host rolls the container to nothing (pinned or linked),
	// so a roll of the whole host does not touch it.
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

		// An unhold names only holds of the token's teams on the host.
		for _, holder := range t.holders {
			if holder == fleet.HolderStaff || !slices.Contains(teams, holder) || !slices.Contains(all, holder) {
				return fmt.Sprintf("does not lift the hold of %s on host %s", Echo(holder), Echo(host))
			}
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
			if !c.Managed || c.Team != client {
				continue
			}

			found = true

			if !ownedBy(teams, teamOf(c)) {
				return fmt.Sprintf("does not cover client %s: container %s on host %s (owners: %s)",
					Echo(client), Echo(c.Name), Echo(host.Name), ownerList(teamOf(c)))
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

// Refuse answers a request the login may not make, logs it at debug level
// and audits it as auth.refused, once per refusalWindow for each actor and
// path.
func (s *authorizer) Refuse(w http.ResponseWriter, r *http.Request, u *User, msg string) {
	s.log.WithFields(logrus.Fields{
		"path": r.URL.Path, "actor": u.Actor, "reason": msg,
	}).Debug("Refused user request")

	if s.auditDue(u.Actor + " " + r.URL.Path) {
		teams := slices.Clone(u.Teams)
		if teams == nil {
			teams = []string{}
		}

		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), auditTimeout)

		if err := s.store.AppendAudit(ctx, fleet.AuditEntry{
			Actor: u.Actor, Action: "auth.refused", Target: Echo(r.URL.Path),
			Detail: map[string]any{"method": r.Method, "reason": msg, "teams": teams},
		}); err != nil {
			s.log.WithError(err).Warn("Could not audit a refused request")
		}

		cancel()
	}

	httpjson.Error(s.log, w, http.StatusForbidden, msg)
}

// auditDue reports whether the refusal of key is audited now, and notes it.
// Pairs older than the window are forgotten once the map is full; while it
// is full of recent ones nothing more is audited.
func (s *authorizer) auditDue(key string) bool {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()

	now := s.now()

	if last, ok := s.refused[key]; ok && now.Sub(last) < refusalWindow {
		return false
	}

	if len(s.refused) >= maxRefusalKeys {
		for k, at := range s.refused {
			if now.Sub(at) >= refusalWindow {
				delete(s.refused, k)
			}
		}

		if len(s.refused) >= maxRefusalKeys {
			return false
		}
	}

	s.refused[key] = now

	return true
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
