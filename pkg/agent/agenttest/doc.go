// Package agenttest is test support: an in-memory host (Host, built from a
// Scenario) that implements host.Host and holds its registry, and a scripted
// controller (Controller) that authenticates requests like the real one and
// signs its answers.
//
// Only tests and the tools under test/ import it; a test enforces that.
package agenttest
