// Package config resolves runtime configuration from environment variables and
// sensible defaults. Paths are resolved relative to the current working
// directory so the binary behaves naturally when launched from any folder.
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Config holds all settings the manager needs at startup. Fields are
// intentionally simple so that the binary remains usable with zero
// configuration.
type Config struct {
	// Addr is the listen address for the local HTTP dashboard.
	Addr string

	// DBPath is the absolute path to the SQLite database file.
	DBPath string

	// MasterKeyPath is the absolute path to the AES-GCM master key file used
	// to encrypt API keys at rest. Created with mode 0600 on first run.
	MasterKeyPath string

	// ArtifactsDir is where session artifacts (downloaded files, handoffs)
	// are persisted.
	ArtifactsDir string
}

// Load reads configuration from the environment, applying defaults for any
// values that were not set. It never fails: missing values are filled with
// defaults rather than erroring out, since the dashboard is meant to work out
// of the box.
func Load() Config {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	cfg := Config{
		Addr:          envOr("DEVINMGR_ADDR", ":8765"),
		DBPath:        envOr("DEVINMGR_DB", filepath.Join(cwd, "devinmgr.db")),
		MasterKeyPath: envOr("DEVINMGR_MASTER_KEY", filepath.Join(cwd, ".master_key")),
		ArtifactsDir:  envOr("DEVINMGR_ARTIFACTS", filepath.Join(cwd, "artifacts")),
	}
	return cfg
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return fallback
}
