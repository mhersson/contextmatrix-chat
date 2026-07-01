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
		LLMEndpoint:      LLMEndpoint{Type: "openrouter", APIKey: "sk-or-test"},
		ImagePullPolicy:  "if-not-present",
		MaxConcurrent:    5,
		Port:             9093,
		SecretsDir:       "/var/run/cm-chat/secrets",
		Compaction:       CompactionConfig{Threshold: 0.85, KeepRecentTurns: 6},
		ChatRunDir:       "/var/run/cm-chat/sessions",
		GitHub: GitHubConfig{
			AuthMode: "pat",
			PAT:      GitHubPATConfig{Token: "ghp_test"},
		},
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
llm_endpoint:
  type: openrouter
  api_key: sk-or-fromfile
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
github:
  auth_mode: app
  app:
    app_id: 12345
    installation_id: 67890
    private_key_path: /etc/key.pem
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
	assert.Equal(t, "openrouter", cfg.LLMEndpoint.Type)
	assert.Equal(t, "sk-or-fromfile", cfg.LLMEndpoint.APIKey)
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
	assert.Equal(t, "app", cfg.GitHub.AuthMode)
	assert.Equal(t, int64(12345), cfg.GitHub.App.AppID)
	assert.Equal(t, int64(67890), cfg.GitHub.App.InstallationID)
	assert.Equal(t, "/etc/key.pem", cfg.GitHub.App.PrivateKeyPath)
	assert.Equal(t, map[string]string{"FOO": "bar", "BAZ": "qux"}, cfg.WorkerExtraEnv)
}

func TestServiceEnvOverridesFile(t *testing.T) {
	clearServiceEnv(t)

	content := `
contextmatrix_url: http://from-file:8080
api_key: filekeyfilekeyfilekeyfilekeyfile
base_image: ghcr.io/example/chat:v1
llm_endpoint:
  api_key: sk-or-file
chat_run_dir: /var/run/file
github:
  auth_mode: pat
  pat:
    token: ghp_file
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	t.Setenv("CMX_CONTEXTMATRIX_URL", "http://from-env:8080")
	t.Setenv("CMX_PORT", "7777")
	t.Setenv("CMX_LLM_ENDPOINT__API_KEY", "sk-or-env")
	t.Setenv("CMX_GITHUB__AUTH_MODE", "pat")
	t.Setenv("CMX_GITHUB__PAT__TOKEN", "ghp_env")
	t.Setenv("CMX_COMPACTION__THRESHOLD", "0.90")
	t.Setenv("CMX_CHAT_RUN_DIR", "/var/run/env")

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.Equal(t, "http://from-env:8080", cfg.ContextMatrixURL)
	assert.Equal(t, 7777, cfg.Port)
	assert.Equal(t, "sk-or-env", cfg.LLMEndpoint.APIKey)
	assert.Equal(t, "ghp_env", cfg.GitHub.PAT.Token)
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

	t.Run("missing llm_endpoint.api_key errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.LLMEndpoint.APIKey = ""
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "llm_endpoint.api_key")
	})

	t.Run("unknown llm_endpoint.type errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.LLMEndpoint = LLMEndpoint{Type: "anthropic", APIKey: "k"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "openrouter")
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

	t.Run("negative max_concurrent errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.MaxConcurrent = -3
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_concurrent")
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

	t.Run("compaction threshold negative errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.Compaction.Threshold = -0.1
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

	t.Run("app auth_mode requires app fields", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub = GitHubConfig{AuthMode: "app"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "app")
	})

	t.Run("pat auth_mode requires token", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub = GitHubConfig{AuthMode: "pat"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "token")
	})

	t.Run("unknown auth_mode errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub = GitHubConfig{AuthMode: "oauth"}
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "auth_mode")
	})
}

func TestGitHubHostDerivesAPIBaseURL(t *testing.T) {
	cases := []struct {
		name       string
		host       string
		apiBaseURL string
		want       string
	}{
		{"bare host derives api/v3", "ghe.example.com", "", "https://ghe.example.com/api/v3"},
		{"explicit api_base_url wins", "ghe.example.com", "https://api.acme.ghe.com", "https://api.acme.ghe.com"},
		{"full-url host accepted", "https://ghe.example.com", "", "https://ghe.example.com/api/v3"},
		{"no host leaves api_base_url empty", "", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := serviceRaw{GitHub: GitHubConfig{Host: tc.host, APIBaseURL: tc.apiBaseURL}}

			cfg, err := raw.toConfig()
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.GitHub.APIBaseURL)
		})
	}
}

func TestGitHubHostEnvBinding(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv("CMX_GITHUB__HOST", "ghe.example.com")

	cfg, err := LoadService(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "ghe.example.com", cfg.GitHub.Host)
	assert.Equal(t, "https://ghe.example.com/api/v3", cfg.GitHub.APIBaseURL,
		"CMX_GITHUB__HOST must bind and derive the api base url")
}

func TestGitHubHostValidation(t *testing.T) {
	t.Run("bare host passes", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub.Host = "ghe.example.com"
		require.NoError(t, cfg.Validate())
	})

	t.Run("full url host passes", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub.Host = "https://ghe.example.com"
		require.NoError(t, cfg.Validate())
	})

	t.Run("garbage host errors", func(t *testing.T) {
		cfg := validServiceConfig()
		cfg.GitHub.Host = "https://"
		err := cfg.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "github.host")
	})
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

func TestServiceAdminPort_FromEnv(t *testing.T) {
	clearServiceEnv(t)
	t.Setenv("CMX_ADMIN_PORT", "9094")

	cfg, err := LoadService(filepath.Join(t.TempDir(), "nope.yaml"))
	require.NoError(t, err)
	assert.Equal(t, 9094, cfg.AdminPort)
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

func TestServiceLLMEndpointLoadsAndValidates(t *testing.T) {
	clearServiceEnv(t)

	content := `
contextmatrix_url: http://contextmatrix:8080
api_key: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
base_image: ghcr.io/example/chat@sha256:` + repeatHex(64) + `
image_pull_policy: if-not-present
max_concurrent: 5
port: 9093
secrets_dir: /var/run/cm-chat/secrets
chat_run_dir: /var/run/cm-chat/sessions
llm_endpoint:
  type: openai
  base_url: https://your-llm-endpoint.example/v1
  api_key: k
compaction:
  threshold: 0.85
  keep_recent_turns: 6
github:
  auth_mode: pat
  pat:
    token: ghp_test
`
	path := filepath.Join(t.TempDir(), "serve.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := LoadService(path)
	require.NoError(t, err)

	assert.Equal(t, "openai", cfg.LLMEndpoint.Type)
	assert.Equal(t, "https://your-llm-endpoint.example/v1", cfg.LLMEndpoint.BaseURL)
	assert.Equal(t, "k", cfg.LLMEndpoint.APIKey)
	require.NoError(t, cfg.Validate())
}

func TestServiceLLMEndpointOpenAIRequiresBaseURL(t *testing.T) {
	cfg := &ServiceConfig{
		ContextMatrixURL: "http://contextmatrix:8080",
		APIKey:           "0123456789abcdef0123456789abcdef",
		LLMEndpoint:      LLMEndpoint{Type: "openai", APIKey: "k"},
	}
	err := cfg.Validate()
	require.Error(t, err)
	assert.ErrorContains(t, err, "base_url")
}

// clearServiceEnv unsets any CMX_* vars that could leak into a default/file
// test from the developer's shell. t.Setenv restores them after the test.
func clearServiceEnv(t *testing.T) {
	t.Helper()

	for _, e := range []string{
		"CMX_CONTEXTMATRIX_URL", "CMX_PORT", "CMX_LLM_ENDPOINT__API_KEY",
		"CMX_LLM_ENDPOINT__TYPE", "CMX_LLM_ENDPOINT__BASE_URL",
		"CMX_API_KEY", "CMX_BASE_IMAGE", "CMX_MAX_CONCURRENT",
		"CMX_GITHUB__AUTH_MODE", "CMX_GITHUB__PAT__TOKEN", "CMX_GITHUB__HOST",
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
