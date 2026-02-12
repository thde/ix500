package ix500

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"time"
)

// Page represents one scanned side with its image data.
// Embeds image.Image for direct use as an image.
type Page struct {
	image.Image     // The scanned image (embedded)
	Side        int // 0=front, 1=back
	Sheet       int // Sheet number (for reordering)
}

// streamingReader wraps the readSide logic to return an io.Reader
// that streams RGB data chunks as they're received.
type streamingReader struct {
	ctx      context.Context
	scanner  *Scanner
	side     int
	chunks   [][]byte
	chunkIdx int
	offset   int
}

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
				r.chunkIdx++
				r.offset = 0
			}
			return toCopy, nil
		}

		// Need more data - read next chunk
		if r.ctx.Err() != nil {
			return 0, r.ctx.Err()
		}

		// Ric with retry
		var ricErr error
		for i := 0; i < r.scanner.opts.RicRetries; i++ {
			if r.ctx.Err() != nil {
				return 0, r.ctx.Err()
			}
			ricErr = Ric(r.scanner.dev, r.side)
			if ricErr == nil {
				break
			}
			time.Sleep(r.scanner.opts.DataPollInterval)
		}
		if ricErr != nil {
			return 0, fmt.Errorf("ric: %w", ricErr)
		}

		// Read data
		resp, err := ReadData(r.scanner.dev, r.side)
		if errors.Is(err, ErrTemporaryNoData) {
			time.Sleep(r.scanner.opts.DataPollInterval)
			continue
		}
		if errors.Is(err, ErrEndOfPaper) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, fmt.Errorf("read data: %w", err)
		}

		if len(resp.Extra) > 0 {
			r.chunks = append(r.chunks, resp.Extra)
		}
	}
}

// imageFromReader creates an image from streaming RGB data.
// Reads data incrementally, building the image row by row.
func imageFromReader(r io.Reader) (image.Image, error) {
	const (
		width        = 4960
		height       = 7016
		bytesPerLine = width * 3
	)

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
				for x := 0; x < n/3; x++ {
					offset := x * 3
					img.Set(x, y, color.RGBA{
						R: rowBuf[offset],
						G: rowBuf[offset+1],
						B: rowBuf[offset+2],
						A: 255,
					})
				}
				y++
			}
			break
		}
		if err != nil {
			return nil, err
		}

		// Convert row to image pixels
		for x := range width {
			offset := x * 3
			img.Set(x, y, color.RGBA{
				R: rowBuf[offset],
				G: rowBuf[offset+1],
				B: rowBuf[offset+2],
				A: 255,
			})
		}
		y++
	}

	// Crop to actual height
	if y < height {
		cropped := image.NewRGBA(image.Rect(0, 0, width, y))
		for cy := 0; cy < y; cy++ {
			for cx := range width {
				cropped.Set(cx, cy, img.At(cx, cy))
			}
		}
		return cropped, nil
	}

	return img, nil
}
