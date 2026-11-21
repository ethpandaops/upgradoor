// Package registry resolves tags to digests at OCI distribution registries.
//
// The tags come from the hosts, so the registries are not trusted and the
// hosts choose where requests go: unless AllowPrivate is set, a connection
// is made only to a public address, whatever name, redirect or token service
// led there. Credentials go only to the registry they belong to, never
// across a redirect to another host, and token URLs are kept out of errors.
// Registry answers are bounded.
package registry

// Config is the controller's registry configuration.
type Config struct {
	// PlainHTTP lists registry hosts to talk to without TLS.
	PlainHTTP []string
	// CAFile is an extra CA bundle for private registries.
	CAFile string
	// AllowPrivate also connects to loopback, private and link-local
	// addresses: a registry inside the cluster or on a development machine.
	AllowPrivate bool
}
