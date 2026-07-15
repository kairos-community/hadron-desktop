// Package buildinfo carries the build-time identity of hadron-agent: the
// binary's own version and the pinned cua-driver revision it is built to run
// against. Both have sane defaults so `hadron-agent version` prints something
// meaningful even for a plain `go build`, and both can be overridden at link
// time with -ldflags -X.
package buildinfo

import "fmt"

// Version is the hadron-agent build version. Override at link time with
//
//	-ldflags "-X github.com/mudler/hadron-desktop/agent/internal/buildinfo.Version=v1.2.3"
var Version = "dev"

// CuaRevision is the pinned cua-driver source revision this binary targets. It
// defaults to the commit pinned in Dockerfile.agent (CUA_COMMIT) so `version`
// reports the real revision without any linker flags; override it at link time
// the same way as Version when building from a different pin.
var CuaRevision = "3cadb5f82e7d2ed071a2082764276ec872a52135"

// String renders the multi-line version banner: the build version plus the
// cua-driver revision the binary is built against.
func String() string {
	return fmt.Sprintf("hadron-agent %s\ncua-driver revision: %s", Version, CuaRevision)
}
