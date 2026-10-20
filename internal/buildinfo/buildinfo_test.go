package buildinfo

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-protocol/changelog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuild(t *testing.T) {
	record := "2026-10-15T12:03:00+02:00\x1fMerge pull request #100 from mhersson/worker-image-jvm\x1ffeat(docker): add jvm-node worker variant\n\x1e"

	tests := []struct {
		name string
		fsys fstest.MapFS
		want []protocol.Change
	}{
		{"no history file", fstest.MapFS{"history/.gitkeep": {}}, []protocol.Change{}},
		{"empty history file", fstest.MapFS{historyFile: {Data: nil}}, []protocol.Change{}},
		{"one merge", fstest.MapFS{historyFile: {Data: []byte(record)}}, []protocol.Change{{
			Number: 100, Kind: protocol.ChangeKindFeat, Title: "add jvm-node worker variant",
			MergedAt: "2026-10-15T10:03:00Z",
		}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := build(tt.fsys).Changes
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuild_CopiesStampedValues(t *testing.T) {
	oldVersion, oldCommit, oldBuildTime := version, commit, buildTime

	t.Cleanup(func() { version, commit, buildTime = oldVersion, oldCommit, oldBuildTime })

	version = "v1.2.3"
	commit = "0123456789abcdef0123456789abcdef01234567"
	buildTime = "2026-10-21T08:00:00Z"

	got := build(fstest.MapFS{})
	assert.Equal(t, "v1.2.3", got.Version)
	assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", got.Commit)
	assert.Equal(t, "2026-10-21T08:00:00Z", got.BuildTime)
}

// The Makefile's git log output is what changelog.Parse reads, so a drift in
// its arguments or target path empties every change list with no other symptom.
func TestMakefileWritesTheEmbeddedHistory(t *testing.T) {
	lines := makefileLines(t)

	var gitLog string

	for _, l := range lines {
		if strings.Contains(l, "git log") {
			gitLog = strings.TrimSpace(l)

			break
		}
	}

	require.NotEmpty(t, gitLog, "no git log recipe line in the Makefile")

	cmd, redirect, ok := strings.Cut(gitLog, ">")
	require.True(t, ok, "git log output is not redirected: %q", gitLog)
	assert.Equal(t, append([]string{"git"}, changelog.GitLogArgs...), strings.Fields(cmd))
	assert.Equal(t, "$(HISTORY)", strings.Fields(redirect)[0])
	assert.Equal(t, "internal/buildinfo/"+historyFile, makeVar(t, lines, "HISTORY"))
}

type pkgMarker struct{}

// The linker silently ignores -X on a missing symbol, so nothing else catches
// a typo in these names.
func TestMakefile_LdflagsStampBuildinfoVars(t *testing.T) {
	lines := makefileLines(t)

	assert.Equal(t, reflect.TypeOf(pkgMarker{}).PkgPath(), makeVar(t, lines, "BUILDINFO"))

	ldflags := makeVar(t, lines, "LDFLAGS")
	for _, v := range []string{"version", "commit", "buildTime"} {
		assert.Contains(t, ldflags, "-X $(BUILDINFO)."+v+"=")
	}
}

func makefileLines(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)

	return strings.Split(string(raw), "\n")
}

// makeVar returns the value of the Makefile's "NAME :=" assignment.
func makeVar(t *testing.T, lines []string, name string) string {
	t.Helper()

	for _, l := range lines {
		lhs, rhs, ok := strings.Cut(l, ":=")
		if ok && strings.TrimSpace(lhs) == name {
			return strings.TrimSpace(rhs)
		}
	}

	require.Failf(t, "missing Makefile variable", "%s :=", name)

	return ""
}
