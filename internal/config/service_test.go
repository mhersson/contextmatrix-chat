package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validServiceConfig returns a ServiceConfig with every required field set so
// individual tests can null out one field and assert the resulting error.
func validServiceConfig() ServiceConfig {
	return ServiceConfig{
		ContextMatrixURL: "http://contextmatrix:8080",
		APIKey:           "0123456789abcdef0123456789abcdef", // 32 chars
		BaseImage:        "ghcr.io/example/chat@sha256:" + repeatHex(64),
		ImagePullPolicy:  "if-not-present",
		MaxConcurrent:    5,
		Port:             9093,
		SecretsDir:       "/var/run/cm-chat/secrets",
		Compaction:       CompactionConfig{Threshold: 0.85, KeepRecentTurns: 6},
		ChatRunDir:       "/var/run/cm-chat/sessions",
	}
}

func repeatHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}

	return string(b)
}

func TestServiceDefaults(t *testing.T) {
	// Loading from a nonexistent path yields defaults+env only. With no CMX_*
	// env set, all defaults must land.
	clearServiceEnv(t)

	cfg, err := LoadService(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)

	assert.Equal(t, 9093, cfg.Port)
	assert.Equal(t, "if-not-present", cfg.ImagePullPolicy)
	assert.Equal(t, 5, cfg.MaxConcurrent)
	assert.Equal(t, int64(8*1024*1024*1024), cfg.ContainerMemoryBytes)
	assert.Equal(t, int64(512), cfg.ContainerPidsLimit)
	assert.Equal(t, "/var/run/cm-chat/secrets", cfg.SecretsDir)
	assert.Equal(t, 330*time.Second, cfg.ReplaySkew)
	assert.Equal(t, 10000, cfg.ReplayCacheSize)
	assert.Equal(t, 10*time.Minute, cfg.MessageDedupTTL)
	assert.Equal(t, 1000, cfg.MessageDedupCacheSize)
	assert.Equal(t, 600, cfg.BashTimeoutMaxSeconds)
	assert.Equal(t, 131072, cfg.ToolOutputMaxBytes)
	assert.InDelta(t, 0.85, cfg.Compaction.Threshold, 1e-9, "compaction.threshold default must be 0.85")
	assert.Equal(t, 6, cfg.Compaction.KeepRecentTurns, "compaction.keep_recent_turns default must be 6")
}

func TestServiceLoadFromFile(t *testing.T) {
	clearServiceEnv(t)

	content := `
contextmatrix_url: http://cm.example:8080
container_contextmatrix_url: http://cm-internal:8080
api_key: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
port: 9999
admin_port: 9994
base_image: ghcr.io/example/chat:v1
image_pull_policy: always
max_concurrent: 12
container_memory_limit: 4294967296
container_pids_limit: 256
secrets_dir: /opt/secrets
webhook_replay_skew_seconds: 120
webhook_replay_cache_size: 4096
message_dedup_ttl_seconds: 300
message_dedup_cache_size: 512
bash_timeout_max_seconds: 900
tool_output_max_bytes: 50000
log_level: debug
compaction:
  threshold: 0.75
  keep_recent_turns: 4
chat_run_dir: /var/run/cm-chat/sessions
worker_extra_env:
  FOO: bar
  BAZ: qux
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.Equal(t, "http://cm.example:8080", cfg.ContextMatrixURL)
	assert.Equal(t, "http://cm-internal:8080", cfg.ContainerContextMatrixURL)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", cfg.APIKey)
	assert.Equal(t, 9999, cfg.Port)
	assert.Equal(t, 9994, cfg.AdminPort)
	assert.Equal(t, "ghcr.io/example/chat:v1", cfg.BaseImage)
	assert.Equal(t, "always", cfg.ImagePullPolicy)
	assert.Equal(t, 12, cfg.MaxConcurrent)
	assert.Equal(t, int64(4294967296), cfg.ContainerMemoryBytes)
	assert.Equal(t, int64(256), cfg.ContainerPidsLimit)
	assert.Equal(t, "/opt/secrets", cfg.SecretsDir)
	assert.Equal(t, 120*time.Second, cfg.ReplaySkew)
	assert.Equal(t, 4096, cfg.ReplayCacheSize)
	assert.Equal(t, 300*time.Second, cfg.MessageDedupTTL)
	assert.Equal(t, 512, cfg.MessageDedupCacheSize)
	assert.Equal(t, 900, cfg.BashTimeoutMaxSeconds)
	assert.Equal(t, 50000, cfg.ToolOutputMaxBytes)
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.InDelta(t, 0.75, cfg.Compaction.Threshold, 1e-9)
	assert.Equal(t, 4, cfg.Compaction.KeepRecentTurns)
	assert.Equal(t, "/var/run/cm-chat/sessions", cfg.ChatRunDir)
	assert.Equal(t, map[string]string{"FOO": "bar", "BAZ": "qux"}, cfg.WorkerExtraEnv)
}

func TestServiceEnvOverridesFile(t *testing.T) {
	clearServiceEnv(t)

	content := `
contextmatrix_url: http://from-file:8080
api_key: filekeyfilekeyfilekeyfilekeyfile
base_image: ghcr.io/example/chat:v1
chat_run_dir: /var/run/file
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	t.Setenv("CMX_CONTEXTMATRIX_URL", "http://from-env:8080")
	t.Setenv("CMX_PORT", "7777")
	t.Setenv("CMX_COMPACTION__THRESHOLD", "0.90")
	t.Setenv("CMX_CHAT_RUN_DIR", "/var/run/env")

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.Equal(t, "http://from-env:8080", cfg.ContextMatrixURL)
	assert.Equal(t, 7777, cfg.Port)
	assert.InDelta(t, 0.90, cfg.Compaction.Threshold, 1e-9)
	assert.Equal(t, "/var/run/env", cfg.ChatRunDir)
	// Untouched file value survives.
	assert.Equal(t, "ghcr.io/example/chat:v1", cfg.BaseImage)
}

func TestServiceValidate(t *testing.T) {
	t.Run("valid passes", func(t *testing.T) {
		cfg := validServiceConfig()
		require.NoError(t, cfg.Validate())
	})

	t.Run("missing contextmatrix_url errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ContextMatrixURL = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "contextmatrix_url")
	})

	t.Run("missing api_key errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.APIKey = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "api_key")
	})

	t.Run("short api_key errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.APIKey = "tooshort"
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "32")
	})

	t.Run("missing base_image errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.BaseImage = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "base_image")
	})

	t.Run("bad image_pull_policy errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ImagePullPolicy = "sometimes"
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "image_pull_policy")
	})

	t.Run("zero max_concurrent errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.MaxConcurrent = 0
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_concurrent")
		// Message must explain that 0 would refuse every launch.
		assert.Contains(t, err.Error(), "refuses every launch")
	})

	t.Run("port zero errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Port = 0
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "port")
	})

	t.Run("port above 65535 errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Port = 70000
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "port")
	})

	t.Run("non-digest base_image passes (warns only)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.BaseImage = "ghcr.io/example/chat:v1"
		require.NoError(t, cfg.Validate())
	})

	t.Run("missing secrets_dir errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.SecretsDir = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "secrets_dir")
	})

	t.Run("compaction threshold zero errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Compaction.Threshold = 0
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compaction.threshold")
	})

	t.Run("compaction threshold above 1 errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Compaction.Threshold = 1.1
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compaction.threshold")
	})

	t.Run("compaction threshold exactly 1 passes", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Compaction.Threshold = 1.0
		require.NoError(t, cfg.Validate())
	})

	t.Run("missing chat_run_dir errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.ChatRunDir = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "chat_run_dir")
	})
}

func TestServiceValidate_ReasoningEffort(t *testing.T) {
	t.Parallel()

	ok := validServiceConfig()
	ok.ReasoningEffort = "high"
	require.NoError(t, ok.Validate())

	bad := validServiceConfig()
	bad.ReasoningEffort = "extreme"
	err := bad.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reasoning_effort")
}

func TestCACertFileValidation(t *testing.T) {
	t.Run("empty disables (valid)", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.CACertFile = ""
		require.NoError(t, cfg.Validate())
	})

	t.Run("missing file errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.CACertFile = filepath.Join(t.TempDir(), "nope.pem")
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ca_cert_file")
	})

	t.Run("existing file passes", func(t *testing.T) {
		cfg := validServiceConfig()
		path := filepath.Join(t.TempDir(), "ca.pem")
		require.NoError(t, os.WriteFile(path, []byte("placeholder"), 0o600))
		cfg.CACertFile = path
		require.NoError(t, cfg.Validate())
	})
}

func TestCACertFileEnvBinding(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv("CMX_CA_CERT_FILE", "/host/ca.pem")

	cfg, err := LoadService(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "/host/ca.pem", cfg.CACertFile)
}

func TestServiceAdminPort_DefaultZero(t *testing.T) {
	clearServiceEnv(t)

	cfg, err := LoadService(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.AdminPort, "admin_port defaults to 0 (disabled)")
}

func TestServiceAdminPort_Validate(t *testing.T) {
	t.Run("disabled is valid", func(t *testing.T) {
		c := validServiceConfig()
		c.AdminPort = 0
		require.NoError(t, c.Validate())
	})

	t.Run("distinct port is valid", func(t *testing.T) {
		c := validServiceConfig()
		c.Port = 9093
		c.AdminPort = 9094
		require.NoError(t, c.Validate())
	})

	t.Run("out of range is rejected", func(t *testing.T) {
		c := validServiceConfig()
		c.AdminPort = 70000
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "admin_port")
	})

	t.Run("collision with port is rejected", func(t *testing.T) {
		c := validServiceConfig()
		c.Port = 9093
		c.AdminPort = 9093
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "admin_port")
	})
}

// writeServeYAML writes body to a temp serve.yaml and returns its path.
func writeServeYAML(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	return path
}

func TestLoadService_ImageListFiltersFromYAML(t *testing.T) {
	path := writeServeYAML(t, `
contextmatrix_url: http://cm:8080
api_key: 0123456789abcdef0123456789abcdef
base_image: img:dev
image_list_filters:
  - contextmatrix-chat
  - ghcr.io/you/my-chat-worker
`)

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"contextmatrix-chat", "ghcr.io/you/my-chat-worker"}, cfg.ImageListFilters)
}

func TestLoadService_ImageListFiltersEmptyFallsBackToDefault(t *testing.T) {
	path := writeServeYAML(t, `
contextmatrix_url: http://cm:8080
api_key: 0123456789abcdef0123456789abcdef
base_image: img:dev
image_list_filters: []
`)

	cfg, err := LoadService(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"contextmatrix-chat"}, cfg.ImageListFilters)
}

func TestValidate_ImageListFiltersBlankEntryRejected(t *testing.T) {
	path := writeServeYAML(t, `
contextmatrix_url: http://cm:8080
api_key: 0123456789abcdef0123456789abcdef
base_image: img:dev
image_list_filters:
  - "  "
`)

	cfg, err := LoadService(path)
	require.NoError(t, err)

	err = cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image_list_filters")
}

// clearServiceEnv unsets any CMX_* vars that could leak into a default/file
// test from the developer's shell. t.Setenv restores them after the test.
func clearServiceEnv(t *testing.T) {
	t.Helper()

	for _, e := range []string{
		"CMX_CONTEXTMATRIX_URL", "CMX_PORT",
		"CMX_API_KEY", "CMX_BASE_IMAGE", "CMX_MAX_CONCURRENT",
		"CMX_ADMIN_PORT",
		"CMX_COMPACTION__THRESHOLD", "CMX_COMPACTION__KEEP_RECENT_TURNS",
		"CMX_CHAT_RUN_DIR", "CMX_CA_CERT_FILE",
	} {
		if _, ok := os.LookupEnv(e); ok {
			t.Setenv(e, "")
			require.NoError(t, os.Unsetenv(e))
		}
	}
}
