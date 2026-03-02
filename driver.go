// Package ix500 implements a driver for the Fujitsu ScanSnap iX500 document scanner.
//
// This driver was developed from scratch based on USB traffic captures and implements
// the SCSI-2 scanner command set over USB bulk transfer. The terminology is consistent
// with the SANE fujitsu driver where appropriate.
//
// The SCSI-2 scanner device type uses a coordinate system with the upper-left corner
// as the origin, the x-axis extending left-to-right (cross-scan direction), and the
// y-axis extending top-to-bottom (scan direction). Measurements use a basic unit of
// 1/1200 inch by default.
//
// Scanner operations follow this sequence:
//  1. INQUIRY (0x12) - identify device
//  2. MODE SELECT (0x15) - configure scanner settings
//  3. SET WINDOW (0x24) - define scan area and parameters
//  4. SEND (0x2A) - transfer lookup tables and calibration data
//  5. OBJECT POSITION (0x31) - load paper from hopper
//  6. SCAN (0x1B) - initiate scanning
//  7. READ (0x28) - retrieve image data
//
// See also:
//   - https://www.staff.uni-mainz.de/tacke/scsi/SCSI2-15.html (SCSI-2 scanner specification)
//   - https://gitlab.com/sane-project/backends/-/raw/master/backend/fujitsu.c (SANE implementation)
//   - https://gitlab.com/sane-project/backends/-/raw/master/backend/fujitsu-scsi.h
package ix500

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

var (
	// ErrShortRead indicates the scanner returned fewer bytes than requested.
	//
	// This error is derived from the SCSI REQUEST SENSE response when the ILI
	// (Incorrect Length Indicator) flag is set. The information field contains
	// the residue (difference between requested and actual transfer length).
	ErrShortRead = errors.New("short read")

	// ErrEndOfPaper indicates the scanner has reached the end of the document.
	//
	// This error is signaled by the EOM (End of Medium) flag in the REQUEST SENSE
	// response. It occurs when scanning reaches the trailing edge of the paper,
	// and no more scan lines are available for the current page.
	ErrEndOfPaper = errors.New("end of paper")

	// ErrTemporaryNoData indicates the scanner has no data ready at this moment.
	//
	// This is a vendor-specific condition (ASC 0x80, ASCQ 0x13) that occurs when
	// the scanner is still processing the image and buffered data is not yet available
	// for transfer. Callers should retry the operation after a brief delay.
	ErrTemporaryNoData = errors.New("temporary no data")

	// ErrHopperEmpty indicates no paper is loaded in the automatic document feeder.
	//
	// This error (ASC 0x80, ASCQ 0x03) is returned by OBJECT POSITION when attempting
	// to load paper from an empty hopper. It signals the normal end of a multi-page
	// scanning session.
	ErrHopperEmpty = errors.New("hopper empty")
)

// device owns the USB ReadWriteCloser and provides the SCSI command layer.
// All scanner protocol functions are methods on device.
type device struct {
	dev        io.ReadWriteCloser
	resolution Resolution
	scanMode   ScanMode
}

// Close sends cleanup commands then closes the underlying USB device.
func (d *device) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = d.cancel(ctx)
	_ = d.lampOff(ctx)
	return d.dev.Close()
}

// usbcmd embeds a SCSI command in the USB bulk transfer format used by the
// Fujitsu ScanSnap iX500.
//
// The iX500 uses a vendor-specific USB command wrapper: a 31-byte structure
// starting with 0x43, followed by 18 bytes of padding (offset 0x01-0x12),
// and then the SCSI command at offset 0x13. This format encapsulates standard
// SCSI commands for transport over USB bulk endpoints.
func usbcmd(scsiCmd []byte) []byte {
	// All usb commands are 31 bytes in length, padded with zeros. The
	// actual command starts at offset 0x13.
	result := make([]byte, 31)
	result[0] = 0x43 // usb command
	copy(result[0x13:], scsiCmd)
	return result
}

// request represents a SCSI command to be sent to the scanner.
type request struct {
	// cmd field contains the SCSI command descriptor block (CDB), typically 6 or 10 bytes.
	cmd []byte
	// extra field optional parameter data sent after the command (for commands
	// like MODE SELECT, SET WINDOW, and SEND).
	extra []byte
	// respLen  specifies how many bytes
	// of response data to read before reading the 32-byte USB status response.
	respLen int
}

// response contains the data returned by a SCSI command.
type response struct {
	// raw contains the 32-byte USB status response that includes the SCSI status byte.
	raw []byte
	// extra contains any data payload returned by the command (e.g., inquiry data,
	// hardware status, or image data from READ commands).
	extra []byte
}

// doWithoutRequestSense executes a SCSI command without automatic error handling.
//
// This low-level function sends the command and extra data (if any), then reads
// the response data and USB status response. It does not check the status byte
// or issue REQUEST SENSE on error. This is used internally by do() and for the
// REQUEST SENSE command itself (to avoid infinite recursion).
func (d *device) doWithoutRequestSense(ctx context.Context, r *request) (*response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := d.dev.Write(usbcmd(r.cmd)); err != nil {
		return nil, err
	}

	if r.extra != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := d.dev.Write(r.extra); err != nil {
			return nil, err
		}
	}

	var resp response

	if r.respLen > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resp.extra = make([]byte, r.respLen)
		num, err := d.dev.Read(resp.extra)
		if err != nil {
			return nil, err
		}
		resp.extra = resp.extra[:num]

	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resp.raw = make([]byte, 32)
	num, err := d.dev.Read(resp.raw)
	if err != nil {
		return nil, err
	}
	resp.raw = resp.raw[:num]
	return &resp, err
}

// do sends a SCSI request to the scanner and handles error reporting.
//
// This function executes the specified request and checks the USB status byte
// at offset 9 in the response. If the status indicates an error (non-zero),
// it automatically issues a SCSI REQUEST SENSE command (0x03) to retrieve
// detailed error information including the sense key, additional sense code (ASC),
// and additional sense code qualifier (ASCQ).
//
// The REQUEST SENSE response also includes the ILI (Incorrect Length Indicator)
// and EOM (End of Medium) flags, which are used to adjust the returned data
// length and signal end-of-paper conditions.
func (d *device) do(ctx context.Context, r *request) (*response, error) {
	resp, err := d.doWithoutRequestSense(ctx, r)
	if err != nil {
		return nil, err
	}

	const usbStatusOffset = 9
	if resp.raw[usbStatusOffset] == 0 {
		return resp, nil
	}

	rsResp, err := d.doWithoutRequestSense(ctx, &request{
		cmd: []byte{
			// see http://self.gutenberg.org/articles/scsi_request_sense_command
			0x03, // SCSI opcode: REQUEST SENSE
			0x00, // byte 7, 6, 5: LUN. rest: reserved
			0x00, // reserved
			0x00, // reserved
			0x12, // allocation length
			0x00, // control
		},
		respLen: 18,
	})
	if err != nil {
		return nil, err
	}

	if rsResp.raw[usbStatusOffset] != 0 {
		return resp, errors.New("request sense failed")
	}

	// 000: f0 00 03 00 00 00 00 0a 00 00 00 00 80 13 00 00 ................
	// 010: 00 00                                           ..

	sense := rsResp.extra[2] & 0x0F
	asc := rsResp.extra[12]
	ascq := rsResp.extra[13]
	rsInfo := rsResp.extra[3 : 3+4]
	rsEom := (rsResp.extra[2]>>6)&0x1 == 0x1
	rsIli := (rsResp.extra[2]>>5)&0x1 == 0x1

	if rsIli {
		n := len(resp.extra) - int(binary.BigEndian.Uint32(rsInfo))
		resp.extra = resp.extra[:n]
	}

	return resp, requestSenseToError(sense, asc, ascq, rsEom, rsIli)
}

// inquire sends the SCSI INQUIRY command (0x12) to identify the scanner.
//
// The INQUIRY command is mandatory for all SCSI devices and retrieves standard
// device information including the device type (0x06 for scanner devices),
// vendor identification, product identification, and product revision level.
// The iX500 returns "FUJITSU" as vendor and "ScanSnap iX500" as product.
func (d *device) inquire(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 12 00 00 00 60 00 00 00 00 00 00 00    .......`.......

	resp, err := d.do(ctx, &request{
		cmd: []byte{
			0x12, // SCSI opcode: INQUIRY
			0x00, // EVPD (enable vital product data): disabled
			0x00, // page code (for EVPD)
			0x00, // reserved
			0x60, // allocation length: 96 bytes
			0x00, // control
		},
		respLen: 96,
	})
	if err != nil {
		return err
	}

	vendor := strings.TrimSpace(string(resp.extra[8:16]))
	product := strings.TrimSpace(string(resp.extra[16:32]))
	if vendor != "FUJITSU" {
		return fmt.Errorf("unsupported vendor: %q", vendor)
	}
	if !strings.Contains(product, "iX500") {
		return fmt.Errorf("unsupported product: %q", product)
	}

	// response:
	// 000: 06 00 92 02 5b 00 00 10 46 55 4a 49 54 53 55 20 ....[...FUJITSU
	// 010: 53 63 61 6e 53 6e 61 70 20 69 58 35 30 30 20 20 ScanSnap iX500
	// 020: 30 4d 30 30 00 00 00 00 00 00 00 00 03 01 00 00 0M00............
	// 030: 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 ................
	// 040: 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 ................
	// 050: 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 ................

	return nil
}

// preread configures scan parameters using the vendor-specific "SET PRE READMODE" command.
//
// This function uses the SCSI SEND DIAGNOSTIC command (0x1D) with vendor-specific
// parameter data to configure the scanner's resolution and paper dimensions before
// scanning. It sets both x and y resolution to 600 dpi (0x0258) and configures
// the paper width and length in units of 1/1200 inch. The composition byte (0x05)
// specifies the image format.
func (d *device) preread(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 1d 00 00 00 20 00 00 00 00 00 00 00    ....... .......

	// extra request:
	// 000: 53 45 54 20 50 52 45 20 52 45 41 44 4d 4f 44 45 SET PRE READMODE
	// 010: 02 58 02 58 00 00 26 c3 00 00 36 d1 05 7f 00 00 .X.X..&...6.....

	res := uint16(d.resolution)
	extra := append([]byte("SET PRE READMODE"),
		byte(res>>8), // x resolution: hi byte
		byte(res),    // x resolution: lo byte

		byte(res>>8), // y resolution: hi byte
		byte(res),    // y resolution: lo byte

		0x00, // paper width
		0x00, // paper width
		0x26, // paper width
		0xc3, // paper width

		0x00, // paper length
		0x00, // paper length
		0x36, // paper length
		0xd1, // paper length

		0x05, // composition
		0x7f, // TODO: where does this come from/what does it mean?
		0x00,
		0x00,
	)

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x1d, // SCSI opcode: SEND_DIAGNOSTIC
			0x00, // page format (bit 4): disabled
			// self test (bit 2): disabled,
			// device offline (bit 1): disabled,
			// unit offline (bit 0): disabled
			0x00, // reserved
			0x00, // parameter list length (MSB)
			0x20, // parameter list length (LSB)
			0x00, // control
		},
		extra: extra,
	})

	return err
}

// modeSelectAuto enables automatic document feeder (ADF) mode using MODE SELECT.
//
// The SCSI MODE SELECT command (0x15) configures device-specific parameters
// via mode pages. This function sends a vendor-specific mode page to enable
// automatic paper feeding with deskew (0x3c) and overscan (0x06) settings.
// The page format bit (0x10) is set to use the SCSI-2 page format.
func (d *device) modeSelectAuto(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 15 10 00 00 0c 00 00 00 00 00 00 00    ...............

	// extra request:
	// 000: 00 00 00 00 3c 06 00 00 00 00 00 00             ....<.......

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x15, // SCSI opcode: mode select
			0x10, // page format (bit 4): enabled
			// save pages (bit 0): disabled
			0x00, // reserved
			0x00, // reserved
			0x0c, // parameter list length
			0x00, // control
		},
		extra: []byte{
			0x00, // pc?
			0x00, // page len?
			0x00, // awd? crop?
			0x00, // ald?
			0x3c, // deskew
			0x06, // overscan
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
		},
	})

	return err
}

// modeSelectDoubleFeed enables double feed detection using MODE SELECT.
//
// Double feed detection is a feature that alerts when multiple sheets of paper
// are fed through the scanner simultaneously. This function uses MODE SELECT (0x15)
// with a vendor-specific mode page (0x38, 0x06) to enable this hardware capability.
func (d *device) modeSelectDoubleFeed(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 15 10 00 00 0c 00 00 00 00 00 00 00    ...............

	// extra request:
	// 000: 00 00 00 00 38 06 00 00 00 00 00 00             ....8.......

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x15, // SCSI opcode: mode select
			0x10, // page format (bit 4): enabled
			// save pages (bit 0): disabled
			0x00, // reserved
			0x00, // reserved
			0x0c, // parameter list length
			0x00, // control
		},
		extra: []byte{
			0x00, // pc?
			0x00, // page len?
			0x00,
			0x00,
			0x38,
			0x06,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
		},
	})

	return err
}

// modeSelectBackground configures background color handling using MODE SELECT.
//
// This function sets the scanner's background color processing mode, which affects
// how the scanner interprets the area around the document. The vendor-specific
// mode page (0x37, 0x06) controls this image processing feature.
func (d *device) modeSelectBackground(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 15 10 00 00 0c 00 00 00 00 00 00 00    ...............

	// extra request:
	// 000: 00 00 00 00 37 06 00 00 00 00 00 00             ....7.......

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x15, // SCSI opcode: mode select
			0x10, // page format (bit 4): enabled
			// save pages (bit 0): disabled
			0x00, // reserved
			0x00, // reserved
			0x0c, // parameter list length
			0x00, // control
		},
		extra: []byte{
			0x00,
			0x00,
			0x00,
			0x00,
			0x37,
			0x06,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
		},
	})

	return err
}

// modeSelectDropout configures color dropout filtering using MODE SELECT.
//
// Color dropout allows the scanner to filter out a specific color (typically red,
// green, or blue) from the scanned image. This is useful for removing colored
// forms or annotations. This function uses a vendor-specific mode page (0x39, 0x08)
// to configure the dropout settings.
func (d *device) modeSelectDropout(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 15 10 00 00 0e 00 00 00 00 00 00 00    ...............

	// extra request:
	// 000: 00 00 00 00 39 08 00 00 00 00 00 00 00 00       ....9.........

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x15, // SCSI opcode: mode select
			0x10, // page format (bit 4): enabled
			// save pages (bit 0): disabled
			0x00, // reserved
			0x00, // reserved
			0x0e, // parameter list length
			0x00, // control
		},
		extra: []byte{
			0x00,
			0x00,
			0x00,
			0x00,
			0x39,
			0x08,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
		},
	})

	return err
}

// modeSelectBuffering enables image buffering using MODE SELECT.
//
// Buffering allows the scanner to store scanned image data in its internal memory
// before transferring it to the host. This can improve scanning performance by
// allowing the scanner to continue scanning while previous data is being transferred.
// The mode page (0x3a, 0x06) with values 0x80, 0xc0 enables this feature.
func (d *device) modeSelectBuffering(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 15 10 00 00 0c 00 00 00 00 00 00 00    ...............

	// extra request:
	// 000: 00 00 00 00 3a 06 80 c0 00 00 00 00             ....:.......

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x15, // SCSI opcode: mode select
			0x10, // page format (bit 4): enabled
			// save pages (bit 0): disabled
			0x00, // reserved
			0x00, // reserved
			0x0c, // parameter list length
			0x00, // control
		},
		extra: []byte{
			0x00,
			0x00,
			0x00,
			0x00,
			0x3a,
			0x06,
			0x80,
			0xc0,
			0x00,
			0x00,
			0x00,
			0x00,
		},
	})

	return err
}

// modeSelectPrepick enables prepick mode using MODE SELECT.
//
// Prepick is a feature where the scanner prepares the next sheet of paper while
// scanning the current one, reducing the time between scans. This function uses
// a vendor-specific mode page (0x33, 0x06) to enable this performance optimization.
func (d *device) modeSelectPrepick(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 15 10 00 00 0c 00 00 00 00 00 00 00    ...............

	// extra request:
	// 000: 00 00 00 00 33 06 00 00 00 00 00 00             ....3.......

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x15, // SCSI opcode: mode select
			0x10, // page format (bit 4): enabled
			// save pages (bit 0): disabled
			0x00, // reserved
			0x00, // reserved
			0x0c, // parameter list length
			0x00, // control
		},
		extra: []byte{
			0x00,
			0x00,
			0x00,
			0x00,
			0x33,
			0x06,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
		},
	})

	return err
}

// setWindow defines the scanning area and image parameters using SET WINDOW.
//
// The SCSI SET WINDOW command (0x24) is mandatory for scanner devices and defines
// one or more scanning windows. Each window descriptor specifies:
//   - Resolution in pixels per inch for both axes (600 dpi = 0x0258)
//   - Upper-left corner coordinates and dimensions in basic measurement units
//   - Image composition (0x05 = color, 0x00 = bi-level, 0x02 = grayscale)
//   - Brightness, threshold, and contrast settings
//   - Compression type (if supported)
//
// The iX500 uses window ID 0x00 for the front side and 0x80 for the back side
// when scanning in duplex mode.
func (d *device) setWindow(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 24 00 00 00 00 00 00 00 88 00 00 00    ...$...........

	// extra request:
	// 000: 00 00 00 00 00 00 00 40 00 00 02 58 02 58 00 00 .......@...X.X..
	// 010: 00 00 00 00 00 00 00 00 26 c0 00 00 36 d0 00 00 ........&...6...
	// 020: 00 05 08 00 00 00 00 00 00 00 00 00 00 00 00 00 ................
	// 030: c1 80 01 00 00 00 00 00 00 00 00 00 00 c0 00 00 ................
	// 040: 26 c3 00 00 36 d1 00 00 80 00 02 58 02 58 00 00 &...6......X.X..
	// 050: 00 00 00 00 00 00 00 00 26 c0 00 00 36 d0 00 00 ........&...6...
	// 060: 00 05 08 00 00 00 00 00 00 00 00 00 00 00 00 00 ................
	// 070: c1 80 01 00 00 00 00 00 00 00 00 00 00 00 00 00 ................
	// 080: 00 00 00 00 00 00 00 00                         ........

	extra := []byte{ // window descriptor, see also https://www.staff.uni-mainz.de/tacke/scsi/SCSI2-15.html#tab282
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00, 0x00, 0x02, 0x58, 0x02, 0x58, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x26, 0xc0, 0x00, 0x00, 0x36, 0xd0, 0x00, 0x00,
		0x00, 0x05, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xc1, 0x80, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xc0, 0x00, 0x00,
		0x26, 0xc3, 0x00, 0x00, 0x36, 0xd1, 0x00, 0x00, 0x80, 0x00, 0x02, 0x58, 0x02, 0x58, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x26, 0xc0, 0x00, 0x00, 0x36, 0xd0, 0x00, 0x00,
		0x00, 0x05, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0xc1, 0x80, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	res := uint16(d.resolution)
	binary.BigEndian.PutUint16(extra[10:12], res) // front X res
	binary.BigEndian.PutUint16(extra[12:14], res) // front Y res
	transferLen := len(extra)
	if d.scanMode == Simplex {
		extra = extra[:0x48]
		transferLen = 0x48
		// Skip back window resolution (no back window sent)
	} else {
		binary.BigEndian.PutUint16(extra[74:76], res) // back X res
		binary.BigEndian.PutUint16(extra[76:78], res) // back Y res
	}

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x24, // SCSI opcode: set window
			0x00, // reserved
			0x00, // reserved
			0x00, // reserved
			0x00, // reserved

			0x00,                   // reserved
			0x00,                   // transfer length (MSB)
			byte(transferLen >> 8), // transfer length
			byte(transferLen),      // transfer length (LSB)
			0x00,                   // control
		},
		extra: extra,
	})

	return err
}

// sendLUT transfers a lookup table (LUT) to the scanner using the SEND command.
//
// The SCSI SEND command (0x2A) transfers data from the host to the scanner.
// The data type code 0x83 indicates a vendor-specific lookup table used for
// gamma correction or other pixel value transformations. The LUT maps input
// pixel values (0-255) to output values, allowing for image enhancement.
// This implementation sends an identity LUT (input = output).
func (d *device) sendLUT(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 2a 00 83 00 00 00 00 01 0a 00 00 00    ...*...........

	// extra request:
	// 000: 00 00 10 00 01 00 01 00 00 00 00 00 01 02 03 04 ................
	// 010: 05 06 07 08 09 0a 0b 0c 0d 0e 0f 10 11 12 13 14 ................
	// 020: 15 16 17 18 19 1a 1b 1c 1d 1e 1f 20 21 22 23 24 ........... !"#$
	// 030: 25 26 27 28 29 2a 2b 2c 2d 2e 2f 30 31 32 33 34 %&'()*+,-./01234
	// 040: 35 36 37 38 39 3a 3b 3c 3d 3e 3f 40 41 42 43 44 56789:;<=>?@ABCD
	// 050: 45 46 47 48 49 4a 4b 4c 4d 4e 4f 50 51 52 53 54 EFGHIJKLMNOPQRST
	// 060: 55 56 57 58 59 5a 5b 5c 5d 5e 5f 60 61 62 63 64 UVWXYZ[\]^_`abcd
	// 070: 65 66 67 68 69 6a 6b 6c 6d 6e 6f 70 71 72 73 74 efghijklmnopqrst
	// 080: 75 76 77 78 79 7a 7b 7c 7d 7e 7f 80 81 82 83 84 uvwxyz{|}~......
	// 090: 85 86 87 88 89 8a 8b 8c 8d 8e 8f 90 91 92 93 94 ................
	// 0a0: 95 96 97 98 99 9a 9b 9c 9d 9e 9f a0 a1 a2 a3 a4 ................
	// 0b0: a5 a6 a7 a8 a9 aa ab ac ad ae af b0 b1 b2 b3 b4 ................
	// 0c0: b5 b6 b7 b8 b9 ba bb bc bd be bf c0 c1 c2 c3 c4 ................
	// 0d0: c5 c6 c7 c8 c9 ca cb cc cd ce cf d0 d1 d2 d3 d4 ................
	// 0e0: d5 d6 d7 d8 d9 da db dc dd de df e0 e1 e2 e3 e4 ................
	// 0f0: e5 e6 e7 e8 e9 ea eb ec ed ee ef f0 f1 f2 f3 f4 ................
	// 100: f5 f6 f7 f8 f9 fa fb fc fd fe                   ..........

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x2a, // SCSI opcode: SEND
			0x00, // reserved
			0x83, // data type code: vendor-specific: lut table
			0x00, // reserved
			0x00, // data type qualifier (MSB)

			0x00, // data type qualifier (LSB)
			0x00, // transfer length (MSB)
			0x01, // transfer length
			0x0a, // transfer length (LSB)
			0x00, // control
		},
		extra: []byte{
			0x00, 0x00, 0x10, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04,
			0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14,
			0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24,
			0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30, 0x31, 0x32, 0x33, 0x34,
			0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f, 0x40, 0x41, 0x42, 0x43, 0x44,
			0x45, 0x46, 0x47, 0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f, 0x50, 0x51, 0x52, 0x53, 0x54,
			0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f, 0x60, 0x61, 0x62, 0x63, 0x64,
			0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f, 0x70, 0x71, 0x72, 0x73, 0x74,
			0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f, 0x80, 0x81, 0x82, 0x83, 0x84,
			0x85, 0x86, 0x87, 0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f, 0x90, 0x91, 0x92, 0x93, 0x94,
			0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f, 0xa0, 0xa1, 0xa2, 0xa3, 0xa4,
			0xa5, 0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xab, 0xac, 0xad, 0xae, 0xaf, 0xb0, 0xb1, 0xb2, 0xb3, 0xb4,
			0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xbb, 0xbc, 0xbd, 0xbe, 0xbf, 0xc0, 0xc1, 0xc2, 0xc3, 0xc4,
			0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xcb, 0xcc, 0xcd, 0xce, 0xcf, 0xd0, 0xd1, 0xd2, 0xd3, 0xd4,
			0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xdb, 0xdc, 0xdd, 0xde, 0xdf, 0xe0, 0xe1, 0xe2, 0xe3, 0xe4,
			0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xeb, 0xec, 0xed, 0xee, 0xef, 0xf0, 0xf1, 0xf2, 0xf3, 0xf4,
			0xf5, 0xf6, 0xf7, 0xf8, 0xf9, 0xfa, 0xfb, 0xfc, 0xfd, 0xfe,
		},
	})

	return err
}

// sendQTable transfers JPEG quantization tables to the scanner using SEND.
//
// Quantization tables are used in JPEG compression to control image quality
// and compression ratio. The data type code 0x88 indicates a vendor-specific
// quantization table. The tables contain the quantization values for the
// luminance and chrominance components of the JPEG encoder.
func (d *device) sendQTable(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 2a 00 88 00 00 00 00 00 8a 00 00 00    ...*...........

	// extra request:
	// 000: 00 00 00 00 00 40 00 40 00 00 04 03 03 04 03 03 .....@.@........
	// 010: 04 04 03 04 05 05 04 05 07 0c 07 07 06 06 07 0e ................
	// 020: 0a 0b 08 0c 11 0f 12 12 11 0f 10 10 13 15 1b 17 ................
	// 030: 13 14 1a 14 10 10 18 20 18 1a 1c 1d 1e 1f 1e 12 ....... ........
	// 040: 17 21 24 21 1e 24 1b 1e 1e 1d 05 05 05 07 06 07 .!$!.$..........
	// 050: 0e 07 07 0e 1d 13 10 13 1d 1d 1d 1d 1d 1d 1d 1d ................
	// 060: 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d ................
	// 070: 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d ................
	// 080: 1d 1d 1d 1d 1d 1d 1d 1d 1d 1d                   ..........

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x2a, // SCSI opcode: SEND
			0x00, // reserved
			0x88, // data type code: vendor-specific: qtable
			0x00, // reserved
			0x00, // data type qualifier (MSB)

			0x00, // data type qualifier (LSB)
			0x00, // transfer length (MSB)
			0x00, // transfer length
			0x8a, // transfer length (MSB)
			0x00, // control
		},
		extra: []byte{
			0x00, 0x00, 0x00, 0x00, 0x00, 0x40, 0x00, 0x40, 0x00, 0x00, 0x04, 0x03, 0x03, 0x04, 0x03, 0x03,
			0x04, 0x04, 0x03, 0x04, 0x05, 0x05, 0x04, 0x05, 0x07, 0x0c, 0x07, 0x07, 0x06, 0x06, 0x07, 0x0e,
			0x0a, 0x0b, 0x08, 0x0c, 0x11, 0x0f, 0x12, 0x12, 0x11, 0x0f, 0x10, 0x10, 0x13, 0x15, 0x1b, 0x17,
			0x13, 0x14, 0x1a, 0x14, 0x10, 0x10, 0x18, 0x20, 0x18, 0x1a, 0x1c, 0x1d, 0x1e, 0x1f, 0x1e, 0x12,
			0x17, 0x21, 0x24, 0x21, 0x1e, 0x24, 0x1b, 0x1e, 0x1e, 0x1d, 0x05, 0x05, 0x05, 0x07, 0x06, 0x07,
			0x0e, 0x07, 0x07, 0x0e, 0x1d, 0x13, 0x10, 0x13, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d,
			0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d,
			0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d,
			0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d, 0x1d,
		},
	})

	return err
}

// lampOn activates the scanner's lamp using the vendor-specific SCANNER_CONTROL command.
//
// The command code 0xf1 is a vendor-specific extension for scanner control functions.
// The scan control function byte 0x05 specifically requests lamp activation.
// The scanner's lamp must be on and warmed up before scanning to ensure proper
// illumination and image quality.
func (d *device) lampOn(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 f1 05 00 00 00 00 00 00 00 00 00 00    ...............

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0xf1, // SCSI opcode: SCANNER_CONTROL
			0x05, // scan control function: lamp on
			0x00,
			0x00,
			0x00,

			0x00,
			0x00,
			0x00,
			0x00,
			0x00,
		},
	})

	return err
}

func (d *device) cancel(ctx context.Context) error {
	_, err := d.do(ctx, &request{
		cmd: []byte{
			0xf1, // SCSI opcode: SCANNER_CONTROL
			0x04, // scan control function: cancel
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
	})
	return err
}

func (d *device) lampOff(ctx context.Context) error {
	_, err := d.do(ctx, &request{
		cmd: []byte{
			0xf1, // SCSI opcode: SCANNER_CONTROL
			0x03, // scan control function: lamp off
			0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		},
	})
	return err
}

// hwStatus represents the scanner's hardware state and sensor readings.
//
// This structure contains status flags retrieved via the vendor-specific
// GET_HW_STATUS command. It includes paper sensor states, button states,
// error conditions, and the detected skew angle of the scanned document.
type hwStatus struct {
	top bool
	a3  bool
	b4  bool
	a4  bool
	b5  bool

	Hopper  bool
	omr     bool
	adfOpen bool

	sleep      bool
	sendSw     bool
	manualFeed bool
	ScanSw     bool

	function byte
	inkEmpty bool

	doubleFeed bool

	errorCode byte

	skewAngle uint16
}

// hardwareStatusFromBytes parses the GET_HW_STATUS response into a hardwareStatus struct.
//
// The response format is vendor-specific. Key fields:
//   - Byte 2: Paper size flags (top, a3, b4, a4, b5)
//   - Byte 3: Hopper, OMR, and ADF cover state
//   - Byte 4: Sleep mode, send switch, manual feed, and scan button state
//   - Byte 5: Function code (lower 4 bits)
//   - Byte 6: Ink status and double-feed detection
//   - Byte 7: Error code
//   - Bytes 8-9: Skew angle (16-bit big-endian)
func hardwareStatusFromBytes(b []byte) hwStatus {
	return hwStatus{
		top: (b[2]>>7)&1 == 1,
		a3:  (b[2]>>3)&1 == 1,
		b4:  (b[2]>>2)&1 == 1,
		a4:  (b[2]>>1)&1 == 1,
		b5:  (b[2]>>0)&1 == 1,

		Hopper:  (b[3]>>7)&1 == 1,
		omr:     (b[3]>>6)&1 == 1,
		adfOpen: (b[3]>>5)&1 == 1,

		sleep:      (b[4]>>7)&1 == 1,
		sendSw:     (b[4]>>2)&1 == 1,
		manualFeed: (b[4]>>1)&1 == 1,
		ScanSw:     (b[4]>>0)&1 == 1,

		function: (b[5] >> 0) & 0xf,
		inkEmpty: (b[6]>>7)&1 == 1,

		doubleFeed: (b[6]>>0)&1 == 1,

		errorCode: b[7],

		skewAngle: (uint16(b[8]) << 8) | uint16(b[9]),
	}
}

// hardwareStatus retrieves the scanner's hardware status using a vendor-specific command.
//
// The GET_HW_STATUS command (0xc2) is a Fujitsu-specific extension that returns
// hardware sensor states including:
//   - Paper presence in hopper (Hopper flag)
//   - Scan button state (ScanSw flag)
//   - ADF cover state (adfOpen flag)
//   - Error conditions (errorCode)
//   - Document skew angle (skewAngle)
//
// This is used to poll for button presses and verify the scanner is ready for operation.
func (d *device) hardwareStatus(ctx context.Context) (hwStatus, error) {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 c2 00 00 00 00 00 00 00 0c 00 00 00    ...............

	// extra request:
	// 000: 00 00 00 00 00 01 00 00 00 00 00 00             ............

	resp, err := d.do(ctx, &request{
		cmd: []byte{
			0xc2, // SCSI opcode: GET_HW_STATUS
			0x00,
			0x00,
			0x00,
			0x00,

			0x00,
			0x00,
			0x00,
			0x0c,
			0x00,
		},
		respLen: 12,
	})
	if err != nil {
		return hwStatus{}, err
	}
	return hardwareStatusFromBytes(resp.extra), nil
}

// objectPosition loads paper from the hopper using the OBJECT POSITION command.
//
// The SCSI OBJECT POSITION command (0x31) is an optional scanner command that
// controls document positioning. The position type byte 0x01 requests "load object",
// which causes the scanner to feed one sheet from the automatic document feeder (ADF)
// into the scanning position. This command returns ErrHopperEmpty when no more
// paper is available in the hopper.
func (d *device) objectPosition(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 31 01 00 00 00 00 00 00 00 00 00 00    ...1...........

	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x31, // SCSI opcode: OBJECT POSITION
			0x01, // load object
			0x00, // count (MSB)
			0x00, // count
			0x00, // count (LSB)

			0x00, // reserved
			0x00, // reserved
			0x00, // reserved
			0x00, // reserved
			0x00, // control
		},
	})
	return err
}

// startScan initiates the scanning operation using the SCAN command.
//
// The SCSI SCAN command (0x1B) is an optional scanner command that begins
// the scanning process for the previously defined windows. The transfer length
// field indicates the number of window IDs being specified. This implementation
// scans both front (0x00) and back (0x80) windows for duplex scanning.
func (d *device) startScan(ctx context.Context) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 1b 00 00 00 02 00 00 00 00 00 00 00    ...............

	windowIDs := []byte{0x00, 0x80}
	if d.scanMode == Simplex {
		windowIDs = []byte{0x00}
	}
	_, err := d.do(ctx, &request{
		cmd: []byte{
			0x1b,                 // SCSI opcode: SCAN
			0x00,                 // reserved
			0x00,                 // reserved
			0x00,                 // reserved
			byte(len(windowIDs)), // transfer length
			0x00,                 // control
		},
		extra: windowIDs,
	})
	return err
}

// pixelSize retrieves the scanned image dimensions using a vendor-specific READ.
//
// This function uses the READ command (0x28) with data type code 0x80 (vendor-specific)
// to query the actual pixel dimensions of the current scan. The response contains:
//   - scan_x: width in pixels (4960 for 600 dpi)
//   - scan_y: height in scan lines (7016 maximum)
//   - paper_w: paper width detected
//   - paper_l: paper length detected
//
// These values determine the number of bytes per scan line (width × 3 for RGB)
// and the total image size.
func (d *device) pixelSize(ctx context.Context) (width, height int, err error) {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 28 00 80 00 00 00 00 00 20 00 00 00    ...(....... ...

	// response extra:
	// 000: 00 00 13 60 00 00 1b 68 00 00 13 60 00 00 1b 68 ...`...h...`...h
	// 010: 00 00 00 00 00 00 00 00 13 60 00 00 1b 68 00 00 .........`...h..

	// first 4 bytes: scan_x (e.g. 4960 at 600 dpi)
	// next 4 bytes:  scan_y (e.g. 7016 at 600 dpi)
	// next 4 bytes:  paper_w
	// next 4 bytes:  paper_l

	resp, err := d.do(ctx, &request{
		cmd: []byte{
			0x28, // SCSI opcode: READ
			0x00, // reserved
			0x80, // data type: vendor-specific
			0x00, // reserved
			0x00, // data type qualifier (MSB)

			0x00, // data type qualifier (LSB)
			0x00, // transfer length (MSB)
			0x00, // transfer length
			0x20, // transfer length (LSB)
			0x00, // control
		},
		respLen: 32,
	})
	if err != nil {
		return width, height, err
	}

	width = int(binary.BigEndian.Uint32(resp.extra[0:4]))
	height = int(binary.BigEndian.Uint32(resp.extra[4:8]))
	width -= width % 2 // ppl_mod=2 alignment: iX500 color mode requires even pixel width
	return width, height, nil
}

// checkImageReady issues a single SCANNER_CONTROL command (0xf1, function 0x10) to check
// whether image data is ready for the specified window (0x00 for front, 0x80 for back).
// It returns nil if data is available, or an error otherwise. Retry policy is the
// caller's responsibility.
func (d *device) checkImageReady(ctx context.Context, side int, chunkSize int) error {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 f1 10 00 00 00 00 03 dc 20 00 00 00    ........... ...

	windowID := byte(0x00)
	if side == 1 {
		windowID = 0x80
	}
	_, err := d.do(ctx, &request{
		cmd: []byte{
			0xf1, // SCSI opcode: SCANNER_CONTROL
			0x10,
			windowID, // window id: front (back is 0x80)
			0x00,
			0x00,
			0x00,
			byte(chunkSize >> 16),
			byte(chunkSize >> 8),
			byte(chunkSize),
			0x00,
		},
	})
	return err
}

// readData retrieves scanned image data using the READ command.
//
// The SCSI READ command (0x28) is mandatory for scanner devices and transfers
// image data from the scanner to the host. The data type code 0x00 indicates
// image data, and the data type qualifier (window ID) specifies which scanning
// window to read from: 0x00 for front side, 0x80 for back side.
//
// The transfer length specifies how many bytes to read (252,960 bytes = 252KB chunk).
// Multiple READ commands may be necessary to retrieve a complete image. The function
// inverts the pixel values (255 - value) as the scanner returns inverted data.
func (d *device) readData(ctx context.Context, side int, chunkSize int) (*response, error) {
	// request:
	// 000: 43 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 C...............
	// 010: 00 00 00 28 00 00 00 00 00 03 dc 20 00 00 00    ...(....... ...

	windowID := byte(0x00)
	if side == 1 {
		windowID = 0x80
	}

	resp, err := d.do(ctx, &request{
		cmd: []byte{
			0x28,                  // SCSI opcode: READ
			0x00,                  // reserved
			0x00,                  // data type code: image
			0x00,                  // reserved
			0x00,                  // data type qualifier (MSB)
			windowID,              // data type qualifier (LSB): window id
			byte(chunkSize >> 16), // transfer length (MSB)
			byte(chunkSize >> 8),  // transfer length
			byte(chunkSize),       // transfer length (LSB)
			0x00,                  // control
		},
		respLen: chunkSize,
	})
	if err != nil {
		return resp, err
	}

	for i := 0; i < len(resp.extra); i++ {
		resp.extra[i] = 255 - resp.extra[i] // invert
	}

	return resp, err
}
