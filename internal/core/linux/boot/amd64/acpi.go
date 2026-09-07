package amd64

import (
	"encoding/binary"
	"fmt"
)

const (
	acpiRSDPAddress  = 0x000e0000
	acpiTableAddress = 0x000e1000

	acpiLocalAPICAddress = 0xfee00000
	acpiIOAPICAddress    = 0xfec00000
	acpiHPETAddress      = 0xfed00000
)

func installBootACPI(memory []byte, memStart uint64, numCPUs int) (uint64, error) {
	if numCPUs < 1 {
		numCPUs = 1
	}
	writer := acpiTableWriter{base: acpiTableAddress}
	facs := writer.appendRaw(buildBootFACS())
	dsdt := writer.append("DSDT", 2, "CCKVMDSD", buildBootDSDT())
	fadt := writer.append("FACP", 6, "CCKVMFAD", buildBootFADT(facs, dsdt))
	madt := writer.append("APIC", 3, "CCKVMAPC", buildBootMADT(numCPUs))
	hpet := writer.append("HPET", 1, "CCKVMHPT", buildBootHPET())
	xsdt := writer.append("XSDT", 1, "CCKVMXSD", buildBootXSDT([]uint64{fadt, madt, hpet}))
	if err := writeAt(memory, memStart, acpiTableAddress, writer.data); err != nil {
		return 0, fmt.Errorf("write ACPI tables: %w", err)
	}
	if err := writeAt(memory, memStart, acpiRSDPAddress, buildBootRSDP(xsdt)); err != nil {
		return 0, fmt.Errorf("write ACPI RSDP: %w", err)
	}
	return acpiRSDPAddress, nil
}

func buildBootDSDT() []byte {
	// Name (_S5, Package (0x04) { 0x05, 0x05, Zero, Zero }). Linux uses
	// this object to translate poweroff into an S5 write to PM1_CONTROL.
	return []byte{
		0x08, '_', 'S', '5', '_',
		0x12, 0x08, 0x04,
		0x0a, 0x05, 0x0a, 0x05, 0x00, 0x00,
	}
}

type acpiTableWriter struct {
	base uint64
	data []byte
}

func (w *acpiTableWriter) append(signature string, revision byte, tableID string, body []byte) uint64 {
	addr := w.base + uint64(len(w.data))
	table := buildACPITable(signature, revision, tableID, body)
	w.data = append(w.data, table...)
	for len(w.data)%16 != 0 {
		w.data = append(w.data, 0)
	}
	return addr
}

func (w *acpiTableWriter) appendRaw(table []byte) uint64 {
	addr := w.base + uint64(len(w.data))
	w.data = append(w.data, table...)
	for len(w.data)%16 != 0 {
		w.data = append(w.data, 0)
	}
	return addr
}

func buildACPITable(signature string, revision byte, tableID string, body []byte) []byte {
	table := make([]byte, 36+len(body))
	copy(table[0:4], acpiFixedString(signature, 4))
	binary.LittleEndian.PutUint32(table[4:], uint32(len(table)))
	table[8] = revision
	copy(table[10:16], acpiFixedString("CCKVM ", 6))
	copy(table[16:24], acpiFixedString(tableID, 8))
	binary.LittleEndian.PutUint32(table[24:], 1)
	copy(table[28:32], acpiFixedString("CC  ", 4))
	binary.LittleEndian.PutUint32(table[32:], 1)
	copy(table[36:], body)
	table[9] = checksum(table)
	return table
}

func buildBootXSDT(entries []uint64) []byte {
	body := make([]byte, 8*len(entries))
	for i, entry := range entries {
		binary.LittleEndian.PutUint64(body[i*8:], entry)
	}
	return body
}

func buildBootMADT(numCPUs int) []byte {
	var body []byte
	body = binary.LittleEndian.AppendUint32(body, acpiLocalAPICAddress)
	body = binary.LittleEndian.AppendUint32(body, 1)
	for cpu := 0; cpu < numCPUs && cpu < 255; cpu++ {
		body = append(body, 0, 8, byte(cpu), byte(cpu))
		body = binary.LittleEndian.AppendUint32(body, 1)
	}
	body = append(body, 1, 12, 0, 0)
	body = binary.LittleEndian.AppendUint32(body, acpiIOAPICAddress)
	body = binary.LittleEndian.AppendUint32(body, 0)
	body = appendMADTInterruptOverride(body, 0, 2, 0)
	body = appendMADTLocalAPICNMI(body)
	return body
}

func appendMADTInterruptOverride(body []byte, irq byte, gsi uint32, flags uint16) []byte {
	body = append(body, 2, 10, 0, irq)
	body = binary.LittleEndian.AppendUint32(body, gsi)
	body = binary.LittleEndian.AppendUint16(body, flags)
	return body
}

func appendMADTLocalAPICNMI(body []byte) []byte {
	body = append(body, 4, 6, 0xff)
	body = binary.LittleEndian.AppendUint16(body, 0)
	body = append(body, 1)
	return body
}

func buildBootHPET() []byte {
	const (
		hpetClockPeriodFemtoseconds = 10_000_000
		hpetVendorID                = 0x8086
		hpetNumTimers               = 3
	)
	var body []byte
	id := uint32(1)
	id |= uint32(hpetNumTimers-1) << 8
	id |= 1 << 13
	id |= uint32(hpetVendorID) << 16
	body = binary.LittleEndian.AppendUint32(body, id)
	body = append(body, 0, 64, 0, 0)
	body = binary.LittleEndian.AppendUint64(body, acpiHPETAddress)
	body = append(body, 0)
	body = binary.LittleEndian.AppendUint16(body, 0x0080)
	body = append(body, 0)
	_ = hpetClockPeriodFemtoseconds
	return body
}

func buildBootFACS() []byte {
	facs := make([]byte, 64)
	copy(facs[0:4], "FACS")
	binary.LittleEndian.PutUint32(facs[4:], uint32(len(facs)))
	binary.LittleEndian.PutUint32(facs[8:], 1)
	return facs
}

func buildBootFADT(facs, dsdt uint64) []byte {
	const (
		fadtBodyLength     = 276 - 36
		sciIRQ             = 9
		acpiPM1EventPort   = 0x400
		acpiPM1ControlPort = 0x404
		fadtFlagWBINVD     = 1 << 0
	)
	body := make([]byte, fadtBodyLength)
	binary.LittleEndian.PutUint32(body[0:], uint32(facs))
	binary.LittleEndian.PutUint32(body[4:], uint32(dsdt))
	body[9] = 0 // Unspecified preferred PM profile.
	binary.LittleEndian.PutUint16(body[10:], sciIRQ)
	binary.LittleEndian.PutUint32(body[20:], acpiPM1EventPort)
	binary.LittleEndian.PutUint32(body[28:], acpiPM1ControlPort)
	body[52] = 4
	body[53] = 2
	binary.LittleEndian.PutUint32(body[76:], fadtFlagWBINVD)
	binary.LittleEndian.PutUint64(body[96:], facs)
	binary.LittleEndian.PutUint64(body[104:], dsdt)
	return body
}

func buildBootRSDP(xsdt uint64) []byte {
	rsdp := make([]byte, 36)
	copy(rsdp[0:8], "RSD PTR ")
	copy(rsdp[9:15], "CCKVM ")
	rsdp[15] = 2
	binary.LittleEndian.PutUint32(rsdp[20:], uint32(len(rsdp)))
	binary.LittleEndian.PutUint64(rsdp[24:], xsdt)
	rsdp[8] = checksum(rsdp[:20])
	rsdp[32] = checksum(rsdp)
	return rsdp
}

func acpiFixedString(value string, size int) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = ' '
	}
	copy(out, value)
	return out
}
