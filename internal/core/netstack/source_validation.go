package netstack

import (
	"encoding/binary"
	"net"
)

type SourceViolation uint8

const (
	SourceValid SourceViolation = iota
	SourceMalformed
	SourceMACViolation
	SourceARPViolation
	SourceIPv4Violation
	SourceUnsupportedProtocol
)

func (v SourceViolation) String() string {
	switch v {
	case SourceMalformed:
		return "malformed"
	case SourceMACViolation:
		return "mac"
	case SourceARPViolation:
		return "arp"
	case SourceIPv4Violation:
		return "ipv4"
	case SourceUnsupportedProtocol:
		return "unsupported"
	default:
		return "valid"
	}
}

// ValidateGuestSource binds a guest-originated Ethernet frame to its assigned
// MAC and IPv4 lease. ARP probes and DHCP discovery are the only zero-address
// exceptions.
func ValidateGuestSource(frame []byte, expectedMAC net.HardwareAddr, expectedIPv4 net.IP) SourceViolation {
	if len(frame) < ethernetHeaderLen || len(expectedMAC) != 6 || expectedIPv4.To4() == nil {
		return SourceMalformed
	}
	if !equalHardwareAddr(frame[6:12], expectedMAC) {
		return SourceMACViolation
	}

	switch binary.BigEndian.Uint16(frame[12:14]) {
	case uint16(etherTypeARP):
		return validateARPSource(frame[ethernetHeaderLen:], expectedMAC, expectedIPv4.To4())
	case uint16(etherTypeIPv4):
		return validateIPv4Source(frame[ethernetHeaderLen:], expectedIPv4.To4())
	case 0x86dd: // The IPv4-only network drops ordinary guest IPv6 control traffic quietly.
		return SourceUnsupportedProtocol
	default:
		return SourceValid
	}
}

func validateARPSource(packet []byte, expectedMAC net.HardwareAddr, expectedIPv4 net.IP) SourceViolation {
	if len(packet) < 28 ||
		binary.BigEndian.Uint16(packet[0:2]) != arpHardwareEthernet ||
		binary.BigEndian.Uint16(packet[2:4]) != arpProtoIPv4 ||
		packet[4] != 6 || packet[5] != 4 {
		return SourceMalformed
	}
	if !equalHardwareAddr(packet[8:14], expectedMAC) {
		return SourceARPViolation
	}
	senderIP := net.IP(packet[14:18])
	if senderIP.Equal(expectedIPv4) {
		return SourceValid
	}
	// RFC 5227 address probes use an unspecified sender while probing the
	// address that the endpoint has been assigned.
	if senderIP.Equal(net.IPv4zero) && net.IP(packet[24:28]).Equal(expectedIPv4) {
		return SourceValid
	}
	return SourceARPViolation
}

func validateIPv4Source(packet []byte, expectedIPv4 net.IP) SourceViolation {
	if len(packet) < ipv4HeaderLen || packet[0]>>4 != 4 {
		return SourceMalformed
	}
	headerLen := int(packet[0]&0x0f) * 4
	if headerLen < ipv4HeaderLen || headerLen > len(packet) {
		return SourceMalformed
	}
	sourceIP := net.IP(packet[12:16])
	if sourceIP.Equal(expectedIPv4) {
		return SourceValid
	}
	if !sourceIP.Equal(net.IPv4zero) || packet[9] != byte(udpProtocolNumber) || len(packet) < headerLen+udpHeaderLen {
		return SourceIPv4Violation
	}
	udp := packet[headerLen:]
	if binary.BigEndian.Uint16(udp[0:2]) == 68 && binary.BigEndian.Uint16(udp[2:4]) == 67 {
		return SourceValid
	}
	return SourceIPv4Violation
}

func equalHardwareAddr(a, b []byte) bool {
	if len(a) != 6 || len(b) != 6 {
		return false
	}
	for i := 0; i < 6; i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
