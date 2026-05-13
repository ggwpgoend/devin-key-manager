// Package version exposes the build-time version string for the devinmgr binary.
package version

// Version is overridden via -ldflags at build time. Defaults to "dev" so local
// builds remain identifiable without requiring a git tag.
var Version = "dev"
