package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newTestCommand(t *testing.T, cfgPath string) *cobra.Command {
	t.Helper()

	rootCmd := &cobra.Command{Use: "test", Run: func(*cobra.Command, []string) {}}
	rootCmd.Flags().String("config", cfgPath, "")
	rootCmd.Flags().String("host", defaultHost, "")
	rootCmd.Flags().Int("port", defaultPort, "")
	rootCmd.Flags().Int("max-messages", defaultMaxMessages, "")
	rootCmd.Flags().Int("max-prompt-tokens", defaultMaxPromptTokens, "")
	rootCmd.Flags().Bool("ngrok", defaultNgrok, "")
	rootCmd.Flags().String("otel-service-name", defaultOTelServiceName, "")
	return rootCmd
}

func loadWith(t *testing.T, cfgYAML string, args []string) *Config {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if cfgYAML != "" {
		if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}

	rootCmd := newTestCommand(t, cfgPath)
	rootCmd.SetArgs(args)
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	viper.Reset()
	if err := viper.BindPFlags(rootCmd.Flags()); err != nil {
		t.Fatalf("bind flags: %v", err)
	}
	viper.SetEnvPrefix("DEEPSEEK")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	cfg := &Config{}
	if err := LoadConfig(rootCmd, cfg); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

const cfgMaxMessages120 = "max_messages: 120\n"

// TestConfigPrecedenceCLIBeatsFile is the regression test for the bug this
// rewrite fixes: a flag passed on the command line must win over config.yaml.
func TestConfigPrecedenceCLIBeatsFile(t *testing.T) {
	t.Setenv("DEEPSEEK_MAX_MESSAGES", "")

	cfg := loadWith(t, cfgMaxMessages120, []string{"--max-messages", "999"})
	if cfg.MaxMessages != 999 {
		t.Errorf("flag must beat file: got %d, want 999", cfg.MaxMessages)
	}
}

func TestConfigPrecedenceFileUsedWhenNoFlag(t *testing.T) {
	t.Setenv("DEEPSEEK_MAX_MESSAGES", "")

	cfg := loadWith(t, cfgMaxMessages120, nil)
	if cfg.MaxMessages != 120 {
		t.Errorf("file value must apply without a flag: got %d, want 120", cfg.MaxMessages)
	}
}

func TestConfigPrecedenceEnvBeatsFile(t *testing.T) {
	t.Setenv("DEEPSEEK_MAX_MESSAGES", "200")

	cfg := loadWith(t, cfgMaxMessages120, nil)
	if cfg.MaxMessages != 200 {
		t.Errorf("env must beat file: got %d, want 200", cfg.MaxMessages)
	}
}

func TestConfigPrecedenceCLIBeatsEnv(t *testing.T) {
	t.Setenv("DEEPSEEK_MAX_MESSAGES", "200")

	cfg := loadWith(t, cfgMaxMessages120, []string{"--max-messages", "999"})
	if cfg.MaxMessages != 999 {
		t.Errorf("flag must beat env: got %d, want 999", cfg.MaxMessages)
	}
}

func TestConfigDefaultsWhenNothingSet(t *testing.T) {
	t.Setenv("DEEPSEEK_MAX_MESSAGES", "")
	t.Setenv("DEEPSEEK_HOST", "")

	cfg := loadWith(t, "port: 9100\n", nil)
	if cfg.MaxMessages != defaultMaxMessages {
		t.Errorf("MaxMessages = %d, want default %d", cfg.MaxMessages, defaultMaxMessages)
	}
	if cfg.Host != defaultHost {
		t.Errorf("Host = %q, want default %q", cfg.Host, defaultHost)
	}
	if cfg.Port != 9100 {
		t.Errorf("Port = %d, want file value 9100", cfg.Port)
	}
}

func TestConfigBoolFlagOverridesFile(t *testing.T) {
	cfg := loadWith(t, "ngrok: false\n", []string{"--ngrok"})
	if !cfg.Ngrok {
		t.Error("--ngrok must override ngrok: false from the file")
	}
}
