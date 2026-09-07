package virgl

import (
	"encoding/binary"
	"testing"
)

func TestDecodeCommandsRejectsTruncatedPayload(t *testing.T) {
	stream := make([]byte, 8)
	binary.LittleEndian.PutUint32(stream, uint32(4)<<16|7)
	if _, err := decodeCommands(stream); err == nil {
		t.Fatal("truncated VirGL command was accepted")
	}
}

func TestDecodeCommandsPreservesFraming(t *testing.T) {
	stream := make([]byte, 20)
	binary.LittleEndian.PutUint32(stream[0:], uint32(2)<<16|uint32(4)<<8|1)
	binary.LittleEndian.PutUint32(stream[4:], 10)
	binary.LittleEndian.PutUint32(stream[8:], 11)
	binary.LittleEndian.PutUint32(stream[12:], uint32(1)<<16|7)
	binary.LittleEndian.PutUint32(stream[16:], 12)
	commands, err := decodeCommands(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || commands[0].Opcode != 1 || commands[0].Object != 4 ||
		len(commands[0].Payload) != 2 || commands[0].Payload[1] != 11 ||
		commands[1].Opcode != 7 || commands[1].Payload[0] != 12 {
		t.Fatalf("decoded commands = %#v", commands)
	}
}
