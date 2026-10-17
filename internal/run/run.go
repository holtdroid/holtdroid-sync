package run

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

type Job struct {
	Name      string
	Short     string
	EnvPrefix string
	Version   string
	Run       func(ctx context.Context, configPath string, once bool) error
}

func Main(job Job) {
	var (
		configPath, logLevel, logFormat string
		once                            bool
	)
	root := &cobra.Command{
		Use:           job.Name,
		Short:         job.Short,
		Version:       job.Version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return setupLogging(logLevel, logFormat)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return job.Run(cmd.Context(), configPath, once)
		},
	}
	root.Flags().StringVarP(&configPath, "config", "c", "", "path to a YAML config file")
	root.Flags().BoolVar(&once, "once", false, "run one pass and exit")
	root.Flags().StringVar(&logLevel, "log-level", envOr(job.EnvPrefix+"LOG_LEVEL", "info"), "debug, info, warn or error")
	root.Flags().StringVar(&logFormat, "log-format", envOr(job.EnvPrefix+"LOG_FORMAT", "json"), "json or text")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := root.ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func Every(ctx context.Context, interval time.Duration, pass func(context.Context)) {
	for {
		pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func setupLogging(level, format string) error {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return err
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "json":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, opts)))
	case "text":
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
	default:
		return fmt.Errorf("unknown log format %q", format)
	}
	return nil
}
