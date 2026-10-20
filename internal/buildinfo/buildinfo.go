// Package buildinfo reports the build serve runs: the stamped version, commit
// and build time, plus the merged PRs recorded in the embedded history.
package buildinfo

import (
	"embed"
	"io/fs"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-protocol/changelog"
)

// The committed .gitkeep keeps the pattern matching when no history was written.
//
//go:embed all:history
var history embed.FS

const historyFile = "history/merges.log"

// The Makefile stamps these through -ldflags -X.
var version, commit, buildTime string

// Info returns this binary's build stamp and its recent changes.
func Info() protocol.BuildInfo {
	return build(history)
}

func build(fsys fs.FS) protocol.BuildInfo {
	// A missing file is the only read error an embedded FS has here: no
	// history was recorded, so raw stays nil.
	raw, _ := fs.ReadFile(fsys, historyFile)

	return protocol.BuildInfo{
		Version:   version,
		Commit:    commit,
		BuildTime: buildTime,
		Changes:   changelog.Parse(raw, changelog.Limit),
	}
}
