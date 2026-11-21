// Package link is the agent's side of the controller: it signs every request
// with the host key, verifies every answer against the configured controller
// keys and paces the syncs.
//
// It never acts on an answer. Nothing of an answer is returned before its
// signature, host, proof id and body hash are verified; a non-2xx,
// unverifiable or undecodable answer is only an error. Answers are bounded
// and requests go over https unless allowInsecure.
package link
