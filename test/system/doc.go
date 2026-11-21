// Package systemtest drives the OS part of the agent host package against a
// real Debian system: real dpkg, real apt, real systemd, a real reboot.
//
// The tests carry the system_integration build tag and must only ever run
// inside the throwaway container that run.sh (make test-system) builds and
// boots: they upgrade packages and reboot the machine they run on. They
// refuse to start anywhere else.
package systemtest
