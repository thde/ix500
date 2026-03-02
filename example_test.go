package ix500_test

import (
	"context"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"

	"thde.io/ix500"
)

// ExampleScanner_Scan demonstrates a complete scan session: find the device,
// initialize, wait for the button, then stream all pages to JPEG files.
func ExampleScanner_Scan() {
	ctx := context.Background()

	dev, err := ix500.FindDevice()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scanner not found:", err)
		return
	}

	scn := ix500.New(dev, &ix500.Options{
		Resolution: ix500.DPI300,
		ScanMode:   ix500.Duplex,
	})
	defer scn.Close()

	if err := scn.Initialize(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "initialization failed:", err)
		return
	}

	if err := scn.WaitForButton(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "button wait failed:", err)
		return
	}

	outDir := os.TempDir()
	pageNum := 0
	for page, err := range scn.Scan(ctx) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "scan error:", err)
			return
		}

		path := filepath.Join(outDir, fmt.Sprintf("page-%03d.jpg", pageNum))
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "create file:", err)
			return
		}
		if encErr := jpeg.Encode(f, page, &jpeg.Options{Quality: 75}); encErr != nil {
			_ = f.Close()
			fmt.Fprintln(os.Stderr, "encode:", encErr)
			return
		}
		_ = f.Close()

		bounds := page.Bounds()
		fmt.Printf("saved %s (%dx%d, sheet=%d, side=%d)\n",
			filepath.Base(path), bounds.Dx(), bounds.Dy(), page.Sheet, page.Side)
		pageNum++
	}

	fmt.Printf("scan complete, %d pages\n", pageNum)
}

// ExampleNew shows how to configure a Scanner with custom options.
func ExampleNew() {
	dev, err := ix500.FindDevice()
	if err != nil {
		fmt.Fprintln(os.Stderr, "scanner not found:", err)
		return
	}

	// Use 600 DPI simplex (front-only) scanning.
	scn := ix500.New(dev, &ix500.Options{
		Resolution: ix500.DPI600,
		ScanMode:   ix500.Simplex,
	})
	defer scn.Close()
}
