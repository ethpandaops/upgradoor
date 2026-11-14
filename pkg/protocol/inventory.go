package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// Inventory is the body of POST /api/agent/v1/inventory: the slow-changing,
// large part of a host's state. It is sent only when the controller asks.
// Everything in it is served to authenticated users only.
type Inventory struct {
	// Hash must equal Inventory.ComputeHash(); the controller recomputes it.
	Hash        string    `json:"hash"`
	CollectedAt time.Time `json:"collectedAt"`

	OS      OSInfo     `json:"os"`
	Kernels KernelInfo `json:"kernels"`
	// Packages is the full installed list, sorted by name then arch.
	Packages []Package `json:"packages"`
	// Upgradable is sorted by name then arch.
	Upgradable []Upgradable `json:"upgradable"`
}

// OSInfo is /etc/os-release plus the machine architecture.
type OSInfo struct {
	ID         string `json:"id"`
	VersionID  string `json:"versionId"`
	Codename   string `json:"codename"`
	PrettyName string `json:"prettyName"`
	Arch       string `json:"arch"`
}

// KernelInfo lists kernel releases.
type KernelInfo struct {
	Running   string   `json:"running"`
	Newest    string   `json:"newest"`
	Installed []string `json:"installed"`
}

// Package is one installed package.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
}

// Upgradable is one package with a newer candidate version.
type Upgradable struct {
	Name      string `json:"name"`
	Arch      string `json:"arch"`
	Current   string `json:"current"`
	Candidate string `json:"candidate"`
	// Origin is the candidate's archive, e.g. "Debian-Security:13/stable-security".
	Origin string `json:"origin"`
	// Security is true when the candidate comes from a security origin.
	Security bool `json:"security"`
	// Blocked is true for packages in the agent's blocked set, which
	// unattended-upgrades must not touch and upgradoor upgrades under a slot.
	Blocked bool `json:"blocked"`
}

// ComputeHash returns the inventory hash: hex SHA-256 over the JSON encoding
// of the inventory with Hash and CollectedAt zeroed.
func (inv *Inventory) ComputeHash() (string, error) {
	c := *inv
	c.Hash = ""
	c.CollectedAt = time.Time{}

	raw, err := json.Marshal(&c)
	if err != nil {
		return "", fmt.Errorf("protocol: encode inventory: %w", err)
	}

	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:]), nil
}

// InventoryResponse is the answer to an inventory upload.
type InventoryResponse struct {
	ServerTime time.Time `json:"serverTime"`
	Hash       string    `json:"hash"`
}
