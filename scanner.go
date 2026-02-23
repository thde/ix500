package ix500

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"time"
)

// Scanner manages the lifecycle and operations of a Fujitsu ScanSnap iX500 scanner.
// It handles initialization, button polling, and scanning operations.
type Scanner struct {
	dev         *device
	initialized bool
	opts        Options
}

var (
	// ErrNoDocument is returned when an operation requires a document in the hopper,
	// but none is detected.
	ErrNoDocument = errors.New("no document in hopper")

	// ErrNotInitialized is returned when scanning is attempted before calling Initialize.
	ErrNotInitialized = errors.New("scanner not initialized")
)

// Options configures timing and retry behavior for Scanner operations.
type Options struct {
	// ButtonPollInterval specifies how often to check the scan button status.
	// Default is 1 second.
	ButtonPollInterval time.Duration

	// DataPollInterval specifies the retry interval when the scanner reports
	// temporary no data during scanning.
	// Default is 500ms.
	DataPollInterval time.Duration

	// RicRetries specifies the maximum number of times to retry the ric command
	// when waiting for image data to become available. Each retry waits DataPollInterval
	// before the next attempt. Default is 120 retries (60 seconds at 500ms intervals).
	RicRetries int
}

// defaultOptions returns the default options for the Scanner.
func defaultOptions() Options {
	return Options{
		ButtonPollInterval: 1 * time.Second,
		DataPollInterval:   500 * time.Millisecond,
		RicRetries:         120,
	}
}

// New creates a new Scanner instance wrapping the provided USB device.
// The opts parameter can be nil to use default options.
// The caller is responsible for closing the underlying device via the Close method
// when the scanner is no longer needed.
func New(dev io.ReadWriteCloser, opts *Options) *Scanner {
	s := &Scanner{
		dev:  &device{dev: dev},
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

// Close releases any resources associated with the scanner and closes the underlying USB device.
func (s *Scanner) Close() error {
	return s.dev.Close()
}

// Initialize prepares the scanner hardware for operation.
//
// This method executes the required SCSI command sequence to configure the scanner:
//  1. INQUIRY - verify device identification
//  2. SEND DIAGNOSTIC (preread) - set 600 dpi resolution
//  3. MODE SELECT (multiple) - configure ADF, double-feed detection, color dropout, buffering, etc.
//  4. SET WINDOW - define front and back scan areas
//  5. SEND - transfer lookup tables and quantization tables
//  6. SCANNER_CONTROL (lamp on) - activate scanning lamp
//  7. GET_HW_STATUS - verify hardware readiness
//
// This method must be called successfully before WaitForButton or Scan.
// It is idempotent; subsequent calls return immediately if already initialized.
func (s *Scanner) Initialize(ctx context.Context) error {
	if s.initialized {
		return nil // idempotent
	}

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"inquire", s.dev.inquire},
		{"preread", s.dev.preread},
		{"mode_select_auto", s.dev.modeSelectAuto},
		{"mode_select_double_feed", s.dev.modeSelectDoubleFeed},
		{"mode_select_background", s.dev.modeSelectBackground},
		{"mode_select_dropout", s.dev.modeSelectDropout},
		{"mode_select_buffering", s.dev.modeSelectBuffering},
		{"mode_select_prepick", s.dev.modeSelectPrepick},
		{"set_window", s.dev.setWindow},
		{"send_lut", s.dev.sendLUT},
		{"send_qtable", s.dev.sendQTable},
		{"lamp_on", s.dev.lampOn},
	}

	for _, step := range steps {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := step.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}

	if _, err := s.dev.hardwareStatus(ctx); err != nil {
		return fmt.Errorf("get_hardware_status: %w", err)
	}

	s.initialized = true
	return nil
}

// IsButtonPressed checks if the scan button is currently pressed.
// It performs a non-blocking check of the hardware status.
func (s *Scanner) IsButtonPressed(ctx context.Context) (bool, error) {
	status, err := s.dev.hardwareStatus(ctx)
	if err != nil {
		return false, err
	}

	return status.ScanSw, nil
}

// WaitForButton blocks until the scan button is pressed or the context is cancelled.
// It polls the scanner status at the interval specified in Options.ButtonPollInterval.
func (s *Scanner) WaitForButton(ctx context.Context) error {
	ticker := time.NewTicker(s.opts.ButtonPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			status, err := s.dev.hardwareStatus(ctx)
			if err != nil {
				return err
			}
			if status.ScanSw {
				return nil
			}
		}
	}
}

// Scan performs a complete duplex scan operation, yielding pages as they are scanned.
//
// This method returns an iterator that yields *Page values for each side of each
// sheet. The scanning sequence for each sheet is:
//  1. OBJECT POSITION - load paper from hopper
//  2. SCAN - initiate scanning of both sides
//  3. READ (vendor-specific) - query pixel dimensions
//  4. For each side (front=0, back=1):
//     - Issue ric commands until data is ready
//     - Execute READ commands to stream image data
//     - Decode RGB data into image.Image
//     - Yield the Page immediately
//
// The iteration continues until the hopper is empty (ErrHopperEmpty), an error
// occurs, or the context is cancelled. Pages are yielded in hardware scan order:
// Sheet N Front, Sheet N Back, Sheet (N-1) Front, etc. The iX500 scans the last
// sheet first. Callers can use Page.Sheet and Page.Side to reorder as needed.
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
			if err := s.dev.objectPosition(ctx); err != nil {
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
			if err := s.dev.startScan(ctx); err != nil {
				yield(nil, fmt.Errorf("start scan: %w", err))
				return
			}

			// Get pixel size
			if err := s.dev.pixelSize(ctx); err != nil {
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
					ctx:              ctx,
					dev:              s.dev,
					side:             side,
					dataPollInterval: s.opts.DataPollInterval,
					ricRetries:       s.opts.RicRetries,
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
					Side:  Side(side),
					Sheet: sheetNum,
				}

				if !yield(page, nil) {
					return // caller stopped iteration
				}
			}
		}
	}
}
