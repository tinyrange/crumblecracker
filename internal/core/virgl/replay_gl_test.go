//go:build darwin || (linux && amd64)

package virgl

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestReplayOpenGLTriangle(t *testing.T) {
	host := newReplayTestHost(t)
	t.Cleanup(func() { _ = host.close() })
	if host.gl.renderer != "" {
		t.Logf("Host GL renderer %s", host.gl.renderer)
	}

	path := filepath.Join(t.TempDir(), "triangle.virgl")
	capture, err := newCaptureHost(host, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = capture.close() })

	target := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 4, Height: 4, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24, Height: 1, Depth: 1, ArraySize: 1}
	const contextID = 1
	for _, description := range []virtio.GPUResource3D{target, positions} {
		if err := capture.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	if err := capture.createContext(contextID); err != nil {
		t.Fatal(err)
	}

	vertices := words(math.Float32bits(-1), math.Float32bits(-1), math.Float32bits(3),
		math.Float32bits(-1), math.Float32bits(-1), math.Float32bits(3))
	if err := capture.queueBufferTransfer(&resource{description: positions, data: vertices}, virtio.GPUTransfer3D{
		ContextID: contextID, ResourceID: positions.ID,
		Box: virtio.GPUBox{Width: positions.Width, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := capture.waitForFence(); err != nil {
		t.Fatal(err)
	}

	shader := func(id, stage uint32, source string) command {
		data := append([]byte(source), 0)
		data = append(data, make([]byte, (4-len(data)%4)%4)...)
		payload, err := fixedWords(data, len(data)/4)
		if err != nil {
			t.Fatal(err)
		}
		return command{Opcode: 1, Object: 4, Payload: append([]uint32{id, stage, uint32(len(source) + 1), 0, 0}, payload...)}
	}
	vertex := shader(20, tgsiVertex, `VERT
DCL IN[0]
DCL OUT[0], POSITION
  0: MOV OUT[0], IN[0]
  1: END`)
	fragment := shader(21, tgsiFragment, `FRAG
DCL OUT[0], COLOR
IMM[0] FLT32 {1.0, 0.0, 0.0, 1.0}
  0: MOV OUT[0], IMM[0]
  1: END`)
	commands := []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, target.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 1, Object: 5, Payload: []uint32{12, 0, 0, 0, 29}},
		{Opcode: 2, Object: 5, Payload: []uint32{12}},
		{Opcode: 6, Payload: []uint32{8, 0, positions.ID}},
		vertex, fragment,
		{Opcode: 31, Payload: []uint32{20, tgsiVertex}},
		{Opcode: 31, Payload: []uint32{21, tgsiFragment}},
		{Opcode: 4, Payload: []uint32{
			0, math.Float32bits(2), math.Float32bits(2), math.Float32bits(0.5),
			math.Float32bits(2), math.Float32bits(2), math.Float32bits(0.5),
		}},
		{Opcode: 8, Payload: []uint32{0, 3, 4, 0, 1, 0, 0, 0, 0, 0, 0, 0}},
	}
	if err := capture.execute(contextID, commands, nil); err != nil {
		t.Fatal(err)
	}
	if err := capture.waitForFence(); err != nil {
		t.Fatal(err)
	}

	targetResource := &resource{description: target}
	rect := image.Rect(0, 0, 4, 4)
	want := bytes.Repeat([]byte{0, 0, 255, 255}, rect.Dx()*rect.Dy())
	pixels, stride, err := capture.readScanout(targetResource, rect)
	if err != nil {
		t.Fatal(err)
	}
	if stride != 16 || !bytes.Equal(pixels, want) {
		t.Fatalf("captured BGRA pixels %v with stride %d", pixels, stride)
	}

	output := filepath.Join(t.TempDir(), "triangle.png")
	frame, err := ReplayCapture(path, output, 0)
	if err != nil || frame != 1 {
		t.Fatalf("replay returned frame %d, error %v", frame, err)
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	replayed, err := png.Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Bounds() != rect {
		t.Fatalf("replayed bounds %v, want %v", replayed.Bounds(), rect)
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			if got := color.RGBAModel.Convert(replayed.At(x, y)); got != (color.RGBA{R: 255, A: 255}) {
				t.Fatalf("replayed pixel at (%d, %d) = %v, want opaque red", x, y, got)
			}
		}
	}

	// Both contexts can coexist. Closing replay must leave the capture context usable.
	pixels, _, err = host.readScanout(targetResource, rect)
	if err != nil || !bytes.Equal(pixels, want) {
		t.Fatalf("original context after replay closed returned %v, error %v", pixels, err)
	}
}
