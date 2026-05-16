package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrksmt/deepseek-cursor-proxy/internal/config"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/server"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/store"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/trace"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/transform"
	"github.com/mrksmt/deepseek-cursor-proxy/internal/tunnel"
)

func main() {
	// Initialize time function for transform package
	transform.SetTimeNow(func() int64 {
		return time.Now().Unix()
	})

	// Build cobra command
	rootCmd, cfg, err := config.BuildRootCommand()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error initializing CLI: %v\n", err)
		os.Exit(1)
	}

	// Override Run to implement actual logic
	rootCmd.RunE = func(cmd *cobra.Command, args []string) error {

		ctx := cmd.Context()

		// Load configuration
		if err := config.LoadConfig(cmd, cfg); err != nil {
			return fmt.Errorf("cannot load config: %w", err)
		}

		// Validate configuration
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("invalid config: %w", err)
		}

		// Warn if upstream uses plain HTTP
		if isPlainHTTP(cfg.UpstreamBaseURL) {
			log.Printf("WARNING: upstream base_url uses plain HTTP; bearer tokens may be exposed")
		}

		// Initialize OpenTelemetry tracing (before store and server)
		var otelTracer *trace.OTelTracer
		if cfg.OTelEndpoint != "" {
			otelCfg := trace.OTelConfig{
				Endpoint:    cfg.OTelEndpoint,
				ServiceName: cfg.OTelServiceName,
			}
			otelTracer, err = trace.InitOTel(ctx, otelCfg)
			if err != nil {
				log.Printf("WARNING: failed to initialize OTel tracer: %v", err)
			}
			if otelTracer != nil {
				defer func() {
					if err := otelTracer.Shutdown(context.Background()); err != nil {
						log.Printf("error shutting down OTel tracer: %v", err)
					}
				}()
			}
		}

		// Initialize reasoning store
		reasoningStore, err := store.NewReasoningStore(
			ctx,
			cfg.ReasoningContentPath,
			cfg.ReasoningCacheMaxAgeSeconds,
			cfg.ReasoningCacheMaxRows,
		)
		if err != nil {
			return fmt.Errorf("cannot initialize reasoning store: %w", err)
		}
		defer func() {
			if err := reasoningStore.Close(ctx); err != nil {
				log.Printf("error closing reasoning store: %v", err)
			}
		}()

		// Handle clear cache
		if cfg.ClearReasoningCache {
			deleted, err := reasoningStore.Clear(ctx)
			if err != nil {
				return fmt.Errorf("cannot clear reasoning cache: %w", err)
			}
			log.Printf("cleared %d reasoning cache row(s)", deleted)
			return nil
		}

		// Create proxy server
		srv := server.NewProxyServer(cfg, reasoningStore)

		// Start ngrok tunnel if enabled
		var publicURL string
		if cfg.Ngrok {
			targetURL := tunnel.LocalTunnelTarget(cfg.Host, cfg.Port)
			ngrokTunnel := tunnel.NewNgrokTunnel(targetURL, cfg.NgrokURL)
			publicURL, err = ngrokTunnel.Start()
			if err != nil {
				return fmt.Errorf("cannot start ngrok tunnel: %w", err)
			}
			defer ngrokTunnel.Stop()
			log.Printf("ngrok tunnel established: %s", publicURL)
		}

		// Print startup info
		localBaseURL := fmt.Sprintf("http://%s:%d/v1", cfg.Host, cfg.Port)
		apiBaseURL := localBaseURL
		if publicURL != "" {
			apiBaseURL = fmt.Sprintf("%s/v1", strings.TrimRight(publicURL, "/"))
		}

		log.Printf("default_model: %s (thinking=%s, effort=%s)",
			cfg.UpstreamModel, cfg.Thinking, cfg.ReasoningEffort)

		if cfg.Verbose {
			displayReasoning := "off"
			if cfg.DisplayReasoning {
				displayReasoning = "on"
				if cfg.CollapsibleReasoning {
					displayReasoning = "on (collapsible)"
				}
			}
			log.Printf("display_reasoning: %s", displayReasoning)
			log.Printf("missing_reasoning_strategy: %s", cfg.MissingReasoningStrategy)
			log.Printf("reasoning_cache: %s", cfg.ReasoningContentPath)
			log.Printf("WARNING: verbose logging enabled; prompts and code may be written to stdout")
		}

		if publicURL == "" && !cfg.Ngrok {
			log.Printf("public_tunnel: off")
		}
		if cfg.Verbose {
			log.Printf("upstream_url: %s/chat/completions", cfg.UpstreamBaseURL)
		}
		log.Printf("local_base_url: %s", localBaseURL)
		log.Printf("api_base_url: %s", apiBaseURL)

		// Start server in background
		go func() {
			log.Printf("listening on %s:%d", cfg.Host, cfg.Port)
			if err := srv.Start(cfg.Host, cfg.Port); err != nil {
				log.Printf("server error: %v", err)
			}
		}()

		// Wait for shutdown signal
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		sig := <-quit
		log.Printf("shutting down (signal: %v)", sig)

		// Graceful shutdown
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("error during shutdown: %v", err)
		}

		return nil
	}

	// Execute
	if err := rootCmd.Execute(); err != nil {
		log.Printf("Error: %v", err)
		os.Exit(1)
	}
}

func isPlainHTTP(url string) bool {
	return strings.HasPrefix(url, "http://") &&
		!strings.Contains(url, "127.0.0.1") &&
		!strings.Contains(url, "localhost") &&
		!strings.Contains(url, "::1")
}
