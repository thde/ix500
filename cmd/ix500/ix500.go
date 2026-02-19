// Package main provides a CLI tool for the Fujitsu ScanSnap iX500 ix500.
// It runs as a daemon, waiting for the scan button to be pressed, and then
// saves scanned pages as JPEG files in the specified output directory.
package main

import (
	"context"
	"flag"
	"fmt"
	"image/jpeg"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"thde.io/ix500"
)

// Config holds the configuration options for the scanner daemon.
type Config struct {
	// OutputDir is the directory where scanned images will be saved.
	OutputDir string
	// LogLevel sets the logging verbosity (debug, info, warn, error).
	LogLevel string
}

func parseFlags() *Config {
	cfg := &Config{}
	flag.StringVar(&cfg.OutputDir, "out-dir", "/perm/scannyd", "Output directory")
	flag.StringVar(&cfg.LogLevel, "log-level", "debug", "Log level (debug|info|warn|error)")
	flag.Parse()
	return cfg
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := parseFlags()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     parseLogLevel(cfg.LogLevel),
		AddSource: true,
	}))

	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		logger.Error("failed to create output directory", "dir", cfg.OutputDir, "err", err)
		os.Exit(1)
	}

	dev, err := ix500.FindDevice()
	if err != nil {
		logger.Error("scanner not found", "err", err)
		os.Exit(1)
	}

	scn := ix500.New(dev, nil)
	defer scn.Close()

	if err := scn.Initialize(ctx); err != nil {
		logger.Error("initialization failed", "err", err)
		os.Exit(1)
	}

	logger.Info("scanner daemon started", "output_dir", cfg.OutputDir)

	if err := loop(ctx, cfg, scn, logger); err != nil {
		logger.Error("daemon error", "err", err)
		os.Exit(1)
	}

	logger.Info("scanner daemon shutdown")
}

func loop(ctx context.Context, cfg *Config, scn *ix500.Scanner, logger *slog.Logger) error {
	for {
		logger.Info("waiting for scan button press")

		if err := scn.WaitForButton(ctx); err != nil {
			if ctx.Err() != nil {
				return nil // graceful shutdown
			}
			logger.Error("button wait error", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}

		timestamp := time.Now().Format("20060102-150405")
		logger.Info("scan button pressed")

		pageNum := 0
		for page, err := range scn.Scan(ctx) {
			if err != nil {
				logger.Error("scan error", "err", err)
				break
			}

			filename := fmt.Sprintf("scan-%s-page-%03d.jpg", timestamp, pageNum)
			path := filepath.Join(cfg.OutputDir, filename)

			if err := savePage(page, path); err != nil {
				logger.Error("save failed", "file", path, "err", err)
				break
			}

			bounds := page.Bounds()
			logger.Info("page saved", "file", filename, "size", fmt.Sprintf("%dx%d", bounds.Dx(), bounds.Dy()), "side", page.Side, "sheet", page.Sheet)
			pageNum++
		}

		logger.Info("scan complete", "pages", pageNum)

		// Wait for button release
		for {
			time.Sleep(100 * time.Millisecond)
			pressed, err := scn.IsButtonPressed(ctx)
			if err != nil || !pressed {
				break
			}
		}
	}
}

func savePage(page *ix500.Page, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return jpeg.Encode(f, page, &jpeg.Options{Quality: 75})
}

func parseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
