// Package host is the only code that reads or changes the machine, behind
// the Host interface. It joins docker (containers, images, pulls, the
// recreate), system (packages, reboot, OS facts) and probe (EL and CL
// probes). It decides nothing: it reports raw facts and carries out exactly
// the call it is given.
//
// No container is created that does not replace one of the same name, and a
// recreate stops nothing unless the local image has the granted digest.
package host
