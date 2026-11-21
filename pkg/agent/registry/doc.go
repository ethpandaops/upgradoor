// Package registry tracks the tags the agent follows.
//
// The controller looks the tags up; the agent never asks a registry. The
// tracker compares the digests with the local images. A lease pulls its
// images itself; the tracker pulls only for the fallback, while the
// controller cannot be reached.
package registry

// logPackage is the package field of the log lines.
const logPackage = "registry"
