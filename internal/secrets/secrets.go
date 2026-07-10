// Package secrets reads and writes KEY=value env files. The worker uses it to
// stage the CM-provisioned git-credentials config at boot (see
// chatwork.gitCredentialsConfigPath) and to read it back from the
// git-credential/gh-wrapper subcommands, which run with a scrubbed
// environment and can only learn secrets from a fixed file path.
package secrets

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Source holds key-value pairs parsed from an env file.
type Source struct{ vals map[string]string }

// Open parses a KEY=value env file. Blank lines and lines beginning with '#'
// are ignored. Values may contain '=' characters. A missing file is not an
// error: it returns an empty, usable Source (every Get returns "") rather than
// failing, so a caller that legitimately has nothing staged yet — e.g. the
// gh-wrapper subcommand before any git-credentials config exists — is not
// forced to special-case "file absent" itself. Any other read error
// (permission denied, path is a directory, ...) still fails.
func Open(path string) (*Source, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Source{vals: make(map[string]string)}, nil
		}

		return nil, fmt.Errorf("open env file: %w", err)
	}

	defer func() { _ = f.Close() }()

	vals := make(map[string]string)
	sc := bufio.NewScanner(f)

	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		vals[k] = v
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan env file: %w", err)
	}

	return &Source{vals: vals}, nil
}

// Get returns the value for key, or "" if not present.
func (s *Source) Get(key string) string {
	return s.vals[key]
}

// WriteEnvFile writes vals to path atomically (write-tmp + rename).
// The directory is created with mode 0700; the file is written with mode 0600.
// Keys are written in sorted order — map iteration is randomized, and the
// output must be byte-identical across rewrites.
func WriteEnvFile(path string, vals map[string]string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}

	var sb strings.Builder

	for _, k := range slices.Sorted(maps.Keys(vals)) {
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(vals[k])
		sb.WriteByte('\n')
	}

	// Write to a temp file in the same dir so rename is atomic on Linux.
	tmp, err := os.CreateTemp(dir, ".env-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	tmpPath := tmp.Name()

	if _, err := tmp.WriteString(sb.String()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)

		return fmt.Errorf("write env file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("chmod env file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("rename env file: %w", err)
	}

	return nil
}
