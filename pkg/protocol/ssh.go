package protocol

import "time"

// FeatureSSHLogins is the SSH login history: an agent with it enabled names
// it in SyncRequest.Features, a controller that stores it in
// SyncResponse.Accepts. An agent sends SyncRequest.SSH only to a controller
// whose verified answer accepts it.
const FeatureSSHLogins = "ssh-logins"

// Bounds of SSHReport.
const (
	// MaxSSHClosed bounds the closed groups of one sync.
	MaxSSHClosed = 100
	// MaxSSHOpen bounds the open groups of one sync.
	MaxSSHOpen = 100
)

// SSHReport is the host's SSH login history since the last acknowledgement:
// every open group, and the oldest closed groups not acknowledged yet.
type SSHReport struct {
	// Open are the groups that may take further sessions; they replace
	// the host's open groups the controller holds.
	Open []SSHLoginGroup `json:"open,omitempty"`
	// Closed are closed groups, oldest first, at most MaxSSHClosed.
	Closed []SSHLoginGroup `json:"closed,omitempty"`
	// Dropped counts the closed groups dropped unsent since the last
	// acknowledgement: the host's queue was full.
	Dropped int64 `json:"dropped,omitempty"`
	// Error says why the journal cannot be read.
	Error string `json:"error,omitempty"`
}

// SSHLoginGroup is a run of SSH sessions of one identity (or key), login
// user and source address, each starting within the grouping window of the
// one before.
type SSHLoginGroup struct {
	// Seq identifies the group on its host; it grows with every group.
	Seq int64 `json:"seq"`
	// Identity is who the key belongs to, by the host's key map
	// ("github:<name>", or the key's comment); empty for an unknown key.
	Identity string `json:"identity,omitempty"`
	// Source is how Identity was found: "github", "comment" or "unknown".
	Source      string `json:"source,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	// KeyType is the key's type, or the method of a login without a key.
	KeyType    string     `json:"keyType,omitempty"`
	User       string     `json:"user"`
	From       string     `json:"from"`
	FirstLogin time.Time  `json:"firstLogin"`
	LastLogout *time.Time `json:"lastLogout,omitempty"`
	Sessions   int        `json:"sessions"`
	// Open counts the sessions logged in now.
	Open int `json:"open,omitempty"`
	// LongestMillis and TotalMillis are over the ended sessions.
	LongestMillis int64 `json:"longestMs,omitempty"`
	TotalMillis   int64 `json:"totalMs,omitempty"`
	// EndUnknown: a session ended without a logout in the journal; it
	// counts until it was last seen.
	EndUnknown bool `json:"endUnknown,omitempty"`
}
