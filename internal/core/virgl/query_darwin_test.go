//go:build darwin

package virgl

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestTimestampQueryCompletesGuestResultState(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	result := virtio.GPUResource3D{ID: 1, Target: 0, Width: 16}
	if err := host.createResource(result); err != nil {
		t.Fatal(err)
	}
	commands := []command{
		{Opcode: 1, Object: 9, Payload: []uint32{7, 2, 0, result.ID}},
		{Opcode: 20, Payload: []uint32{7}},
		{Opcode: 21, Payload: []uint32{7, 0}},
	}
	if err := host.execute(contextID, commands, nil); err != nil {
		t.Fatal(err)
	}
	state := host.resources[result.ID].bufferBytes
	if got := binary.LittleEndian.Uint32(state[0:4]); got != 1 {
		t.Fatalf("query state = %d, want done", got)
	}
	if got := binary.LittleEndian.Uint32(state[4:8]); got != 8 {
		t.Fatalf("query result size = %d, want 8", got)
	}
	if err := host.dispatch(func() error {
		if glError := host.gl.getError(); glError != 0 {
			return fmt.Errorf("timestamp query produced GL error %#x", glError)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{Opcode: 3, Object: 9, Payload: []uint32{7}}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestQueryRejectsOutOfBoundsGuestResultRange(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	result := virtio.GPUResource3D{ID: 1, Target: 0, Width: 16}
	if err := host.createResource(result); err != nil {
		t.Fatal(err)
	}
	err := host.execute(contextID, []command{{
		Opcode: 1, Object: 9, Payload: []uint32{7, 1, 8, result.ID},
	}}, nil)
	if err == nil {
		t.Fatal("query accepted a result state extending past the buffer")
	}
}

func TestOcclusionQueryCanControlConditionalRendering(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	result := virtio.GPUResource3D{ID: 1, Target: 0, Width: 16}
	if err := host.createResource(result); err != nil {
		t.Fatal(err)
	}
	commands := []command{
		{Opcode: 1, Object: 9, Payload: []uint32{7, 1, 0, result.ID}},
		{Opcode: 19, Payload: []uint32{7}},
		{Opcode: 20, Payload: []uint32{7}},
		{Opcode: 21, Payload: []uint32{7, 1}},
		{Opcode: 26, Payload: []uint32{7, 0, 0}},
		{Opcode: 26, Payload: []uint32{0, 0, 0}},
	}
	if err := host.execute(contextID, commands, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if glError := host.gl.getError(); glError != 0 {
			return fmt.Errorf("conditional occlusion query produced GL error %#x", glError)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
