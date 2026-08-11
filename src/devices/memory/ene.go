package memory

// Package: memory
// File: ene.go
// Description: Support for ENE-protocol DRAM RGB controllers, the ASUS
// Aura-compatible reference design licensed by several third-party memory
// vendors. Confirmed against ADATA XPG Lancer and TeamGroup T-Force Delta
// DDR5 modules, both reporting device string "AUDA0-E6K5-0101". This is a
// different SMBus address range and wire protocol than the Corsair DDR5
// support elsewhere in this package.
// Author: Matthew Thiel
// License: GPL-3.0 or later

import (
	"OpenLinkHub/src/logger"
	"OpenLinkHub/src/smbus"
	"os"
	"strings"
)

const protocolEne = "ene"

const (
	eneRegDeviceName     uint16 = 0x1000
	eneRegMicronCheck    uint16 = 0x1030
	eneRegConfigLedCount uint16 = 0x1C02
	eneRegColorsDirect   uint16 = 0x8000
	eneRegColorsDirectV2 uint16 = 0x8100
	eneRegDirect         uint16 = 0x8020
	eneRegApply          uint16 = 0x80A0

	eneApplyVal byte = 0x01
)

// eneVersionDirectRegister maps the 16-byte device identity string read
// from ENE_REG_DEVICE_NAME to the base register used for direct-mode LED
// colors. Versions not listed here fall back to the first-generation
// register, matching OpenRGB's own default for unrecognized strings.
var eneVersionDirectRegister = map[string]uint16{
	"LED-0116":        eneRegColorsDirect,
	"DIMM_LED-0102":   eneRegColorsDirect,
	"AUDA0-E6K5-0101": eneRegColorsDirectV2,
	"AUMA0-E6K5-0106": eneRegColorsDirectV2,
	"AUMA0-E6K5-0105": eneRegColorsDirectV2,
	"AUMA0-E6K5-0104": eneRegColorsDirectV2,
}

// eneRamAddresses are the SMBus addresses ENE DRAM RGB controllers answer
// on once individually addressed. On the hardware this was validated
// against, modules already answer uniquely here at boot; the "shared 0x77,
// remap to a free slot" negotiation some platforms require before modules
// are individually addressable has not been exercised against real
// hardware and is intentionally not implemented here.
//
// ORDER IS LOAD-BEARING. Channel ids are derived from a module's position
// in this pool, and persisted Labels/RGBProfiles/RGBOverride/RGBPerLed are
// keyed by those ids. Reordering, inserting into the middle of, or removing
// an entry remaps every existing user's saved settings onto the wrong
// physical DIMM. Append new addresses at the end only.
//
// The addresses beyond 0x70-0x76 (0x4F, 0x66-0x67, 0x39-0x3D) are unvalidated
// against real hardware: on many boards those ranges hold non-ENE SMBus
// peripherals (PMIC/RCD/SPD), and detection relies solely on the 0xA0-0xAF
// echo signature. They match OpenRGB's own pool and pass there, but no
// ENE module has been confirmed on them.
var eneRamAddresses = []byte{0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x4F, 0x66, 0x67, 0x39, 0x3A, 0x3B, 0x3C, 0x3D}

// eneToSpdAddressOffset is the offset between an ENE DRAM RGB controller's
// address and its own DIMM's SPD hub address (0x70 -> 0x50, 0x71 -> 0x51,
// ...), used to find that DIMM's temperature sensor. This assumes both
// chips on a given DIMM are wired to the same slot-select strapping and so
// enumerate in the same relative order -- true on the hardware this was
// validated against, but not verified on hardware where the two ranges
// might not line up slot-for-slot.
const eneToSpdAddressOffset = 0x20

type eneModule struct {
	Index     int
	Address   byte
	LedCount  int
	DirectReg uint16
	Version   string
}

// eneRegisterPointer selects a 16-bit register for the next read/write by
// writing it, byte-swapped, to command 0x00.
func eneRegisterPointer(f *os.File, addr byte, reg uint16) error {
	swapped := ((reg << 8) & 0xFF00) | ((reg >> 8) & 0x00FF)
	return smbus.WriteWordData(f, addr, 0x00, swapped)
}

// eneReadByte reads a byte via the register-pointer protocol: set the
// pointer, then read command 0x81.
func eneReadByte(f *os.File, addr byte, reg uint16) (byte, error) {
	if err := eneRegisterPointer(f, addr, reg); err != nil {
		return 0, err
	}
	return smbus.ReadRegister(f, addr, 0x81)
}

// eneWriteByte writes a byte via the register-pointer protocol: set the
// pointer, then write command 0x01.
func eneWriteByte(f *os.File, addr byte, reg uint16, val byte) error {
	if err := eneRegisterPointer(f, addr, reg); err != nil {
		return err
	}
	return smbus.WriteByteData(f, addr, 0x01, val)
}

// eneWriteBlock writes a byte block via the register-pointer protocol: set
// the pointer, then block-write command 0x03. Uncached because command
// 0x03 is a generic "write to whatever was just pointed at" channel, not a
// fixed destination register.
func eneWriteBlock(f *os.File, addr byte, reg uint16, data []byte) error {
	if err := eneRegisterPointer(f, addr, reg); err != nil {
		return err
	}
	return smbus.WriteBlockDataUncached(f, addr, 0x03, data)
}

// eneSelfTest replicates OpenRGB's TestForENESMBusController signature
// check: registers 0xA0-0xAF, read directly with no pointer preamble, must
// echo back their own offset from 0xA0.
func eneSelfTest(f *os.File, addr byte) bool {
	for i := 0; i < 16; i++ {
		v, err := smbus.ReadRegister(f, addr, byte(0xA0+i))
		if err != nil || int(v) != i {
			return false
		}
	}
	return true
}

// eneReadString reads a NUL-terminated (or length-bounded) ASCII string
// starting at base, one pointer-addressed byte at a time.
func eneReadString(f *os.File, addr byte, base uint16, length int) string {
	buf := make([]byte, 0, length)
	for i := 0; i < length; i++ {
		b, err := eneReadByte(f, addr, base+uint16(i))
		if err != nil || b == 0 {
			break
		}
		buf = append(buf, b)
	}
	return string(buf)
}

// detectEneModules scans the ENE DRAM RGB controller address pool for
// modules that pass the self-test and aren't a Micron SPD chip (which
// implements a subset of the same low registers).
func detectEneModules(f *os.File) []eneModule {
	var modules []eneModule

	for idx, addr := range eneRamAddresses {
		if !eneSelfTest(f, addr) {
			continue
		}

		if strings.Contains(eneReadString(f, addr, eneRegMicronCheck, 16), "Micron") {
			continue
		}

		ledCount, err := eneReadByte(f, addr, eneRegConfigLedCount)
		if err != nil || ledCount == 0 {
			continue
		}

		version := eneReadString(f, addr, eneRegDeviceName, 16)
		directReg, ok := eneVersionDirectRegister[version]
		if !ok {
			directReg = eneRegColorsDirect
		}

		modules = append(modules, eneModule{
			Index:     idx,
			Address:   addr,
			LedCount:  int(ledCount),
			DirectReg: directReg,
			Version:   version,
		})
	}

	return modules
}

// transferEne writes an R,G,B buffer (as produced by rgb.SetColor, one
// triplet per LED) to an ENE-protocol DRAM RGB controller. The wire order
// is R,B,G and each LED needs its own register-pointer + block-write pair
// -- there's no single "write everything at once" command here the way
// Corsair's checksummed block transfer works.
func (d *Device) transferEne(device *Devices, buffer []byte) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	ledCount := int(device.LedChannels)
	for i := 0; i < ledCount; i++ {
		offset := i * 3
		if offset+2 >= len(buffer) {
			break
		}
		r, g, b := buffer[offset], buffer[offset+1], buffer[offset+2]
		reg := device.EneDirectRegister + uint16(offset)
		if err := eneWriteBlock(d.dev.File, device.EneAddress, reg, []byte{r, b, g}); err != nil {
			logger.Log(logger.Fields{"error": err, "address": device.EneAddress}).Error("Unable to write ENE LED color")
		}
	}
}

// eneSetDirect enables or releases host (direct) control of an ENE DRAM
// RGB controller. Releasing it hands lighting back to the module's
// internal effect engine, which is what produces the on-board default
// rainbow behavior these modules ship with. Locks d.mutex itself, matching
// transfer()/transferEne()'s self-locking discipline, since this is a
// stateful pointer-then-value sequence that must not interleave with any
// other I2C transaction on the same bus handle.
func (d *Device) eneSetDirect(addr byte, enabled bool) {
	d.mutex.Lock()
	defer d.mutex.Unlock()

	var val byte
	if enabled {
		val = 1
	}
	f := d.dev.File
	if err := eneWriteByte(f, addr, eneRegDirect, val); err != nil {
		logger.Log(logger.Fields{"error": err, "address": addr}).Warn("Unable to set ENE direct mode")
	}
	if err := eneWriteByte(f, addr, eneRegApply, eneApplyVal); err != nil {
		logger.Log(logger.Fields{"error": err, "address": addr}).Warn("Unable to apply ENE direct mode change")
	}
}
