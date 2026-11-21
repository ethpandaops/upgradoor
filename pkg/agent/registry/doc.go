// Package registry resolves tags to digests, loads registry credentials from
// Docker's config.json and tracks and pulls the images the agent needs.
//
// An image is pulled by its tag only while the pull window names it at the
// digest of the tracker's check, and accepted only at that digest; the
// controller cannot name a digest to pull. Credentials go only to the
// registry they belong to, never across a redirect to another host, and
// token URLs are kept out of errors. Registry answers are bounded.
package registry

// logPackage is the package field of the log lines.
const logPackage = "registry"
