// Package config loads layered chat service configuration: defaults < file <
// env (CMX_*), via koanf.
package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

const envPrefix = "CMX_"

// defaultSecretsDir is a filesystem PATH, not a credential. Named via const to
// avoid the gosec G101 false-positive that fires on path literals.
const defaultSecretsDir = "/var/run/cm-chat/secrets" //nolint:gosec // path, not a credential

// GitHubAppConfig holds GitHub App credentials for minting installation tokens.
// Mirrors the runner/agent field shape so operators carry one mental model.
type GitHubAppConfig struct {
	AppID          int64  `koanf:"app_id"`
	InstallationID int64  `koanf:"installation_id"`
	PrivateKeyPath string `koanf:"private_key_path"`
}

// GitHubPATConfig holds a fine-grained personal access token used instead of a
// GitHub App where App creation is restricted.
type GitHubPATConfig struct {
	Token string `koanf:"token"`
}

// GitHubConfig is the unified GitHub auth block. Set AuthMode to "app" or "pat".
type GitHubConfig struct {
	AuthMode   string          `koanf:"auth_mode"`
	APIBaseURL string          `koanf:"api_base_url"`
	App        GitHubAppConfig `koanf:"app"`
	PAT        GitHubPATConfig `koanf:"pat"`
}

// CompactionConfig controls in-context compaction. When the conversation
// reaches Threshold of the model's context window, older turns are dropped and
// only the most recent KeepRecentTurns turns are retained verbatim.
type CompactionConfig struct {
	Threshold       float64 `koanf:"threshold"`
	KeepRecentTurns int     `koanf:"keep_recent_turns"`
}

// ServiceConfig is the host-side chat service configuration. ContextMatrix
// POSTs lifecycle webhooks at the service; it launches one worker container per
// chat session.
type ServiceConfig struct {
	ContextMatrixURL          string
	ContainerContextMatrixURL string
	APIKey                    string
	Port                      int
	AdminPort                 int
	BaseImage                 string
	ImagePullPolicy           string
	MaxConcurrent             int
	ContainerMemoryBytes      int64
	ContainerPidsLimit        int64
	SecretsDir                string
	OpenRouterAPIKey          string
	GitHub                    GitHubConfig
	WorkerExtraEnv            map[string]string
	ReplaySkew                time.Duration
	ReplayCacheSize           int
	MessageDedupTTL           time.Duration
	MessageDedupCacheSize     int
	BashTimeoutMaxSeconds     int
	ToolOutputMaxBytes        int
	LogLevel                  string
	Compaction                CompactionConfig
	ChatRunDir                string
}

// serviceRaw is the koanf-unmarshalled wire shape. Duration fields are split:
// "<n>_seconds" keys are ints converted on load; other durations are Go
// duration strings. Keeping the wire shape separate from ServiceConfig means
// the public struct never carries half-parsed values.
type serviceRaw struct {
	ContextMatrixURL          string            `koanf:"contextmatrix_url"`
	ContainerContextMatrixURL string            `koanf:"container_contextmatrix_url"`
	APIKey                    string            `koanf:"api_key"`
	Port                      int               `koanf:"port"`
	AdminPort                 int               `koanf:"admin_port"`
	BaseImage                 string            `koanf:"base_image"`
	ImagePullPolicy           string            `koanf:"image_pull_policy"`
	MaxConcurrent             int               `koanf:"max_concurrent"`
	ContainerMemoryLimit      int64             `koanf:"container_memory_limit"`
	ContainerPidsLimit        int64             `koanf:"container_pids_limit"`
	SecretsDir                string            `koanf:"secrets_dir"`
	OpenRouterAPIKey          string            `koanf:"openrouter_api_key"`
	GitHub                    GitHubConfig      `koanf:"github"`
	WorkerExtraEnv            map[string]string `koanf:"worker_extra_env"`
	ReplaySkewSeconds         int               `koanf:"webhook_replay_skew_seconds"`
	ReplayCacheSize           int               `koanf:"webhook_replay_cache_size"`
	MessageDedupTTLSeconds    int               `koanf:"message_dedup_ttl_seconds"`
	MessageDedupCacheSize     int               `koanf:"message_dedup_cache_size"`
	BashTimeoutMaxSeconds     int               `koanf:"bash_timeout_max_seconds"`
	ToolOutputMaxBytes        int               `koanf:"tool_output_max_bytes"`
	LogLevel                  string            `koanf:"log_level"`
	Compaction                CompactionConfig  `koanf:"compaction"`
	ChatRunDir                string            `koanf:"chat_run_dir"`
}

// serviceDefaults is the lowest-precedence layer.
func serviceDefaults() serviceRaw {
	return serviceRaw{
		Port:                   9093,
		ImagePullPolicy:        "if-not-present",
		MaxConcurrent:          5,
		ContainerMemoryLimit:   8 * 1024 * 1024 * 1024, // 8 GiB
		ContainerPidsLimit:     512,
		SecretsDir:             defaultSecretsDir,
		ReplaySkewSeconds:      330,
		ReplayCacheSize:        10000,
		MessageDedupTTLSeconds: 600,
		MessageDedupCacheSize:  1000,
		BashTimeoutMaxSeconds:  600,
		ToolOutputMaxBytes:     131072,
		Compaction: CompactionConfig{
			Threshold:       0.85,
			KeepRecentTurns: 6,
		},
	}
}

// LoadService merges defaults < file (if it loads) < env (CMX_*). A
// nonexistent path is not an error: the file layer is skipped and the result
// is defaults+env, matching agent behavior.
func LoadService(path string) (*ServiceConfig, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(serviceDefaults(), "koanf"), nil); err != nil {
		return nil, fmt.Errorf("load service defaults: %w", err)
	}

	if path != "" {
		// A missing file is allowed (defaults+env only); other read/parse errors are not.
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil && !isNotExist(err) {
			return nil, fmt.Errorf("load service config file %q: %w", path, err)
		}
	}

	// CMX_FOO_BAR -> "foo_bar"; nested keys use "__":
	// CMX_GITHUB__AUTH_MODE -> "github.auth_mode"
	// CMX_COMPACTION__THRESHOLD -> "compaction.threshold"
	envCb := func(s string) string {
		s = strings.ToLower(strings.TrimPrefix(s, envPrefix))

		return strings.ReplaceAll(s, "__", ".")
	}
	if err := k.Load(env.Provider(envPrefix, ".", envCb), nil); err != nil {
		return nil, fmt.Errorf("load service env: %w", err)
	}

	var raw serviceRaw
	if err := k.UnmarshalWithConf("", &raw, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return nil, fmt.Errorf("unmarshal service config: %w", err)
	}

	return raw.toConfig()
}

// toConfig assembles the typed config from the wire form.
func (r serviceRaw) toConfig() (*ServiceConfig, error) {
	return &ServiceConfig{
		ContextMatrixURL:          r.ContextMatrixURL,
		ContainerContextMatrixURL: r.ContainerContextMatrixURL,
		APIKey:                    r.APIKey,
		Port:                      r.Port,
		AdminPort:                 r.AdminPort,
		BaseImage:                 r.BaseImage,
		ImagePullPolicy:           r.ImagePullPolicy,
		MaxConcurrent:             r.MaxConcurrent,
		ContainerMemoryBytes:      r.ContainerMemoryLimit,
		ContainerPidsLimit:        r.ContainerPidsLimit,
		SecretsDir:                r.SecretsDir,
		OpenRouterAPIKey:          r.OpenRouterAPIKey,
		GitHub:                    r.GitHub,
		WorkerExtraEnv:            r.WorkerExtraEnv,
		ReplaySkew:                time.Duration(r.ReplaySkewSeconds) * time.Second,
		ReplayCacheSize:           r.ReplayCacheSize,
		MessageDedupTTL:           time.Duration(r.MessageDedupTTLSeconds) * time.Second,
		MessageDedupCacheSize:     r.MessageDedupCacheSize,
		BashTimeoutMaxSeconds:     r.BashTimeoutMaxSeconds,
		ToolOutputMaxBytes:        r.ToolOutputMaxBytes,
		LogLevel:                  r.LogLevel,
		Compaction:                r.Compaction,
		ChatRunDir:                r.ChatRunDir,
	}, nil
}

// isNotExist reports whether err is a missing-file error from the file
// provider. The provider wraps os errors, so match on the message tail.
func isNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such file or directory")
}

// Validate checks the service config invariants after merging. A non-digest
// BaseImage is permitted but warns via slog so operators notice tag drift.
func (c *ServiceConfig) Validate() error {
	if c.ContextMatrixURL == "" {
		return fmt.Errorf("contextmatrix_url is required")
	}

	if c.APIKey == "" {
		return fmt.Errorf("api_key is required")
	}

	if len(c.APIKey) < 32 {
		return fmt.Errorf("api_key must be at least 32 characters, got %d", len(c.APIKey))
	}

	if c.OpenRouterAPIKey == "" {
		return fmt.Errorf("openrouter_api_key is required")
	}

	if c.BaseImage == "" {
		return fmt.Errorf("base_image is required")
	}

	if !strings.Contains(c.BaseImage, "@sha256:") {
		slog.Warn("base_image is not pinned to a digest; tag drift is possible",
			"base_image", c.BaseImage)
	}

	switch c.ImagePullPolicy {
	case "never", "if-not-present", "always":
	default:
		return fmt.Errorf("image_pull_policy must be never|if-not-present|always, got %q", c.ImagePullPolicy)
	}

	if c.MaxConcurrent < 1 {
		return fmt.Errorf(
			"max_concurrent must be >= 1, got %d: 0 disables the webhook capacity pre-check "+
				"while the tracker refuses every launch — triggers would be accepted then all fail",
			c.MaxConcurrent)
	}

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be in 1..65535, got %d", c.Port)
	}

	if c.AdminPort != 0 && (c.AdminPort < 1 || c.AdminPort > 65535) {
		return fmt.Errorf("admin_port must be 0 (disabled) or in 1..65535, got %d", c.AdminPort)
	}

	if c.AdminPort != 0 && c.AdminPort == c.Port {
		return fmt.Errorf("admin_port must differ from port (both set to %d)", c.Port)
	}

	if c.SecretsDir == "" {
		return fmt.Errorf("secrets_dir is required")
	}

	if c.Compaction.Threshold <= 0 || c.Compaction.Threshold > 1 {
		return fmt.Errorf("compaction.threshold must be in (0, 1], got %g", c.Compaction.Threshold)
	}

	if c.ChatRunDir == "" {
		return fmt.Errorf("chat_run_dir is required")
	}

	return c.GitHub.validate()
}

// validate checks the GitHub auth block, mirroring the runner/agent contract:
// exactly one auth path is populated per auth_mode.
func (g *GitHubConfig) validate() error {
	switch g.AuthMode {
	case "app":
		if g.App.AppID == 0 {
			return fmt.Errorf("github.app.app_id is required when github.auth_mode is \"app\"")
		}

		if g.App.InstallationID == 0 {
			return fmt.Errorf("github.app.installation_id is required when github.auth_mode is \"app\"")
		}

		if g.App.PrivateKeyPath == "" {
			return fmt.Errorf("github.app.private_key_path is required when github.auth_mode is \"app\"")
		}

		if g.PAT.Token != "" {
			return fmt.Errorf("github.pat.token must be empty when github.auth_mode is \"app\"")
		}
	case "pat":
		if g.PAT.Token == "" {
			return fmt.Errorf("github.pat.token is required when github.auth_mode is \"pat\"")
		}

		if g.App.AppID != 0 || g.App.InstallationID != 0 || g.App.PrivateKeyPath != "" {
			return fmt.Errorf("github.app.* must be empty when github.auth_mode is \"pat\"")
		}
	default:
		return fmt.Errorf("github.auth_mode is required: must be \"app\" or \"pat\" (got %q)", g.AuthMode)
	}

	return nil
}
