package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	AppDirName                   = ".deepseek-cursor-proxy"
	ConfigFileName               = "config.yaml"
	ReasoningContentFileName     = "reasoning_content.sqlite3"

	defaultHost                   = "127.0.0.1"
	defaultPort                   = 9000
	defaultUpstreamBaseURL       = "https://api.deepseek.com"
	defaultUpstreamModel         = "deepseek-v4-pro"
	defaultThinking              = "enabled"
	defaultReasoningEffort       = "max"
	defaultDisplayReasoning      = true
	defaultCollapsibleReasoning  = true
	defaultNgrok                 = true
	defaultVerbose               = false
	defaultRequestTimeout        = 300.0
	defaultMaxRequestBodyBytes   = 20 * 1024 * 1024
	defaultCORS                  = false
	defaultMissingReasoningStrategy = "recover"
	defaultReasoningCacheMaxAge  = 30 * 24 * 60 * 60
	defaultReasoningCacheMaxRows = 100000
	defaultMaxConcurrentRequests = 10
)

// Config holds all configuration for the proxy.
type Config struct {
	Host                       string `mapstructure:"host"`
	Port                       int    `mapstructure:"port"`
	UpstreamBaseURL            string `mapstructure:"base_url"`
	UpstreamModel              string `mapstructure:"model"`
	Thinking                   string `mapstructure:"thinking"`
	ReasoningEffort            string `mapstructure:"reasoning_effort"`
	RequestTimeout             float64 `mapstructure:"request_timeout"`
	MaxRequestBodyBytes        int64  `mapstructure:"max_request_body_bytes"`
	ReasoningContentPath       string `mapstructure:"reasoning_content_path"`
	MissingReasoningStrategy   string `mapstructure:"missing_reasoning_strategy"`
	ReasoningCacheMaxAgeSeconds int   `mapstructure:"reasoning_cache_max_age_seconds"`
	ReasoningCacheMaxRows      int    `mapstructure:"reasoning_cache_max_rows"`
	DisplayReasoning           bool   `mapstructure:"display_reasoning"`
	CollapsibleReasoning       bool   `mapstructure:"collapsible_reasoning"`
	CORS                       bool   `mapstructure:"cors"`
	Verbose                    bool   `mapstructure:"verbose"`
	Ngrok                      bool   `mapstructure:"ngrok"`
	NgrokURL                   string `mapstructure:"ngrok_url"`
	TraceDir                   string `mapstructure:"trace_dir"`
	MaxConcurrentRequests      int    `mapstructure:"max_concurrent_requests"`
	OTelEndpoint               string `mapstructure:"otel_endpoint"`
	OTelServiceName            string `mapstructure:"otel_service_name"`
	ClearReasoningCache        bool   `mapstructure:"-"`
}

// defaultAppDir returns ~/.deepseek-cursor-proxy.
func defaultAppDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, AppDirName), nil
}

// defaultConfigPath returns the default config file path.
func defaultConfigPath() (string, error) {
	dir, err := defaultAppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ConfigFileName), nil
}

// defaultReasoningContentPath returns the default SQLite path.
func defaultReasoningContentPath() (string, error) {
	dir, err := defaultAppDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ReasoningContentFileName), nil
}

// NewDefaultConfig creates a Config with all defaults applied.
func NewDefaultConfig() *Config {
	return &Config{
		Host:                       defaultHost,
		Port:                       defaultPort,
		UpstreamBaseURL:            defaultUpstreamBaseURL,
		UpstreamModel:              defaultUpstreamModel,
		Thinking:                   defaultThinking,
		ReasoningEffort:            defaultReasoningEffort,
		RequestTimeout:             defaultRequestTimeout,
		MaxRequestBodyBytes:        defaultMaxRequestBodyBytes,
		MissingReasoningStrategy:   defaultMissingReasoningStrategy,
		ReasoningCacheMaxAgeSeconds: defaultReasoningCacheMaxAge,
		ReasoningCacheMaxRows:      defaultReasoningCacheMaxRows,
		DisplayReasoning:           defaultDisplayReasoning,
		CollapsibleReasoning:       defaultCollapsibleReasoning,
		CORS:                       defaultCORS,
		Verbose:                    defaultVerbose,
		Ngrok:                      defaultNgrok,
		MaxConcurrentRequests:      defaultMaxConcurrentRequests,
	}
}

// defaultConfigYAML returns the default YAML config content.
func defaultConfigYAML() string {
	return fmt.Sprintf(`# This file was created automatically at ~/.deepseek-cursor-proxy/config.yaml.
# API keys are read from Cursor's Authorization header and forwarded upstream.

# ` + "`model`" + ` is the fallback when a request has no model; Cursor's requested
# DeepSeek model name is otherwise respected.
base_url: %s
model: %s
thinking: %s
reasoning_effort: %s
display_reasoning: %t
collapsible_reasoning: %t

host: %s
port: %d
ngrok: %t
verbose: %t
request_timeout: %.0f
max_request_body_bytes: %d
cors: %t

reasoning_content_path: %s
missing_reasoning_strategy: %s
reasoning_cache_max_age_seconds: %d
reasoning_cache_max_rows: %d
max_concurrent_requests: %d
`,
		defaultUpstreamBaseURL,
		defaultUpstreamModel,
		defaultThinking,
		defaultReasoningEffort,
		defaultDisplayReasoning,
		defaultCollapsibleReasoning,
		defaultHost,
		defaultPort,
		defaultNgrok,
		defaultVerbose,
		defaultRequestTimeout,
		defaultMaxRequestBodyBytes,
		defaultCORS,
		ReasoningContentFileName,
		defaultMissingReasoningStrategy,
		defaultReasoningCacheMaxAge,
		defaultReasoningCacheMaxRows,
		defaultMaxConcurrentRequests,
	)
}

// ensureDefaultConfig creates the default config file if it does not exist.
func ensureDefaultConfig(configPath string) error {
	if _, err := os.Stat(configPath); err == nil {
		return nil // exists
	} else if !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("cannot create config directory %s: %w", dir, err)
	}
	if err := os.WriteFile(configPath, []byte(defaultConfigYAML()), 0600); err != nil {
		return fmt.Errorf("cannot write default config %s: %w", configPath, err)
	}
	return nil
}

// resolveReasoningContentPath resolves the reasoning content path.
// If relative, it resolves relative to the config directory.
func resolveReasoningContentPath(path string, configDir string) (string, error) {
	if path == "" {
		return defaultReasoningContentPath()
	}
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Abs(filepath.Join(configDir, path))
}

// BuildRootCommand builds the cobra root command with all flags and viper binding.
func BuildRootCommand() (*cobra.Command, *Config, error) {
	cfg := NewDefaultConfig()

	rootCmd := &cobra.Command{
		Use:   "deepseek-cursor-proxy",
		Short: "OpenAI-compatible HTTP proxy for DeepSeek API",
		Long: `A local proxy that sits between Cursor and the DeepSeek API,
solving compatibility issues with DeepSeek's thinking-mode tool-call API.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil // actual run is in main
		},
	}

	// Config path flag
	defaultCfgPath, err := defaultConfigPath()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot determine default config path: %w", err)
	}
	rootCmd.Flags().String("config", defaultCfgPath, "Path to YAML config file")

	// Bind CLI flags (viper compatible naming)
	rootCmd.Flags().String("host", defaultHost, "Bind host")
	rootCmd.Flags().Int("port", defaultPort, "Bind port")
	rootCmd.Flags().String("base-url", defaultUpstreamBaseURL, "DeepSeek base URL")
	rootCmd.Flags().String("model", defaultUpstreamModel, "Fallback DeepSeek model")
	rootCmd.Flags().String("thinking", defaultThinking, "Thinking mode: enabled or disabled")
	rootCmd.Flags().String("reasoning-effort", defaultReasoningEffort, "Reasoning effort: low, medium, high, max, xhigh")
	rootCmd.Flags().Float64("request-timeout", defaultRequestTimeout, "Upstream request timeout in seconds")
	rootCmd.Flags().Int64("max-request-body-bytes", defaultMaxRequestBodyBytes, "Maximum accepted request body size")
	rootCmd.Flags().String("missing-reasoning-strategy", defaultMissingReasoningStrategy, "Strategy: recover or reject")
	rootCmd.Flags().Int("reasoning-cache-max-age-seconds", defaultReasoningCacheMaxAge, "Maximum cache row age in seconds")
	rootCmd.Flags().Int("reasoning-cache-max-rows", defaultReasoningCacheMaxRows, "Maximum cache rows")
	rootCmd.Flags().Bool("display-reasoning", defaultDisplayReasoning, "Mirror reasoning_content into visible content")
	rootCmd.Flags().Bool("collapsible-reasoning", defaultCollapsibleReasoning, "Use Markdown details for mirrored reasoning")
	rootCmd.Flags().Bool("cors", defaultCORS, "Send permissive CORS headers")
	rootCmd.Flags().Bool("verbose", defaultVerbose, "Log detailed request information")
	rootCmd.Flags().Bool("ngrok", defaultNgrok, "Start an ngrok tunnel")
	rootCmd.Flags().String("ngrok-url", "", "Reserved ngrok endpoint / custom domain")
	rootCmd.Flags().String("trace-dir", "", "Write structured request traces to directory")
	rootCmd.Flags().String("otel-endpoint", "", "OpenTelemetry OTLP gRPC endpoint (e.g. host.docker.internal:4317)")
	rootCmd.Flags().String("otel-service-name", "deepseek-cursor-proxy-go", "OpenTelemetry service name")
	// Note: OTel flags are NOT bound to Viper to avoid pflag defaults overriding env vars.
	// They are read directly from os.Getenv in LoadConfig.
	rootCmd.Flags().Int("max-concurrent-requests", defaultMaxConcurrentRequests, "Maximum concurrent requests")
	rootCmd.Flags().Bool("clear-reasoning-cache", false, "Clear the reasoning cache and exit")

	// Bind pflags to viper
	if err := viper.BindPFlags(rootCmd.Flags()); err != nil {
		return nil, nil, fmt.Errorf("cannot bind flags: %w", err)
	}

	// Environment variable mapping
	viper.SetEnvPrefix("DEEPSEEK")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	return rootCmd, cfg, nil
}

// LoadConfig loads configuration from file and CLI flags.
// configPath is the path to the YAML config file; if empty, uses default.
func LoadConfig(rootCmd *cobra.Command, cfg *Config) error {
	configPath, _ := rootCmd.Flags().GetString("config")

	// Ensure default config file exists
	if configPath != "" {
		if err := ensureDefaultConfig(configPath); err != nil {
			return fmt.Errorf("cannot ensure config file: %w", err)
		}
	}

	// Set viper config file
	if configPath != "" {
		viper.SetConfigFile(configPath)
		if err := viper.ReadInConfig(); err != nil {
			// It's okay if the file doesn't exist yet for custom paths
			if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
				return fmt.Errorf("cannot read config file: %w", err)
			}
		}
	}

	// Unmarshal into config struct
	if err := viper.Unmarshal(cfg); err != nil {
		return fmt.Errorf("cannot unmarshal config: %w", err)
	}

	// Normalize values
	cfg.Thinking = normalizeString(cfg.Thinking, defaultThinking)
	if cfg.Thinking != "enabled" && cfg.Thinking != "disabled" {
		cfg.Thinking = defaultThinking
	}

	cfg.MissingReasoningStrategy = normalizeString(cfg.MissingReasoningStrategy, defaultMissingReasoningStrategy)
	if cfg.MissingReasoningStrategy != "recover" && cfg.MissingReasoningStrategy != "reject" {
		cfg.MissingReasoningStrategy = defaultMissingReasoningStrategy
	}

	// Resolve reasoning content path
	configDir := filepath.Dir(configPath)
	resolvedPath, err := resolveReasoningContentPath(cfg.ReasoningContentPath, configDir)
	if err != nil {
		return fmt.Errorf("cannot resolve reasoning content path: %w", err)
	}
	cfg.ReasoningContentPath = resolvedPath

	// Strip trailing slash from base URL
	cfg.UpstreamBaseURL = strings.TrimRight(cfg.UpstreamBaseURL, "/")

	// Check clear-reasoning-cache flag
	cfg.ClearReasoningCache, _ = rootCmd.Flags().GetBool("clear-reasoning-cache")

	// Read OTel values directly from env to avoid pflag defaults overriding env vars
	if v := os.Getenv("DEEPSEEK_OTEL_ENDPOINT"); v != "" {
		cfg.OTelEndpoint = v
	}
	if v := os.Getenv("DEEPSEEK_OTEL_SERVICE_NAME"); v != "" {
		cfg.OTelServiceName = v
	}

	return nil
}

func normalizeString(val, defaultVal string) string {
	val = strings.TrimSpace(strings.ToLower(val))
	if val == "" {
		return defaultVal
	}
	return val
}

// Validate checks the configuration for validity.
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if c.MaxRequestBodyBytes < 1024 {
		return fmt.Errorf("max_request_body_bytes must be at least 1024")
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request_timeout must be positive")
	}
	if c.MaxConcurrentRequests < 1 {
		return fmt.Errorf("max_concurrent_requests must be at least 1")
	}
	return nil
}
