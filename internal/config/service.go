// Package config loads layered chat service configuration: defaults < file <
// env (CMX_*), via koanf.
package config

import (
	"fmt"
	"log/slog"
	"os"
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
	AdminBindAddr             string
	MetricsToken              string
	BaseImage                 string
	ImagePullPolicy           string
	ImageListFilters          []string
	MaxConcurrent             int
	ContainerMemoryBytes      int64
	ContainerPidsLimit        int64
	SecretsDir                string
	CACertFile                string
	WorkerExtraEnv            map[string]string
	ReasoningEffort           string
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
	AdminBindAddr             string            `koanf:"admin_bind_addr"`
	MetricsToken              string            `koanf:"metrics_token"`
	BaseImage                 string            `koanf:"base_image"`
	ImagePullPolicy           string            `koanf:"image_pull_policy"`
	ImageListFilters          []string          `koanf:"image_list_filters"`
	MaxConcurrent             int               `koanf:"max_concurrent"`
	ContainerMemoryLimit      int64             `koanf:"container_memory_limit"`
	ContainerPidsLimit        int64             `koanf:"container_pids_limit"`
	SecretsDir                string            `koanf:"secrets_dir"`
	CACertFile                string            `koanf:"ca_cert_file"`
	WorkerExtraEnv            map[string]string `koanf:"worker_extra_env"`
	ReasoningEffort           string            `koanf:"reasoning_effort"`
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
		AdminBindAddr:          "127.0.0.1",
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

	return raw.toConfig(), nil
}

// toConfig assembles the typed config from the wire form.
func (r serviceRaw) toConfig() *ServiceConfig {
	imageListFilters := r.ImageListFilters
	if len(imageListFilters) == 0 {
		// Omitted or explicitly empty both fall back to the family default so
		// a misconfiguration can never expose the node's whole image inventory.
		imageListFilters = []string{"contextmatrix-chat"}
	}

	return &ServiceConfig{
		ContextMatrixURL:          r.ContextMatrixURL,
		ContainerContextMatrixURL: r.ContainerContextMatrixURL,
		APIKey:                    r.APIKey,
		Port:                      r.Port,
		AdminPort:                 r.AdminPort,
		AdminBindAddr:             r.AdminBindAddr,
		MetricsToken:              r.MetricsToken,
		BaseImage:                 r.BaseImage,
		ImagePullPolicy:           r.ImagePullPolicy,
		ImageListFilters:          imageListFilters,
		MaxConcurrent:             r.MaxConcurrent,
		ContainerMemoryBytes:      r.ContainerMemoryLimit,
		ContainerPidsLimit:        r.ContainerPidsLimit,
		SecretsDir:                r.SecretsDir,
		CACertFile:                r.CACertFile,
		WorkerExtraEnv:            r.WorkerExtraEnv,
		ReasoningEffort:           r.ReasoningEffort,
		ReplaySkew:                time.Duration(r.ReplaySkewSeconds) * time.Second,
		ReplayCacheSize:           r.ReplayCacheSize,
		MessageDedupTTL:           time.Duration(r.MessageDedupTTLSeconds) * time.Second,
		MessageDedupCacheSize:     r.MessageDedupCacheSize,
		BashTimeoutMaxSeconds:     r.BashTimeoutMaxSeconds,
		ToolOutputMaxBytes:        r.ToolOutputMaxBytes,
		LogLevel:                  r.LogLevel,
		Compaction:                r.Compaction,
		ChatRunDir:                r.ChatRunDir,
	}
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

	for _, f := range c.ImageListFilters {
		if strings.TrimSpace(f) == "" {
			return fmt.Errorf("image_list_filters entries must be non-empty")
		}
	}

	switch c.ReasoningEffort {
	case "", "low", "medium", "high":
	default:
		return fmt.Errorf("reasoning_effort must be low|medium|high (or empty), got %q", c.ReasoningEffort)
	}

	if c.MaxConcurrent < 1 {
		return fmt.Errorf(
			"max_concurrent must be >= 1, got %d: 0 disables the webhook capacity pre-check "+
				"while the tracker refuses every launch - triggers would be accepted then all fail",
			c.MaxConcurrent,
		)
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

	if c.AdminBindAddr == "" {
		c.AdminBindAddr = "127.0.0.1"
	}

	if c.SecretsDir == "" {
		return fmt.Errorf("secrets_dir is required")
	}

	// ca_cert_file is optional (empty disables it). When set it must exist on the
	// host so the worker container's read-only bind mount cannot fail at launch.
	if c.CACertFile != "" {
		if _, err := os.Stat(c.CACertFile); err != nil {
			return fmt.Errorf("ca_cert_file does not exist: %w", err)
		}
	}

	if c.Compaction.Threshold <= 0 || c.Compaction.Threshold > 1 {
		return fmt.Errorf("compaction.threshold must be in (0, 1], got %g", c.Compaction.Threshold)
	}

	if c.ChatRunDir == "" {
		return fmt.Errorf("chat_run_dir is required")
	}

	return nil
}
