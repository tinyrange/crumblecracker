//go:build darwin

package virgl

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestStreamoutCapturesGuestVertexOutputs(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	positions := virtio.GPUResource3D{ID: 1, Target: 0, Width: 96}
	captures := [4]virtio.GPUResource3D{}
	for index := range captures {
		captures[index] = virtio.GPUResource3D{ID: uint32(index + 2), Target: 0, Width: 24}
	}
	resources := []virtio.GPUResource3D{positions}
	resources = append(resources, captures[:]...)
	for _, description := range resources {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	want := make([]byte, positions.Width)
	values := []float32{
		-1, -0.5, 0.25, 0.75,
		1, 0, -0.25, 0.5,
		0.125, -0.75, 0.625, -0.375,
		0.875, -0.625, 0.375, -0.125,
		-0.875, 0.625, -0.375, 0.125,
		0.5, -1, 1, -0.5,
	}
	for index, value := range values {
		binary.LittleEndian.PutUint32(want[index*4:], math.Float32bits(value))
	}
	if err := host.transferToHost(&resource{description: positions, data: want}, virtio.GPUTransfer3D{
		ResourceID: positions.ID,
		Box:        virtio.GPUBox{Width: positions.Width},
	}); err != nil {
		t.Fatal(err)
	}

	const vertexTGSI = `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], IN[0]
  2: END
`
	shaderText := append([]byte(vertexTGSI), 0)
	for len(shaderText)%4 != 0 {
		shaderText = append(shaderText, 0)
	}
	// Capture each component of OUT[1] into a separate buffer. This exercises
	// all four advertised streamout slots and native gl_NextBuffer layout.
	shaderPayload := []uint32{
		20, tgsiVertex, uint32(len(vertexTGSI) + 1), 0, 4,
		1, 1, 1, 1,
	}
	for index := uint32(0); index < 4; index++ {
		packedOutput := uint32(1 | index<<8 | 1<<10 | index<<13)
		shaderPayload = append(shaderPayload, packedOutput, 0)
	}
	shaderPayload = append(shaderPayload, shaderTestWords(shaderText)...)
	context := host.contexts[contextID]
	if err := context.createShader(shaderPayload); err != nil {
		t.Fatal(err)
	}
	context.shaders[21] = hostShader{
		stage: tgsiFragment,
		source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(0.0); }`,
	}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 5, Payload: []uint32{30, 0, 0, 0, virglFormatR32G32B32A32Float}},
		{Opcode: 2, Object: 5, Payload: []uint32{30}},
		{Opcode: 6, Payload: []uint32{16, 0, positions.ID}},
		{Opcode: 1, Object: 10, Payload: []uint32{40, captures[0].ID, 0, captures[0].Width}},
		{Opcode: 1, Object: 10, Payload: []uint32{41, captures[1].ID, 0, captures[1].Width}},
		{Opcode: 1, Object: 10, Payload: []uint32{42, captures[2].ID, 0, captures[2].Width}},
		{Opcode: 1, Object: 10, Payload: []uint32{43, captures[3].ID, 0, captures[3].Width}},
		{Opcode: 25, Payload: []uint32{0, 40, 41, 42, 43}},
		{Opcode: 1, Object: 2, Payload: []uint32{50, 1 << 3, math.Float32bits(1), 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{50}},
		{Opcode: 8, Payload: []uint32{0, 2, 1, 0, 1, 0, 0, 0, 0, 0, 2, 0}},
		{Opcode: 8, Payload: []uint32{2, 2, 1, 0, 1, 0, 0, 0, 0, 0, 2, 0}},
		{Opcode: 25, Payload: []uint32{0}},
		{Opcode: 25, Payload: []uint32{0xf, 40, 41, 42, 43}},
		{Opcode: 8, Payload: []uint32{4, 2, 1, 0, 1, 0, 0, 0, 0, 0, 2, 0}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{Opcode: 25, Payload: []uint32{0}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if code := host.gl.getError(); code != 0 {
			return fmt.Errorf("host GL error after streamout draw: %#x", code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for component, capture := range captures {
		readback := &resource{description: capture, data: make([]byte, capture.Width)}
		if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
			ResourceID: capture.ID,
			Box:        virtio.GPUBox{Width: capture.Width},
		}); err != nil {
			t.Fatal(err)
		}
		expected := make([]byte, capture.Width)
		for vertex := 0; vertex < 6; vertex++ {
			binary.LittleEndian.PutUint32(expected[vertex*4:], math.Float32bits(values[vertex*4+component]))
		}
		if string(readback.data) != string(expected) {
			t.Fatalf("captured component %d bytes = %v, want %v", component, readback.data, expected)
		}
	}
}

func TestStreamoutCapturesEmittedGeometryOutputs(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	positions := virtio.GPUResource3D{ID: 1, Target: 0, Width: 48}
	capture := virtio.GPUResource3D{ID: 2, Target: 0, Width: 96}
	color := virtio.GPUResource3D{ID: 3, Target: 2, Format: virglFormatR8G8B8A8UNorm, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{positions, capture, color} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	values := []float32{-1, -1, 0, 1, 1, -1, 0, 1, 0, 1, 0, 1}
	positionBytes := make([]byte, positions.Width)
	for index, value := range values {
		binary.LittleEndian.PutUint32(positionBytes[index*4:], math.Float32bits(value))
	}
	if err := host.transferToHost(&resource{description: positions, data: positionBytes}, virtio.GPUTransfer3D{
		ResourceID: positions.ID, Box: virtio.GPUBox{Width: positions.Width},
	}); err != nil {
		t.Fatal(err)
	}

	const vertexTGSI = `VERT
DCL IN[0]
DCL OUT[0], POSITION
0: MOV OUT[0], IN[0]
1: END
`
	const geometryTGSI = `GEOM
PROPERTY GS_INPUT_PRIMITIVE TRIANGLES
PROPERTY GS_OUTPUT_PRIMITIVE TRIANGLE_STRIP
PROPERTY GS_MAX_OUTPUT_VERTICES 3
PROPERTY GS_INVOCATIONS 1
DCL IN[][0], POSITION
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
IMM[0] INT32 {0, 0, 0, 0}
0: MOV OUT[0], IN[2][0]
1: MOV OUT[1], IN[2][0]
2: EMIT IMM[0].xxxx
3: MOV OUT[0], IN[1][0]
4: MOV OUT[1], IN[1][0]
5: EMIT IMM[0].xxxx
6: MOV OUT[0], IN[0][0]
7: MOV OUT[1], IN[0][0]
8: EMIT IMM[0].xxxx
9: ENDPRIM IMM[0].xxxx
10: END
`
	shaderPayload := func(handle, stage uint32, tgsi string, outputs uint32) []uint32 {
		text := append([]byte(tgsi), 0)
		for len(text)%4 != 0 {
			text = append(text, 0)
		}
		payload := []uint32{handle, stage, uint32(len(tgsi) + 1), 0, outputs}
		if outputs != 0 {
			payload = append(payload, 4, 0, 0, 0, uint32(1|4<<10), 0)
		}
		return append(payload, shaderTestWords(text)...)
	}
	context := host.contexts[contextID]
	if err := context.createShader(shaderPayload(20, tgsiVertex, vertexTGSI, 0)); err != nil {
		t.Fatal(err)
	}
	if err := context.createShader(shaderPayload(22, tgsiGeometry, geometryTGSI, 1)); err != nil {
		t.Fatal(err)
	}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(0.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21
	context.boundShaders[tgsiGeometry] = 22
	context.viewportSet = 1
	context.viewports[0].adjustY = -1
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 5, Payload: []uint32{30, 0, 0, 0, virglFormatR32G32B32A32Float}},
		{Opcode: 2, Object: 5, Payload: []uint32{30}},
		{Opcode: 6, Payload: []uint32{16, 0, positions.ID}},
		{Opcode: 1, Object: 8, Payload: []uint32{35, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 35}},
		{Opcode: 1, Object: 10, Payload: []uint32{40, capture.ID, 0, capture.Width}},
		{Opcode: 25, Payload: []uint32{0, 40}},
		{Opcode: 1, Object: 2, Payload: []uint32{50, 1 << 3, math.Float32bits(1), 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{50}},
		{Opcode: 8, Payload: []uint32{0, 3, 4, 0, 1, 0, 0, 0, 0, 0, 2, 0}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{Opcode: 8, Payload: []uint32{0, 3, 4, 0, 1, 0, 0, 0, 0, 0, 2, 0}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{{Opcode: 25, Payload: []uint32{0}}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if code := host.gl.getError(); code != 0 {
			return fmt.Errorf("host GL error after geometry streamout draw: %#x", code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: capture, data: make([]byte, capture.Width)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{ResourceID: capture.ID, Box: virtio.GPUBox{Width: capture.Width}}); err != nil {
		t.Fatal(err)
	}
	for outputVertex := 0; outputVertex < 6; outputVertex++ {
		inputVertex := 2 - outputVertex%3
		for component := 0; component < 4; component++ {
			offset := (outputVertex*4 + component) * 4
			got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data[offset:]))
			want := values[inputVertex*4+component]
			if got != want {
				all := make([]float32, len(readback.data)/4)
				for index := range all {
					all[index] = math.Float32frombits(binary.LittleEndian.Uint32(readback.data[index*4:]))
				}
				t.Fatalf("captured geometry vertex %d component %d = %g, want %g; all = %v", outputVertex, component, got, want, all)
			}
		}
	}
}

func TestStreamoutCapturesPointGeometryFromPatchDraw(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	capture := virtio.GPUResource3D{ID: 1, Target: 0, Width: 16}
	color := virtio.GPUResource3D{ID: 2, Target: 2, Format: virglFormatR8G8B8A8UNorm, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{capture, color} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}

	const controlTGSI = `TESS_CTRL
PROPERTY TCS_VERTICES_OUT 1
DCL IN[][0], POSITION
DCL OUT[][0], POSITION
DCL OUT[1], TESSOUTER
DCL OUT[2], TESSINNER
DCL SV[0], INVOCATIONID
IMM[0] FLT32 {1.0, 1.0, 1.0, 1.0}
0: MOV OUT[SV[0].x][0], IN[SV[0].x][0]
1: MOV OUT[1], IMM[0]
2: MOV OUT[2], IMM[0]
3: END`
	const evaluationTGSI = `TESS_EVAL
PROPERTY TES_PRIM_MODE 1
PROPERTY TES_SPACING 2
PROPERTY TES_VERTEX_ORDER_CW 0
PROPERTY TES_POINT_MODE 1
DCL IN[][0], POSITION
DCL OUT[0], POSITION
0: MOV OUT[0], IN[0][0]
1: END`
	const geometryTGSI = `GEOM
PROPERTY GS_INPUT_PRIMITIVE POINTS
PROPERTY GS_OUTPUT_PRIMITIVE POINTS
PROPERTY GS_MAX_OUTPUT_VERTICES 1
PROPERTY GS_INVOCATIONS 1
DCL IN[][0], POSITION
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
IMM[0] FLT32 {0.25, 0.5, 0.75, 1.0}
IMM[1] INT32 {0, 0, 0, 0}
IMM[2] FLT32 {0.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[2]
1: MOV OUT[1], IMM[0]
2: EMIT IMM[1].xxxx
3: ENDPRIM IMM[1].xxxx
4: END`
	_, controlGLSL, err := translateTGSI(controlTGSI)
	if err != nil {
		t.Fatal(err)
	}
	_, evaluationGLSL, err := translateTGSI(evaluationTGSI)
	if err != nil {
		t.Fatal(err)
	}
	geometryText := append([]byte(geometryTGSI), 0)
	for len(geometryText)%4 != 0 {
		geometryText = append(geometryText, 0)
	}
	geometryPayload := []uint32{
		23, tgsiGeometry, uint32(len(geometryTGSI) + 1), 0, 1,
		4, 0, 0, 0,
		uint32(1 | 4<<10), 0,
	}
	geometryPayload = append(geometryPayload, shaderTestWords(geometryText)...)
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() { gl_Position = vec4(0, 0, 0, 1); }`}
	context.shaders[21] = hostShader{stage: tgsiTessControl, source: controlGLSL}
	context.shaders[22] = hostShader{stage: tgsiTessEvaluation, source: evaluationGLSL}
	if err := context.createShader(geometryPayload); err != nil {
		t.Fatal(err)
	}
	context.shaders[24] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(1.0, 0.0, 0.0, 1.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiTessControl] = 21
	context.boundShaders[tgsiTessEvaluation] = 22
	context.boundShaders[tgsiGeometry] = 23
	context.boundShaders[tgsiFragment] = 24
	if err := host.dispatch(func() error {
		host.gl.viewport(0, 0, 1, 1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{30, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 30}},
		{Opcode: 1, Object: 10, Payload: []uint32{40, capture.ID, 0, capture.Width}},
		{Opcode: 25, Payload: []uint32{0, 40}},
		{Opcode: 1, Object: 2, Payload: []uint32{50, 1 << 1, math.Float32bits(1), 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{50}},
		{Opcode: 8, Payload: []uint32{0, 1, 14, 0, 1, 0, 0, 0, 0, 0, 2, 0, 1, 0}},
		{Opcode: 25, Payload: []uint32{0}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: capture, data: make([]byte, capture.Width)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: capture.ID,
		Box:        virtio.GPUBox{Width: capture.Width},
	}); err != nil {
		t.Fatal(err)
	}
	want := []float32{0.25, 0.5, 0.75, 1}
	for component, expected := range want {
		got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data[component*4:]))
		if got != expected {
			t.Fatalf("captured patch geometry component %d = %g, want %g", component, got, expected)
		}
	}
	var pixel [4]byte
	if err := host.dispatch(func() error {
		if code := host.gl.getError(); code != 0 {
			return fmt.Errorf("host GL error after patch geometry streamout draw: %#x", code)
		}
		host.gl.readPixels(0, 0, 1, 1, glRGBA, glUnsignedByte, glPointer(pixel[:]))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if expected := [4]byte{255, 0, 0, 255}; pixel != expected {
		t.Fatalf("patch geometry fragment = %v, want %v", pixel, expected)
	}
}

func TestStreamoutCapturesTessEvaluationOutputFromPatchDraw(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	capture := virtio.GPUResource3D{ID: 1, Target: 0, Width: 16}
	color := virtio.GPUResource3D{ID: 2, Target: 2, Format: virglFormatR8G8B8A8UNorm, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{capture, color} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}

	const controlTGSI = `TESS_CTRL
PROPERTY TCS_VERTICES_OUT 1
DCL IN[][0], POSITION
DCL OUT[][0], POSITION
DCL OUT[1], TESSOUTER
DCL OUT[2], TESSINNER
DCL SV[0], INVOCATIONID
IMM[0] FLT32 {1.0, 1.0, 1.0, 1.0}
0: MOV OUT[SV[0].x][0], IN[SV[0].x][0]
1: MOV OUT[1], IMM[0]
2: MOV OUT[2], IMM[0]
3: END`
	const evaluationTGSI = `TESS_EVAL
PROPERTY TES_PRIM_MODE 1
PROPERTY TES_SPACING 2
PROPERTY TES_VERTEX_ORDER_CW 0
PROPERTY TES_POINT_MODE 1
DCL IN[][0], POSITION
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
IMM[0] FLT32 {0.25, 0.5, 0.75, 1.0}
0: MOV OUT[0], IN[0][0]
1: MOV OUT[1], IMM[0]
2: END`
	_, controlGLSL, err := translateTGSI(controlTGSI)
	if err != nil {
		t.Fatal(err)
	}
	evaluationText := append([]byte(evaluationTGSI), 0)
	for len(evaluationText)%4 != 0 {
		evaluationText = append(evaluationText, 0)
	}
	evaluationPayload := []uint32{
		22, tgsiTessEvaluation, uint32(len(evaluationTGSI) + 1), 0, 1,
		4, 0, 0, 0,
		uint32(1 | 4<<10), 0,
	}
	evaluationPayload = append(evaluationPayload, shaderTestWords(evaluationText)...)
	context := host.contexts[contextID]
	context.shaders[20] = hostShader{stage: tgsiVertex, source: `#version 410 core
void main() { gl_Position = vec4(0, 0, 0, 1); }`}
	context.shaders[21] = hostShader{stage: tgsiTessControl, source: controlGLSL}
	if err := context.createShader(evaluationPayload); err != nil {
		t.Fatal(err)
	}
	context.shaders[23] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(1); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiTessControl] = 21
	context.boundShaders[tgsiTessEvaluation] = 22
	context.boundShaders[tgsiFragment] = 23

	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 8, Payload: []uint32{30, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 30}},
		{Opcode: 1, Object: 10, Payload: []uint32{40, capture.ID, 0, capture.Width}},
		{Opcode: 25, Payload: []uint32{0, 40}},
		{Opcode: 1, Object: 2, Payload: []uint32{50, 1 << 3, 0, 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{50}},
		{Opcode: 8, Payload: []uint32{0, 1, 14, 0, 1, 0, 0, 0, 0, 0, 2, 0, 1, 0}},
		{Opcode: 25, Payload: []uint32{0}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	readback := &resource{description: capture, data: make([]byte, capture.Width)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: capture.ID,
		Box:        virtio.GPUBox{Width: capture.Width},
	}); err != nil {
		t.Fatal(err)
	}
	for component, expected := range []float32{0.25, 0.5, 0.75, 1} {
		got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data[component*4:]))
		if got != expected {
			t.Fatalf("captured tessellation component %d = %g, want %g", component, got, expected)
		}
	}
}

func TestStreamoutCapturesMultipleGeometryStreams(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	positions := virtio.GPUResource3D{ID: 1, Target: 0, Width: 16}
	captures := [2]virtio.GPUResource3D{
		{ID: 2, Target: 0, Width: 16},
		{ID: 3, Target: 0, Width: 16},
	}
	color := virtio.GPUResource3D{ID: 4, Target: 2, Format: virglFormatR8G8B8A8UNorm, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{positions, captures[0], captures[1], color} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	input := []float32{0.25, -0.5, 0.75, 1}
	positionBytes := make([]byte, positions.Width)
	for index, value := range input {
		binary.LittleEndian.PutUint32(positionBytes[index*4:], math.Float32bits(value))
	}
	if err := host.transferToHost(&resource{description: positions, data: positionBytes}, virtio.GPUTransfer3D{
		ResourceID: positions.ID, Box: virtio.GPUBox{Width: positions.Width},
	}); err != nil {
		t.Fatal(err)
	}

	const vertexTGSI = `VERT
DCL IN[0]
DCL OUT[0], POSITION
0: MOV OUT[0], IN[0]
1: END
`
	const geometryTGSI = `GEOM
PROPERTY GS_INPUT_PRIMITIVE POINTS
PROPERTY GS_OUTPUT_PRIMITIVE POINTS
PROPERTY GS_MAX_OUTPUT_VERTICES 2
PROPERTY GS_INVOCATIONS 1
DCL IN[][0], POSITION
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
DCL OUT[2], GENERIC[1]
IMM[0] INT32 {0, 1, 2, 3}
IMM[1] FLT32 {4.0, 5.0, 6.0, 7.0}
0: MOV OUT[0], IN[0][0]
1: MOV OUT[1], IN[0][0]
2: EMIT IMM[0].xxxx
3: ENDPRIM IMM[0].xxxx
4: MOV OUT[0], IN[0][0]
5: MOV OUT[2], IMM[1]
6: EMIT IMM[0].yyyy
7: ENDPRIM IMM[0].yyyy
8: END
`
	shaderPayload := func(handle, stage uint32, tgsi string, outputs []tgsiStreamOutput, strides [4]uint32) []uint32 {
		shaderText := append([]byte(tgsi), 0)
		for len(shaderText)%4 != 0 {
			shaderText = append(shaderText, 0)
		}
		payload := []uint32{handle, stage, uint32(len(tgsi) + 1), 0, uint32(len(outputs))}
		if len(outputs) != 0 {
			payload = append(payload, strides[:]...)
			for _, output := range outputs {
				packed := output.registerIndex | output.startComponent<<8 | output.numComponents<<10 |
					output.buffer<<13 | output.dstOffset<<16
				payload = append(payload, packed, output.stream)
			}
		}
		return append(payload, shaderTestWords(shaderText)...)
	}
	context := host.contexts[contextID]
	if err := context.createShader(shaderPayload(20, tgsiVertex, vertexTGSI, nil, [4]uint32{})); err != nil {
		t.Fatal(err)
	}
	outputs := []tgsiStreamOutput{
		{registerIndex: 1, numComponents: 4, buffer: 0, stream: 0},
		{registerIndex: 2, numComponents: 4, buffer: 1, stream: 1},
	}
	if err := context.createShader(shaderPayload(22, tgsiGeometry, geometryTGSI, outputs, [4]uint32{4, 4})); err != nil {
		t.Fatal(err)
	}
	context.shaders[21] = hostShader{stage: tgsiFragment, source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(0.0); }`}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21
	context.boundShaders[tgsiGeometry] = 22
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 5, Payload: []uint32{30, 0, 0, 0, virglFormatR32G32B32A32Float}},
		{Opcode: 2, Object: 5, Payload: []uint32{30}},
		{Opcode: 6, Payload: []uint32{16, 0, positions.ID}},
		{Opcode: 1, Object: 8, Payload: []uint32{35, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 35}},
		{Opcode: 1, Object: 10, Payload: []uint32{40, captures[0].ID, 0, captures[0].Width}},
		{Opcode: 1, Object: 10, Payload: []uint32{41, captures[1].ID, 0, captures[1].Width}},
		{Opcode: 25, Payload: []uint32{0, 40, 41}},
		{Opcode: 1, Object: 2, Payload: []uint32{50, 1 << 3, math.Float32bits(1), 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{50}},
		{Opcode: 8, Payload: []uint32{0, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0}},
		{Opcode: 25, Payload: []uint32{0}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if code := host.gl.getError(); code != 0 {
			return fmt.Errorf("host GL error after multiple-stream geometry draw: %#x", code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wants := [2][]float32{input, {4, 5, 6, 7}}
	for stream, capture := range captures {
		readback := &resource{description: capture, data: make([]byte, capture.Width)}
		if err := host.transferFromHost(readback, virtio.GPUTransfer3D{ResourceID: capture.ID, Box: virtio.GPUBox{Width: capture.Width}}); err != nil {
			t.Fatal(err)
		}
		for component, want := range wants[stream] {
			got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data[component*4:]))
			if got != want {
				t.Fatalf("captured stream %d component %d = %g, want %g; bytes = %v", stream, component, got, want, readback.data)
			}
		}
	}
}

func TestStreamoutCapturesMaximumInterleavedVertexOutputs(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const (
		contextID = 1
		vertices  = 3
		vectors   = 16
		stride    = vectors * 4
	)
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	positions := virtio.GPUResource3D{ID: 1, Target: 0, Width: vertices * 16}
	capture := virtio.GPUResource3D{ID: 2, Target: 0, Width: vertices * stride * 4}
	color := virtio.GPUResource3D{ID: 3, Target: 2, Format: virglFormatR8G8B8A8UNorm, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	for _, description := range []virtio.GPUResource3D{positions, capture, color} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	positionValues := []float32{-1, -1, 0, 1, 1, -1, 0, 1, 0, 1, 0, 1}
	positionBytes := make([]byte, positions.Width)
	for index, value := range positionValues {
		binary.LittleEndian.PutUint32(positionBytes[index*4:], math.Float32bits(value))
	}
	if err := host.transferToHost(&resource{description: positions, data: positionBytes}, virtio.GPUTransfer3D{
		ResourceID: positions.ID,
		Box:        virtio.GPUBox{Width: positions.Width},
	}); err != nil {
		t.Fatal(err)
	}

	var tgsi strings.Builder
	tgsi.WriteString("VERT\nDCL IN[0]\nDCL OUT[0], POSITION\n")
	for vector := 1; vector < vectors; vector++ {
		fmt.Fprintf(&tgsi, "DCL OUT[%d], GENERIC[%d]\n", vector, vector-1)
		base := (vector - 1) * 4
		fmt.Fprintf(&tgsi, "IMM[%d] FLT32 {%d.0, %d.0, %d.0, %d.0}\n", vector-1, base, base+1, base+2, base+3)
	}
	tgsi.WriteString("0: MOV OUT[0], IN[0]\n")
	for vector := 1; vector < vectors; vector++ {
		fmt.Fprintf(&tgsi, "%d: MOV OUT[%d], IMM[%d]\n", vector, vector, vector-1)
	}
	tgsi.WriteString("16: END\n")
	vertexTGSI := tgsi.String()
	shaderText := append([]byte(vertexTGSI), 0)
	for len(shaderText)%4 != 0 {
		shaderText = append(shaderText, 0)
	}
	shaderPayload := []uint32{20, tgsiVertex, uint32(len(vertexTGSI) + 1), 0, vectors, stride, 0, 0, 0}
	for vector := 1; vector < vectors; vector++ {
		packed := uint32(vector | 4<<10 | ((vector - 1) * 4 << 16))
		shaderPayload = append(shaderPayload, packed, 0)
	}
	shaderPayload = append(shaderPayload, uint32(4<<10|((vectors-1)*4)<<16), 0)
	shaderPayload = append(shaderPayload, shaderTestWords(shaderText)...)
	context := host.contexts[contextID]
	if err := context.createShader(shaderPayload); err != nil {
		t.Fatal(err)
	}
	context.shaders[21] = hostShader{
		stage: tgsiFragment,
		source: `#version 410 core
layout(location = 0) out vec4 color;
void main() { color = vec4(0.0); }`,
	}
	context.boundShaders[tgsiVertex] = 20
	context.boundShaders[tgsiFragment] = 21

	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 5, Payload: []uint32{30, 0, 0, 0, virglFormatR32G32B32A32Float}},
		{Opcode: 2, Object: 5, Payload: []uint32{30}},
		{Opcode: 6, Payload: []uint32{16, 0, positions.ID}},
		{Opcode: 1, Object: 8, Payload: []uint32{35, color.ID}},
		{Opcode: 5, Payload: []uint32{1, 0, 35}},
		{Opcode: 1, Object: 10, Payload: []uint32{40, capture.ID, 0, capture.Width}},
		{Opcode: 25, Payload: []uint32{0, 40}},
		{Opcode: 1, Object: 2, Payload: []uint32{50, 1 << 3, math.Float32bits(1), 0, 0, 0, 0, 0, 0}},
		{Opcode: 2, Object: 2, Payload: []uint32{50}},
		{Opcode: 8, Payload: []uint32{0, vertices, 0, 0, 1, 0, 0, 0, 0, 0, 2, 0}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := host.dispatch(func() error {
		if code := host.gl.getError(); code != 0 {
			return fmt.Errorf("host GL error after maximum-width streamout draw: %#x", code)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	readback := &resource{description: capture, data: make([]byte, capture.Width)}
	if err := host.transferFromHost(readback, virtio.GPUTransfer3D{
		ResourceID: capture.ID,
		Box:        virtio.GPUBox{Width: capture.Width},
	}); err != nil {
		t.Fatal(err)
	}
	for vertex := 0; vertex < vertices; vertex++ {
		for component := 0; component < stride; component++ {
			want := float32(component)
			if component >= (vectors-1)*4 {
				want = positionValues[vertex*4+component-(vectors-1)*4]
			}
			offset := (vertex*stride + component) * 4
			got := math.Float32frombits(binary.LittleEndian.Uint32(readback.data[offset:]))
			if got != want {
				values := make([]float32, stride)
				var nonzero []int
				for index := 0; index < len(readback.data)/4; index++ {
					at := index * 4
					if math.Float32frombits(binary.LittleEndian.Uint32(readback.data[at:])) != 0 {
						nonzero = append(nonzero, index)
					}
				}
				for index := range values {
					at := (vertex*stride + index) * 4
					values[index] = math.Float32frombits(binary.LittleEndian.Uint32(readback.data[at:]))
				}
				t.Fatalf("vertex %d component %d = %g, want %g; vertex data = %v; nonzero float indices = %v", vertex, component, got, want, values, nonzero)
			}
		}
	}
}

func TestEmptyVertexBufferStateClearsAllBindings(t *testing.T) {
	host := newDarwinTestHost(t)
	defer host.close()

	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	buffer := virtio.GPUResource3D{ID: 1, Target: 0, Width: 16}
	if err := host.createResource(buffer); err != nil {
		t.Fatal(err)
	}
	if err := host.execute(contextID, []command{
		{Opcode: 6, Payload: []uint32{16, 0, buffer.ID}},
		{Opcode: 6},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for index, binding := range host.contexts[contextID].vertexBuffers {
		if binding.resource != nil || binding.resourceID != 0 {
			t.Fatalf("vertex buffer %d remained bound after empty state: %+v", index, binding)
		}
	}
}
