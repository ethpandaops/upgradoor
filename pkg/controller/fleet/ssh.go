package fleet

import (
	"slices"
	"time"
)

// SSH login key sources.
const (
	SSHSourceGitHub  = "github"
	SSHSourceComment = "comment"
	SSHSourceUnknown = "unknown"
)

// SSHLoginGroup is one SSH login group a host reported: the sessions of one
// key (or identity), login user and source address that followed each other
// within the host's grouping window.
type SSHLoginGroup struct {
	ID   int64  `json:"id"`
	Host string `json:"host"`
	// Identity is who the key belongs to ("github:<name>", or the key's
	// comment); empty for a key the host's key map does not know.
	Identity string `json:"identity,omitempty"`
	// Source is SSHSourceGitHub, SSHSourceComment or SSHSourceUnknown.
	Source      string `json:"source"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// KeyType is the key's type, or the method of a login without a key.
	KeyType string `json:"keyType,omitempty"`
	User    string `json:"user"`
	From    string `json:"from"`
	// FirstLogin is the start of the first session; LastLogout the newest
	// end so far, FirstLogin while none ended.
	FirstLogin time.Time `json:"firstLogin"`
	LastLogout time.Time `json:"lastLogout"`
	Sessions   int       `json:"sessions"`
	// Open counts the sessions logged in now.
	Open int `json:"open"`
	// Closed: the group takes no further session.
	Closed    bool  `json:"closed"`
	LongestMs int64 `json:"longestMs"`
	TotalMs   int64 `json:"totalMs"`
	// EndUnknown: a session ended without a logout in the journal (reboot,
	// agent restart); it counts until it was last seen.
	EndUnknown bool `json:"endUnknown,omitempty"`
}

// Actor is the audit actor of the group: "ssh:" and its identity, its
// fingerprint, or the login method.
func (g *SSHLoginGroup) Actor() string {
	switch {
	case g.Identity != "":
		return AuditSSHPrefix + g.Identity
	case g.Fingerprint != "":
		return AuditSSHPrefix + g.Fingerprint
	case g.KeyType != "":
		return AuditSSHPrefix + g.KeyType
	default:
		return AuditSSHPrefix + SSHSourceUnknown
	}
}

// SSHLoginCluster is the login groups of one identity (or key) and login
// user across the fleet whose times overlap: an Ansible run is one cluster.
type SSHLoginCluster struct {
	Identity    string `json:"identity,omitempty"`
	Source      string `json:"source"`
	Fingerprint string `json:"fingerprint,omitempty"`
	KeyType     string `json:"keyType,omitempty"`
	User        string `json:"user"`
	// From are the source addresses, at most MaxSSHClusterFrom.
	From []string `json:"from"`
	// FirstLogin is the earliest start, LastStart the newest start and
	// LastLogout the newest end of its groups.
	FirstLogin time.Time `json:"firstLogin"`
	LastStart  time.Time `json:"lastStart"`
	LastLogout time.Time `json:"lastLogout"`
	// Hosts counts its groups' hosts; HostNames are the first of them, at
	// most MaxSSHClusterHosts.
	Hosts      int      `json:"hosts"`
	HostNames  []string `json:"hostNames"`
	Groups     int      `json:"groups"`
	Sessions   int      `json:"sessions"`
	Open       int      `json:"open"`
	LongestMs  int64    `json:"longestMs"`
	TotalMs    int64    `json:"totalMs"`
	EndUnknown bool     `json:"endUnknown,omitempty"`
}

// Bounds of a cluster's lists.
const (
	MaxSSHClusterFrom  = 10
	MaxSSHClusterHosts = 5
)

// SSHClusterGap is how far apart two groups of one identity and user may
// start and end and still be one cluster.
const SSHClusterGap = 10 * time.Minute

// SSHClusters builds the clusters of login groups added newest start first.
type SSHClusters struct {
	now  time.Time
	list []*sshCluster
	// cur is the newest cluster of each key, by index into list.
	cur map[string]int
	// minStart is the earliest start of all clusters.
	minStart time.Time
}

type sshCluster struct {
	SSHLoginCluster

	hosts map[string]struct{}
}

// NewSSHClusters starts empty clusters; an open group counts as ending now.
func NewSSHClusters(now time.Time) *SSHClusters {
	return &SSHClusters{now: now, cur: make(map[string]int, 16)}
}

// Len is the number of clusters.
func (c *SSHClusters) Len() int { return len(c.list) }

// Quiet reports whether g, and so every group that starts before it, ends
// more than SSHClusterGap before every cluster starts: no cluster built so
// far takes a further group.
func (c *SSHClusters) Quiet(g *SSHLoginGroup) bool {
	return len(c.list) > 0 && c.end(g).Add(SSHClusterGap).Before(c.minStart)
}

// Add adds a group that starts no later than any group added before.
func (c *SSHClusters) Add(g *SSHLoginGroup) {
	key := sshClusterKey(g)

	var cl *sshCluster

	if i, ok := c.cur[key]; ok && !c.end(g).Add(SSHClusterGap).Before(c.list[i].FirstLogin) {
		cl = c.list[i]
	} else {
		cl = &sshCluster{
			SSHLoginCluster: SSHLoginCluster{
				Identity: g.Identity, Source: g.Source, Fingerprint: g.Fingerprint, KeyType: g.KeyType,
				User: g.User, From: []string{}, HostNames: []string{},
				FirstLogin: g.FirstLogin, LastStart: g.FirstLogin, LastLogout: g.LastLogout,
			},
			hosts: make(map[string]struct{}, 8),
		}
		c.cur[key] = len(c.list)
		c.list = append(c.list, cl)
	}

	if g.FirstLogin.Before(cl.FirstLogin) {
		cl.FirstLogin = g.FirstLogin
	}

	if g.FirstLogin.After(cl.LastStart) {
		cl.LastStart = g.FirstLogin
	}

	if g.LastLogout.After(cl.LastLogout) {
		cl.LastLogout = g.LastLogout
	}

	if _, ok := cl.hosts[g.Host]; !ok {
		cl.hosts[g.Host] = struct{}{}
		cl.Hosts++

		if len(cl.HostNames) < MaxSSHClusterHosts {
			cl.HostNames = append(cl.HostNames, g.Host)
		}
	}

	if len(cl.From) < MaxSSHClusterFrom && !slices.Contains(cl.From, g.From) {
		cl.From = append(cl.From, g.From)
	}

	cl.Groups++
	cl.Sessions += g.Sessions
	cl.Open += g.Open
	cl.TotalMs += g.TotalMs
	cl.LongestMs = max(cl.LongestMs, g.LongestMs)
	cl.EndUnknown = cl.EndUnknown || g.EndUnknown

	if c.minStart.IsZero() || g.FirstLogin.Before(c.minStart) {
		c.minStart = g.FirstLogin
	}
}

// Clusters returns the clusters, newest start first.
func (c *SSHClusters) Clusters() []SSHLoginCluster {
	out := make([]SSHLoginCluster, 0, len(c.list))
	for _, cl := range c.list {
		out = append(out, cl.SSHLoginCluster)
	}

	return out
}

// end is when a group ends for the clusters: now while a session is open.
func (c *SSHClusters) end(g *SSHLoginGroup) time.Time {
	if g.Open > 0 && c.now.After(g.LastLogout) {
		return c.now
	}

	return g.LastLogout
}

// sshClusterKey is what the groups of one cluster share: the identity, or
// the key or the method without one, and the login user.
func sshClusterKey(g *SSHLoginGroup) string {
	who := "i:" + g.Identity

	switch {
	case g.Identity != "":
	case g.Fingerprint != "":
		who = "f:" + g.Fingerprint
	default:
		who = "m:" + g.KeyType
	}

	return who + "\x00" + g.User
}
