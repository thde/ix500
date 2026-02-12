package ix500

import (
	"errors"
	"fmt"
)

// senseCode represents a SCSI error classification using ASC and ASCQ.
//
// In the SCSI REQUEST SENSE response, the Additional Sense Code (ASC) and
// Additional Sense Code Qualifier (ASCQ) provide detailed information about
// error conditions. Together they form a hierarchical error code where ASC
// identifies the error category and ASCQ provides specific details within
// that category.
type senseCode struct {
	code      byte // additional sense code (asc)
	qualifier byte // additional sense code qualifier (ascq)
}

// SCSI sense key values define the general category of error conditions.
// These are standard values defined in the SCSI-2 specification.
const (
	noSense         byte = iota // No error or special condition
	recoveredError              // Error recovered by device (data valid)
	notReady                    // Device is not ready for operation
	mediumError                 // Medium-related error (paper jam, etc.)
	hardwareError               // Hardware malfunction detected
	illegalRequest              // Invalid command or parameter
	unitAttention               // Device state changed (reset, power event)
	dataProtect                 // Write-protected or access denied
	firmwareError               // Firmware or vendor-specific error
	_                           // Reserved
	abortedCommand              // Command was aborted
	equal                       // SEARCH DATA satisfied (not used in scanners)
	volumeOverflow              // Buffer overflow condition
	miscompare                  // Verify operation found mismatch
)

// requestSenseToError converts SCSI REQUEST SENSE response data into a Go error.
//
// This function interprets the three-level SCSI error hierarchy:
//  1. Sense Key - general error category (e.g., mediumError, hardwareError)
//  2. ASC (Additional Sense Code) - specific error type
//  3. ASCQ (Additional Sense Code Qualifier) - detailed error variant
//
// Additional parameters provide context:
//   - rsInfo: Information bytes (typically the residue count for short reads)
//   - rsEom: End of Medium flag, set when reaching end of paper
//   - rsIli: Incorrect Length Indicator, set when actual transfer length differs from requested
//
// The function maps Fujitsu-specific vendor codes (ASC 0x80) to meaningful errors
// like paper jams, hopper empty, and button presses. It returns nil for successful
// operations (noSense with ASC 0x00 or 0x80).
func requestSenseToError(sense byte, asc byte, ascq byte, rsInfo []byte, rsEom bool, rsIli bool) error {
	switch sense {
	case noSense:
		if asc == 0x80 { // why 0x80?
			return nil // no sense
		}
		if asc != 0x00 {
			return errors.New("unknown asc")
		}
		if ascq != 0x00 {
			return errors.New("unknown ascq")
		}
		if rsEom {
			return ErrEndOfPaper
		}
		if rsIli {
			return ErrShortRead
		}
		return nil // ready

	case notReady:
		if asc != 0x00 {
			return errors.New("unknown asc")
		}
		if ascq != 0x00 {
			return errors.New("unknown ascq")
		}
		return errors.New("busy")

	case mediumError:
		errorByCode := map[senseCode]error{
			{0x80, 0x1}:  errors.New("paper jam"),
			{0x80, 0x2}:  errors.New("cover open"),
			{0x80, 0x3}:  ErrHopperEmpty,
			{0x80, 0x4}:  errors.New("unusual paper"),
			{0x80, 0x7}:  errors.New("double feed"),
			{0x80, 0x8}:  errors.New("ADF setup error"),
			{0x80, 0x9}:  errors.New("carrier sheet"),
			{0x80, 0x10}: errors.New("no ink cartridge"),
			{0x80, 0x13}: ErrTemporaryNoData,
			{0x80, 0x14}: errors.New("endorser error"),
			{0x80, 0x20}: errors.New("stop button"),
			{0x80, 0x22}: errors.New("scanning halted"),
			{0x80, 0x30}: errors.New("not enough paper"),
			{0x80, 0x31}: errors.New("scanning disabled"),
			{0x80, 0x32}: errors.New("scanning paused"),
			{0x80, 0x33}: errors.New("WiFi control error"),
		}
		if err, ok := errorByCode[senseCode{asc, ascq}]; ok {
			return err
		}

	case hardwareError:
		errorByCode := map[senseCode]error{
			{0x44, 0x00}: errors.New("EEPROM error"),
			{0x80, 0x1}:  errors.New("FB motor fuse"),
			{0x80, 0x2}:  errors.New("heater fuse"),
			{0x80, 0x3}:  errors.New("lamp fuse"),
			{0x80, 0x4}:  errors.New("ADF motor fuse"),
			{0x80, 0x5}:  errors.New("mechanical error"),
			{0x80, 0x6}:  errors.New("optical error"),
			{0x80, 0x7}:  errors.New("fan error"),
			{0x80, 0x8}:  errors.New("IPC option error"),
			{0x80, 0x10}: errors.New("endorser error"),
			{0x80, 0x11}: errors.New("endorser fuse"),
			{0x80, 0x80}: errors.New("interface board timeout"),
			{0x80, 0x81}: errors.New("interface board error 1"),
			{0x80, 0x82}: errors.New("interface board error 2"),
		}
		if err, ok := errorByCode[senseCode{asc, ascq}]; ok {
			return err
		}

	case illegalRequest:
		errorByCode := map[senseCode]error{
			{0x00, 0x00}: errors.New("paper edge detected too soon"),
			{0x1a, 0x00}: errors.New("parameter list error"),
			{0x20, 0x00}: errors.New("invalid command"),
			{0x24, 0x00}: errors.New("invalid CDB field"),
			{0x25, 0x00}: errors.New("unsupported logical unit"),
			{0x26, 0x00}: errors.New("invalid field in parm list"),
			{0x2c, 0x00}: errors.New("command sequence error"),
			{0x2c, 0x02}: errors.New("wrong window combination"),
		}
		if err, ok := errorByCode[senseCode{asc, ascq}]; ok {
			return err
		}

	case unitAttention:
		errorByCode := map[senseCode]error{
			{0x00, 0x00}: errors.New("device reset"),
			{0x80, 0x01}: errors.New("power saving"),
		}
		if err, ok := errorByCode[senseCode{asc, ascq}]; ok {
			return err
		}

	case abortedCommand:
		errorByCode := map[senseCode]error{
			{0x43, 0x00}: errors.New("message error"),
			{0x45, 0x00}: errors.New("select failure"),
			{0x47, 0x00}: errors.New("SCSI parity error"),
			{0x48, 0x00}: errors.New("initiator error message"),
			{0x4e, 0x00}: errors.New("overlapped commands"),
			{0x80, 0x01}: errors.New("image transfer error"),
			{0x80, 0x03}: errors.New("JPEG overflow error"),
		}
		if err, ok := errorByCode[senseCode{asc, ascq}]; ok {
			return err
		}
	}

	return fmt.Errorf("unknown code: sense code %d (0x%x), ASC %d (0x%x), ASCQ %d (0x%x)", sense, sense, asc, asc, ascq, ascq)
}
