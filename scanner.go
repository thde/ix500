package ix500

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"time"
)

// Scanner manages scanner lifecycle.
type Scanner struct {
	dev         io.ReadWriteCloser
	initialized bool
	opts        Options
}

var (
	ErrNoDocument     = errors.New("no document in hopper")
	ErrNotInitialized = errors.New("scanner not initialized")
)

// Options configures scanner behavior.
type Options struct {
	ButtonPollInterval time.Duration // How often to check button (default: 1s)
	DataPollInterval   time.Duration // Retry interval for ErrTemporaryNoData (default: 500ms)
	RicRetries         int           // Max Ric() retries (default: 120)
}

func defaultOptions() Options {
	return Options{
		ButtonPollInterval: 1 * time.Second,
		DataPollInterval:   500 * time.Millisecond,
		RicRetries:         120,
	}
}

// HardwareStatus contains scanner state.
type HardwareStatus struct {
	Hopper bool // Paper loaded
	ScanSw bool // Scan button pressed
}

func hwStatusFromDriver(status hardwareStatus) *HardwareStatus {
	return &HardwareStatus{
		Hopper: status.Hopper,
		ScanSw: status.ScanSw,
	}
}

// FindScanner discovers and opens the scanner USB device.
func FindScanner() (io.ReadWriteCloser, error) {
	return FindDevice()
}

// New creates a Scanner wrapping the USB device.
// Call Close() when done to release resources.
func New(dev io.ReadWriteCloser, opts *Options) *Scanner {
	s := &Scanner{
		dev:  dev,
		opts: defaultOptions(),
	}
	if opts != nil {
		if opts.ButtonPollInterval > 0 {
			s.opts.ButtonPollInterval = opts.ButtonPollInterval
		}
		if opts.DataPollInterval > 0 {
			s.opts.DataPollInterval = opts.DataPollInterval
		}
		if opts.RicRetries > 0 {
			s.opts.RicRetries = opts.RicRetries
		}
	}
	return s
}

// Close releases all resources and closes USB device.
func (s *Scanner) Close() error {
	return s.dev.Close()
}

// Initialize prepares scanner hardware (executes 13-step sequence).
// Must be called before WaitForButton or Scan.
func (s *Scanner) Initialize(ctx context.Context) error {
	if s.initialized {
		return nil // idempotent
	}

	steps := []struct {
		name string
		fn   func(io.ReadWriter) error
	}{
		{"inquire", Inquire},
		{"preread", Preread},
		{"mode_select_auto", ModeSelectAuto},
		{"mode_select_double_feed", ModeSelectDoubleFeed},
		{"mode_select_background", ModeSelectBackground},
		{"mode_select_dropout", ModeSelectDropout},
		{"mode_select_buffering", ModeSelectBuffering},
		{"mode_select_prepick", ModeSelectPrepick},
		{"set_window", SetWindow},
		{"send_lut", SendLut},
		{"send_qtable", SendQtable},
		{"lamp_on", LampOn},
	}

	for _, step := range steps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := step.fn(s.dev); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}

	// Final status check
	if _, err := GetHardwareStatus(s.dev); err != nil {
		return fmt.Errorf("get_hardware_status: %w", err)
	}

	s.initialized = true
	return nil
}

// GetStatus returns current hardware status.
func (s *Scanner) GetStatus(ctx context.Context) (*HardwareStatus, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	status, err := GetHardwareStatus(s.dev)
	if err != nil {
		return nil, fmt.Errorf("get hardware status: %w", err)
	}

	return hwStatusFromDriver(status), nil
}

// IsButtonPressed checks button state without blocking.
func (s *Scanner) IsButtonPressed(ctx context.Context) (bool, error) {
	status, err := s.GetStatus(ctx)
	if err != nil {
		return false, err
	}
	return status.ScanSw, nil
}

// WaitForButton blocks until scan button is pressed or ctx cancelled.
func (s *Scanner) WaitForButton(ctx context.Context) error {
	ticker := time.NewTicker(s.opts.ButtonPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			status, err := s.GetStatus(ctx)
			if err != nil {
				return err
			}
			if status.ScanSw {
				return nil
			}
		}
	}
}

// Scan performs complete scan, yielding each side as it's scanned.
// Sides are yielded immediately as they're processed (streaming).
// Order: sheet N side 0, sheet N side 1, sheet N-1 side 0, ...
// (Hardware scans last sheet first)
// Use Page.Sheet and Page.Side fields to reorder if needed.
// Iterator continues until hopper empty, error, or ctx cancelled.
func (s *Scanner) Scan(ctx context.Context) iter.Seq2[*Page, error] {
	return func(yield func(*Page, error) bool) {
		if !s.initialized {
			yield(nil, ErrNotInitialized)
			return
		}

		for sheetNum := 0; ; sheetNum++ {
			if ctx.Err() != nil {
				yield(nil, ctx.Err())
				return
			}

			// Load next sheet
			if err := ObjectPosition(s.dev); err != nil {
				if errors.Is(err, ErrHopperEmpty) {
					if sheetNum == 0 {
						yield(nil, ErrNoDocument)
					}
					return // end of documents
				}
				yield(nil, fmt.Errorf("object position: %w", err))
				return
			}

			// Start scan
			if err := StartScan(s.dev); err != nil {
				yield(nil, fmt.Errorf("start scan: %w", err))
				return
			}

			// Get pixel size
			if err := GetPixelSize(s.dev); err != nil {
				yield(nil, fmt.Errorf("get pixel size: %w", err))
				return
			}

			// Scan both sides - yield each immediately
			for side := range 2 {
				if ctx.Err() != nil {
					yield(nil, ctx.Err())
					return
				}

				// Create streaming reader for this side
				reader := &streamingReader{
					ctx:     ctx,
					scanner: s,
					side:    side,
				}

				// Convert to image while streaming
				img, err := imageFromReader(reader)
				if err != nil {
					yield(nil, fmt.Errorf("scan side %d: %w", side, err))
					return
				}

				// Yield this side immediately
				page := &Page{
					Image: img,
					Side:  side,
					Sheet: sheetNum,
				}

				if !yield(page, nil) {
					return // caller stopped iteration
				}
			}
		}
	}
}
