package ix500

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/unix"
)

// usbdevfsBulkTransfer represents the Linux kernel's usbdevfs_bulktransfer structure
// used for USB bulk transfers via ioctl.
//
// This structure mirrors the kernel's definition and should eventually be contributed
// to golang.org/x/sys/unix to avoid duplication across projects.
type usbdevfsBulkTransfer struct {
	// Endpoint is the USB endpoint address (bit 7 set = IN, clear = OUT).
	Endpoint uint32
	// Len is the number of bytes to transfer; on return, contains bytes actually transferred.
	Len uint32
	// Timeout specifies the transfer timeout in milliseconds.
	Timeout uint32
	// 4 bytes of padding for 64-bit pointer alignment.
	_ [4]byte
	// Data points to the buffer for the transfer.
	Data *byte
}

// Linux usbdevfs ioctl request codes.
// These values are defined in the kernel's include/uapi/linux/usbdevice_fs.h.
const (
	// usbDevFSBulk performs a bulk transfer on the specified endpoint.
	usbDevFSBulk = 0xc0185502
	// usbDevFSClaimInterface claims an interface for exclusive access.
	usbDevFSClaimInterface = 0x8004550f
	// usbDevFSReleaseInterface releases a previously claimed interface.
	usbDevFSReleaseInterface = 0x80045510
)

// usbDevicesRoot is the sysfs directory containing USB device information.
const usbDevicesRoot = "/sys/bus/usb/devices"

// USB identifiers and endpoint addresses for the Fujitsu ScanSnap iX500.
const (
	// product is the USB product ID for the ScanSnap iX500 (0x132b).
	product = "132b"
	// vendor is Fujitsu's USB vendor ID (0x04c5).
	vendor = "04c5"
	// deviceToHost is the USB IN endpoint address for bulk transfers from device to host.
	// Endpoint 1, IN direction (0x81 = 0x01 | 0x80).
	deviceToHost = 129
	// hostToDevice is the USB OUT endpoint address for bulk transfers from host to device.
	// Endpoint 2, OUT direction (0x02).
	hostToDevice = 2
)

// Device represents a USB connection to a Fujitsu ScanSnap iX500 scanner on Linux.
//
// It uses the Linux kernel's usbdevfs interface for USB bulk transfers and sysfs
// for device enumeration. The device interface must be claimed before use and
// released via Close when finished.
type Device struct {
	// name is the device identifier within /sys/bus/usb/devices (e.g., "1-1.2").
	name string
	// devName is the device node name within /dev (e.g., "bus/usb/001/005").
	devName string
	// f is the open file descriptor for the USB device node.
	f *os.File
}

// newDevice creates and initializes a Device for the USB device with the given sysfs name.
//
// The function:
//  1. Reads the uevent file to determine the /dev path.
//  2. Opens the USB device file.
//  3. Claims interface 0 for exclusive access.
//
// The iX500 uses interface 0 for all scanner operations.
func newDevice(name string) (*Device, error) {
	dev := &Device{name: name}

	// Read DEVNAME= from uevent to locate the device within /dev.
	uevent, err := os.ReadFile(dev.sysPath("uevent"))
	if err != nil {
		return nil, err
	}
	for line := range strings.SplitSeq(string(uevent), "\n") {
		if after, found := strings.CutPrefix(line, "DEVNAME="); found {
			dev.devName = after
		}
	}
	if dev.devName == "" {
		return nil, fmt.Errorf("%q unexpectedly did not contain a DEVNAME= line", dev.sysPath("uevent"))
	}

	dev.f, err = os.OpenFile(filepath.Join("/dev", dev.devName), os.O_RDWR, 0o664)
	if err != nil {
		return nil, err
	}

	// Claim interface 0. The iX500 uses only this interface for scanner operations.
	var interfaceNumber uint32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(dev.f.Fd()), usbDevFSClaimInterface, uintptr(unsafe.Pointer(&interfaceNumber))); errno != 0 {
		return nil, errno
	}

	return dev, nil
}

// sysPath returns the full path to a file within the device's sysfs directory.
func (d *Device) sysPath(filename string) string {
	return filepath.Join(usbDevicesRoot, d.name, filename)
}

// Read implements io.Reader by performing a USB bulk IN transfer.
//
// It transfers up to len(p) bytes from the scanner to the host via the IN endpoint
// (endpoint 1). The transfer uses a 3-second timeout and blocks until data is
// available or the timeout expires. The number of bytes actually received is returned.
func (d *Device) Read(p []byte) (n int, err error) {
	bulk := usbdevfsBulkTransfer{
		Endpoint: deviceToHost,
		Len:      uint32(len(p)),
		Timeout:  uint32((3 * time.Second) / time.Millisecond),
		Data:     &(p[0]),
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(d.f.Fd()), usbDevFSBulk, uintptr(unsafe.Pointer(&bulk))); errno != 0 {
		return 0, errno
	}
	return int(bulk.Len), nil
}

// Write implements io.Writer by performing a USB bulk OUT transfer.
//
// It transfers all of p from the host to the scanner via the OUT endpoint
// (endpoint 2). The transfer uses a 3-second timeout and blocks until all data
// is sent or the timeout expires. Returns the number of bytes written.
func (d *Device) Write(p []byte) (n int, err error) {
	bulk := usbdevfsBulkTransfer{
		Endpoint: hostToDevice,
		Len:      uint32(len(p)),
		Timeout:  uint32((3 * time.Second) / time.Millisecond),
		Data:     &(p[0]),
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(d.f.Fd()), usbDevFSBulk, uintptr(unsafe.Pointer(&bulk))); errno != 0 {
		return 0, errno
	}
	return len(p), nil
}

// Close releases the USB interface and closes the device file.
//
// After calling Close, the Device must not be used. This releases interface 0
// (the only interface used by the iX500) and closes the underlying file descriptor.
func (d *Device) Close() error {
	// Release interface 0. The iX500 uses only this interface.
	var interfaceNumber uint32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(d.f.Fd()), usbDevFSReleaseInterface, uintptr(unsafe.Pointer(&interfaceNumber))); errno != 0 {
		return errno
	}

	return d.f.Close()
}

// isInvalidDeviceName returns true if the name does not represent a USB device.
//
// The Linux USB subsystem creates sysfs entries for host controllers, interfaces,
// and other entities in addition to actual devices. Valid USB device names follow
// the pattern: <bus>-<port>[.<port>...] (e.g., "1-1.2", "2-4"), consisting only
// of digits, dots, and dashes, and always starting with a digit.
func isInvalidDeviceName(name string) bool {
	if name == "" {
		return true
	}

	r, _ := utf8.DecodeRuneInString(name)
	if !unicode.IsDigit(r) {
		return true
	}

	for _, r := range name {
		if r != '.' && r != '-' && !unicode.IsDigit(r) {
			return true
		}
	}

	return false
}

// FindDevice locates and opens a connected Fujitsu ScanSnap iX500 scanner.
//
// It searches /sys/bus/usb/devices for a device matching the iX500's vendor ID
// (0x04c5) and product ID (0x132b). If found, it opens the device, claims the
// scanner interface, and returns a ready-to-use Device. Returns an error if
// the scanner is not connected or cannot be accessed.
func FindDevice() (*Device, error) {
	f, err := os.Open(usbDevicesRoot)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}

	for _, dev := range names {
		if isInvalidDeviceName(dev) {
			continue
		}
		idProduct, err := os.ReadFile(filepath.Join(usbDevicesRoot, dev, "idProduct"))
		if err != nil {
			return nil, err
		}
		idVendor, err := os.ReadFile(filepath.Join(usbDevicesRoot, dev, "idVendor"))
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(idProduct)) == product &&
			strings.TrimSpace(string(idVendor)) == vendor {
			return newDevice(dev)
		}
	}
	return nil, fmt.Errorf("device with product==%q, vendor==%q not found", product, vendor)
}
