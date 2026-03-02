# ix500

[![Go Reference](https://pkg.go.dev/badge/thde.io/ix500.svg)](https://pkg.go.dev/thde.io/ix500) [![test](https://github.com/thde/ix500/actions/workflows/test.yml/badge.svg)](https://github.com/thde/ix500/actions/workflows/test.yml) [![Go Report Card](https://goreportcard.com/badge/thde.io/ix500)](https://goreportcard.com/report/thde.io/ix500)

A Linux driver and scan daemon for the Fujitsu ScanSnap iX500 document scanner based on the [stapelberg/scan2drive](https://github.com/stapelberg/scan2drive) implementation. Supports custom DPI, simplex/duplex scanning.

Implements the SCSI-2 scanner command set over Linux's `usbdevfs` bulk transfer interface. No SANE, libusb, or kernel module required.

## Requirements

- Linux
- Fujitsu ScanSnap iX500
- Read/write access to the USB device node (see [Permissions](#permissions))

## Library

The `thde.io/ix500` package can be used independently:

```go
dev, err := ix500.FindDevice()
scn := ix500.New(dev, &ix500.Options{
    Resolution: ix500.DPI300,
    ScanMode:   ix500.Duplex,
})
defer scn.Close()

_ = scn.Initialize(ctx)
_ = scn.WaitForButton(ctx)

for page, err := range scn.Scan(ctx) {
    // page.Image implements image.Image
    // page.Sheet, page.Side identify position
}
```

Check out the CLI tool [`ix500`](./cmd/ix500/ix500.go) for an example of how to use the library.

## CLI

### Install

```sh
go install thde.io/ix500/cmd/ix500@latest
```

### Usage

```
ix500 --help
```

The daemon waits for the scan button to be pressed, scans all pages from the ADF, and writes them as JPEG files named `scan-<timestamp>-page-<NNN>.jpg`. Pages are yielded in hardware scan order (last sheet first); use `Page.Sheet` and `Page.Side` to reorder if needed.

### Permissions

The daemon opens the USB device node directly (e.g. `/dev/bus/usb/001/005`). To run without root, add a udev rule:

```
SUBSYSTEM=="usb", ATTRS{idVendor}=="04c5", ATTRS{idProduct}=="132b", MODE="0664", GROUP="scanner"
```

## References

- [SCSI-2 Scanner Commands](https://www.staff.uni-mainz.de/tacke/scsi/SCSI2-15.html)
