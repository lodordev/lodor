// Package buildinfo carries the release version stamped into the binary at
// build time. Dev builds carry "dev" and the update modes treat that as
// "never offer an update" — a dev binary must not nag or self-clobber.
package buildinfo

// Version is set at release time via:
//
//	go build -ldflags "-X lodor/buildinfo.Version=<version>"
//
// release.sh owns the stamp; anything not built through it stays "dev".
var Version = "dev"
