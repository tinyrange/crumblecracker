//go:build darwin

package virgl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestOpenArenaVertexFormats(t *testing.T) {
	for _, test := range []struct {
		name       string
		format     uint32
		components int32
		dataType   uint32
		normalized bool
	}{
		{name: "R10G10B10A2_UNORM", format: 8, components: 4, dataType: glUnsignedInt2101010Rev, normalized: true},
		{name: "R10G10B10A2_USCALED", format: 123, components: 4, dataType: glUnsignedInt2101010Rev, normalized: false},
		{name: "R10G10B10A2_SSCALED", format: 172, components: 4, dataType: glInt2101010Rev, normalized: false},
		{name: "R10G10B10A2_SNORM", format: 173, components: 4, dataType: glInt2101010Rev, normalized: true},
		{name: "R16G16B16A16_UNORM", format: 51, components: 4, dataType: glUnsignedShort, normalized: true},
		{name: "R32G32B32_UNORM", format: 34, components: 3, dataType: glUnsignedInt, normalized: true},
		{name: "R32G32B32_USCALED", format: 38, components: 3, dataType: glUnsignedInt, normalized: false},
		{name: "R32G32B32_SNORM", format: 42, components: 3, dataType: glInt, normalized: true},
		{name: "R32G32B32_SSCALED", format: 46, components: 3, dataType: glInt, normalized: false},
		{name: "R16G16_SSCALED", format: 61, components: 2, dataType: glShort, normalized: false},
		{name: "R8G8B8A8_USCALED", format: 72, components: 4, dataType: glUnsignedByte, normalized: false},
		{name: "R8G8B8A8_SNORM", format: 77, components: 4, dataType: glByte, normalized: true},
		{name: "R8G8B8A8_SSCALED", format: 85, components: 4, dataType: glByte, normalized: false},
		{name: "R32G32_FIXED", format: 88, components: 2, dataType: glFixed, normalized: false},
		{name: "R16G16B16_FLOAT", format: 93, components: 3, dataType: glHalfFloat, normalized: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			components, dataType, normalized, ok := vertexFormat(test.format)
			if !ok {
				t.Fatalf("format %d is unsupported", test.format)
			}
			if components != test.components || dataType != test.dataType || normalized != test.normalized {
				t.Fatalf(
					"mapping = (%d, %#x, normalized=%t), want (%d, %#x, normalized=%t)",
					components, dataType, normalized, test.components, test.dataType, test.normalized,
				)
			}
		})
	}
}

func TestFixedPointVertexFormatRendersThroughDarwinHost(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Format: 88, Width: 24, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(positions); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, positions.Width)
	for index, value := range []int32{-65536, -65536, 3 * 65536, -65536, -65536, 3 * 65536} {
		binary.LittleEndian.PutUint32(data[index*4:], uint32(value))
	}
	if err := host.transferToHost(&resource{description: positions, data: data}, virtio.GPUTransfer3D{
		ResourceID: positions.ID,
		Box:        virtio.GPUBox{Width: positions.Width, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 150
in vec2 position;
void main() { gl_Position = vec4(position, 0.0, 1.0); }`
	fragment := `#version 150
out vec4 result;
void main() { result = vec4(1.0, 0.0, 0.0, 1.0); }`
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		target := host.resources[output.ID]
		buffer := host.resources[positions.ID]
		host.publishBuffer(buffer)
		host.gl.bindFramebuffer(glFramebuffer, target.framebuffer)
		host.gl.viewport(0, 0, 1, 1)
		host.gl.useProgram(program)
		host.gl.bindVertexArray(host.vao)
		host.gl.bindBuffer(glArrayBuffer, buffer.buffer)
		host.gl.vertexAttribPtr(0, 2, glFixed, false, 8, 0)
		host.gl.enableVertexAttrib(0)
		host.gl.drawArrays(glTriangles, 0, 3)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: output}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("fixed-point vertex draw BGRA = %v, want %v", got, want)
	}
}

func TestVertexElementPreservesInstanceDivisor(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{
		Opcode:  1,
		Object:  5,
		Payload: []uint32{27, 12, 3, 1, 67},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		elements := host.contexts[contextID].vertexElements[27]
		if len(elements) != 1 || elements[0].instanceDivisor != 3 {
			return fmt.Errorf("vertex element instance divisor = %+v, want 3", elements)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDestroyObjectDoesNotDeleteOtherObjectTypesWithSameHandle(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	const handle = 42
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 1, Payload: append([]uint32{handle}, make([]uint32, 10)...)},
		{Opcode: 1, Object: 3, Payload: []uint32{handle, 0, 0, 0, 0}},
		{Opcode: 3, Object: 1, Payload: []uint32{handle}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if _, ok := host.contexts[contextID].depthStencilAlpha[handle]; !ok {
			return fmt.Errorf("destroying blend object %d also deleted DSA object %d", handle, handle)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProgramCacheEvictsLeastRecentlyUsedProgram(t *testing.T) {
	var deleted []uint32
	host := &darwinHost{
		gl: &hostGL{
			deleteProgram: func(program uint32) {
				deleted = append(deleted, program)
			},
			useProgram: func(uint32) {},
		},
		programs: make(map[hostProgramKey]hostProgram),
	}
	contexts := []*hostContext{newHostContext(), newHostContext(), newHostContext()}
	for index, context := range contexts {
		key := hostProgramKey{context: context}
		host.programs[key] = hostProgram{id: uint32(index + 1), lastUsed: uint64(index + 1)}
	}
	host.currentProgram = 1

	host.evictPrograms(2)

	if len(host.programs) != 2 {
		t.Fatalf("program cache size = %d, want 2", len(host.programs))
	}
	if len(deleted) != 1 || deleted[0] != 2 {
		t.Fatalf("deleted programs = %v, want [2]", deleted)
	}
	if host.currentProgram != 1 {
		t.Fatalf("current program = %d, want 1", host.currentProgram)
	}
}

func TestSurfaceKeepsResourceAliveAfterGuestUnref(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 55, Target: 2, Format: 1, Width: 4, Height: 4, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{5, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 5}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.unrefResource(color.ID); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{
		Opcode: 7,
		Payload: []uint32{
			0x4,
			math.Float32bits(1),
			math.Float32bits(0),
			math.Float32bits(0),
			math.Float32bits(1),
		},
	}}, nil); err != nil {
		t.Fatalf("clear through surface after guest resource unref: %v", err)
	}
	if err := host.execute(contextID, []command{{Opcode: 3, Object: 8, Payload: []uint32{5}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if len(host.allResources) != 0 {
			return fmt.Errorf("%d host resources remain after final surface reference was destroyed", len(host.allResources))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUnrefCommitsQueuedBufferTransferForRetainedBinding(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	description := virtio.GPUResource3D{ID: 55, Target: 0, Width: 8}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{
		Opcode:  6,
		Payload: []uint32{4, 0, description.ID},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.queueBufferTransfer(&resource{
		description: description,
		data:        []byte{10, 20, 30, 40},
	}, virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{X: 2, Width: 4},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.unrefResource(description.ID); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		retained := host.contexts[contextID].vertexBuffers[0].resource
		if retained == nil {
			return errors.New("vertex binding did not retain buffer")
		}
		if got, want := retained.bufferBytes, []byte{0, 0, 10, 20, 30, 40, 0, 0}; string(got) != string(want) {
			return fmt.Errorf("retained buffer bytes = %v, want %v", got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSparseVertexBufferSlotsRenderMixedAttributes(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24}
	colors := virtio.GPUResource3D{ID: 3, Target: 0, Width: 48}
	for _, description := range []virtio.GPUResource3D{color, positions, colors} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	floatBytes := func(values ...float32) []byte {
		result := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
		}
		return result
	}
	for _, upload := range []struct {
		description virtio.GPUResource3D
		data        []byte
	}{
		{positions, floatBytes(-1, -1, 3, -1, -1, 3)},
		{colors, floatBytes(1, 0, 0, 1, 1, 0, 0, 1, 1, 0, 0, 1)},
	} {
		if err := host.transferToHost(&resource{description: upload.description, data: upload.data}, virtio.GPUTransfer3D{
			ResourceID: upload.description.ID,
			Box:        virtio.GPUBox{Width: upload.description.Width},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Mesa consolidates adjacent fixed-point attributes into one float buffer.
	// A later integer attribute can therefore occupy slot 2 while slot 1 is
	// explicitly unbound in the VirGL vertex-buffer array.
	if err := host.execute(contextID, []command{{Opcode: 6, Payload: []uint32{
		8, 0, positions.ID,
		0, 0, 0,
		16, 0, colors.ID,
	}}}, nil); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11
	context.vertexElements[12] = []hostVertexElement{
		{bufferIndex: 0, format: 29},
		{bufferIndex: 2, format: 31},
	}
	context.boundVertexElements = 12
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
layout(location = 1) in vec4 vertexColor;
out vec4 varyingColor;
void main() {
	gl_Position = vec4(position, 0.0, 1.0);
	varyingColor = vertexColor;
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
in vec4 varyingColor;
layout(location = 0) out vec4 color;
void main() { color = varyingColor; }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21
	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		host.gl.viewport(0, 0, 1, 1)
		host.gl.clearColor(0, 0, 0, 1)
		host.gl.clear(glColorBufferBit)
		return host.draw(context, []uint32{0, 3, 4, 0})
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("sparse vertex-buffer draw BGRA = %v, want %v", got, want)
	}
}

func TestSubcontextsKeepIndependentFramebufferState(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	first := virtio.GPUResource3D{ID: 1, Target: 2, Format: 1, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	second := virtio.GPUResource3D{ID: 2, Target: 2, Format: 1, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{first, second} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	createSurface := func(resourceID uint32) command {
		return command{Opcode: 1, Object: 8, Payload: []uint32{11, resourceID}}
	}
	setFramebuffer := command{Opcode: 5, Payload: []uint32{1, 0, 11}}
	clear := func(red, green, blue float32) command {
		return command{Opcode: 7, Payload: []uint32{
			0x4,
			math.Float32bits(red),
			math.Float32bits(green),
			math.Float32bits(blue),
			math.Float32bits(1),
		}}
	}
	if err := host.execute(contextID, []command{
		createSurface(first.ID),
		setFramebuffer,
		{Opcode: 29, Payload: []uint32{7}},
		{Opcode: 28, Payload: []uint32{7}},
		createSurface(second.ID),
		setFramebuffer,
		clear(0, 0, 1),
		{Opcode: 28, Payload: []uint32{0}},
		clear(1, 0, 0),
	}, nil); err != nil {
		t.Fatal(err)
	}

	firstPixels, _, err := host.readScanout(&resource{description: first}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	secondPixels, _, err := host.readScanout(&resource{description: second}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := firstPixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("default subcontext framebuffer BGRA = %v, want %v", got, want)
	}
	if got, want := secondPixels, []byte{255, 0, 0, 255}; string(got) != string(want) {
		t.Fatalf("created subcontext framebuffer BGRA = %v, want %v", got, want)
	}
}

func TestClearTargetsBoundVirGLFramebufferAfterBlit(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}

	first := virtio.GPUResource3D{ID: 1, Target: 2, Format: 1, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	second := virtio.GPUResource3D{ID: 2, Target: 2, Format: 1, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(first); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(second); err != nil {
		t.Fatal(err)
	}

	createSurface := func(handle, resourceID uint32) command {
		return command{Opcode: 1, Object: 8, Payload: []uint32{handle, resourceID}}
	}
	setFramebuffer := func(surface uint32) command {
		return command{Opcode: 5, Payload: []uint32{1, 0, surface}}
	}
	clear := func(red, green, blue float32) command {
		return command{Opcode: 7, Payload: []uint32{
			0x4,
			math.Float32bits(red),
			math.Float32bits(green),
			math.Float32bits(blue),
			math.Float32bits(1),
		}}
	}
	if err := host.execute(contextID, []command{
		createSurface(11, first.ID),
		createSurface(12, second.ID),
		setFramebuffer(11),
		clear(1, 0, 0),
	}, nil); err != nil {
		t.Fatal(err)
	}

	blit := make([]uint32, 21)
	blit[0] = 0xf
	blit[3] = second.ID
	blit[9], blit[10], blit[11] = 1, 1, 1
	blit[12] = first.ID
	blit[18], blit[19], blit[20] = 1, 1, 1
	if err := host.execute(contextID, []command{
		{Opcode: 16, Payload: blit},
		clear(0, 0, 1),
	}, nil); err != nil {
		t.Fatal(err)
	}

	firstPixels, _, err := host.readScanout(&resource{description: first}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	secondPixels, _, err := host.readScanout(&resource{description: second}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := firstPixels, []byte{255, 0, 0, 255}; string(got) != string(want) {
		t.Fatalf("active framebuffer pixel BGRA = %v, want %v", got, want)
	}
	if got, want := secondPixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("blit destination pixel BGRA = %v, want %v", got, want)
	}
}

func TestBlitConvertsSharedExponentTextureToRGBA32F(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	source := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatR9G9B9E5Float,
		Width: 3, Height: 1, Depth: 1, ArraySize: 1,
	}
	destination := virtio.GPUResource3D{
		ID: 2, Target: 2, Format: virglFormatR32G32B32A32Float,
		Width: 3, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(source); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(destination); err != nil {
		t.Fatal(err)
	}

	packed := make([]byte, 12)
	binary.LittleEndian.PutUint32(packed[0:], 15<<27|256<<18|128<<9|64)
	binary.LittleEndian.PutUint32(packed[4:], 15<<27|64<<18|128<<9|256)
	// A shared exponent must preserve a lone red mantissa too. This is the
	// transfer shape produced by glTexImage(GL_RGB9_E5, GL_RED, ...).
	binary.LittleEndian.PutUint32(packed[8:], 15<<27|72)
	if err := host.transferToHost(&resource{description: source, data: packed}, virtio.GPUTransfer3D{
		ResourceID: source.ID, Stride: 12,
		Box: virtio.GPUBox{Width: 3, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		// Leave an unrelated context-global GL error pending. The checked
		// shared-exponent transfer below must not misattribute it to readback.
		host.gl.bindTexture(^uint32(0), 0)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	blit := make([]uint32, 21)
	blit[0] = 0xf
	blit[3] = destination.ID
	blit[5] = destination.Format
	blit[9], blit[10], blit[11] = 3, 1, 1
	blit[12] = source.ID
	blit[14] = source.Format
	blit[18], blit[19], blit[20] = 3, 1, 1
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: destination, data: make([]byte, 3*4*4)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: destination.ID, Stride: 3 * 4 * 4,
		Box: virtio.GPUBox{Width: 3, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	want := []float32{
		0.125, 0.25, 0.5, 1,
		0.5, 0.25, 0.125, 1,
		0.140625, 0, 0, 1,
	}
	for index, expected := range want {
		got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data[index*4:]))
		if got != expected {
			t.Fatalf("destination component %d = %g, want %g (RGBA32F bytes %x)", index, got, expected, readback.data)
		}
	}
}

func TestBlitConvertsSharedExponent3DTextureSliceToRGBA32F(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	source := virtio.GPUResource3D{
		ID: 1, Target: 3, Format: virglFormatR9G9B9E5Float,
		Width: 2, Height: 1, Depth: 2, ArraySize: 1,
	}
	destination := virtio.GPUResource3D{
		ID: 2, Target: 2, Format: virglFormatR32G32B32A32Float,
		Width: 2, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(source); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(destination); err != nil {
		t.Fatal(err)
	}

	packed := make([]byte, 16)
	// The first slice deliberately contains different values so the assertion
	// also verifies that the VirGL source layer selects the requested 3D slice.
	binary.LittleEndian.PutUint32(packed[0:], 15<<27|1<<18|2<<9|3)
	binary.LittleEndian.PutUint32(packed[4:], 15<<27|4<<18|5<<9|6)
	binary.LittleEndian.PutUint32(packed[8:], 15<<27|256<<18|128<<9|64)
	binary.LittleEndian.PutUint32(packed[12:], 15<<27|64<<18|128<<9|256)
	if err := host.transferToHost(&resource{description: source, data: packed}, virtio.GPUTransfer3D{
		ResourceID: source.ID, Stride: 8, LayerStride: 8,
		Box: virtio.GPUBox{Width: 2, Height: 1, Depth: 2},
	}); err != nil {
		t.Fatal(err)
	}

	blit := make([]uint32, 21)
	blit[0] = 0xf
	blit[3] = destination.ID
	blit[5] = destination.Format
	blit[9], blit[10], blit[11] = 2, 1, 1
	blit[12] = source.ID
	blit[14] = source.Format
	blit[17] = 1
	blit[18], blit[19], blit[20] = 2, 1, 1
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: destination, data: make([]byte, 2*4*4)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: destination.ID, Stride: 2 * 4 * 4,
		Box: virtio.GPUBox{Width: 2, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	want := []float32{
		0.125, 0.25, 0.5, 1,
		0.5, 0.25, 0.125, 1,
	}
	for index, expected := range want {
		got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data[index*4:]))
		if got != expected {
			t.Fatalf("destination component %d = %g, want %g (RGBA32F bytes %x)", index, got, expected, readback.data)
		}
	}
}

func TestBlitPreservesSingleChannelTextureData(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	source := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatR8UNorm,
		Width: 3, Height: 1, Depth: 1, ArraySize: 1,
	}
	destination := source
	destination.ID = 2
	if err := host.createResource(source); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(destination); err != nil {
		t.Fatal(err)
	}
	want := []byte{255, 127, 31}
	if err := host.transferToHost(&resource{description: source, data: want}, virtio.GPUTransfer3D{
		ResourceID: source.ID, Stride: 3,
		Box: virtio.GPUBox{Width: 3, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	blit := make([]uint32, 21)
	blit[0] = 0xf
	blit[3] = destination.ID
	blit[5] = destination.Format
	blit[9], blit[10], blit[11] = 3, 1, 1
	blit[12] = source.ID
	blit[14] = source.Format
	blit[18], blit[19], blit[20] = 3, 1, 1
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: destination, data: make([]byte, 3)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: destination.ID, Stride: 3,
		Box: virtio.GPUBox{Width: 3, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if string(readback.data) != string(want) {
		t.Fatalf("R8 blit result = %v, want %v", readback.data, want)
	}
}

func TestBlitConvertsBetweenLinearAndSRGBFramebuffers(t *testing.T) {
	for _, test := range []struct {
		name                     string
		sourceFormat, destFormat uint32
		want                     byte
	}{
		{name: "encode", sourceFormat: virglFormatR8G8B8A8UNorm, destFormat: virglFormatR8G8B8A8SRGB, want: 137},
		{name: "decode", sourceFormat: virglFormatR8G8B8A8SRGB, destFormat: virglFormatR8G8B8A8UNorm, want: 13},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := newDarwinTestHost(t)
			defer host.close()
			if err := host.createContext(1); err != nil {
				t.Fatal(err)
			}
			source := virtio.GPUResource3D{ID: 1, Target: 2, Format: test.sourceFormat, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
			destination := virtio.GPUResource3D{ID: 2, Target: 2, Format: test.destFormat, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
			if err := host.createResource(source); err != nil {
				t.Fatal(err)
			}
			if err := host.createResource(destination); err != nil {
				t.Fatal(err)
			}
			if err := host.transferToHost(&resource{description: source, data: []byte{64, 64, 64, 255}}, virtio.GPUTransfer3D{
				ResourceID: source.ID, Stride: 4,
				Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
			}); err != nil {
				t.Fatal(err)
			}
			blit := make([]uint32, 21)
			blit[0] = 0xf
			blit[3], blit[5] = destination.ID, destination.Format
			blit[9], blit[10], blit[11] = 1, 1, 1
			blit[12], blit[14] = source.ID, source.Format
			blit[18], blit[19], blit[20] = 1, 1, 1
			if err := host.execute(1, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
				t.Fatal(err)
			}
			readback := &resource{description: destination, data: make([]byte, 4)}
			if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
				ResourceID: destination.ID, Stride: 4,
				Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
			}); err != nil {
				t.Fatal(err)
			}
			for channel := 0; channel < 3; channel++ {
				if difference := int(readback.data[channel]) - int(test.want); difference < -1 || difference > 1 {
					t.Fatalf("blit pixel = %v, want RGB near %d", readback.data, test.want)
				}
			}
		})
	}
}

func TestBlitInterpretsA8B8G8R8PackedOrdering(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	source := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatA8B8G8R8UNorm,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	destination := virtio.GPUResource3D{
		ID: 2, Target: 2, Format: virglFormatR32G32B32A32Float,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(source); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(destination); err != nil {
		t.Fatal(err)
	}
	// A8B8G8R8 is laid out in memory as A, B, G, R. This pixel therefore
	// represents yellow color channels with zero alpha.
	packed := []byte{0, 0, 255, 255}
	if err := host.transferToHost(&resource{description: source, data: packed}, virtio.GPUTransfer3D{
		ResourceID: source.ID, Stride: 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	blit := make([]uint32, 21)
	blit[0] = 0xf
	blit[3] = destination.ID
	blit[5] = destination.Format
	blit[9], blit[10], blit[11] = 1, 1, 1
	blit[12] = source.ID
	blit[14] = source.Format
	blit[18], blit[19], blit[20] = 1, 1, 1
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: destination, data: make([]byte, 4*4)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: destination.ID, Stride: 4 * 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	want := []float32{1, 1, 0, 0}
	for index, expected := range want {
		got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data[index*4:]))
		if got != expected {
			t.Fatalf("destination component %d = %g, want %g (RGBA32F bytes %x)", index, got, expected, readback.data)
		}
	}
}

func TestScissoredBlitPreservesDestinationOutsideRectangle(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	source := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 1}
	destination := virtio.GPUResource3D{ID: 2, Target: 2, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 1}
	depthSource := virtio.GPUResource3D{ID: 3, Target: 2, Format: 19, Width: 2, Height: 2, Depth: 1, ArraySize: 1}
	depthDestination := virtio.GPUResource3D{ID: 4, Target: 2, Format: 19, Width: 2, Height: 2, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{source, destination, depthSource, depthDestination} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	upload := func(description virtio.GPUResource3D, pixel []byte) {
		data := make([]byte, 2*2*4)
		for offset := 0; offset < len(data); offset += 4 {
			copy(data[offset:], pixel)
		}
		if err := host.transferToHost(&resource{description: description, data: data}, virtio.GPUTransfer3D{
			ResourceID: description.ID, Box: virtio.GPUBox{Width: 2, Height: 2, Depth: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}
	upload(source, []byte{0, 255, 0, 255})
	upload(destination, []byte{0, 0, 0, 255})

	blit := make([]uint32, 21)
	blit[0] = 0xf | 1<<10 | 1<<11
	blit[2] = 1 | 1<<16
	blit[3] = destination.ID
	blit[9], blit[10], blit[11] = 2, 2, 1
	blit[12] = source.ID
	blit[18], blit[19], blit[20] = 2, 2, 1
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}
	depthBlit := append([]uint32(nil), blit...)
	depthBlit[0] = 0x30 | 1<<10 | 1<<11
	depthBlit[3] = depthDestination.ID
	depthBlit[12] = depthSource.ID
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: depthBlit}}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: destination, data: make([]byte, 2*2*4)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: destination.ID, Box: virtio.GPUBox{Width: 2, Height: 2, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	green, black := 0, 0
	for offset := 0; offset < len(readback.data); offset += 4 {
		pixel := readback.data[offset : offset+4]
		switch string(pixel) {
		case string([]byte{0, 255, 0, 255}):
			green++
		case string([]byte{0, 0, 0, 255}):
			black++
		default:
			t.Fatalf("destination pixel %d RGBA = %v", offset/4, pixel)
		}
	}
	if green != 1 || black != 3 {
		t.Fatalf("scissored destination contains %d green and %d black pixels, want 1 and 3; bytes = %v", green, black, readback.data)
	}
}

func TestSampleShadingStateUsesBoundFramebufferSampleCount(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1, Samples: 4,
	}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 1, Object: 2, Payload: []uint32{50, 1<<25 | 1<<31, math.Float32bits(1), 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{50}},
		{Opcode: 24, Payload: []uint32{0b0101}},
		{Opcode: 33, Payload: []uint32{2}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if !host.gl.isEnabled(glMultisample) || !host.gl.isEnabled(glSampleMask) || !host.gl.isEnabled(glSampleShading) {
			return errors.New("sample shading rasterizer switches are not enabled")
		}
		var value float32
		host.gl.getFloatv(glMinSampleShadingValue, &value)
		if value != 0.5 {
			return fmt.Errorf("minimum sample shading = %g, want 0.5", value)
		}
		if code := host.gl.getError(); code != 0 {
			return fmt.Errorf("host GL error after sample shading state: %#x", code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFramebufferClearTemporarilyDisablesRasterizerDiscard(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 1, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	clear := func(red, green, blue float32) command {
		return command{Opcode: 7, Payload: []uint32{
			0x4,
			math.Float32bits(red),
			math.Float32bits(green),
			math.Float32bits(blue),
			math.Float32bits(1),
		}}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		clear(0, 0, 1),
		{Opcode: 1, Object: 2, Payload: []uint32{12, 1 << 3, math.Float32bits(1), 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{12}},
		clear(1, 0, 0),
	}, nil); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("clear with stale rasterizer discard BGRA = %v, want %v", got, want)
	}
}

func TestFramebufferClearUpdatesEveryColorTarget(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	first := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	second := virtio.GPUResource3D{ID: 2, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{first, second} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, first.ID}},
		{Opcode: 1, Object: 8, Payload: []uint32{12, second.ID}},
		{Opcode: 5, Payload: []uint32{2, 0, 11, 12}},
		{Opcode: 7, Payload: []uint32{
			0xc, math.Float32bits(1), math.Float32bits(0.25), math.Float32bits(0.5), math.Float32bits(1),
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	want := []byte{128, 64, 255, 255}
	for _, description := range []virtio.GPUResource3D{first, second} {
		pixels, _, err := host.readScanout(&resource{description: description}, image.Rect(0, 0, 1, 1))
		if err != nil {
			t.Fatal(err)
		}
		if string(pixels) != string(want) {
			t.Fatalf("color target %d BGRA = %v, want %v", description.ID, pixels, want)
		}
	}
}

func TestFramebufferClearUpdatesOnlySelectedColorTarget(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	first := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	second := virtio.GPUResource3D{ID: 2, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{first, second} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, first.ID}},
		{Opcode: 1, Object: 8, Payload: []uint32{12, second.ID}},
		{Opcode: 5, Payload: []uint32{2, 0, 11, 12}},
		{Opcode: 7, Payload: []uint32{0xc, 0, 0, math.Float32bits(1), math.Float32bits(1)}},
		{Opcode: 7, Payload: []uint32{0x8, math.Float32bits(1), 0, 0, math.Float32bits(1)}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		description virtio.GPUResource3D
		want        []byte
	}{
		{description: first, want: []byte{255, 0, 0, 255}},
		{description: second, want: []byte{0, 0, 255, 255}},
	} {
		pixels, _, err := host.readScanout(&resource{description: test.description}, image.Rect(0, 0, 1, 1))
		if err != nil {
			t.Fatal(err)
		}
		if string(pixels) != string(test.want) {
			t.Fatalf("color target %d BGRA = %v, want %v", test.description.ID, pixels, test.want)
		}
	}
}

func TestArrayTextureLayersCanBeRenderedAndReadBack(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	description := virtio.GPUResource3D{
		ID: 1, Target: 7, Format: 67,
		Width: 1, Height: 1, Depth: 1, ArraySize: 2,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	clear := func(red, green float32) command {
		return command{Opcode: 7, Payload: []uint32{
			0x4, math.Float32bits(red), math.Float32bits(green), 0, math.Float32bits(1),
		}}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, description.ID, description.Format, 0, 0}},
		{Opcode: 1, Object: 8, Payload: []uint32{12, description.ID, description.Format, 0, 1 | 1<<16}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		clear(1, 0),
		{Opcode: 5, Payload: []uint32{1, 0, 12}},
		clear(0, 1),
	}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: description, data: make([]byte, 8)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID:  description.ID,
		Stride:      4,
		LayerStride: 4,
		Box:         virtio.GPUBox{Width: 1, Height: 1, Depth: 2},
	}); err != nil {
		t.Fatal(err)
	}
	want := []byte{255, 0, 0, 255, 0, 255, 0, 255}
	if string(readback.data) != string(want) {
		t.Fatalf("array texture layer pixels = %v, want %v", readback.data, want)
	}
}

func TestUniformBufferSuppliesFragmentConstants(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24}
	uniforms := virtio.GPUResource3D{ID: 3, Target: 0, Width: 16}
	for _, description := range []virtio.GPUResource3D{color, positions, uniforms} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	floatBytes := func(values ...float32) []byte {
		data := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(data[index*4:], math.Float32bits(value))
		}
		return data
	}
	for _, upload := range []struct {
		description virtio.GPUResource3D
		data        []byte
	}{
		{positions, floatBytes(-1, -1, 3, -1, -1, 3)},
		{uniforms, floatBytes(1, 0.25, 0.5, 1)},
	} {
		if err := host.transferToHost(&resource{description: upload.description, data: upload.data}, virtio.GPUTransfer3D{
			ResourceID: upload.description.ID,
			Box:        virtio.GPUBox{Width: upload.description.Width},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 6, Payload: []uint32{8, 0, positions.ID}},
		{Opcode: 27, Payload: []uint32{tgsiFragment, 1, 0, 16, uniforms.ID}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11
	context.vertexElements[12] = []hostVertexElement{{bufferIndex: 0, format: 29}}
	context.boundVertexElements = 12
	_, vertexSource, err := translateTGSI(`VERT
DCL IN[0]
DCL OUT[0], POSITION
  0: MOV OUT[0], IN[0]
  1: END`)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentSource, err := translateTGSI(`FRAG
DCL CONST[1][0]
DCL OUT[0], COLOR
  0: MOV OUT[0], CONST[1][0]
  1: END`)
	if err != nil {
		t.Fatal(err)
	}
	context.shaders[20] = hostShader{stage: tgsiVertex, source: vertexSource}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: fragmentSource}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		return host.draw(context, []uint32{0, 3, 4, 0})
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{128, 64, 255, 255}
	if string(pixels) != string(want) {
		t.Fatalf("uniform-buffer draw BGRA = %v, want %v", pixels, want)
	}
}

func TestLargeUniformOnlyFragmentShaderProducesIntegerResult(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: virglFormatR32SInt, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	uniforms := virtio.GPUResource3D{ID: 2, Target: 0, Width: capsetMaxUniformBlockSize}
	for _, description := range []virtio.GPUResource3D{color, uniforms} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	uniformData := make([]byte, capsetMaxUniformBlockSize)
	for index := 0; index < capsetMaxUniformBlockSize/16; index++ {
		bits := math.Float64bits(float64(index + 1))
		binary.LittleEndian.PutUint64(uniformData[index*16:], bits)
	}
	if err := host.transferToHost(&resource{description: uniforms, data: uniformData}, virtio.GPUTransfer3D{
		ResourceID: uniforms.ID,
		Box:        virtio.GPUBox{Width: uniforms.Width},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 27, Payload: []uint32{tgsiFragment, 1, 0, uniforms.Width, uniforms.ID}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	const fragmentTGSI = `FRAG
DCL OUT[0], COLOR
DCL CONST[1][0..4095]
DCL TEMP[0..7]
DCL ADDR[0]
IMM[0] UINT32 {0, 1, 268435455, 2}
IMM[1] UINT32 {4096, 0, 0, 0}
DCL TEMP[8..11]
0: MOV TEMP[1].x, IMM[0].xxxx
1: MOV TEMP[2].x, IMM[0].yyyy
2: BGNLOOP :0
3: UADD TEMP[0].x, TEMP[1].xxxx, IMM[0].yyyy
4: I2D TEMP[3].xy, TEMP[0].xxxx
5: AND TEMP[4].x, TEMP[1].xxxx, IMM[0].zzzz
6: UARL ADDR[0].x, TEMP[4].xxxx
7: MOV TEMP[5].xy, CONST[1][ADDR[0].x].xyyy
8: MOV TEMP[8].xy, TEMP[3].xyxy
9: MOV TEMP[9].xy, TEMP[5].xyxy
10: DSNE TEMP[6].x, TEMP[8], TEMP[9]
11: UCMP TEMP[2].x, TEMP[6].xxxx, IMM[0].wwww, TEMP[2]
12: ISGE TEMP[7].x, TEMP[0].xxxx, IMM[1].xxxx
13: UIF TEMP[7].xxxx :13
14: BRK
15: ENDIF
16: MOV TEMP[1].x, TEMP[0].xxxx
17: ENDLOOP :0
18: MOV OUT[0].x, TEMP[2].xxxx
19: END`
	_, fragmentSource, err := translateTGSI(fragmentTGSI)
	if err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID], format: color.Format}
	context.colorSurfaces[0] = 11
	context.shaders[20] = hostShader{stage: tgsiVertex, tgsi: "VERT\n", source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, tgsi: fragmentTGSI, source: fragmentSource}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	var pixel [4]byte
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glRedInteger, glInt, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := int32(binary.LittleEndian.Uint32(pixel[:])); got != 1 {
		t.Fatalf("large uniform-only fragment result = %d, want 1", got)
	}
}

func TestEmptyConstantBufferClearForUnadvertisedStageIsAccepted(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{Opcode: 12, Payload: []uint32{2, 0}}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestConstantBuffersAreTrackedByProtocolIndex(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	value := math.Float32bits(640)
	commands := []command{
		{Opcode: 12, Payload: []uint32{tgsiVertex, 0, value}},
		{Opcode: 12, Payload: []uint32{tgsiVertex, 1}},
	}
	if err := host.execute(contextID, commands, nil); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	if got := context.constants[tgsiVertex][0]; len(got) != 1 || got[0] != 640 {
		t.Fatalf("vertex constant buffer 0 = %v, want [640]", got)
	}
	if got := context.constants[tgsiVertex][1]; len(got) != 0 {
		t.Fatalf("vertex constant buffer 1 = %v, want empty", got)
	}
}

func TestExplicitShaderLinkValidatesTheCompleteStageSet(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.shaders[10] = hostShader{stage: tgsiVertex}
	context.shaders[11] = hostShader{stage: tgsiFragment}
	if err := host.execute(contextID, []command{{Opcode: 52, Payload: []uint32{10, 11, 0, 0, 0, 0}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{Opcode: 52, Payload: []uint32{11, 10, 0, 0, 0, 0}}}, nil); err == nil {
		t.Fatal("shader link accepted swapped vertex and fragment stages")
	}
}

func TestClearIgnoresBoundDepthWriteMask(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 1, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	depth := virtio.GPUResource3D{ID: 2, Target: 2, Format: 21, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(depth); err != nil {
		t.Fatal(err)
	}

	clearDepth := func(value float64) command {
		bits := math.Float64bits(value)
		return command{Opcode: 7, Payload: []uint32{
			0x1, 0, 0, 0, 0, uint32(bits), uint32(bits >> 32),
		}}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, color.ID}},
		{Opcode: 1, Object: 8, Payload: []uint32{12, depth.ID}},
		{Opcode: 5, Payload: []uint32{1, 12, 11}},
		{Opcode: 1, Object: 3, Payload: []uint32{13, 0x2, 0, 0, 0}},
		{Opcode: 2, Object: 3, Payload: []uint32{13}},
		clearDepth(0.75),
		{Opcode: 1, Object: 3, Payload: []uint32{14, 0, 0, 0, 0}},
		{Opcode: 2, Object: 3, Payload: []uint32{14}},
		clearDepth(0.25),
	}, nil); err != nil {
		t.Fatal(err)
	}

	var raw [4]byte
	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(host.contexts[contextID]); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glDepthComponent, glFloat, glPointer(raw[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := math.Float32frombits(binary.LittleEndian.Uint32(raw[:]))
	if math.Abs(float64(got-0.25)) > 0.001 {
		t.Fatalf("depth after masked clear = %g, want 0.25", got)
	}
}

func TestClearIgnoresBoundStencilWriteMask(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 1, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	stencilOnly := virtio.GPUResource3D{ID: 2, Target: 2, Format: virglFormatS8UInt, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(stencilOnly); err != nil {
		t.Fatal(err)
	}

	// Stencil is enabled with an all-zero write mask. A Gallium clear must
	// still update every stencil bit and restore the mask afterwards.
	stencil := uint32(1 | (7 << 1) | (0xff << 13))
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, color.ID}},
		{Opcode: 1, Object: 8, Payload: []uint32{12, stencilOnly.ID}},
		{Opcode: 5, Payload: []uint32{1, 12, 11}},
		{Opcode: 1, Object: 3, Payload: []uint32{13, 0, stencil, 0, 0}},
		{Opcode: 2, Object: 3, Payload: []uint32{13}},
		{Opcode: 7, Payload: []uint32{0x2, 0, 0, 0, 0, 0, 0, 0x7b}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	var raw [1]byte
	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(host.contexts[contextID]); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glStencilIndex, glUnsignedByte, glPointer(raw[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if raw[0] != 0x7b {
		t.Fatalf("stencil after masked clear = %#x, want 0x7b", raw[0])
	}
}

func TestLogicalStencil8ControlsAColorDraw(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	stencil := virtio.GPUResource3D{ID: 2, Target: 2, Format: virglFormatS8UInt, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 3, Target: 0, Width: 24}
	for _, description := range []virtio.GPUResource3D{color, stencil, positions} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	positionBytes := make([]byte, positions.Width)
	// Clockwise order exercises the implicit back-face state. With separate
	// back-face stencil disabled, Gallium requires the complete front state,
	// including its reference value, to apply to this draw.
	for index, value := range []float32{-1, -1, -1, 3, 3, -1} {
		binary.LittleEndian.PutUint32(positionBytes[index*4:], math.Float32bits(value))
	}
	if err := host.transferToHost(&resource{description: positions, data: positionBytes}, virtio.GPUTransfer3D{
		ResourceID: positions.ID,
		Box:        virtio.GPUBox{Width: positions.Width},
	}); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.surfaces[12] = hostSurface{resourceID: stencil.ID, resource: host.resources[stencil.ID]}
	context.colorSurfaces[0] = 11
	context.depthSurface = 12
	context.vertexElements[13] = []hostVertexElement{{bufferIndex: 0, format: 29}}
	context.boundVertexElements = 13
	context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: positions.ID, resource: host.resources[positions.ID]}
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
void main() { gl_Position = vec4(position, 0.0, 1.0); }`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(1.0, 0.0, 0.0, 1.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	// Write 0x7b through the stencil z-pass operation, clear only color, then
	// require that exact stencil value for the visible draw. This exercises the
	// operation ordering used by dEQP's stencil-clear verification cases.
	writeStencilState := uint32(1 | (7 << 1) | (2 << 7) | (0xff << 13) | (0xff << 21))
	compareStencilState := uint32(1 | (2 << 1) | (0xff << 13) | (0xff << 21))
	context.depthStencilAlpha[14] = hostDepthStencilAlpha{stencil: [2]uint32{writeStencilState}}
	context.boundDSA = 14
	context.stencilRef = [2]uint8{0x7b, 0}
	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		host.gl.viewport(0, 0, 1, 1)
		host.gl.frontFace(glCCW)
		host.gl.clearColor(0, 0, 0, 1)
		host.gl.clearStencil(0)
		host.gl.stencilMaskSeparate(glFrontAndBack, 0xff)
		host.gl.clear(glColorBufferBit | glStencilBufferBit)
		host.applyDepthStencilAlpha(context, context.depthStencilAlpha[14])
		if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
			return err
		}
		host.gl.clear(glColorBufferBit)
		context.depthStencilAlpha[14] = hostDepthStencilAlpha{stencil: [2]uint32{compareStencilState}}
		context.stencilRef = [2]uint8{0x7b, 0x7b}
		host.applyDepthStencilAlpha(context, context.depthStencilAlpha[14])
		return host.draw(context, []uint32{0, 3, 4, 0})
	}); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("stencil-tested color draw BGRA = %v, want %v", got, want)
	}
}

func TestBlendAndScissorAffectRenderedPixels(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 1, Width: 2, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	clear := func(red, green, blue float32) command {
		return command{Opcode: 7, Payload: []uint32{
			0x4,
			math.Float32bits(red),
			math.Float32bits(green),
			math.Float32bits(blue),
			math.Float32bits(1),
		}}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 4, Payload: []uint32{
			0,
			math.Float32bits(1), math.Float32bits(0.5), math.Float32bits(1),
			math.Float32bits(1), math.Float32bits(0.5), math.Float32bits(0),
		}},
		clear(0, 0, 1),
	}, nil); err != nil {
		t.Fatal(err)
	}

	drawColor := func(red, green, blue, alpha float32) {
		t.Helper()
		vertex := `#version 150
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
		fragment := fmt.Sprintf(`#version 150
out vec4 result;
void main() { result = vec4(%g, %g, %g, %g); }`, red, green, blue, alpha)
		if err := host.dispatch(func() error {
			program, err := host.gl.compileProgram(vertex, fragment)
			if err != nil {
				return err
			}
			defer host.gl.deleteProgram(program)
			if err := host.bindContextFramebuffer(host.contexts[contextID]); err != nil {
				return err
			}
			host.gl.useProgram(program)
			host.gl.bindVertexArray(host.vao)
			host.gl.drawArrays(glTriangles, 0, 3)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Source-alpha red over blue must produce 25% red and 75% blue.
	renderTarget := uint32(1 | (3 << 4) | (0x13 << 9) | (1 << 17) | (0x11 << 22) | (0xf << 27))
	blendPayload := []uint32{12, 0, 0, renderTarget, 0, 0, 0, 0, 0, 0, 0}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 1, Payload: blendPayload},
		{Opcode: 2, Object: 1, Payload: []uint32{12}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	drawColor(1, 0, 0, 0.25)
	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	for pixel := 0; pixel < 2; pixel++ {
		got := pixels[pixel*4 : pixel*4+4]
		want := []byte{191, 0, 64, 64}
		for channel := range want {
			if difference := int(got[channel]) - int(want[channel]); difference < -1 || difference > 1 {
				t.Fatalf("blended pixel %d BGRA = %v, want approximately %v", pixel, got, want)
			}
		}
	}

	// With rasterizer scissoring enabled, only the left pixel is replaced.
	if err := host.execute(contextID, []command{
		{Opcode: 2, Object: 1, Payload: []uint32{0}},
		{Opcode: 1, Object: 2, Payload: []uint32{13, 1 << 14, 0, 0, 0, 0, 0, 0, 0}},
		{Opcode: 15, Payload: []uint32{0, 0, 1 | (1 << 16)}},
		{Opcode: 2, Object: 2, Payload: []uint32{13}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	drawColor(0, 1, 0, 1)
	pixels, _, err = host.readScanout(&resource{description: color}, image.Rect(0, 0, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels[:4], []byte{0, 255, 0, 255}; string(got) != string(want) {
		t.Fatalf("scissored left pixel BGRA = %v, want %v", got, want)
	}
	wantRight := []byte{191, 0, 64, 64}
	for channel := range wantRight {
		if difference := int(pixels[4+channel]) - int(wantRight[channel]); difference < -1 || difference > 1 {
			t.Fatalf("scissored right pixel BGRA = %v, want approximately %v", pixels[4:8], wantRight)
		}
	}
}

func TestIncompleteFramebufferDrawDoesNotAbortFollowingCommands(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 8, Payload: []uint32{0, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0}},
		{Opcode: 1, Object: 8, Payload: []uint32{11, color.ID, color.Format, 0, 0}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 7, Payload: []uint32{1 << 2, math.Float32bits(1), 0, 0, math.Float32bits(1)}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("post-error clear BGRA = %v, want %v", got, want)
	}
}

func TestRasterizerWindingAccountsForHostFramebufferOrigin(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 4, Payload: []uint32{
			0,
			math.Float32bits(0.5), math.Float32bits(0.5), math.Float32bits(1),
			math.Float32bits(0.5), math.Float32bits(0.5), math.Float32bits(0),
		}},
		// Cull back faces with Gallium's clockwise front face.
		{Opcode: 1, Object: 2, Payload: []uint32{12, 2 << 8, 0, 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{12}},
		{Opcode: 7, Payload: []uint32{0x4, 0, 0, 0, math.Float32bits(1)}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 150
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
	fragment := `#version 150
out vec4 result;
void main() { result = vec4(1.0, 0.0, 0.0, 1.0); }`
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		if err := host.bindContextFramebuffer(host.contexts[contextID]); err != nil {
			return err
		}
		host.gl.viewport(0, 0, 1, 1)
		host.gl.useProgram(program)
		host.gl.bindVertexArray(host.vao)
		host.gl.drawArrays(glTriangles, 0, 3)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("front-facing Gallium triangle BGRA = %v, want %v", got, want)
	}
}

func TestDrawDisablesRetiredVertexAttributes(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 32}
	coordinates := virtio.GPUResource3D{ID: 3, Target: 0, Width: 32}
	for _, resource := range []virtio.GPUResource3D{color, positions, coordinates} {
		if err := host.createResource(resource); err != nil {
			t.Fatal(err)
		}
	}
	floatBytes := func(values ...float32) []byte {
		result := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
		}
		return result
	}
	if err := host.dispatch(func() error {
		for id, data := range map[uint32][]byte{
			positions.ID:   floatBytes(-1, -1, 1, -1, -1, 1, 1, 1),
			coordinates.ID: floatBytes(0, 0, 1, 0, 0, 1, 1, 1),
		} {
			host.gl.bindBuffer(glArrayBuffer, host.resources[id].buffer)
			host.gl.bufferSubData(glArrayBuffer, 0, len(data), glPointer(data))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11
	context.vertexElements[12] = []hostVertexElement{
		{bufferIndex: 0, format: 29},
		{bufferIndex: 1, format: 29},
	}
	context.boundVertexElements = 12
	context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: positions.ID, resource: host.resources[positions.ID]}
	context.vertexBuffers[1] = hostVertexBuffer{stride: 8, resourceID: coordinates.ID, resource: host.resources[coordinates.ID]}
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
layout(location = 1) in vec2 coordinate;
out vec2 varyingCoordinate;
void main() {
	gl_Position = vec4(position, 0.0, 1.0);
	varyingCoordinate = coordinate;
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
in vec2 varyingCoordinate;
layout(location = 0) out vec4 color;
void main() { color = vec4(1.0, varyingCoordinate.x * 0.0, 0.0, 1.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		host.gl.viewport(0, 0, 1, 1)
		host.gl.clearColor(0, 0, 0, 1)
		host.gl.clear(glColorBufferBit)

		// Model the preceding draw having one more array than this draw. Once
		// Mesa retires that buffer, leaving attribute 2 enabled makes macOS
		// reject the otherwise valid draw with GL_INVALID_OPERATION.
		var retired uint32
		host.gl.genBuffers(1, &retired)
		host.gl.bindVertexArray(host.vao)
		host.gl.bindBuffer(glArrayBuffer, retired)
		host.gl.bufferData(glArrayBuffer, 4, 0, glStaticDraw)
		host.gl.vertexAttribPtr(2, 1, glFloat, false, 4, 0)
		host.gl.enableVertexAttrib(2)
		host.gl.deleteBuffers(1, &retired)
		host.enabledVertexAttributes = 0x7

		return host.draw(context, []uint32{0, 4, 5, 0})
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("draw after retiring an unused vertex attribute BGRA = %v, want %v", got, want)
	}
}

func TestZeroStrideVertexBufferSuppliesAConstantAttribute(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24}
	constant := virtio.GPUResource3D{ID: 3, Target: 0, Width: 8}
	for _, description := range []virtio.GPUResource3D{color, positions, constant} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	floatBytes := func(values ...float32) []byte {
		result := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
		}
		return result
	}
	for _, upload := range []struct {
		description virtio.GPUResource3D
		data        []byte
	}{
		{positions, floatBytes(-1, -1, 3, -1, -1, 3)},
		{constant, floatBytes(0, 1)},
	} {
		if err := host.transferToHost(&resource{description: upload.description, data: upload.data}, virtio.GPUTransfer3D{
			ResourceID: upload.description.ID,
			Box:        virtio.GPUBox{Width: upload.description.Width},
		}); err != nil {
			t.Fatal(err)
		}
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11
	context.vertexElements[12] = []hostVertexElement{
		{bufferIndex: 0, format: 29},
		{bufferIndex: 1, offset: 4, format: 28},
	}
	context.boundVertexElements = 12
	context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: positions.ID, resource: host.resources[positions.ID]}
	context.vertexBuffers[1] = hostVertexBuffer{stride: 0, resourceID: constant.ID, resource: host.resources[constant.ID]}
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
layout(location = 1) in float constantValue;
out vec4 value;
void main() {
	gl_Position = vec4(position, 0.0, 1.0);
	value = vec4(constantValue);
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
in vec4 value;
layout(location = 0) out vec4 color;
void main() { color = vec4(value.x, 0.0, 0.0, 1.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		return host.draw(context, []uint32{0, 3, 4, 0})
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("zero-stride constant attribute BGRA = %v, want %v", got, want)
	}
}

func TestPointSpriteCoordinateModeControlsVerticalOrigin(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 1}
	position := virtio.GPUResource3D{ID: 2, Target: 0, Width: 8}
	for _, description := range []virtio.GPUResource3D{color, position} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	positionBytes := make([]byte, 8)
	if err := host.transferToHost(&resource{description: position, data: positionBytes}, virtio.GPUTransfer3D{
		ResourceID: position.ID,
		Box:        virtio.GPUBox{Width: position.Width},
	}); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11
	context.vertexElements[12] = []hostVertexElement{{bufferIndex: 0, format: 29}}
	context.boundVertexElements = 12
	context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: position.ID, resource: host.resources[position.ID]}
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
void main() {
	gl_Position = vec4(position, 0.0, 1.0);
	gl_PointSize = 2.0;
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(0.0, gl_PointCoord.y, 0.0, 1.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	render := func(state uint32) [16]byte {
		t.Helper()
		var pixels [16]byte
		if err := host.dispatch(func() error {
			rasterizer := hostRasterizer{state: state}
			context.rasterizers[13] = rasterizer
			context.boundRasterizer = 13
			host.applyRasterizer(context, rasterizer)
			host.gl.viewport(0, 0, 2, 2)
			host.gl.clearColor(0, 0, 0, 1)
			host.gl.clear(glColorBufferBit)
			if err := host.draw(context, []uint32{0, 1, 0, 0}); err != nil {
				return err
			}
			host.gl.readPixels(0, 0, 2, 2, glRGBA, glUnsignedByte, glPointer(pixels[:]))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return pixels
	}

	const pointQuadAndPerVertexSize = uint32(1<<7 | 1<<24)
	lowerLeft := render(pointQuadAndPerVertexSize)
	if bottom, top := lowerLeft[1], lowerLeft[9]; bottom >= top {
		t.Fatalf("lower-left point coordinates have bottom/top green %d/%d, want bottom < top", bottom, top)
	}
	upperLeft := render(pointQuadAndPerVertexSize | 1<<6)
	if bottom, top := upperLeft[1], upperLeft[9]; bottom <= top {
		t.Fatalf("upper-left point coordinates have bottom/top green %d/%d, want bottom > top", bottom, top)
	}
}

func TestRasterizerDepthClipBitControlsDepthClamping(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11

	rasterizerPayload := func(handle, state uint32) []uint32 {
		return []uint32{handle, state, math.Float32bits(1), 0, 0, 0, 0, 0, 0}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 2, Payload: rasterizerPayload(20, 1<<1)}, // depth clip
		{Opcode: 1, Object: 2, Payload: rasterizerPayload(21, 0)},    // depth clamp
	}, nil); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 150
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 2.0, 1.0);
}`
	fragment := `#version 150
out vec4 result;
void main() { result = vec4(1.0, 0.0, 0.0, 1.0); }`
	var program uint32
	if err := host.dispatch(func() error {
		var err error
		program, err = host.gl.compileProgram(vertex, fragment)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer host.dispatch(func() error { host.gl.deleteProgram(program); return nil })

	render := func(rasterizer uint32) [4]byte {
		t.Helper()
		if err := host.execute(contextID, []command{{Opcode: 2, Object: 2, Payload: []uint32{rasterizer}}}, nil); err != nil {
			t.Fatal(err)
		}
		var pixel [4]byte
		if err := host.dispatch(func() error {
			if err := host.bindContextFramebuffer(context); err != nil {
				return err
			}
			host.gl.viewport(0, 0, 1, 1)
			host.gl.clearColor(0, 0, 0, 1)
			host.gl.clear(glColorBufferBit)
			host.gl.useProgram(program)
			host.gl.bindVertexArray(host.vao)
			host.gl.drawArrays(glTriangles, 0, 3)
			host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return pixel
	}

	if clipped := render(20); clipped[0] != 0 {
		t.Fatalf("depth-clipped pixel = %v, want clear color", clipped)
	}
	if clamped := render(21); clamped[0] < 250 {
		t.Fatalf("depth-clamped pixel = %v, want rendered red", clamped)
	}
}

func TestRasterizerClipPlaneMaskControlsShaderClipDistance(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11

	rasterizerPayload := func(handle uint32, clipMask uint8) []uint32 {
		return []uint32{handle, 1 << 1, math.Float32bits(1), 0, uint32(clipMask) << 24, 0, 0, 0, 0}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 2, Payload: rasterizerPayload(20, 0)},
		{Opcode: 1, Object: 2, Payload: rasterizerPayload(21, 1)},
	}, nil); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
	gl_ClipDistance[0] = -1.0;
}`
	fragment := `#version 410 core
out vec4 result;
void main() { result = vec4(1.0, 0.0, 0.0, 1.0); }`
	var program uint32
	if err := host.dispatch(func() error {
		var err error
		program, err = host.gl.compileProgram(vertex, fragment)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	defer host.dispatch(func() error { host.gl.deleteProgram(program); return nil })

	render := func(rasterizer uint32) [4]byte {
		t.Helper()
		if err := host.execute(contextID, []command{{Opcode: 2, Object: 2, Payload: []uint32{rasterizer}}}, nil); err != nil {
			t.Fatal(err)
		}
		var pixel [4]byte
		if err := host.dispatch(func() error {
			if err := host.bindContextFramebuffer(context); err != nil {
				return err
			}
			host.gl.viewport(0, 0, 1, 1)
			host.gl.clearColor(0, 0, 0, 1)
			host.gl.clear(glColorBufferBit)
			host.gl.useProgram(program)
			host.gl.bindVertexArray(host.vao)
			host.gl.drawArrays(glTriangles, 0, 3)
			host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return pixel
	}

	if unmasked := render(20); unmasked[0] < 250 {
		t.Fatalf("disabled clip distance pixel = %v, want rendered red", unmasked)
	}
	if masked := render(21); masked[0] != 0 {
		t.Fatalf("enabled negative clip distance pixel = %v, want clear color", masked)
	}
}

func TestTranslatedClipDistancesReachFragmentShader(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: virglFormatR8G8B8A8UNorm, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11

	vertexTGSI := `VERT
DCL OUT[0], POSITION
DCL OUT[1], CLIPDIST
DCL OUT[2], CLIPDIST[1]
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
IMM[1] FLT32 {0.25, 0.5, 0.75, 1.0}
0: MOV OUT[0], IMM[0]
1: MOV OUT[1], IMM[1]
2: MOV OUT[2], IMM[1]
3: END
`
	fragmentTGSI := `FRAG
DCL IN[0], CLIPDIST, PERSPECTIVE
DCL IN[1], CLIPDIST[1], PERSPECTIVE
DCL OUT[0], COLOR
0: MOV OUT[0], IN[0]
1: END
`
	_, vertex, err := translateTGSI(vertexTGSI)
	if err != nil {
		t.Fatal(err)
	}
	_, fragment, err := translateTGSI(fragmentTGSI)
	if err != nil {
		t.Fatal(err)
	}
	vertex = linkTGSIInterfaces(vertex, fragment)
	var program uint32
	if err := host.dispatch(func() error {
		program, err = host.gl.compileProgram(vertex, fragment)
		return err
	}); err != nil {
		t.Fatalf("compile translated clip-distance shaders: %v\nvertex:\n%s\nfragment:\n%s", err, vertex, fragment)
	}
	defer host.dispatch(func() error { host.gl.deleteProgram(program); return nil })

	for _, clipMask := range []uint8{0, 0xff} {
		var pixel [4]byte
		if err := host.dispatch(func() error {
			if err := host.bindContextFramebuffer(context); err != nil {
				return err
			}
			host.gl.viewport(0, 0, 1, 1)
			host.gl.clearColor(0, 0, 0, 1)
			host.gl.clear(glColorBufferBit)
			host.applyClipPlaneEnable(clipMask)
			host.gl.useProgram(program)
			if location := uniformLocation(host.gl, program, "uWinsysAdjustY"); location >= 0 {
				host.gl.uniform1f(location, 1)
			}
			host.gl.bindVertexArray(host.vao)
			host.gl.drawArrays(glPoints, 0, 1)
			host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		want := [4]byte{64, 128, 191, 255}
		for component := range pixel {
			if difference := int(pixel[component]) - int(want[component]); difference < -1 || difference > 1 {
				t.Fatalf("clip mask %#x fragment pixel = %v, want approximately %v\nvertex:\n%s\nfragment:\n%s", clipMask, pixel, want, vertex, fragment)
			}
		}
	}
}

func TestSRGBFramebufferEncodesFragmentColor(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatR8G8B8A8SRGB,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{
		resourceID: color.ID,
		resource:   host.resources[color.ID],
		format:     color.Format,
	}
	context.colorSurfaces[0] = 11

	const vertex = `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
	const fragment = `#version 410 core
layout(location = 0) out vec4 fragmentColor0;
void main() { fragmentColor0 = vec4(0.25, 0.25, 0.25, 1.0); }`

	var pixel [4]byte
	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		host.gl.viewport(0, 0, 1, 1)
		host.gl.useProgram(program)
		host.gl.bindVertexArray(host.vao)
		host.gl.drawArrays(glTriangles, 0, 3)
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for channel := 0; channel < 3; channel++ {
		if pixel[channel] < 135 || pixel[channel] > 139 {
			t.Fatalf("sRGB framebuffer pixel = %v, want encoded RGB near 137", pixel)
		}
	}
}

func TestTranslatedCTSClipDistanceArrayProgram(t *testing.T) {
	const vertexTGSI = `VERT
DCL OUT[0], POSITION
DCL OUT[1], CLIPDIST
DCL OUT[2], CLIPDIST[1]
DCL TEMP[0..1], ARRAY(1), LOCAL
DCL TEMP[2..24]
DCL ADDR[0]
IMM[0] UINT32 {0, 1, 1090519040, 3}
IMM[1] UINT32 {2, 1, 3, 8}
IMM[2] UINT32 {0, 1065353216, 0, 0}
DCL TEMP[25..28]
DCL TEMP[29..30]
0: MOV TEMP[13].x, IMM[0].xxxx
1: MOV TEMP[0].x, TEMP[13]
2: MOV TEMP[12].y, IMM[0].xxxx
3: MOV TEMP[0].y, TEMP[12]
4: MOV TEMP[11].z, IMM[0].xxxx
5: MOV TEMP[0].z, TEMP[11]
6: MOV TEMP[10].w, IMM[0].xxxx
7: MOV TEMP[0].w, TEMP[10]
8: MOV TEMP[9].x, IMM[0].xxxx
9: MOV TEMP[1].x, TEMP[9]
10: MOV TEMP[8].y, IMM[0].xxxx
11: MOV TEMP[1].y, TEMP[8]
12: MOV TEMP[7].z, IMM[0].xxxx
13: MOV TEMP[1].z, TEMP[7]
14: MOV TEMP[6].w, IMM[0].xxxx
15: MOV TEMP[1].w, TEMP[6]
16: MOV TEMP[15].x, IMM[0].xxxx
17: BGNLOOP :0
18: UADD TEMP[14].x, TEMP[15].xxxx, IMM[0].yyyy
19: I2F TEMP[16].x, TEMP[14].xxxx
20: DIV TEMP[17].x, TEMP[16].xxxx, IMM[0].zzzz
21: AND TEMP[18].x, TEMP[15].xxxx, IMM[0].wwww
22: ISHR TEMP[19].x, TEMP[15].xxxx, IMM[1].xxxx
23: ISLT TEMP[20].xyz, TEMP[18].xxxx, IMM[1].xyzx
24: MOV TEMP[21].x, TEMP[20].yxxx
25: MOV TEMP[22].x, TEMP[20].zxxx
26: MOV TEMP[23].x, TEMP[20].xxxx
27: UIF TEMP[23].xxxx :37
28: UIF TEMP[21].xxxx :32
29: MOV TEMP[5].x, TEMP[17].xxxx
30: UARL ADDR[0].x, TEMP[19].xxxx
31: MOV TEMP[ADDR[0].x](1).x, TEMP[5]
32: ELSE :36
33: MOV TEMP[4].y, TEMP[17].xxxx
34: UARL ADDR[0].x, TEMP[19].xxxx
35: MOV TEMP[ADDR[0].x](1).y, TEMP[4]
36: ENDIF
37: ELSE :47
38: UIF TEMP[22].xxxx :42
39: MOV TEMP[3].z, TEMP[17].xxxx
40: UARL ADDR[0].x, TEMP[19].xxxx
41: MOV TEMP[ADDR[0].x](1).z, TEMP[3]
42: ELSE :46
43: MOV TEMP[2].w, TEMP[17].xxxx
44: UARL ADDR[0].x, TEMP[19].xxxx
45: MOV TEMP[ADDR[0].x](1).w, TEMP[2]
46: ENDIF
47: ENDIF
48: ISGE TEMP[24].x, TEMP[14].xxxx, IMM[1].wwww
49: UIF TEMP[24].xxxx :51
50: BRK
51: ENDIF
52: MOV TEMP[15].x, TEMP[14].xxxx
53: ENDLOOP :0
54: MOV OUT[0], IMM[2].xxxy
55: MOV TEMP[29], TEMP[0]
56: MOV OUT[1], TEMP[29]
57: MOV TEMP[30], TEMP[1]
58: MOV OUT[2], TEMP[30]
59: END
`
	const fragmentTGSI = `FRAG
DCL IN[0], CLIPDIST, PERSPECTIVE
DCL IN[1], CLIPDIST[1], PERSPECTIVE
DCL OUT[0], COLOR
DCL TEMP[0..1], ARRAY(1), LOCAL
DCL TEMP[2..20]
DCL ADDR[0]
IMM[0] UINT32 {1, 0, 3, 2}
IMM[1] UINT32 {1090519040, 1011666125, 8, 1065353216}
IMM[2] FLT32 {0x3f800000, 0x00000000, 0x00000000, 0x00000000}
IMM[3] UINT32 {1065353216, 0, 0, 0}
DCL TEMP[21..24]
0: MOV TEMP[0], IN[0]
1: MOV TEMP[1], IN[1]
2: MOV TEMP[5].x, IMM[0].xxxx
3: MOV TEMP[3].x, IMM[0].xxxx
4: MOV TEMP[2].x, IMM[0].yyyy
5: BGNLOOP :0
6: AND TEMP[6].x, TEMP[2].xxxx, IMM[0].zzzz
7: ISHR TEMP[7].x, TEMP[2].xxxx, IMM[0].wwww
8: UARL ADDR[0].x, TEMP[7].xxxx
9: MOV TEMP[8], TEMP[ADDR[0].x](1)
10: ISLT TEMP[9].xyz, TEMP[6].xxxx, IMM[0].wxzw
11: UCMP TEMP[10].xy, TEMP[9].yzzx, TEMP[8].xzzw, TEMP[8].ywzw
12: UCMP TEMP[11].x, TEMP[9].xxxx, TEMP[10].xxxx, TEMP[10].yxxx
13: I2F TEMP[12].x, TEMP[3].xxxx
14: DIV TEMP[13].x, TEMP[12].xxxx, IMM[1].xxxx
15: ADD TEMP[15].x, TEMP[11].xxxx, -TEMP[13].xxxx
16: MAX TEMP[16].x, TEMP[15].xxxx, -TEMP[15].xxxx
17: FSLT TEMP[17].x, IMM[1].yyyy, TEMP[16].xxxx
18: AND TEMP[18].x, TEMP[17].xxxx, IMM[2].xxxx
19: KILL_IF -TEMP[18].xxxx
20: ISGE TEMP[19].x, TEMP[5].xxxx, IMM[1].zzzz
21: OR TEMP[20].x, TEMP[19].xxxx, TEMP[17].xxxx
22: UIF TEMP[20].xxxx :24
23: BRK
24: ENDIF
25: UADD TEMP[3].x, IMM[0].wwww, TEMP[2].xxxx
26: UADD TEMP[4].x, TEMP[5].xxxx, IMM[0].xxxx
27: MOV TEMP[2].x, TEMP[5].xxxx
28: MOV TEMP[5].x, TEMP[4].xxxx
29: ENDLOOP :0
30: MOV OUT[0], IMM[3].xyyx
31: END
`
	_, vertex, err := translateTGSI(vertexTGSI)
	if err != nil {
		t.Fatal(err)
	}
	_, fragment, err := translateTGSI(fragmentTGSI)
	if err != nil {
		t.Fatal(err)
	}
	vertex = linkTGSIInterfaces(vertex, fragment)

	host := newDarwinTestHost(t)
	defer host.close()
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: virglFormatR8G8B8A8UNorm, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(color); err != nil {
		t.Fatal(err)
	}
	var pixel [4]byte
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		target := host.resources[color.ID]
		host.gl.bindFramebuffer(glFramebuffer, target.framebuffer)
		host.gl.viewport(0, 0, 1, 1)
		host.gl.clearColor(0, 0, 0, 1)
		host.gl.clear(glColorBufferBit)
		host.applyClipPlaneEnable(0)
		host.gl.useProgram(program)
		host.gl.uniform1f(uniformLocation(host.gl, program, "uWinsysAdjustY"), 1)
		host.gl.bindVertexArray(host.vao)
		host.gl.drawArrays(glPoints, 0, 1)
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatalf("compile or draw captured CTS shaders: %v\nvertex:\n%s\nfragment:\n%s", err, vertex, fragment)
	}
	if pixel[0] < 250 || pixel[3] < 250 {
		t.Fatalf("captured CTS clip-distance program pixel = %v, want red; vertex:\n%s\nfragment:\n%s", pixel, vertex, fragment)
	}
}

func TestIndexedDrawAppliesBaseVertex(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 48}
	indices := virtio.GPUResource3D{ID: 3, Target: 0, Width: 6}
	for _, resource := range []virtio.GPUResource3D{color, positions, indices} {
		if err := host.createResource(resource); err != nil {
			t.Fatal(err)
		}
	}
	floatBytes := func(values ...float32) []byte {
		result := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(result[index*4:], math.Float32bits(value))
		}
		return result
	}
	indexBytes := make([]byte, 6)
	binary.LittleEndian.PutUint16(indexBytes[0:], 0)
	binary.LittleEndian.PutUint16(indexBytes[2:], 1)
	binary.LittleEndian.PutUint16(indexBytes[4:], 2)
	if err := host.dispatch(func() error {
		for id, data := range map[uint32][]byte{
			positions.ID: floatBytes(
				2, 2, 2, 2, 2, 2,
				-1, -1, 3, -1, -1, 3,
			),
			indices.ID: indexBytes,
		} {
			host.gl.bindBuffer(glArrayBuffer, host.resources[id].buffer)
			host.gl.bufferSubData(glArrayBuffer, 0, len(data), glPointer(data))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11
	context.vertexElements[12] = []hostVertexElement{{bufferIndex: 0, format: 29}}
	context.boundVertexElements = 12
	context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: positions.ID, resource: host.resources[positions.ID]}
	context.indexBuffer = indices.ID
	context.indexResource = host.resources[indices.ID]
	context.indexSize = 2
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
void main() { gl_Position = vec4(position, 0.0, 1.0); }`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(1.0, 0.0, 0.0, 1.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		host.gl.viewport(0, 0, 1, 1)
		host.gl.clearColor(0, 0, 0, 1)
		host.gl.clear(glColorBufferBit)
		return host.draw(context, []uint32{0, 3, 4, 1, 1, 3, 0, 0, 0, 0, 2, 0})
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
		t.Fatalf("indexed draw with base vertex BGRA = %v, want %v", got, want)
	}
}

func TestIndirectDrawRendersArraysAndElements(t *testing.T) {
	for _, test := range []struct {
		name    string
		indexed bool
	}{
		{name: "arrays"},
		{name: "elements", indexed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := newDarwinTestHost(t)
			defer host.close()

			const contextID = 1
			if err := host.createContext(contextID); err != nil {
				t.Fatal(err)
			}
			color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
			positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24}
			indices := virtio.GPUResource3D{ID: 3, Target: 0, Width: 6}
			indirectWidth := uint32(16)
			if test.indexed {
				indirectWidth = 20
			}
			indirect := virtio.GPUResource3D{ID: 4, Target: 0, Width: indirectWidth}
			for _, description := range []virtio.GPUResource3D{color, positions, indices, indirect} {
				if err := host.createResource(description); err != nil {
					t.Fatal(err)
				}
			}

			positionBytes := make([]byte, positions.Width)
			for index, value := range []float32{-1, -1, 3, -1, -1, 3} {
				binary.LittleEndian.PutUint32(positionBytes[index*4:], math.Float32bits(value))
			}
			indexBytes := make([]byte, indices.Width)
			binary.LittleEndian.PutUint16(indexBytes[0:], 0)
			binary.LittleEndian.PutUint16(indexBytes[2:], 1)
			binary.LittleEndian.PutUint16(indexBytes[4:], 2)
			indirectBytes := make([]byte, indirect.Width)
			binary.LittleEndian.PutUint32(indirectBytes[0:], 3)
			binary.LittleEndian.PutUint32(indirectBytes[4:], 1)
			for description, data := range map[virtio.GPUResource3D][]byte{
				positions: positionBytes,
				indices:   indexBytes,
				indirect:  indirectBytes,
			} {
				if err := host.transferToHost(&resource{description: description, data: data}, virtio.GPUTransfer3D{
					ResourceID: description.ID,
					Box:        virtio.GPUBox{Width: description.Width, Height: 1, Depth: 1},
				}); err != nil {
					t.Fatal(err)
				}
			}

			context := host.contexts[contextID]
			context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
			context.colorSurfaces[0] = 11
			context.vertexElements[12] = []hostVertexElement{{bufferIndex: 0, format: 29}}
			context.boundVertexElements = 12
			context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: positions.ID, resource: host.resources[positions.ID]}
			if test.indexed {
				context.indexBuffer = indices.ID
				context.indexResource = host.resources[indices.ID]
				context.indexSize = 2
			}
			context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
void main() { gl_Position = vec4(position, 0.0, 1.0); }`}
			context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(1.0, 0.0, 0.0, 1.0); }`}
			context.boundShaders[tgsiVertex] = 20
			context.boundShaders[tgsiFragment] = 21

			if err := host.dispatch(func() error {
				if err := host.bindContextFramebuffer(context); err != nil {
					return err
				}
				host.gl.viewport(0, 0, 1, 1)
				host.gl.clearColor(0, 0, 0, 1)
				host.gl.clear(glColorBufferBit)
				indexed := uint32(0)
				if test.indexed {
					indexed = 1
				}
				return host.draw(context, []uint32{
					0, 0, 4, indexed, 0, 0, 0, 0, 0, 0, 2, 0,
					0, 0, indirect.ID, 0, 0, 1, 0, 0,
				})
			}); err != nil {
				t.Fatal(err)
			}
			pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
				t.Fatalf("indirect %s draw BGRA = %v, want %v", test.name, got, want)
			}
		})
	}
}

func TestTextureBufferRGB32FormatsRender(t *testing.T) {
	wordBytes := func(values ...uint32) []byte {
		result := make([]byte, len(values)*4)
		for index, value := range values {
			binary.LittleEndian.PutUint32(result[index*4:], value)
		}
		return result
	}
	floatBytes := func(values ...float32) []byte {
		words := make([]uint32, len(values))
		for index, value := range values {
			words[index] = math.Float32bits(value)
		}
		return wordBytes(words...)
	}
	for _, test := range []struct {
		name       string
		format     uint32
		returnType string
		conversion string
		data       []byte
	}{
		{name: "float", format: virglFormatR32G32B32Float, returnType: "FLOAT", data: floatBytes(1, 0, 0)},
		{name: "uint", format: virglFormatR32G32B32UInt, returnType: "UINT", conversion: "1: U2F TEMP[0], TEMP[0]\n", data: wordBytes(1, 0, 0)},
		{name: "sint", format: virglFormatR32G32B32SInt, returnType: "SINT", conversion: "1: I2F TEMP[0], TEMP[0]\n", data: wordBytes(1, 0, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := newDarwinTestHost(t)
			defer host.close()

			const contextID = 1
			if err := host.createContext(contextID); err != nil {
				t.Fatal(err)
			}
			color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
			positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24}
			texels := virtio.GPUResource3D{ID: 3, Target: 0, Width: uint32(len(test.data))}
			for _, description := range []virtio.GPUResource3D{color, positions, texels} {
				if err := host.createResource(description); err != nil {
					t.Fatal(err)
				}
			}
			positionBytes := floatBytes(-1, -1, 3, -1, -1, 3)
			for description, data := range map[virtio.GPUResource3D][]byte{positions: positionBytes, texels: test.data} {
				if err := host.transferToHost(&resource{description: description, data: data}, virtio.GPUTransfer3D{
					ResourceID: description.ID,
					Box:        virtio.GPUBox{Width: description.Width, Height: 1, Depth: 1},
				}); err != nil {
					t.Fatal(err)
				}
			}

			identitySwizzle := uint32(0 | (1 << 3) | (2 << 6) | (3 << 9))
			if err := host.execute(contextID, []command{{
				Opcode: 1, Object: 6,
				Payload: []uint32{30, texels.ID, test.format, 0, 0, identitySwizzle},
			}}, nil); err != nil {
				t.Fatal(err)
			}
			fragmentTGSI := fmt.Sprintf(`FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], BUFFER, %s
DCL TEMP[0]
IMM[0] INT32 {0, 0, 0, 0}
0: TXF TEMP[0], IMM[0], SAMP[0], BUFFER
%s2: MOV OUT[0], TEMP[0]
3: END
`, test.returnType, test.conversion)
			_, fragmentGLSL, err := translateTGSI(fragmentTGSI)
			if err != nil {
				t.Fatal(err)
			}
			context := host.contexts[contextID]
			context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
			context.colorSurfaces[0] = 11
			context.vertexElements[12] = []hostVertexElement{{bufferIndex: 0, format: 29}}
			context.boundVertexElements = 12
			context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: positions.ID, resource: host.resources[positions.ID]}
			context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
void main() { gl_Position = vec4(position, 0.0, 1.0); }`}
			context.shaders[21] = hostShader{stage: tgsiFragment, source: fragmentGLSL}
			context.boundShaders[tgsiVertex] = 20
			context.boundShaders[tgsiFragment] = 21
			context.boundSamplerViews[tgsiFragment][0] = 30

			if err := host.dispatch(func() error {
				if err := host.bindContextFramebuffer(context); err != nil {
					return err
				}
				host.gl.viewport(0, 0, 1, 1)
				host.gl.clearColor(0, 0, 0, 1)
				host.gl.clear(glColorBufferBit)
				return host.draw(context, []uint32{0, 3, 4, 0})
			}); err != nil {
				t.Fatal(err)
			}
			pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := pixels, []byte{0, 0, 255, 255}; string(got) != string(want) {
				t.Fatalf("RGB32 %s texture-buffer draw BGRA = %v, want %v", test.name, got, want)
			}
		})
	}
}

func TestCubeArraySamplesDistinctCubes(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24}
	cubes := virtio.GPUResource3D{ID: 3, Target: 8, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 12}
	for _, description := range []virtio.GPUResource3D{color, positions, cubes} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	positionBytes := make([]byte, positions.Width)
	for index, value := range []float32{-1, -1, 3, -1, -1, 3} {
		binary.LittleEndian.PutUint32(positionBytes[index*4:], math.Float32bits(value))
	}
	if err := host.transferToHost(&resource{description: positions, data: positionBytes}, virtio.GPUTransfer3D{
		ResourceID: positions.ID, Box: virtio.GPUBox{Width: positions.Width, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	for _, upload := range []struct {
		layer uint32
		color []byte
	}{
		{layer: 0, color: []byte{255, 0, 0, 255}},
		{layer: 6, color: []byte{0, 255, 0, 255}},
	} {
		if err := host.transferToHost(&resource{description: cubes, data: upload.color}, virtio.GPUTransfer3D{
			ResourceID: cubes.ID,
			Box:        virtio.GPUBox{Z: upload.layer, Width: 1, Height: 1, Depth: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}
	identitySwizzle := uint32(0 | (1 << 3) | (2 << 6) | (3 << 9))
	if err := host.execute(contextID, []command{{
		Opcode: 1, Object: 6,
		Payload: []uint32{30, cubes.ID, cubes.Format, 11 << 16, 0, identitySwizzle},
	}}, nil); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11
	context.vertexElements[12] = []hostVertexElement{{bufferIndex: 0, format: 29}}
	context.boundVertexElements = 12
	context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: positions.ID, resource: host.resources[positions.ID]}
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
void main() { gl_Position = vec4(position, 0.0, 1.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundSamplerViews[tgsiFragment][0] = 30
	for index, sample := range []struct {
		cube float32
		want []byte
	}{
		{cube: 0, want: []byte{0, 0, 255, 255}},
		{cube: 1, want: []byte{0, 255, 0, 255}},
	} {
		fragmentTGSI := fmt.Sprintf(`FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], CUBE_ARRAY, FLOAT
IMM[0] FLT32 {1.0, 0.0, 0.0, %g}
0: TEX OUT[0], IMM[0], SAMP[0], CUBE_ARRAY
1: END
`, sample.cube)
		_, fragmentGLSL, err := translateTGSI(fragmentTGSI)
		if err != nil {
			t.Fatal(err)
		}
		shaderHandle := uint32(21 + index)
		context.shaders[shaderHandle] = hostShader{stage: tgsiFragment, source: fragmentGLSL}
		context.boundShaders[tgsiFragment] = shaderHandle
		if err := host.dispatch(func() error {
			if err := host.bindContextFramebuffer(context); err != nil {
				return err
			}
			host.gl.viewport(0, 0, 1, 1)
			host.gl.clearColor(0, 0, 0, 1)
			host.gl.clear(glColorBufferBit)
			return host.draw(context, []uint32{0, 3, 4, 0})
		}); err != nil {
			t.Fatal(err)
		}
		pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
		if err != nil {
			t.Fatal(err)
		}
		if string(pixels) != string(sample.want) {
			t.Fatalf("cube-array sample %d BGRA = %v, want %v", index, pixels, sample.want)
		}
	}
}

func TestSamplerViewAndStateAffectRenderedPixels(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 2, Height: 1, Depth: 1, ArraySize: 1}
	texture := virtio.GPUResource3D{ID: 2, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(texture); err != nil {
		t.Fatal(err)
	}

	identitySwizzle := uint32(0 | (1 << 3) | (2 << 6) | (3 << 9))
	samplerBits := uint32(3 | (3 << 3) | (2 << 11))
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 4, Payload: []uint32{
			0,
			math.Float32bits(1), math.Float32bits(0.5), math.Float32bits(1),
			math.Float32bits(1), math.Float32bits(0.5), math.Float32bits(0),
		}},
		{Opcode: 1, Object: 6, Payload: []uint32{20, texture.ID, 67, 0, 0, identitySwizzle}},
		{Opcode: 1, Object: 7, Payload: []uint32{
			21, samplerBits,
			math.Float32bits(0), math.Float32bits(0), math.Float32bits(0),
			math.Float32bits(0.25), math.Float32bits(0.5), math.Float32bits(0.75), math.Float32bits(1),
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.transferToHost(&resource{
		description: texture,
		data:        []byte{90, 91, 92, 10, 20, 30, 40},
	}, virtio.GPUTransfer3D{
		ResourceID: texture.ID,
		Offset:     3,
		Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 150
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
	fragment := `#version 150
uniform sampler2D source;
out vec4 result;
void main() {
	vec2 coordinate = gl_FragCoord.x < 1.0 ? vec2(0.5) : vec2(-1.0, 0.5);
	result = texture(source, coordinate);
}`
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		source := host.resources[texture.ID]
		host.gl.activeTexture(glTexture0)
		host.gl.bindTexture(glTexture2D, source.texture)
		context := host.contexts[contextID]
		if err := host.applySamplerView(context.samplerViews[20]); err != nil {
			return err
		}
		host.gl.bindSampler(0, context.samplerStates[21].id)
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		host.gl.useProgram(program)
		host.gl.uniform1i(uniformLocation(host.gl, program, "source"), 0)
		host.gl.bindVertexArray(host.vao)
		host.gl.drawArrays(glTriangles, 0, 3)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: output}, image.Rect(0, 0, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels[:4], []byte{30, 20, 10, 40}; string(got) != string(want) {
		t.Fatalf("sampled texture pixel BGRA = %v, want %v", got, want)
	}
	wantBorder := []byte{191, 128, 64, 255}
	for channel := range wantBorder {
		if difference := int(pixels[4+channel]) - int(wantBorder[channel]); difference < -1 || difference > 1 {
			t.Fatalf("sampler border pixel BGRA = %v, want approximately %v", pixels[4:8], wantBorder)
		}
	}
}

func TestTextureSizeQueryRendersDimensionsAndAccessibleLevels(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	color := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24}
	texture := virtio.GPUResource3D{ID: 3, Target: 2, Format: 67, Width: 4, Height: 2, Depth: 1, ArraySize: 1, LastLevel: 2}
	for _, description := range []virtio.GPUResource3D{color, positions, texture} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	positionWords := []uint32{
		math.Float32bits(-1), math.Float32bits(-1),
		math.Float32bits(3), math.Float32bits(-1),
		math.Float32bits(-1), math.Float32bits(3),
	}
	positionBytes := make([]byte, len(positionWords)*4)
	for index, word := range positionWords {
		binary.LittleEndian.PutUint32(positionBytes[index*4:], word)
	}
	if err := host.transferToHost(&resource{description: positions, data: positionBytes}, virtio.GPUTransfer3D{
		ResourceID: positions.ID,
		Box:        virtio.GPUBox{Width: positions.Width, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	identitySwizzle := uint32(0 | (1 << 3) | (2 << 6) | (3 << 9))
	if err := host.execute(contextID, []command{{
		Opcode: 1, Object: 6,
		Payload: []uint32{30, texture.ID, texture.Format, 0, texture.LastLevel << 8, identitySwizzle},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	fragmentTGSI := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
DCL TEMP[0..1]
IMM[0] INT32 {0, 0, 0, 0}
IMM[1] FLT32 {0.25, 0.5, 0.33333334, 0.0}
IMM[2] FLT32 {0.0, 0.0, 0.0, 1.0}
0: TXQ TEMP[0], IMM[0], SAMP[0], 2D
1: I2F TEMP[1], TEMP[0]
2: MUL OUT[0].xy, TEMP[1], IMM[1]
3: MUL OUT[0].z, TEMP[1].wwww, IMM[1].zzzz
4: MOV OUT[0].w, IMM[2].wwww
5: END
`
	_, fragmentGLSL, err := translateTGSI(fragmentTGSI)
	if err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: color.ID, resource: host.resources[color.ID]}
	context.colorSurfaces[0] = 11
	context.vertexElements[12] = []hostVertexElement{{bufferIndex: 0, format: 29}}
	context.boundVertexElements = 12
	context.vertexBuffers[0] = hostVertexBuffer{stride: 8, resourceID: positions.ID, resource: host.resources[positions.ID]}
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
layout(location = 0) in vec2 position;
void main() { gl_Position = vec4(position, 0.0, 1.0); }`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: fragmentGLSL}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21
	context.boundSamplerViews[tgsiFragment][0] = 30

	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		host.gl.viewport(0, 0, 1, 1)
		host.gl.clearColor(0, 0, 0, 1)
		host.gl.clear(glColorBufferBit)
		return host.draw(context, []uint32{0, 3, 4, 0})
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: color}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{255, 255, 255, 255}; string(pixels) != string(want) {
		t.Fatalf("texture query result BGRA = %v, want %v", pixels, want)
	}
}

func TestUnusedSamplerViewDoesNotOverrideActiveStage(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	texture := virtio.GPUResource3D{ID: 2, Target: 2, Format: 64, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(texture); err != nil {
		t.Fatal(err)
	}
	// PIPE_SWIZZLE_0, PIPE_SWIZZLE_1, PIPE_SWIZZLE_X,
	// PIPE_SWIZZLE_W: [ZERO, ONE, RED, ALPHA].
	viewSwizzle := uint32(4 | (5 << 3) | (0 << 6) | (3 << 9))
	identitySwizzle := uint32(0 | (1 << 3) | (2 << 6) | (3 << 9))
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 1, Object: 6, Payload: []uint32{20, texture.ID, 64, 0, 0, viewSwizzle}},
		{Opcode: 1, Object: 6, Payload: []uint32{21, texture.ID, 64, 0, 0, identitySwizzle}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.transferToHost(&resource{
		description: texture,
		data:        []byte{37},
	}, virtio.GPUTransfer3D{
		ResourceID: texture.ID,
		Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 410 core
uniform sampler2D vertexSampler0;
flat out vec4 sampled;
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
	sampled = texelFetch(vertexSampler0, ivec2(0), 0);
}`
	fragment := `#version 410 core
flat in vec4 sampled;
out vec4 result;
void main() {
	result = sampled;
}`
	context := host.contexts[contextID]
	context.shaders[30] = hostShader{stage: tgsiVertex, source: vertex}
	context.shaders[31] = hostShader{stage: tgsiFragment, source: fragment}
	context.boundShaders[tgsiVertex] = 30
	context.boundShaders[tgsiFragment] = 31
	context.boundSamplerViews[tgsiVertex][0] = 20
	// Gallium can leave an old fragment view bound after the fragment shader
	// stops sampling. It must not replace the different view used by vertex.
	context.boundSamplerViews[tgsiFragment][0] = 21
	if err := host.dispatch(func() error {
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		host.gl.viewport(0, 0, 1, 1)
		return host.draw(context, []uint32{0, 3, 4, 0})
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: output}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels[:4], []byte{37, 255, 0, 255}; string(got) != string(want) {
		t.Fatalf("swizzled texel BGRA = %v, want %v", got, want)
	}
}

func TestOneDimensionalTextureUploadsAndSamples(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	texture := virtio.GPUResource3D{ID: 2, Target: 1, Format: 67, Width: 2, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{output, texture} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.transferToHost(&resource{
		description: texture,
		data:        []byte{255, 0, 0, 255, 0, 255, 0, 255},
	}, virtio.GPUTransfer3D{
		ResourceID: texture.ID,
		Box:        virtio.GPUBox{Width: 2, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 150
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
	fragment := `#version 150
uniform sampler1D source;
out vec4 result;
void main() { result = texture(source, 0.75); }`
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		source := host.resources[texture.ID]
		host.gl.activeTexture(glTexture0)
		host.gl.bindTexture(glTexture1D, source.texture)
		if err := host.bindContextFramebuffer(host.contexts[contextID]); err != nil {
			return err
		}
		host.gl.useProgram(program)
		host.gl.uniform1i(uniformLocation(host.gl, program, "source"), 0)
		host.gl.viewport(0, 0, 1, 1)
		host.gl.bindVertexArray(host.vao)
		host.gl.drawArrays(glTriangles, 0, 3)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: output}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 255, 0, 255}; string(got) != string(want) {
		t.Fatalf("sampled 1D texture pixel BGRA = %v, want %v", got, want)
	}
}

func TestOneDimensionalArrayTextureLayerReadback(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 6, Format: 67,
		Width: 2, Height: 1, Depth: 1, ArraySize: 2,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		255, 0, 0, 255, 0, 255, 0, 255,
		0, 0, 255, 255, 255, 255, 255, 255,
	}
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{Width: 2, Height: 1, Depth: 2},
	}
	if err := host.transferToHost(&resource{description: description, data: want}, transfer); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: description, data: make([]byte, len(want))}
	if err := host.transferFromHost(readback, transfer); err != nil {
		t.Fatal(err)
	}
	if got := readback.data; string(got) != string(want) {
		t.Fatalf("1D array texture layer readback = %v, want %v", got, want)
	}
}

func TestRectangleTextureReadback(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 5, Format: 67,
		Width: 2, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	want := []byte{255, 0, 0, 255, 0, 255, 0, 255}
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{Width: 2, Height: 1, Depth: 1},
	}
	if err := host.transferToHost(&resource{description: description, data: want}, transfer); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: description, data: make([]byte, len(want))}
	if err := host.transferFromHost(readback, transfer); err != nil {
		t.Fatal(err)
	}
	if got := readback.data; string(got) != string(want) {
		t.Fatalf("rectangle texture readback = %v, want %v", got, want)
	}
}

func TestThreeDimensionalTextureSliceReadback(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 3, Format: 67,
		Width: 1, Height: 1, Depth: 2, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	want := []byte{255, 0, 0, 255, 0, 255, 0, 255}
	transfer := virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 2},
	}
	if err := host.transferToHost(&resource{description: description, data: want}, transfer); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: description, data: make([]byte, len(want))}
	if err := host.transferFromHost(readback, transfer); err != nil {
		t.Fatal(err)
	}
	if got := readback.data; string(got) != string(want) {
		t.Fatalf("3D texture slice readback = %v, want %v", got, want)
	}
}

func TestMultisampleTextureRendersAndResolves(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	multisample := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67,
		Width: 2, Height: 2, Depth: 1, ArraySize: 1, Samples: 4,
	}
	resolved := virtio.GPUResource3D{
		ID: 2, Target: 2, Format: 67,
		Width: 2, Height: 2, Depth: 1, ArraySize: 1,
	}
	for _, description := range []virtio.GPUResource3D{multisample, resolved} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.dispatch(func() error {
		source := host.resources[multisample.ID]
		destination := host.resources[resolved.ID]
		host.gl.bindFramebuffer(glFramebuffer, source.framebuffer)
		if status := host.gl.checkFramebuffer(glFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("multisample framebuffer status %#x", status)
		}
		host.gl.clearColor(1, 0, 0, 1)
		host.gl.clear(glColorBufferBit)
		host.gl.bindFramebuffer(glReadFramebuffer, source.framebuffer)
		host.gl.bindFramebuffer(glDrawFramebuffer, destination.framebuffer)
		host.gl.blitFramebuffer(0, 0, 2, 2, 0, 0, 2, 2, glColorBufferBit, glNearest)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: resolved}, image.Rect(0, 0, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	for offset := 0; offset < len(pixels); offset += 4 {
		if got, want := pixels[offset:offset+4], []byte{0, 0, 255, 255}; string(got) != string(want) {
			t.Fatalf("resolved multisample pixel %d BGRA = %v, want %v", offset/4, got, want)
		}
	}
}

func TestMultisampleSamplerViewSwizzleAffectsTexelFetch(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	multisample := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1, Samples: 4,
	}
	output := virtio.GPUResource3D{
		ID: 2, Target: 2, Format: 67,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	for _, description := range []virtio.GPUResource3D{multisample, output} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	// [ZERO, ONE, RED, ALPHA]. Base/max-level parameters are invalid for
	// multisample targets, but component swizzles remain texture state.
	viewSwizzle := uint32(4 | (5 << 3) | (0 << 6) | (3 << 9))
	if err := host.execute(contextID, []command{{
		Opcode: 1, Object: 6, Payload: []uint32{20, multisample.ID, 67, 0, 0, viewSwizzle},
	}}, nil); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: output.ID, resource: host.resources[output.ID]}
	context.colorSurfaces[0] = 11
	_, fragmentGLSL, err := translateTGSI(`FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D_MSAA, FLOAT
DCL TEMP[0]
IMM[0] INT32 {0, 0, 0, 0}
0: TXF TEMP[0], IMM[0], SAMP[0], 2D_MSAA
1: MOV OUT[0], TEMP[0]
2: END
`)
	if err != nil {
		t.Fatal(err)
	}
	context.shaders[30] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[31] = hostShader{stage: tgsiFragment, source: fragmentGLSL}
	context.boundShaders[tgsiVertex] = 30
	context.boundShaders[tgsiFragment] = 31
	context.boundSamplerViews[tgsiFragment][0] = 20

	if err := host.dispatch(func() error {
		source := host.resources[multisample.ID]
		host.gl.bindFramebuffer(glFramebuffer, source.framebuffer)
		host.gl.clearColor(0.25, 0.5, 0.75, 0.125)
		host.gl.clear(glColorBufferBit)
		host.gl.viewport(0, 0, 1, 1)
		return host.draw(context, []uint32{0, 3, 4, 0})
	}); err != nil {
		t.Fatal(err)
	}
	pixels, _, err := host.readScanout(&resource{description: output}, image.Rect(0, 0, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{64, 255, 0, 32}
	for channel, value := range pixels[:4] {
		if difference := int(value) - int(want[channel]); difference < -1 || difference > 1 {
			t.Fatalf("multisample swizzled texel BGRA = %v, want approximately %v", pixels[:4], want)
		}
	}
}

func TestDepth24TransferPreservesMaximumForSwizzledFetch(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	depth := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatZ24X8UNorm,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	output := virtio.GPUResource3D{
		ID: 2, Target: 2, Format: virglFormatR32Float,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	for _, description := range []virtio.GPUResource3D{depth, output} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	depthBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(depthBytes, 0x00ffffff)
	if err := host.transferToHost(&resource{description: depth, data: depthBytes}, virtio.GPUTransfer3D{
		ResourceID: depth.ID,
		Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	viewSwizzle := uint32(0 | (4 << 3) | (4 << 6) | (4 << 9))
	if err := host.execute(contextID, []command{{
		Opcode: 1, Object: 6,
		Payload: []uint32{20, depth.ID, virglFormatZ24X8UNorm, 0, 0, viewSwizzle},
	}}, nil); err != nil {
		t.Fatal(err)
	}

	_, fragmentGLSL, err := translateTGSI(`FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
DCL TEMP[0]
IMM[0] INT32 {0, 0, 0, 0}
0: TXF TEMP[0], IMM[0], SAMP[0], 2D
1: MOV OUT[0], TEMP[0]
2: END
`)
	if err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.surfaces[11] = hostSurface{resourceID: output.ID, resource: host.resources[output.ID]}
	context.colorSurfaces[0] = 11
	context.shaders[30] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[31] = hostShader{stage: tgsiFragment, source: fragmentGLSL}
	context.boundShaders[tgsiVertex] = 30
	context.boundShaders[tgsiFragment] = 31
	context.boundSamplerViews[tgsiFragment][0] = 20
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		return host.draw(context, []uint32{0, 3, 4, 0})
	}); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: output, data: make([]byte, 4)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: output.ID,
		Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data)); got != 1 {
		t.Fatalf("depth24 maximum sampled as %g, want 1", got)
	}
}

func TestIntegerMultisampleDrawPopulatesEverySample(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatR32G32B32A32UInt,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1, Samples: 4,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.surfaces[10] = hostSurface{resourceID: description.ID, resource: host.resources[description.ID], format: description.Format}
	context.colorSurfaces[0] = 10
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 fragmentColor0;
void main() { fragmentColor0 = uintBitsToFloat(uvec4(7, 8, 9, 10));
}`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
			return err
		}
		var framebuffer uint32
		host.gl.genFramebuffers(1, &framebuffer)
		defer host.gl.deleteFramebuffers(1, &framebuffer)
		host.gl.bindFramebuffer(glReadFramebuffer, framebuffer)
		for sample := uint32(0); sample < description.Samples; sample++ {
			host.gl.framebufferTextureLayer(glReadFramebuffer, glColorAttachment0, host.resources[description.ID].texture, 0, int32(sample))
			if status := host.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
				return fmt.Errorf("sample %d framebuffer status %#x", sample, status)
			}
			pixels := make([]byte, 16)
			host.gl.readPixels(0, 0, 1, 1, glRGBAInteger, glUnsignedInt, glPointer(pixels))
			got := [4]uint32{
				binary.LittleEndian.Uint32(pixels[0:4]), binary.LittleEndian.Uint32(pixels[4:8]),
				binary.LittleEndian.Uint32(pixels[8:12]), binary.LittleEndian.Uint32(pixels[12:16]),
			}
			if want := [4]uint32{7, 8, 9, 10}; got != want {
				return fmt.Errorf("sample %d pixel = %v, want %v", sample, got, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndependentSamplerStatesCanShareOneTexture(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 2, Height: 1, Depth: 1, ArraySize: 1}
	texture := virtio.GPUResource3D{ID: 2, Target: 2, Format: 67, Width: 2, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(texture); err != nil {
		t.Fatal(err)
	}
	if err := host.transferToHost(&resource{
		description: texture,
		data: []byte{
			255, 0, 0, 255,
			0, 0, 255, 255,
		},
	}, virtio.GPUTransfer3D{
		ResourceID: texture.ID,
		Box:        virtio.GPUBox{Width: 2, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	statePayload := func(handle, state uint32) []uint32 {
		return []uint32{handle, state, 0, 0, math.Float32bits(1000), 0, 0, 0, 0}
	}
	if err := host.execute(contextID, []command{
		// PIPE_TEX_WRAP_CLAMP_TO_EDGE for S on sampler 10; repeat on 11.
		{Opcode: 1, Object: 7, Payload: statePayload(10, 2)},
		{Opcode: 1, Object: 7, Payload: statePayload(11, 0)},
	}, nil); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 150
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
	fragment := `#version 150
uniform sampler2D clamped;
uniform sampler2D repeated;
out vec4 result;
void main() {
	result = gl_FragCoord.x < 1.0 ? texture(clamped, vec2(-0.25, 0.5))
	                                  : texture(repeated, vec2(-0.25, 0.5));
}`
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		source := host.resources[texture.ID]
		for unit, sampler := range []uint32{10, 11} {
			host.gl.activeTexture(glTexture0 + uint32(unit))
			host.gl.bindTexture(glTexture2D, source.texture)
			host.gl.bindSampler(uint32(unit), host.contexts[contextID].samplerStates[sampler].id)
		}
		host.gl.bindFramebuffer(glFramebuffer, host.resources[output.ID].framebuffer)
		host.gl.viewport(0, 0, 2, 1)
		host.gl.useProgram(program)
		host.gl.uniform1i(uniformLocation(host.gl, program, "clamped"), 0)
		host.gl.uniform1i(uniformLocation(host.gl, program, "repeated"), 1)
		host.gl.bindVertexArray(host.vao)
		host.gl.drawArrays(glTriangles, 0, 3)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: output}, image.Rect(0, 0, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255, 255, 0, 0, 255}; string(got) != string(want) {
		t.Fatalf("independent sampler pixels BGRA = %v, want %v", got, want)
	}
}

func TestSamplerStateAppliesMaximumAnisotropy(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	statePayload := func(handle, anisotropy uint32) []uint32 {
		return []uint32{handle, anisotropy << 20, 0, 0, math.Float32bits(1000), 0, 0, 0, 0}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 7, Payload: statePayload(10, 0)},
		{Opcode: 1, Object: 7, Payload: statePayload(11, 16)},
		{Opcode: 1, Object: 7, Payload: statePayload(12, 31)},
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := host.dispatch(func() error {
		for _, test := range []struct {
			handle uint32
			want   float32
		}{{10, 1}, {11, 16}, {12, capsetMaxAnisotropy}} {
			var got float32
			host.gl.getSamplerParameterfv(host.contexts[contextID].samplerStates[test.handle].id,
				glTextureMaxAnisotropyExt, &got)
			if got != test.want {
				return fmt.Errorf("sampler %d anisotropy = %v, want %v", test.handle, got, test.want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGalliumMipFilterNoneDoesNotSelectMipLevels(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 2, Height: 1, Depth: 1, ArraySize: 1}
	texture := virtio.GPUResource3D{ID: 2, Target: 2, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 1, LastLevel: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(texture); err != nil {
		t.Fatal(err)
	}
	level0 := []byte{
		255, 0, 0, 255, 255, 0, 0, 255,
		255, 0, 0, 255, 255, 0, 0, 255,
	}
	if err := host.transferToHost(&resource{description: texture, data: level0}, virtio.GPUTransfer3D{
		ResourceID: texture.ID,
		Box:        virtio.GPUBox{Width: 2, Height: 2, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.transferToHost(&resource{description: texture, data: []byte{0, 0, 255, 255}}, virtio.GPUTransfer3D{
		ResourceID: texture.ID,
		Level:      1,
		Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	statePayload := func(handle, state uint32) []uint32 {
		return []uint32{handle, state, 0, 0, math.Float32bits(1000), 0, 0, 0, 0}
	}
	const clampToEdge = uint32(2 | (2 << 3))
	if err := host.execute(contextID, []command{
		// Gallium values: NONE=2 and NEAREST=0.
		{Opcode: 1, Object: 7, Payload: statePayload(10, clampToEdge|(2<<11))},
		{Opcode: 1, Object: 7, Payload: statePayload(11, clampToEdge)},
	}, nil); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 150
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
	fragment := `#version 150
uniform sampler2D withoutMips;
uniform sampler2D withMips;
out vec4 result;
void main() {
	result = gl_FragCoord.x < 1.0 ? textureLod(withoutMips, vec2(0.5), 1.0)
	                                  : textureLod(withMips, vec2(0.5), 1.0);
}`
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		source := host.resources[texture.ID]
		for unit, sampler := range []uint32{10, 11} {
			host.gl.activeTexture(glTexture0 + uint32(unit))
			host.gl.bindTexture(glTexture2D, source.texture)
			host.gl.bindSampler(uint32(unit), host.contexts[contextID].samplerStates[sampler].id)
		}
		host.gl.bindFramebuffer(glFramebuffer, host.resources[output.ID].framebuffer)
		host.gl.viewport(0, 0, 2, 1)
		host.gl.useProgram(program)
		host.gl.uniform1i(uniformLocation(host.gl, program, "withoutMips"), 0)
		host.gl.uniform1i(uniformLocation(host.gl, program, "withMips"), 1)
		host.gl.bindVertexArray(host.vao)
		host.gl.drawArrays(glTriangles, 0, 3)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: output}, image.Rect(0, 0, 2, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pixels, []byte{0, 0, 255, 255, 255, 0, 0, 255}; string(got) != string(want) {
		t.Fatalf("mip filter pixels BGRA = %v, want %v", got, want)
	}
}

func TestCubeMapTransfersAndSamplingRenderEveryFace(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 6, Height: 1, Depth: 1, ArraySize: 1}
	cube := virtio.GPUResource3D{ID: 2, Target: 4, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 6}
	staging := virtio.GPUResource3D{ID: 3, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(cube); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(staging); err != nil {
		t.Fatal(err)
	}

	faces := [6][4]byte{
		{255, 0, 0, 255},
		{0, 255, 0, 255},
		{0, 0, 255, 255},
		{255, 255, 0, 255},
		{255, 0, 255, 255},
		{0, 255, 255, 255},
	}
	for face, pixel := range faces {
		if face == 0 {
			continue
		}
		if err := host.transferToHost(&resource{
			description: cube,
			data:        pixel[:],
		}, virtio.GPUTransfer3D{
			ResourceID: cube.ID,
			Box:        virtio.GPUBox{Z: uint32(face), Width: 1, Height: 1, Depth: 1},
		}); err != nil {
			t.Fatalf("upload cube face %d: %v", face, err)
		}
	}
	if err := host.transferToHost(&resource{description: staging, data: faces[0][:]}, virtio.GPUTransfer3D{
		ResourceID: staging.ID,
		Box:        virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{
		Opcode: 17,
		Payload: []uint32{
			cube.ID, 0, 0, 0, 0,
			staging.ID, 0, 0, 0, 0,
			1, 1, 1,
		},
	}}, nil); err != nil {
		t.Fatalf("copy staging texture into cube face: %v", err)
	}

	identitySwizzle := uint32(0 | (1 << 3) | (2 << 6) | (3 << 9))
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 1, Object: 6, Payload: []uint32{20, cube.ID, 67, 0, 0, identitySwizzle}},
		{Opcode: 1, Object: 7, Payload: []uint32{
			21, 0,
			math.Float32bits(0), math.Float32bits(0), math.Float32bits(0),
			math.Float32bits(0), math.Float32bits(0), math.Float32bits(0), math.Float32bits(0),
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	vertex := `#version 150
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
	fragment := `#version 150
uniform samplerCube source;
out vec4 result;
void main() {
	float x = gl_FragCoord.x;
	vec3 direction = x < 1.0 ? vec3(1.0, 0.0, 0.0) :
	                 x < 2.0 ? vec3(-1.0, 0.0, 0.0) :
	                 x < 3.0 ? vec3(0.0, 1.0, 0.0) :
	                 x < 4.0 ? vec3(0.0, -1.0, 0.0) :
	                 x < 5.0 ? vec3(0.0, 0.0, 1.0) : vec3(0.0, 0.0, -1.0);
	result = texture(source, direction);
}`
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		resource := host.resources[cube.ID]
		context := host.contexts[contextID]
		host.gl.activeTexture(glTexture0)
		host.gl.bindTexture(resource.textureTarget, resource.texture)
		if err := host.applySamplerView(context.samplerViews[20]); err != nil {
			return err
		}
		host.gl.bindSampler(0, context.samplerStates[21].id)
		if err := host.bindContextFramebuffer(context); err != nil {
			return err
		}
		host.gl.viewport(0, 0, 6, 1)
		host.gl.useProgram(program)
		host.gl.uniform1i(uniformLocation(host.gl, program, "source"), 0)
		host.gl.bindVertexArray(host.vao)
		host.gl.drawArrays(glTriangles, 0, 3)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: output}, image.Rect(0, 0, 6, 1))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0, 0, 255, 255,
		0, 255, 0, 255,
		255, 0, 0, 255,
		0, 255, 255, 255,
		255, 0, 255, 255,
		255, 255, 0, 255,
	}
	if string(pixels) != string(want) {
		t.Fatalf("sampled cube-map pixels BGRA = %v, want %v", pixels, want)
	}
}

func TestSamplerStateSeamlessCubeBitFiltersAcrossFaces(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	cube := virtio.GPUResource3D{ID: 2, Target: 4, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 6}
	for _, description := range []virtio.GPUResource3D{output, cube} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	// Transfer bytes for format 67 are BGRA. Distinct +X red and +Z blue
	// faces make cross-face filtering observable at direction (1, 0, 1).
	faces := [6][4]byte{
		{0, 0, 255, 255}, {0, 255, 0, 255}, {0, 255, 0, 255},
		{0, 255, 0, 255}, {255, 0, 0, 255}, {0, 255, 0, 255},
	}
	for face := range faces {
		if err := host.transferToHost(&resource{description: cube, data: faces[face][:]}, virtio.GPUTransfer3D{
			ResourceID: cube.ID,
			Box:        virtio.GPUBox{Z: uint32(face), Width: 1, Height: 1, Depth: 1},
		}); err != nil {
			t.Fatalf("upload cube face %d: %v", face, err)
		}
	}

	identitySwizzle := uint32(0 | (1 << 3) | (2 << 6) | (3 << 9))
	const linearNoMips = uint32((1 << 9) | (2 << 11) | (1 << 13))
	samplerPayload := func(handle, state uint32) []uint32 {
		return []uint32{handle, state, 0, 0, math.Float32bits(1000), 0, 0, 0, 0}
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 1, Object: 6, Payload: []uint32{20, cube.ID, 67, 0, 0, identitySwizzle}},
		{Opcode: 1, Object: 7, Payload: samplerPayload(21, linearNoMips)},
		{Opcode: 1, Object: 7, Payload: samplerPayload(22, linearNoMips|(1<<19))},
		{Opcode: 10, Payload: []uint32{tgsiFragment, 0, 20}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	context := host.contexts[contextID]
	context.shaders[30] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[31] = hostShader{stage: tgsiFragment, source: `#version 410 core
uniform samplerCube fragmentSampler0;
layout(location = 0) out vec4 result;
void main() { result = texture(fragmentSampler0, vec3(1.0, 0.0, 1.0)); }`}
	context.boundShaders[tgsiVertex] = 30
	context.boundShaders[tgsiFragment] = 31

	render := func(sampler uint32) [4]byte {
		t.Helper()
		if err := host.execute(contextID, []command{{Opcode: 18, Payload: []uint32{tgsiFragment, 0, sampler}}}, nil); err != nil {
			t.Fatal(err)
		}
		var pixel [4]byte
		if err := host.dispatch(func() error {
			host.gl.viewport(0, 0, 1, 1)
			host.gl.clearColor(0, 0, 0, 1)
			host.gl.clear(glColorBufferBit)
			if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
				return err
			}
			host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return pixel
	}

	nonSeamless := render(21)
	if !((nonSeamless[0] > 245 && nonSeamless[2] < 10) || (nonSeamless[2] > 245 && nonSeamless[0] < 10)) {
		t.Fatalf("non-seamless cube edge = %v, want one unblended face", nonSeamless)
	}
	seamless := render(22)
	if seamless[0] < 80 || seamless[0] > 175 || seamless[2] < 80 || seamless[2] > 175 {
		t.Fatalf("seamless cube edge = %v, want red/blue cross-face blend", seamless)
	}
}

func TestIntegerFragmentOutputRendersRawRegisterBits(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatR32G32B32A32UInt,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 fragmentColor0;
void main() { fragmentColor0 = uintBitsToFloat(uvec4(1u, 2u, 3u, 4u)); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	var pixel [16]byte
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBAInteger, glUnsignedInt, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for index, want := range []uint32{1, 2, 3, 4} {
		if got := binary.LittleEndian.Uint32(pixel[index*4:]); got != want {
			t.Fatalf("integer output component %d = %d, want %d (pixel %v)", index, got, want, pixel)
		}
	}
}

func TestIntegerFragmentOutputPreservesSpilledSubnormalRegisterBits(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: virglFormatR32SInt,
		Width: 1, Height: 1, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 fragmentColor0;
void main() {
	vec4 temporary[184];
	int index = 179 + int(gl_FragCoord.x) - int(gl_FragCoord.x);
	temporary[index].x = uintBitsToFloat(0xffffffffu);
	temporary[index + 1].x = uintBitsToFloat(floatBitsToUint(temporary[index].x) & 1u);
	fragmentColor0.x = temporary[index + 1].x;
}`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	var pixel [4]byte
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glRedInteger, glInt, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := int32(binary.LittleEndian.Uint32(pixel[:])); got != 1 {
		t.Fatalf("integer output from spilled subnormal bits = %d, want 1", got)
	}
}

func TestFP64TGSIArithmeticAndRoundEvenRenderExpectedResult(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	const fragmentTGSI = `FRAG
DCL OUT[0], COLOR
DCL TEMP[0..5]
IMM[0] FLT64 {1.25, 2.0}
IMM[1] FLT32 {6.5, 0.0, 0.0, 0.0}
IMM[2] FLT32 {1.0, 0.0, 0.0, 1.0}
IMM[3] FLT32 {0.0, 1.0, 0.0, 1.0}
IMM[4] FLT64 {-511.5, 0.0}
IMM[5] FLT32 {-512.0, 0.0, 0.0, 0.0}
0: DADD TEMP[0].xy, IMM[0].xyxy, IMM[0].zwzw
1: DMUL TEMP[1].zw, TEMP[0].xyxy, IMM[0].zwzw
2: D2F TEMP[2].x, TEMP[1].zwzw
3: FSEQ TEMP[3].x, TEMP[2].xxxx, IMM[1].xxxx
4: DROUND TEMP[4].xy, IMM[4].xyxy
5: D2F TEMP[5].x, TEMP[4].xyxy
6: FSEQ TEMP[3].y, TEMP[5].xxxx, IMM[5].xxxx
7: AND TEMP[3].x, TEMP[3].xxxx, TEMP[3].yyyy
8: UCMP OUT[0], TEMP[3].xxxx, IMM[2], IMM[3]
9: END`
	_, fragmentGLSL, err := translateTGSI(fragmentTGSI)
	if err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: fragmentGLSL}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	var pixel [4]byte
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := [4]byte{255, 0, 0, 255}; pixel != want {
		t.Fatalf("FP64 arithmetic pixel = %v, want %v", pixel, want)
	}
}

func TestLargeFragmentFP64EqualityRendersExpectedResult(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
	}, nil); err != nil {
		t.Fatal(err)
	}

	const fragmentTGSI = `FRAG
DCL OUT[0], COLOR
DCL TEMP[0..127]
IMM[0] FLT64 {0.0, -0.0}
IMM[1] FLT64 {NaN, 1.25}
IMM[2] FLT32 {1.0, 0.0, 0.0, 1.0}
IMM[3] FLT32 {0.0, 1.0, 0.0, 1.0}
0: DSEQ TEMP[0].x, IMM[0].xyxy, IMM[0].zwzw
1: DSNE TEMP[0].y, IMM[1].xyxy, IMM[1].xyxy
2: DSEQ TEMP[0].z, IMM[1].zwzw, IMM[1].zwzw
3: AND TEMP[0].x, TEMP[0].xxxx, TEMP[0].yyyy
4: AND TEMP[0].x, TEMP[0].xxxx, TEMP[0].zzzz
5: UCMP OUT[0], TEMP[0].xxxx, IMM[2], IMM[3]
6: END`
	_, fragmentGLSL, err := translateTGSI(fragmentTGSI)
	if err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: fragmentGLSL}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	var pixel [4]byte
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := [4]byte{255, 0, 0, 255}; pixel != want {
		t.Fatalf("large FP64 equality fragment = %v, want %v", pixel, want)
	}
}

func TestTessellationTGSIRendersPatch(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 32, Payload: []uint32{
			math.Float32bits(1), math.Float32bits(1), math.Float32bits(1), math.Float32bits(1),
			math.Float32bits(1), math.Float32bits(1),
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	const controlTGSI = `TESS_CTRL
PROPERTY TCS_VERTICES_OUT 3
DCL IN[][0], POSITION
DCL OUT[][0], POSITION
DCL OUT[1], TESSOUTER
DCL OUT[2], TESSINNER
DCL SV[0], INVOCATIONID
IMM[0] FLT32 {1.0, 1.0, 1.0, 1.0}
0: MOV OUT[SV[0].x][0], IN[SV[0].x][0]
1: MOV OUT[1], IMM[0]
2: MOV OUT[2], IMM[0]
3: BARRIER
4: END`
	const evaluationTGSI = `TESS_EVAL
PROPERTY TES_PRIM_MODE 4
PROPERTY TES_SPACING 2
PROPERTY TES_VERTEX_ORDER_CW 0
PROPERTY TES_POINT_MODE 0
DCL IN[][0], POSITION
DCL SV[0], TESSCOORD
DCL OUT[0], POSITION
DCL TEMP[0]
0: MUL TEMP[0], IN[0][0], SV[0].xxxx
1: MAD TEMP[0], IN[1][0], SV[0].yyyy, TEMP[0]
2: MAD OUT[0], IN[2][0], SV[0].zzzz, TEMP[0]
3: END`
	_, controlGLSL, err := translateTGSI(controlTGSI)
	if err != nil {
		t.Fatal(err)
	}
	_, evaluationGLSL, err := translateTGSI(evaluationTGSI)
	if err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	const vec2 positions[3] = vec2[3](vec2(-1, -1), vec2(3, -1), vec2(-1, 3));
	gl_Position = vec4(positions[gl_VertexID], 0, 1);
}`}
	context.shaders[21] = hostShader{stage: tgsiTessControl, source: controlGLSL}
	context.shaders[22] = hostShader{stage: tgsiTessEvaluation, source: evaluationGLSL}
	context.shaders[23] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(1, 0, 0, 1); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiTessControl] = 21
	context.boundShaders[tgsiTessEvaluation] = 22
	context.boundShaders[tgsiFragment] = 23

	var pixel [4]byte
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		host.gl.clearColor(0, 0, 0, 1)
		host.gl.clear(glColorBufferBit)
		if err := host.draw(context, []uint32{0, 3, 14, 0, 1, 0, 0, 0, 0, 0, 2, 0, 3, 0}); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := [4]byte{255, 0, 0, 255}; pixel != want {
		t.Fatalf("tessellated patch pixel = %v, want %v", pixel, want)
	}
}

func TestViewportArrayRoutesGeometryPrimitive(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 2, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	viewport := func(scaleX, translateX float32) []uint32 {
		return []uint32{
			math.Float32bits(scaleX), math.Float32bits(0.5), math.Float32bits(0.5),
			math.Float32bits(translateX), math.Float32bits(0.5), math.Float32bits(0.5),
		}
	}
	viewportState := []uint32{0}
	viewportState = append(viewportState, viewport(0.5, 0.5)...)
	viewportState = append(viewportState, viewport(0.5, 1.5)...)
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 4, Payload: viewportState},
	}, nil); err != nil {
		t.Fatal(err)
	}
	const geometryTGSI = `GEOM
PROPERTY GS_INPUT_PRIMITIVE POINTS
PROPERTY GS_OUTPUT_PRIMITIVE TRIANGLE_STRIP
PROPERTY GS_MAX_OUTPUT_VERTICES 3
DCL IN[][0], POSITION
DCL OUT[0], POSITION
DCL OUT[1], VIEWPORT_INDEX
IMM[0] FLT32 {-1.0, -1.0, 0.0, 1.0}
IMM[1] FLT32 {3.0, -1.0, 0.0, 1.0}
IMM[2] FLT32 {-1.0, 3.0, 0.0, 1.0}
IMM[3] INT32 {1, 0, 0, 0}
0: MOV OUT[1].x, IMM[3].xxxx
1: MOV OUT[0], IMM[0]
2: EMIT IMM[3].yyyy
3: MOV OUT[0], IMM[1]
4: EMIT IMM[3].yyyy
5: MOV OUT[0], IMM[2]
6: EMIT IMM[3].yyyy
7: ENDPRIM
8: END`
	_, geometryGLSL, err := translateTGSI(geometryTGSI)
	if err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() { gl_Position = vec4(0, 0, 0, 1); }`}
	context.shaders[21] = hostShader{stage: tgsiGeometry, source: geometryGLSL}
	context.shaders[22] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(1, 0, 0, 1); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiGeometry] = 21
	context.boundShaders[tgsiFragment] = 22

	var pixels [8]byte
	if err := host.dispatch(func() error {
		host.gl.clearColor(0, 0, 0, 1)
		host.gl.clear(glColorBufferBit)
		if err := host.draw(context, []uint32{0, 1, 0, 0}); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 2, 1, glRGBA, glUnsignedByte, glPointer(pixels[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := [8]byte{0, 0, 0, 255, 255, 0, 0, 255}
	if pixels != want {
		t.Fatalf("viewport-array pixels = %v, want %v", pixels, want)
	}
}

func TestDualSourceBlendUsesSecondFragmentColor(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	// Source color is white and the SRC1_COLOR factor is (0.25, 0.5,
	// 0.75). Destination factors are zero, so the rendered pixel directly
	// reveals whether the second shader output was linked at location 0/index 1.
	target := uint32(1 | (9 << 4) | (0x11 << 9) | (1 << 17) | (0x11 << 22) | (0xf << 27))
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{11, output.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 11}},
		{Opcode: 1, Object: 1, Payload: []uint32{12, 0, 0, target, 0, 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 1, Payload: []uint32{12}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 fragmentColor0;
layout(location = 1) out vec4 fragmentColor1;
void main() {
	fragmentColor0 = vec4(1.0);
	fragmentColor1 = vec4(0.25, 0.5, 0.75, 1.0);
}`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	var pixel [4]byte
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		if err := host.draw(context, []uint32{0, 3, 4, 0}); err != nil {
			return err
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := [4]byte{64, 128, 191, 255}
	for index := range pixel {
		if difference := int(pixel[index]) - int(want[index]); difference < -1 || difference > 1 {
			t.Fatalf("dual-source pixel = %v, want approximately %v", pixel, want)
		}
	}
}

func TestBufferTransfersHonorInlineAndBackingOffsets(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{ID: 1, Target: 0, Format: 64, Width: 16}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	resource := &resource{
		description: description,
		data:        []byte{90, 91, 92, 93, 10, 20, 30, 40, 94},
	}
	if err := host.transferToHost(resource, virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Offset:     4,
		Box:        virtio.GPUBox{X: 6, Width: 4},
	}); err != nil {
		t.Fatal(err)
	}
	if err := host.createContext(1); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(1, []command{{
		Opcode: 9,
		Payload: []uint32{
			description.ID, 0, 0, 0, 0,
			1, 0, 0, 4, 1, 1,
			80<<24 | 70<<16 | 60<<8 | 50,
		},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	resource.data = []byte{99, 98}
	if err := host.transferToHost(resource, virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{X: 12, Width: 2},
	}); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, description.Width)
	if err := host.dispatch(func() error {
		hostResource := host.resources[description.ID]
		host.publishBuffer(hostResource)
		host.gl.bindBuffer(glArrayBuffer, hostResource.buffer)
		host.gl.getBufferSubData(glArrayBuffer, 0, len(got), glPointer(got))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, description.Width)
	copy(want[1:5], []byte{50, 60, 70, 80})
	copy(want[6:10], []byte{10, 20, 30, 40})
	copy(want[12:14], []byte{99, 98})
	if string(got) != string(want) {
		t.Fatalf("transferred buffer bytes = %v, want %v", got, want)
	}
}

func TestZeroStridePartialTextureTransferUsesFullMipWidth(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67,
		Width: 4, Height: 2, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	red := []byte{255, 0, 0, 255}
	green := []byte{0, 255, 0, 255}
	blue := []byte{0, 0, 255, 255}
	white := []byte{255, 255, 255, 255}
	data := make([]byte, 24)
	copy(data[0:4], red)
	copy(data[4:8], green)
	copy(data[16:20], blue)
	copy(data[20:24], white)
	if err := host.transferToHost(&resource{description: description, data: data}, virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{X: 1, Width: 2, Height: 2, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	pixels, _, err := host.readScanout(&resource{description: description}, image.Rect(0, 0, 4, 2))
	if err != nil {
		t.Fatal(err)
	}
	// readScanout returns BGRA rows in guest scanout order, with GL y=0 first.
	// Both transferred rows must retain their own colors instead of consuming
	// the padding between full-width rows.
	got := append([]byte(nil), pixels[4:12]...)
	got = append(got, pixels[20:28]...)
	want := []byte{
		0, 0, 255, 255, 0, 255, 0, 255,
		255, 0, 0, 255, 255, 255, 255, 255,
	}
	if string(got) != string(want) {
		t.Fatalf("zero-stride partial texture pixels BGRA = %v, want %v", got, want)
	}
}

func TestTransferFromHostReturnsTextureRowsToGuestBacking(t *testing.T) {
	host := newDarwinTestHost(t)
	renderer := NewRenderer(host)
	defer renderer.Close()

	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67,
		Width: 4, Height: 2, Depth: 1, ArraySize: 1,
	}
	if err := renderer.CreateResource(description); err != nil {
		t.Fatal(err)
	}
	pixels := []byte{
		1, 2, 3, 255, 11, 12, 13, 255, 21, 22, 23, 255, 31, 32, 33, 255,
		41, 42, 43, 255, 51, 52, 53, 255, 61, 62, 63, 255, 71, 72, 73, 255,
	}
	if err := host.dispatch(func() error {
		host.gl.bindTexture(glTexture2D, host.resources[description.ID].texture)
		host.gl.texSubImage2D(glTexture2D, 0, 0, 0, 4, 2, glRGBA, glUnsignedByte, glPointer(pixels))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	backing := transferBacking(make([]byte, 36))
	for index := range backing {
		backing[index] = 0xee
	}
	if err := renderer.TransferFromHost(virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{X: 1, Width: 2, Height: 2, Depth: 1},
		Offset:     4,
		Stride:     16,
		Backing:    backing,
	}); err != nil {
		t.Fatal(err)
	}

	want := transferBacking(make([]byte, len(backing)))
	for index := range want {
		want[index] = 0xee
	}
	copy(want[4:12], pixels[4:12])
	copy(want[20:28], pixels[20:28])
	if string(backing) != string(want) {
		t.Fatalf("guest backing after texture readback = %v, want %v", backing, want)
	}
}

func TestBlitTargetsRequestedMipLevels(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67,
		Width: 2, Height: 2, Depth: 1, ArraySize: 1, LastLevel: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	levelZero := []byte{
		255, 0, 0, 255, 255, 0, 0, 255,
		255, 0, 0, 255, 255, 0, 0, 255,
	}
	if err := host.transferToHost(&resource{
		description: description,
		data:        levelZero,
	}, virtio.GPUTransfer3D{
		ResourceID: description.ID,
		Box:        virtio.GPUBox{Width: 2, Height: 2, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	blit := make([]uint32, 21)
	blit[0] = 0xf | (1 << 8)
	blit[3], blit[4] = description.ID, 1
	blit[9], blit[10], blit[11] = 1, 1, 1
	blit[12], blit[13] = description.ID, 0
	blit[18], blit[19], blit[20] = 2, 2, 1
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 4)
	if err := host.dispatch(func() error {
		resource := host.resources[description.ID]
		host.gl.bindFramebuffer(glReadFramebuffer, host.blitReadFBO)
		host.gl.framebufferTexture(glReadFramebuffer, glColorAttachment0, glTexture2D, resource.texture, 1)
		if status := host.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("mip framebuffer status %#x", status)
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(got))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []byte{255, 0, 0, 255}; string(got) != string(want) {
		t.Fatalf("mip level 1 pixel RGBA = %v, want %v", got, want)
	}
}

func TestBlitPopulatesRequestedCubeFaceAndMipLevel(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	src := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 1}
	dst := virtio.GPUResource3D{ID: 2, Target: 4, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 6, LastLevel: 1}
	if err := host.createResource(src); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(dst); err != nil {
		t.Fatal(err)
	}
	want := []byte{19, 83, 211, 255}
	pixels := make([]byte, 2*2*4)
	for offset := 0; offset < len(pixels); offset += 4 {
		copy(pixels[offset:], want)
	}
	if err := host.transferToHost(&resource{description: src, data: pixels}, virtio.GPUTransfer3D{
		ResourceID: src.ID,
		Box:        virtio.GPUBox{Width: 2, Height: 2, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}

	blit := make([]uint32, 21)
	blit[0] = 0xf | (1 << 8)
	blit[3], blit[4], blit[8] = dst.ID, 1, 3
	blit[9], blit[10], blit[11] = 1, 1, 1
	blit[12] = src.ID
	blit[18], blit[19], blit[20] = 2, 2, 1
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 4)
	if err := host.dispatch(func() error {
		resource := host.resources[dst.ID]
		host.gl.bindFramebuffer(glReadFramebuffer, host.blitReadFBO)
		host.gl.framebufferTexture(glReadFramebuffer, glColorAttachment0, glTextureCubeMapPositiveX+3, resource.texture, 1)
		if status := host.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("cube mip framebuffer status %#x", status)
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(got))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("cube face mip pixel RGBA = %v, want %v", got, want)
	}
}

func TestResourceCopyRegionPopulatesRequestedMipLevel(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	src := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 1, LastLevel: 1}
	dst := virtio.GPUResource3D{ID: 2, Target: 2, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 1, LastLevel: 1}
	if err := host.createResource(src); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(dst); err != nil {
		t.Fatal(err)
	}
	want := []byte{19, 83, 211, 255}
	if err := host.dispatch(func() error {
		host.gl.bindTexture(glTexture2D, host.resources[src.ID].texture)
		host.gl.texSubImage2D(glTexture2D, 1, 0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(want))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	copyRegion := []uint32{
		dst.ID, 1, 0, 0, 0,
		src.ID, 1, 0, 0, 0,
		1, 1, 1,
	}
	if err := host.execute(contextID, []command{{Opcode: 17, Payload: copyRegion}}, nil); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, 4)
	if err := host.dispatch(func() error {
		host.gl.bindFramebuffer(glFramebuffer, host.blitReadFBO)
		host.gl.framebufferTexture(glFramebuffer, glColorAttachment0, glTexture2D, host.resources[dst.ID].texture, 1)
		if status := host.gl.checkFramebuffer(glFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("destination mip framebuffer status %#x", status)
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(got))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("copied mip pixel RGBA = %v, want %v", got, want)
	}
}

func TestResourceCopyRegionCopiesMultipleArrayLayers(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	src := virtio.GPUResource3D{ID: 1, Target: 7, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 3}
	dst := virtio.GPUResource3D{ID: 2, Target: 7, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 3}
	for _, description := range []virtio.GPUResource3D{src, dst} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	want := []byte{19, 83, 211, 255, 7, 131, 41, 255}
	if err := host.transferToHost(&resource{description: src, data: want}, virtio.GPUTransfer3D{
		ResourceID:  src.ID,
		Stride:      4,
		LayerStride: 4,
		Box:         virtio.GPUBox{Z: 1, Width: 1, Height: 1, Depth: 2},
	}); err != nil {
		t.Fatal(err)
	}
	copyRegion := []uint32{
		dst.ID, 0, 0, 0, 0,
		src.ID, 0, 0, 0, 1,
		1, 1, 2,
	}
	if err := host.execute(contextID, []command{{Opcode: 17, Payload: copyRegion}}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: dst, data: make([]byte, len(want))}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID:  dst.ID,
		Stride:      4,
		LayerStride: 4,
		Box:         virtio.GPUBox{Width: 1, Height: 1, Depth: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if string(readback.data) != string(want) {
		t.Fatalf("copied array texture layers = %v, want %v", readback.data, want)
	}
}

func TestBlitCopiesMultipleArrayLayers(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	src := virtio.GPUResource3D{ID: 1, Target: 7, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 2}
	dst := virtio.GPUResource3D{ID: 2, Target: 7, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 2}
	for _, description := range []virtio.GPUResource3D{src, dst} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	want := []byte{19, 83, 211, 255, 7, 131, 41, 255}
	if err := host.transferToHost(&resource{description: src, data: want}, virtio.GPUTransfer3D{
		ResourceID: src.ID, Stride: 4, LayerStride: 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 2},
	}); err != nil {
		t.Fatal(err)
	}
	blit := make([]uint32, 21)
	blit[0] = 0xf
	blit[3], blit[5] = dst.ID, dst.Format
	blit[9], blit[10], blit[11] = 1, 1, 2
	blit[12], blit[14] = src.ID, src.Format
	blit[18], blit[19], blit[20] = 1, 1, 2
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: dst, data: make([]byte, len(want))}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: dst.ID, Stride: 4, LayerStride: 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if string(readback.data) != string(want) {
		t.Fatalf("blitted array texture layers = %v, want %v", readback.data, want)
	}
}

func TestBlitScalesArrayLayerRanges(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	src := virtio.GPUResource3D{ID: 1, Target: 7, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 2}
	dst := virtio.GPUResource3D{ID: 2, Target: 7, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{src, dst} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	sourcePixels := []byte{255, 0, 0, 255, 0, 255, 0, 255}
	if err := host.transferToHost(&resource{description: src, data: sourcePixels}, virtio.GPUTransfer3D{
		ResourceID: src.ID, Stride: 4, LayerStride: 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 2},
	}); err != nil {
		t.Fatal(err)
	}
	blit := make([]uint32, 21)
	blit[0] = 0xf
	blit[3], blit[5] = dst.ID, dst.Format
	blit[9], blit[10], blit[11] = 1, 1, 1
	blit[12], blit[14] = src.ID, src.Format
	blit[18], blit[19], blit[20] = 1, 1, 2
	if err := host.execute(contextID, []command{{Opcode: 16, Payload: blit}}, nil); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: dst, data: make([]byte, 4)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: dst.ID, Stride: 4, LayerStride: 4,
		Box: virtio.GPUBox{Width: 1, Height: 1, Depth: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := readback.data, sourcePixels[4:]; string(got) != string(want) {
		t.Fatalf("scaled array texture layer = %v, want %v", got, want)
	}
}

func TestResourceCopyRegionCopiesSharedExponentTextures(t *testing.T) {
	for _, test := range []struct {
		name      string
		target    uint32
		depth     uint32
		arraySize uint32
		srcLayer  uint32
	}{
		{name: "2D", target: 2, depth: 1, arraySize: 1},
		{name: "cube", target: 4, depth: 1, arraySize: 1, srcLayer: 1},
		{name: "3D", target: 3, depth: 2, arraySize: 1, srcLayer: 1},
		{name: "2D-array", target: 7, depth: 1, arraySize: 2, srcLayer: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := newDarwinTestHost(t)
			defer host.close()

			const contextID = 1
			if err := host.createContext(contextID); err != nil {
				t.Fatal(err)
			}
			src := virtio.GPUResource3D{
				ID: 1, Target: test.target, Format: virglFormatR9G9B9E5Float,
				Width: 3, Height: 3, Depth: test.depth, ArraySize: test.arraySize,
			}
			dst := src
			dst.ID = 2
			for _, description := range []virtio.GPUResource3D{src, dst} {
				if err := host.createResource(description); err != nil {
					t.Fatal(err)
				}
			}

			want := make([]byte, 2*2*4)
			for index := range 4 {
				binary.LittleEndian.PutUint32(want[index*4:], 15<<27|uint32(index+3)<<18|uint32(index+2)<<9|uint32(index+1))
			}
			if err := host.dispatch(func() error {
				resource := host.resources[src.ID]
				host.gl.bindTexture(resource.textureTarget, resource.texture)
				switch test.target {
				case 3, 7:
					host.gl.texSubImage3D(resource.textureTarget, 0, 1, 0, int32(test.srcLayer), 2, 2, 1,
						glRGB, glUnsignedInt5999Rev, glPointer(want))
				case 4:
					host.gl.texSubImage2D(glTextureCubeMapPositiveX+test.srcLayer, 0, 1, 0, 2, 2,
						glRGB, glUnsignedInt5999Rev, glPointer(want))
				default:
					host.gl.texSubImage2D(resource.textureTarget, 0, 1, 0, 2, 2,
						glRGB, glUnsignedInt5999Rev, glPointer(want))
				}
				if glError := host.gl.getError(); glError != 0 {
					return fmt.Errorf("source upload GL error %#x", glError)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			copyRegion := []uint32{
				dst.ID, 0, 0, 0, 0,
				src.ID, 0, 1, 0, test.srcLayer,
				2, 2, 1,
			}
			if err := host.execute(contextID, []command{{Opcode: 17, Payload: copyRegion}}, nil); err != nil {
				t.Fatal(err)
			}

			got := make([]byte, 3*3*4)
			if err := host.dispatch(func() error {
				resource := host.resources[dst.ID]
				host.gl.bindTexture(resource.textureTarget, resource.texture)
				imageTarget := resource.textureTarget
				if test.target == 4 {
					imageTarget = glTextureCubeMapPositiveX
				}
				host.gl.getTexImage(imageTarget, 0, glRGB, glUnsignedInt5999Rev, glPointer(got))
				if glError := host.gl.getError(); glError != 0 {
					return fmt.Errorf("destination readback GL error %#x", glError)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for row := range 2 {
				if actual := got[row*3*4 : row*3*4+2*4]; string(actual) != string(want[row*2*4:(row+1)*2*4]) {
					t.Fatalf("copied row %d = %x, want %x", row, actual, want[row*2*4:(row+1)*2*4])
				}
			}
		})
	}
}

func TestPartialNativeScanoutPublishesCompleteFrame(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	host.sharedPresentation = true

	description := virtio.GPUResource3D{
		ID: 1, Target: 2, Format: 67,
		Width: 4, Height: 2, Depth: 1, ArraySize: 1,
	}
	if err := host.createResource(description); err != nil {
		t.Fatal(err)
	}
	pixels := []byte{
		1, 2, 3, 255, 11, 12, 13, 255, 21, 22, 23, 255, 31, 32, 33, 255,
		41, 42, 43, 255, 51, 52, 53, 255, 61, 62, 63, 255, 71, 72, 73, 255,
	}
	if err := host.dispatch(func() error {
		resource := host.resources[description.ID]
		host.gl.bindTexture(glTexture2D, resource.texture)
		host.gl.texSubImage2D(glTexture2D, 0, 0, 0, 4, 2, glRGBA, glUnsignedByte, glPointer(pixels))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	damage := image.Rect(1, 0, 3, 1)
	frame, available, err := host.nativeScanout(&resource{description: description}, damage)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("native frame is unavailable")
	}
	defer frame.ReleaseFrame(0)
	if frame.Width != 4 || frame.Height != 2 {
		t.Fatalf("native frame size = %dx%d, want 4x2", frame.Width, frame.Height)
	}
	if frame.Damage != damage {
		t.Fatalf("native frame damage = %v, want %v", frame.Damage, damage)
	}

	got := make([]byte, len(pixels))
	if err := host.dispatch(func() error {
		host.gl.bindFramebuffer(glReadFramebuffer, host.blitReadFBO)
		host.gl.framebufferTexture(glReadFramebuffer, glColorAttachment0, glTexture2D, frame.Texture, 0)
		if status := host.gl.checkFramebuffer(glReadFramebuffer); status != glFramebufferComplete {
			return fmt.Errorf("native frame framebuffer status %#x", status)
		}
		host.gl.finish()
		host.gl.readPixels(0, 0, 4, 2, glRGBA, glUnsignedByte, glPointer(got))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), pixels[16:]...), pixels[:16]...)
	if string(got) != string(want) {
		t.Fatalf("native frame pixels = %v, want complete vertically flipped frame %v", got, want)
	}
}
