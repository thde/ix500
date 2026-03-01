package ix500

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"time"
)

// Side represents the side of a sheet.
type Side int

const (
	// FrontSide indicates the front side of the sheet.
	Front Side = iota
	// BackSide indicates the back side of the sheet.
	Back
)

// Page represents a single scanned side of a document.
// It embeds [image.Image], allowing it to be used directly as an image.
type Page struct {
	image.Image
	// Side indicates which side of the sheet this page represents.
	// 0 usually denotes the front side, and 1 denotes the back side.
	Side Side
	// Sheet indicates the index of the sheet in the scanning sequence.
	// Note that scanners may scan the last sheet first.
	Sheet int
}

// streamingReader implements [io.Reader] for scanner image data retrieval.
//
// The iX500 returns image data in chunks via the READ command. This reader
// abstracts the chunk-based retrieval into a continuous stream suitable for
// progressive image decoding. It handles:
//   - Issuing ric (read image check) commands to verify data availability
//   - Executing READ commands to retrieve data chunks
//   - Retrying on ErrTemporaryNoData when the scanner needs more time
//   - Detecting ErrEndOfPaper to signal EOF
//
// The scanner processes and transfers data asynchronously, so reads may block
// waiting for the next chunk to become available.
type streamingReader struct {
	ctx              context.Context
	dev              *device
	side             int
	dataPollInterval time.Duration
	ricRetries       int
	chunkSize        int
	chunks           [][]byte
	chunkIdx         int
	offset           int
}

// Read implements [io.Reader] for streaming image data from the scanner.
//
// This method returns data from previously fetched chunks or, when exhausted,
// issues ric (read image check) and readData commands to retrieve the next chunk.
// It retries automatically when the scanner returns ErrTemporaryNoData and
// returns io.EOF when the scanner signals ErrEndOfPaper.
func (r *streamingReader) Read(p []byte) (n int, err error) {
	for {
		// If we have a current chunk, read from it
		if r.chunkIdx < len(r.chunks) {
			chunk := r.chunks[r.chunkIdx]
			remaining := len(chunk) - r.offset
			toCopy := min(len(p), remaining)
			copy(p, chunk[r.offset:r.offset+toCopy])
			r.offset += toCopy
			if r.offset >= len(chunk) {
				r.chunks[r.chunkIdx] = nil // allow GC
				r.chunkIdx++
				r.offset = 0
			}
			return toCopy, nil
		}

		// Need more data - read next chunk
		if r.ctx.Err() != nil {
			return 0, r.ctx.Err()
		}

		// Check image ready with retry
		var ricErr error
		for i := 0; i < r.ricRetries; i++ {
			if r.ctx.Err() != nil {
				return 0, r.ctx.Err()
			}
			ricErr = r.dev.checkImageReady(r.ctx, r.side, r.chunkSize)
			if ricErr == nil {
				break
			}
			select {
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			case <-time.After(r.dataPollInterval):
			}
		}
		if ricErr != nil {
			return 0, fmt.Errorf("check image ready: %w", ricErr)
		}

		// Read data
		resp, err := r.dev.readData(r.ctx, r.side, r.chunkSize)
		if errors.Is(err, ErrTemporaryNoData) {
			select {
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			case <-time.After(r.dataPollInterval):
			}
			continue
		}
		if errors.Is(err, ErrEndOfPaper) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, fmt.Errorf("read data: %w", err)
		}

		if len(resp.extra) > 0 {
			r.chunks = append(r.chunks, resp.extra)
		}
	}
}

// imageFromReader decodes raw RGB image data into an image.Image.
//
// The width and height are provided by the scanner via pixelSize(). Format is
// 24-bit RGB, 3 bytes per pixel. The function reads row-by-row, constructing an
// RGBA image. It handles variable document heights by detecting EOF and cropping
// the final image to the actual number of scan lines received.
func imageFromReader(r io.Reader, width, height int) (image.Image, error) {
	bytesPerLine := width * 3

	// We don't know final height until we've read all data
	// Allocate for max height, then adjust
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	rowBuf := make([]byte, bytesPerLine)
	y := 0

	for y < height {
		// Read one row
		n, err := io.ReadFull(r, rowBuf)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			// End of data - actual height is y
			if n > 0 {
				// Process partial row
				pixOffset := y * img.Stride
				for x := 0; x < n/3; x++ {
					srcOff := x * 3
					dstOff := pixOffset + x*4
					img.Pix[dstOff] = rowBuf[srcOff]
					img.Pix[dstOff+1] = rowBuf[srcOff+1]
					img.Pix[dstOff+2] = rowBuf[srcOff+2]
					img.Pix[dstOff+3] = 255
				}
				y++
			}
			break
		}
		if err != nil {
			return nil, err
		}

		// Convert row to image pixels
		pixOffset := y * img.Stride
		for x := range width {
			srcOff := x * 3
			dstOff := pixOffset + x*4
			img.Pix[dstOff] = rowBuf[srcOff]
			img.Pix[dstOff+1] = rowBuf[srcOff+1]
			img.Pix[dstOff+2] = rowBuf[srcOff+2]
			img.Pix[dstOff+3] = 255
		}
		y++
	}

	// Crop to actual height
	if y < height {
		cropped := image.NewRGBA(image.Rect(0, 0, width, y))
		rowBytes := width * 4
		for cy := range y {
			copy(cropped.Pix[cy*cropped.Stride:cy*cropped.Stride+rowBytes],
				img.Pix[cy*img.Stride:cy*img.Stride+rowBytes])
		}
		return cropped, nil
	}

	return img, nil
}
