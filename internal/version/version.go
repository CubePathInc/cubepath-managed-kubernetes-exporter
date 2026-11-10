// Package version holds the build metadata, set at link time with -ldflags.
package version

// Version and Revision are overridden by the release build.
var (
	Version  = "dev"
	Revision = "unknown"
)

// UserAgent is the User-Agent header sent to the CubePath API.
func UserAgent() string {
	return "cubepath-managed-kubernetes-exporter/" + Version
}
