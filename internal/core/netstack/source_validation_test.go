package netstack

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestValidateGuestSource(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0x00, 0x02}
	ip := net.IPv4(10, 42, 0, 2)

	tests := []struct {
		name  string
		frame []byte
		want  SourceViolation
	}{
		{name: "assigned IPv4", frame: sourceValidationIPv4Frame(mac, ip, 1, 0, 0), want: SourceValid},
		{name: "spoofed Ethernet source", frame: sourceValidationIPv4Frame(net.HardwareAddr{0x02, 0x42, 0x0a, 0x2a, 0, 3}, ip, 1, 0, 0), want: SourceMACViolation},
		{name: "spoofed IPv4 source", frame: sourceValidationIPv4Frame(mac, net.IPv4(10, 42, 0, 3), 1, 0, 0), want: SourceIPv4Violation},
		{name: "unsupported IPv6", frame: sourceValidationEtherTypeFrame(mac, 0x86dd), want: SourceUnsupportedProtocol},
		{name: "DHCP discovery", frame: sourceValidationIPv4Frame(mac, net.IPv4zero, byte(udpProtocolNumber), 68, 67), want: SourceValid},
		{name: "non-DHCP unspecified IPv4", frame: sourceValidationIPv4Frame(mac, net.IPv4zero, byte(udpProtocolNumber), 1234, 67), want: SourceIPv4Violation},
		{name: "assigned ARP", frame: sourceValidationARPFrame(mac, ip, net.IPv4(10, 42, 0, 1)), want: SourceValid},
		{name: "ARP probe", frame: sourceValidationARPFrame(mac, net.IPv4zero, ip), want: SourceValid},
		{name: "spoofed ARP sender MAC", frame: func() []byte {
			frame := sourceValidationARPFrame(mac, ip, net.IPv4(10, 42, 0, 1))
			frame[ethernetHeaderLen+8] ^= 1
			return frame
		}(), want: SourceARPViolation},
		{name: "spoofed ARP sender IP", frame: sourceValidationARPFrame(mac, net.IPv4(10, 42, 0, 3), net.IPv4(10, 42, 0, 1)), want: SourceARPViolation},
		{name: "malformed", frame: []byte{1, 2, 3}, want: SourceMalformed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidateGuestSource(tt.frame, mac, ip); got != tt.want {
				t.Fatalf("ValidateGuestSource() = %s, want %s", got, tt.want)
			}
		})
	}
}

func sourceValidationEtherTypeFrame(mac net.HardwareAddr, etherType uint16) []byte {
	frame := make([]byte, ethernetHeaderLen)
	copy(frame[0:6], []byte{0x02, 0x42, 0x0a, 0x2a, 0, 1})
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], etherType)
	return frame
}

func sourceValidationIPv4Frame(mac net.HardwareAddr, sourceIP net.IP, protocol byte, sourcePort, destinationPort uint16) []byte {
	frame := make([]byte, ethernetHeaderLen+ipv4HeaderLen+udpHeaderLen)
	copy(frame[0:6], []byte{0x02, 0x42, 0x0a, 0x2a, 0, 1})
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], uint16(etherTypeIPv4))
	packet := frame[ethernetHeaderLen:]
	packet[0] = 0x45
	packet[9] = protocol
	copy(packet[12:16], sourceIP.To4())
	copy(packet[16:20], net.IPv4(10, 42, 0, 1).To4())
	binary.BigEndian.PutUint16(packet[20:22], sourcePort)
	binary.BigEndian.PutUint16(packet[22:24], destinationPort)
	return frame
}

func sourceValidationARPFrame(mac net.HardwareAddr, senderIP, targetIP net.IP) []byte {
	frame := make([]byte, ethernetHeaderLen+28)
	for i := 0; i < 6; i++ {
		frame[i] = 0xff
	}
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], uint16(etherTypeARP))
	packet := frame[ethernetHeaderLen:]
	binary.BigEndian.PutUint16(packet[0:2], arpHardwareEthernet)
	binary.BigEndian.PutUint16(packet[2:4], arpProtoIPv4)
	packet[4], packet[5] = 6, 4
	binary.BigEndian.PutUint16(packet[6:8], 1)
	copy(packet[8:14], mac)
	copy(packet[14:18], senderIP.To4())
	copy(packet[24:28], targetIP.To4())
	return frame
}
