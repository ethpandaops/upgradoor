// Package link is the agent's side of the controller: it signs every request
// with the host key, seals every sync and inventory to the controller's
// encryption key, verifies every answer against the configured controller
// keys and paces the syncs. It sends no host state before it holds an
// encryption key from a verified answer, and never sends it unsealed.
//
// It never acts on an answer. Nothing of an answer is returned before its
// signature, host, proof id and body hash are verified; a non-2xx,
// unverifiable or undecodable answer is only an error. Answers are bounded
// and requests go over https unless allowInsecure.
package link
