//go:build darwin

package virgl

import (
	"encoding/binary"
	"fmt"
	"image"
	"math"
	"strings"
	"testing"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

func TestNativeHostEmulatesVertexIDAtSixteenAttributeLimit(t *testing.T) {
	var vertex strings.Builder
	vertex.WriteString("#version 410 core\n")
	for index := 0; index < 16; index++ {
		fmt.Fprintf(&vertex, "layout(location = %d) in vec4 attribute%d;\n", index, index)
	}
	vertex.WriteString("out float value;\nvoid main() { value = float(gl_VertexID)")
	for index := 0; index < 16; index++ {
		fmt.Fprintf(&vertex, " + attribute%d.x", index)
	}
	vertex.WriteString("; gl_Position = vec4(0.0); }\n")
	const fragment = "#version 410 core\nvoid main() { discard; }\n"
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		source := emulateVertexSystemValueSource(vertex.String(), hostVertexSystemEmulation{
			systemValue: emulatedVertexID, attribute: 15,
		})
		program, err := host.gl.compileProgram(source, fragment)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVertexTGSIRequestsPositionInvarianceForMultipassDepth(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL OUT[0], POSITION
  0: MOV OUT[0], IN[0]
  1: END`
	stage, glsl, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	if stage != tgsiVertex {
		t.Fatalf("translated stage = %d, want vertex", stage)
	}
	lines := strings.Split(glsl, "\n")
	got := ""
	if len(lines) >= 2 {
		got = lines[1]
	}
	if got != "invariant gl_Position;" {
		t.Fatalf("vertex position qualifier = %q, want %q", got, "invariant gl_Position;")
	}
}

func TestFragmentedTGSIShaderIsReassembledBeforeTranslation(t *testing.T) {
	const source = "VERT\nDCL IN[0]\nDCL OUT[0], POSITION\n  0: MOV OUT[0], IN[0]\n  1: END\n"
	text := append([]byte(source), 0)
	for len(text)%4 != 0 {
		text = append(text, 0)
	}
	const split = 20
	first := []uint32{41, tgsiVertex, uint32(len(source) + 1), 32, 0}
	first = append(first, shaderTestWords(text[:split])...)
	continuation := []uint32{41, tgsiVertex, uint32(1<<31 | split), 32, 0}
	continuation = append(continuation, shaderTestWords(text[split:])...)

	context := newHostContext()
	if err := context.createShader(first); err != nil {
		t.Fatal(err)
	}
	if _, complete := context.shaders[41]; complete {
		t.Fatal("fragmented shader was translated before its continuation")
	}
	if err := context.createShader(continuation); err != nil {
		t.Fatal(err)
	}
	if _, complete := context.shaders[41]; !complete {
		t.Fatal("shader was not completed after its final continuation")
	}
	if _, pending := context.shaderAssemblies[41]; pending {
		t.Fatal("completed shader retained its assembly buffer")
	}
}

func shaderTestWords(data []byte) []uint32 {
	result := make([]uint32, len(data)/4)
	for index := range result {
		result[index] = binary.LittleEndian.Uint32(data[index*4:])
	}
	return result
}

func TestKMSCubeTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL IN[1]
DCL IN[2]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[9]
DCL CONST[0..10]
DCL TEMP[0..16]
IMM[0] UINT32 {1073741824, 1101004800, 0, 1065353216}
DCL TEMP[17..20]
  0: MUL TEMP[0], CONST[5], IN[0].yyyy
  1: MAD TEMP[1], CONST[4], IN[0].xxxx, TEMP[0]
  2: MAD TEMP[2], CONST[6], IN[0].zzzz, TEMP[1]
  3: MAD OUT[0], CONST[7], IN[0].wwww, TEMP[2]
  4: MUL TEMP[3], CONST[1], IN[0].yyyy
  5: MAD TEMP[4], CONST[0], IN[0].xxxx, TEMP[3]
  6: MAD TEMP[5], CONST[2], IN[0].zzzz, TEMP[4]
  7: MAD TEMP[6], CONST[3], IN[0].wwww, TEMP[5]
  8: DIV TEMP[7].xyz, TEMP[6].xyzz, TEMP[6].wwwx
  9: ADD TEMP[8].xyz, IMM[0].xxyx, -TEMP[7].xyzx
 10: MUL TEMP[9].xyz, CONST[9].xyzz, IN[1].yyyx
 11: MAD TEMP[10].xyz, CONST[8].xyzx, IN[1].xxxx, TEMP[9].xyzx
 12: MAD TEMP[11].xyz, CONST[10].xyzx, IN[1].zzzx, TEMP[10].xyzx
 13: DP3 TEMP[12].x, TEMP[8].xyzx, TEMP[8].xyzx
 14: RSQ TEMP[13].x, TEMP[12].xxxx
 15: MUL TEMP[14].xyz, TEMP[8].xyzz, TEMP[13].xxxx
 16: DP3 TEMP[15].x, TEMP[11].xyzx, TEMP[14].xyzx
 17: MAX TEMP[16].x, IMM[0].zzzz, TEMP[15].xxxx
 18: MUL OUT[1].xyz, TEMP[16].xxxx, IN[2].xyzz
 19: MOV OUT[1].w, IMM[0].wwww
 20: END`
	const fragment = `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL IN[0], GENERIC[9], PERSPECTIVE
DCL OUT[0], COLOR
DCL TEMP[0..3]
  0: MOV OUT[0], IN[0]
  1: END`

	vertexStage, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	fragmentStage, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if vertexStage != tgsiVertex || fragmentStage != tgsiFragment {
		t.Fatalf("translated stages = %d/%d", vertexStage, fragmentStage)
	}
	if !strings.Contains(fragmentGLSL, "fragmentColor7 = fragmentColor0;") {
		t.Fatalf("translated all-color-buffer shader does not replicate color zero:\n%s", fragmentGLSL)
	}

	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndirectSamplerTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL OUT[0], POSITION
0: MOV OUT[0], IN[0]
1: END`
	const fragment = `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL OUT[0], COLOR
DCL SAMP[0..1]
DCL SVIEW[0..1], 2D, FLOAT
DCL TEMP[0..10]
DCL ADDR[0..2]
IMM[0] UINT32 {1, 0, 3, 1132396544}
IMM[1] UINT32 {4294967295, 1, 1056964608, 0}
0: MOV TEMP[1].x, IMM[0].xxxx
1: MOV TEMP[0], IMM[0].yyyy
2: UARL ADDR[2].x, TEMP[1].xxxx
3: MOV TEMP[10], IMM[1]
4: TEX TEMP[9], TEMP[10].zzzz, SAMP[ADDR[2].x], 2D
5: ADD TEMP[0], TEMP[0], TEMP[9]
6: MOV OUT[0], TEMP[0]
7: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFragmentDepthBeforeColorTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL OUT[0], POSITION
0: MOV OUT[0], IN[0]
1: END`
	const fragment = `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL OUT[0], POSITION
DCL OUT[1], COLOR
IMM[0] UINT32 {1065353216, 0, 0, 0}
0: MOV OUT[0].z, IMM[0].xxxx
1: MOV OUT[1], IMM[0].xyyx
2: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGeometryTGSICompilesAndLinksInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[20]
0: MOV OUT[0], IN[0]
1: MOV OUT[1], IN[0]
2: END
`
	const geometry = `GEOM
PROPERTY GS_INPUT_PRIMITIVE TRIANGLES
PROPERTY GS_OUTPUT_PRIMITIVE TRIANGLE_STRIP
PROPERTY GS_MAX_OUTPUT_VERTICES 3
PROPERTY GS_INVOCATIONS 1
DCL IN[][0], POSITION
DCL IN[][1], GENERIC[20]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[20]
IMM[0] INT32 {0, 0, 0, 0}
0: MOV OUT[0], IN[0][0]
1: MOV OUT[1], IN[0][1]
2: EMIT IMM[0].xxxx
3: MOV OUT[0], IN[1][0]
4: MOV OUT[1], IN[1][1]
5: EMIT IMM[0].xxxx
6: MOV OUT[0], IN[2][0]
7: MOV OUT[1], IN[2][1]
8: EMIT IMM[0].xxxx
9: ENDPRIM
10: END
`
	const fragment = `FRAG
DCL IN[0], GENERIC[20]
DCL OUT[0], COLOR
0: MOV OUT[0], IN[0]
1: END
`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, geometryGLSL, err := translateTGSI(geometry)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	vertexGLSL = linkTGSIInterfaces(vertexGLSL, geometryGLSL)
	geometryGLSL = linkTGSIInterfaces(geometryGLSL, fragmentGLSL)

	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgramWithGeometryTransformFeedback(vertexGLSL, geometryGLSL, fragmentGLSL, nil)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTexturedIndexedSceneTGSICompilesAndLinks(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL IN[1]
DCL OUT[0], POSITION
DCL OUT[1]
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1], IN[1]
  2: END`
	const fragment = `FRAG
DCL IN[0], GENERIC[9], PERSPECTIVE
DCL IN[1], GENERIC[10], PERSPECTIVE
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
DCL TEMP[0]
  0: TEX TEMP[0], IN[0], SAMP[0], 2D
  1: ADD OUT[0], TEMP[0], IN[1]
  2: END`

	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	vertexGLSL = linkTGSIInterfaces(vertexGLSL, fragmentGLSL)
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCubeSamplerTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
DCL SAMP[0]
DCL SVIEW[0], 2D, FLOAT
DCL TEMP[0]
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
  0: TEX TEMP[0], IMM[0], SAMP[0], 2D
  1: MUL TEMP[0], TEMP[0], IMM[0].xxxx
  2: ADD OUT[0], IMM[0], TEMP[0]
  3: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], CUBE, FLOAT
DCL TEMP[0..2]
IMM[0] FLT32 {1.0, 0.0, 0.0, 0.5}
  0: TEX TEMP[0], IMM[0], SAMP[0], CUBE
  1: TXB TEMP[1], IMM[0], SAMP[0], CUBE
  2: TXL TEMP[2], IMM[0], SAMP[0], CUBE
  3: ADD TEMP[0], TEMP[0], TEMP[1]
  4: ADD OUT[0], TEMP[0], TEMP[2]
  5: END`

	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTextureQueryLODTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
  0: MOV OUT[0], IMM[0]
  1: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL SAMP[0..2]
DCL SVIEW[0], 1D, FLOAT
DCL SVIEW[1], 2D, FLOAT
DCL SVIEW[2], 3D, FLOAT
DCL TEMP[0..2]
IMM[0] FLT32 {0.25, 0.5, 0.75, 1.0}
  0: LODQ TEMP[0], IMM[0], SAMP[0], 1D
  1: LODQ TEMP[1], IMM[0], SAMP[1], 2D
  2: LODQ TEMP[2], IMM[0], SAMP[2], 3D
  3: ADD TEMP[0], TEMP[0], TEMP[1]
  4: ADD OUT[0], TEMP[0], TEMP[2]
  5: END`

	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSampleShadingTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[0]
1: MOV OUT[1], IMM[0]
2: END`
	const fragment = `FRAG
DCL SV[0], SAMPLEID
DCL SV[1], SAMPLEPOS
DCL SV[2], SAMPLEMASK
DCL OUT[0], COLOR
DCL OUT[1], SAMPLEMASK
DCL TEMP[0]
0: ADD TEMP[0], SV[0], SV[1]
1: ADD OUT[0], TEMP[0], SV[2]
2: MOV OUT[1], SV[2]
3: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTextureBufferTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[0]
1: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL SAMP[0..2]
DCL SVIEW[0], BUFFER, FLOAT
DCL SVIEW[1], BUFFER, UINT
DCL SVIEW[2], BUFFER, SINT
DCL TEMP[0..2]
IMM[0] INT32 {0, 0, 0, 0}
0: TXF TEMP[0], IMM[0], SAMP[0], BUFFER
1: TXF TEMP[1], IMM[0], SAMP[1], BUFFER
2: TXF TEMP[2], IMM[0], SAMP[2], BUFFER
3: ADD TEMP[0], TEMP[0], TEMP[1]
4: ADD OUT[0], TEMP[0], TEMP[2]
5: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCubeArrayTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[0]
1: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], CUBE_ARRAY, FLOAT
DCL TEMP[0..4]
IMM[0] FLT32 {1.0, 0.0, 0.0, 1.0}
IMM[1] FLT32 {0.5, 0.0, 0.0, 0.0}
0: TEX TEMP[0], IMM[0], SAMP[0], CUBE_ARRAY
1: TEX2 TEMP[1], IMM[0], IMM[1], SAMP[0], CUBE_ARRAY
2: TXL2 TEMP[2], IMM[0], IMM[1], SAMP[0], CUBE_ARRAY
3: TXB2 TEMP[3], IMM[0], IMM[1], SAMP[0], CUBE_ARRAY
4: LODQ TEMP[4], IMM[0], SAMP[0], CUBE_ARRAY
5: MOV OUT[0], TEMP[0]
6: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWebGL2TextureAndUnsignedArithmeticTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[0]
1: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL SAMP[0..3]
DCL SVIEW[0], SHADOWCUBE, FLOAT
DCL SVIEW[1], SHADOW2D_ARRAY, FLOAT
DCL SVIEW[2], CUBE, FLOAT
DCL SVIEW[3], 3D, FLOAT
DCL TEMP[0..8]
IMM[0] FLT32 {0.25, 0.5, 0.75, 1.0}
IMM[1] FLT32 {0.01, 0.02, 0.03, 0.25}
IMM[2] UINT32 {17, 19, 23, 29}
IMM[3] UINT32 {3, 4, 5, 6}
0: TEX TEMP[0], IMM[0], SAMP[0], SHADOWCUBE
1: TEX TEMP[1], IMM[0], SAMP[1], SHADOW2D_ARRAY
2: TXD TEMP[2], IMM[0], IMM[1], IMM[1], SAMP[2], CUBE
3: TXD TEMP[3], IMM[0], IMM[1], IMM[1], SAMP[3], 3D
4: TXB2 TEMP[4], IMM[0], IMM[1], SAMP[0], SHADOWCUBE
5: TXP TEMP[5], IMM[0], SAMP[3], 3D
6: UDIV TEMP[6], IMM[2], IMM[3]
7: UMOD TEMP[7], IMM[2], IMM[3]
8: ADD TEMP[8], TEMP[0], TEMP[1]
9: ADD OUT[0], TEMP[8], TEMP[2]
10: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWebGL2TextureOffsetTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[0]
1: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL SAMP[0..3]
DCL SVIEW[0], 2D, FLOAT
DCL SVIEW[1], 2D_ARRAY, FLOAT
DCL SVIEW[2], 3D, FLOAT
DCL SVIEW[3], SHADOW2D, FLOAT
DCL TEMP[0..7]
IMM[0] FLT32 {0.25, 0.5, 0.75, 1.0}
IMM[1] INT32 {-1, 1, 0, 0}
IMM[2] FLT32 {0.01, 0.02, 0.03, 0.25}
0: TEX TEMP[0], IMM[0], SAMP[0], 2D, IMM[1].xyx
1: TXP TEMP[1], IMM[0], SAMP[0], 2D, IMM[1].xyx
2: TXB TEMP[2], IMM[0], SAMP[1], 2D_ARRAY, IMM[1].xyx
3: TXL TEMP[3], IMM[0], SAMP[2], 3D, IMM[1].xyz
4: TXD TEMP[4], IMM[0], IMM[2], IMM[2], SAMP[1], 2D_ARRAY, IMM[1].xyx
5: TXF TEMP[5], IMM[0], SAMP[0], 2D, IMM[1].xyx
6: TEX TEMP[6].x, IMM[0], SAMP[3], SHADOW2D, IMM[1].xyx
7: ADD TEMP[7], TEMP[0], TEMP[1]
8: ADD OUT[0], TEMP[7], TEMP[6]
9: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatalf("%v\n%s", err, fragmentGLSL)
	}
}

func TestFP64TGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[0]
1: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL TEMP[0..30]
IMM[0] FLT64 {1.25, 2.0}
IMM[1] FLT32 {6.5, 0.0, 0.0, 0.0}
IMM[2] FLT32 {1.0, 0.0, 0.0, 1.0}
IMM[3] FLT32 {0.0, 1.0, 0.0, 1.0}
IMM[4] INT32 {2, -2, 0, 0}
IMM[5] UINT32 {3, 0, 0, 0}
0: DADD TEMP[0].xy, IMM[0].xyxy, IMM[0].zwzw
1: DMUL TEMP[1].zw, TEMP[0].xyxy, IMM[0].zwzw
2: D2F TEMP[2].x, TEMP[1].zwzw
3: FSEQ TEMP[3].x, TEMP[2].xxxx, IMM[1].xxxx
4: DABS TEMP[4].xy, TEMP[0].xyxy
5: DNEG TEMP[5].xy, TEMP[4].xyxy
6: DRCP TEMP[6].xy, TEMP[4].xyxy
7: DSQRT TEMP[7].xy, TEMP[4].xyxy
8: DFRAC TEMP[8].xy, TEMP[4].xyxy
9: DRSQ TEMP[9].xy, TEMP[4].xyxy
10: DTRUNC TEMP[10].xy, TEMP[4].xyxy
11: DCEIL TEMP[11].xy, TEMP[4].xyxy
12: DFLR TEMP[12].xy, TEMP[4].xyxy
13: DROUND TEMP[13].xy, TEMP[4].xyxy
14: DSSG TEMP[14].xy, TEMP[5].xyxy
15: DDIV TEMP[15].xy, TEMP[4].xyxy, IMM[0].zwzw
16: DMAX TEMP[16].xy, TEMP[4].xyxy, IMM[0].zwzw
17: DMIN TEMP[17].xy, TEMP[4].xyxy, IMM[0].zwzw
18: DMAD TEMP[18].xy, TEMP[4].xyxy, IMM[0].zwzw, IMM[0].xyxy
19: DFMA TEMP[19].xy, TEMP[4].xyxy, IMM[0].zwzw, IMM[0].xyxy
20: I2D TEMP[20].xy, IMM[4].xxxx
21: U2D TEMP[21].xy, IMM[5].xxxx
22: F2D TEMP[22].xy, IMM[1].xxxx
23: D2I TEMP[23].x, TEMP[20].xyxy
24: D2U TEMP[24].x, TEMP[21].xyxy
25: DSEQ TEMP[25].x, TEMP[4].xyxy, TEMP[4].xyxy
26: DSLT TEMP[26].x, TEMP[4].xyxy, IMM[0].zwzw
27: DSNE TEMP[27].x, TEMP[4].xyxy, IMM[0].zwzw
28: DSGE TEMP[28].x, TEMP[4].xyxy, IMM[0].zwzw
29: DLDEXP TEMP[29].xy, TEMP[4].xyxy, IMM[4].xxxx
30: DFRACEXP TEMP[29].zw, TEMP[30].x, TEMP[4].xyxy
31: DDX TEMP[20].zw, TEMP[4].xyxy
32: DDY TEMP[21].zw, TEMP[4].xyxy
33: UCMP OUT[0], TEMP[3].xxxx, IMM[2], IMM[3]
34: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTessellationTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[0]
1: END`
	const control = `TESS_CTRL
PROPERTY TCS_VERTICES_OUT 3
DCL IN[][0], POSITION
DCL OUT[][0], POSITION
DCL OUT[][3], GENERIC[0]
DCL OUT[1], TESSOUTER
DCL OUT[2], TESSINNER
DCL SV[0], INVOCATIONID
DCL ADDR[0]
IMM[0] FLT32 {1.0, 1.0, 1.0, 1.0}
0: UARL ADDR[0].x, SV[0]
1: MOV OUT[ADDR[0].x][0], IN[ADDR[0].x][0]
2: MOV OUT[ADDR[0].x][3], IMM[0]
3: MOV OUT[1], IMM[0]
4: MOV OUT[2], IMM[0]
5: BARRIER
6: END`
	const evaluation = `TESS_EVAL
PROPERTY TES_PRIM_MODE 4
PROPERTY TES_SPACING 2
PROPERTY TES_VERTEX_ORDER_CW 0
PROPERTY TES_POINT_MODE 0
DCL IN[][0], POSITION
DCL IN[][3], GENERIC[0]
DCL SV[0], TESSCOORD
DCL OUT[0], POSITION
DCL TEMP[0]
0: MUL TEMP[0], IN[0][0], SV[0].xxxx
1: MAD TEMP[0], IN[1][0], SV[0].yyyy, TEMP[0]
2: MAD OUT[0], IN[2][0], SV[0].zzzz, TEMP[0]
3: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
IMM[0] FLT32 {1.0, 0.0, 0.0, 1.0}
0: MOV OUT[0], IMM[0]
1: END`

	stages := []string{vertex, control, evaluation, fragment}
	glsl := make([]string, len(stages))
	for index, tgsi := range stages {
		_, translated, err := translateTGSI(tgsi)
		if err != nil {
			t.Fatalf("translate stage %d: %v", index, err)
		}
		glsl[index] = translated
	}
	glsl[0] = linkTGSIInterfaces(glsl[0], glsl[1])
	glsl[1] = linkTGSIInterfaces(glsl[1], glsl[2])
	glsl[2] = linkTGSIInterfaces(glsl[2], glsl[3])
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgramWithTessellationGeometryTransformFeedback(glsl[0], glsl[1], glsl[2], "", glsl[3], nil)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArraySamplerTGSICompilesInDarwinHostContext(t *testing.T) {
	const vertex = `VERT
DCL OUT[0], POSITION
IMM[0] FLT32 {0.0, 0.0, 0.0, 1.0}
  0: MOV OUT[0], IMM[0]
  1: END`
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D_ARRAY, FLOAT
DCL TEMP[0..2]
IMM[0] FLT32 {0.25, 0.75, 1.0, 0.5}
  0: TEX TEMP[0], IMM[0], SAMP[0], 2D_ARRAY
  1: TXB TEMP[1], IMM[0], SAMP[0], 2D_ARRAY
  2: TXL TEMP[2], IMM[0], SAMP[0], 2D_ARRAY
  3: ADD TEMP[0], TEMP[0], TEMP[1]
  4: ADD OUT[0], TEMP[0], TEMP[2]
  5: END`

	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestVertexCubeExplicitLODUsesESMagnificationCrossover(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
DCL SAMP[0]
DCL SVIEW[0], CUBE, FLOAT
IMM[0] FLT32 {1.0, 0.0, 0.0, 0.19264513}
  0: MOV OUT[0], IN[0]
  1: TXL OUT[1], IMM[0], SAMP[0], CUBE
  2: END`
	const fragment = `FRAG
DCL IN[0], GENERIC[0], PERSPECTIVE
DCL OUT[0], COLOR
  0: MOV OUT[0], IN[0]
  1: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	vertexGLSL = linkTGSIInterfaces(vertexGLSL, fragmentGLSL)

	host := newDarwinTestHost(t)
	defer host.close()
	const contextID = 1
	if err := host.createContext(contextID); err != nil {
		t.Fatal(err)
	}
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	cube := virtio.GPUResource3D{ID: 2, Target: 4, Format: 67, Width: 2, Height: 2, Depth: 1, ArraySize: 6, LastLevel: 1}
	positions := virtio.GPUResource3D{ID: 3, Target: 0, Width: 24}
	for _, description := range []virtio.GPUResource3D{output, cube, positions} {
		if err := host.createResource(description); err != nil {
			t.Fatal(err)
		}
	}
	black, white := [4]byte{0, 0, 0, 255}, [4]byte{255, 255, 255, 255}
	level0 := append(append(append(append([]byte{}, black[:]...), white[:]...), white[:]...), black[:]...)
	for face := uint32(0); face < 6; face++ {
		if err := host.transferToHost(&resource{description: cube, data: level0}, virtio.GPUTransfer3D{
			ResourceID: cube.ID,
			Box:        virtio.GPUBox{Z: face, Width: 2, Height: 2, Depth: 1},
		}); err != nil {
			t.Fatalf("upload cube face %d level 0: %v", face, err)
		}
		if err := host.transferToHost(&resource{description: cube, data: black[:]}, virtio.GPUTransfer3D{
			ResourceID: cube.ID,
			Level:      1,
			Box:        virtio.GPUBox{Z: face, Width: 1, Height: 1, Depth: 1},
		}); err != nil {
			t.Fatalf("upload cube face %d level 1: %v", face, err)
		}
	}
	identitySwizzle := uint32(0 | (1 << 3) | (2 << 6) | (3 << 9))
	if err := host.execute(contextID, []command{
		{Opcode: 1, Object: 6, Payload: []uint32{20, cube.ID, 67, 0, 1 << 8, identitySwizzle}},
		{Opcode: 1, Object: 7, Payload: []uint32{
			21, 1 << 13,
			math.Float32bits(0), math.Float32bits(0), math.Float32bits(1000),
			math.Float32bits(0), math.Float32bits(0), math.Float32bits(0), math.Float32bits(0),
		}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	positionData := make([]byte, positions.Width)
	for index, value := range []float32{-1, -1, 3, -1, -1, 3} {
		binary.LittleEndian.PutUint32(positionData[index*4:], math.Float32bits(value))
	}

	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		context := host.contexts[contextID]
		view, state := context.samplerViews[20], context.samplerStates[21]
		host.gl.bindFramebuffer(glFramebuffer, host.resources[output.ID].framebuffer)
		host.gl.viewport(0, 0, 1, 1)
		host.gl.useProgram(program)
		host.gl.uniform1f(uniformLocation(host.gl, program, "uWinsysAdjustY"), 1)
		host.gl.uniform1i(uniformLocation(host.gl, program, "vertexSampler0"), 0)
		host.gl.uniform1f(uniformLocation(host.gl, program, "vertexSampler0LODCrossover"), explicitLODCrossover(view, state))
		host.gl.activeTexture(glTexture0)
		host.gl.bindTexture(host.resources[cube.ID].textureTarget, host.resources[cube.ID].texture)
		if err := host.applySamplerView(view); err != nil {
			return err
		}
		host.gl.bindSampler(0, state.id)
		host.gl.bindVertexArray(host.vao)
		host.gl.bindBuffer(glArrayBuffer, host.resources[positions.ID].buffer)
		host.gl.bufferSubData(glArrayBuffer, 0, len(positionData), glPointer(positionData))
		host.gl.vertexAttribPtr(0, 2, glFloat, false, 8, 0)
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
	for channel, value := range pixels[:3] {
		if value < 120 || value > 136 {
			t.Fatalf("explicit-LOD cube magnification channel %d = %d, want linear-filtered value near 128 (BGRA %v)", channel, value, pixels)
		}
	}
}

func TestPointSizeAndPointCoordinatesUseGLBuiltins(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], PSIZE
IMM[0] FLT32 {5.0, 0.0, 0.0, 0.0}
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1].x, IMM[0].x
  2: END`
	const fragment = `FRAG
DCL IN[0], PCOORD, PERSPECTIVE
DCL OUT[0], COLOR
  0: MOV OUT[0].xy, IN[0].xy
  1: MOV OUT[0].zw, IN[0].xx
  2: END`

	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(vertexGLSL, "gl_PointSize = varying_psize.x;") {
		t.Fatalf("vertex shader does not publish point size:\n%s", vertexGLSL)
	}
	if !strings.Contains(fragmentGLSL, "vec4(gl_PointCoord, 0.0, 1.0)") ||
		strings.Contains(fragmentGLSL, "in vec4 varying_pcoord") {
		t.Fatalf("fragment shader does not consume point coordinates as a builtin:\n%s", fragmentGLSL)
	}

	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPointSpriteRasterizerCoordinatesReplaceGenericInput(t *testing.T) {
	fragment := `#version 150
in vec4 varying_generic_8;
in vec4 varying_generic_9;
out vec4 result;
void main() {
	result = varying_generic_8 + varying_generic_9;
}`
	fragment = pointSpriteFragmentSource(fragment, 1<<8)
	if strings.Contains(fragment, "in vec4 varying_generic_8") {
		t.Fatal("point-sprite generic input remains declared")
	}
	if !strings.Contains(fragment, "vec4(gl_PointCoord, 0.0, 1.0) + varying_generic_9") {
		t.Fatalf("point-sprite coordinate was not substituted:\n%s", fragment)
	}
	if !strings.Contains(fragment, "in vec4 varying_generic_9") {
		t.Fatal("unselected generic input was removed")
	}

	host := newDarwinTestHost(t)
	defer host.close()
	vertex := `#version 150
out vec4 varying_generic_9;
void main() {
	gl_Position = vec4(0.0);
	varying_generic_9 = vec4(1.0);
}`
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertex, fragment)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMaskedTGSIfaceBuiltinDoesNotShiftGenericVaryings(t *testing.T) {
	const vertex = `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1].xy, GENERIC[9]
IMM[0] FLT32 {0.5, 0.0, 0.0, 1.0}
  0: MOV OUT[0], IN[0]
  1: MOV OUT[1].xy, IMM[0].xyxx
  2: END`
	const fragment = `FRAG
DCL IN[0].x, FACE, CONSTANT
DCL IN[1].xy, GENERIC[9], PERSPECTIVE
DCL OUT[0], COLOR
DCL TEMP[0..1]
IMM[0] FLT32 {0.0, 0.0, 0.0, 0.0}
IMM[1] FLT32 {1.0, 0.0, 0.0, 1.0}
IMM[2] FLT32 {0.0, 0.0, 1.0, 1.0}
  0: SGE TEMP[0], IN[0], IMM[0]
  1: UCMP TEMP[1], TEMP[0], IMM[1], IMM[2]
  2: MOV TEMP[1].y, IN[1].x
  3: MOV OUT[0], TEMP[1]
  4: END`
	_, vertexGLSL, err := translateTGSI(vertex)
	if err != nil {
		t.Fatal(err)
	}
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	vertexGLSL = linkTGSIInterfaces(vertexGLSL, fragmentGLSL)

	host := newDarwinTestHost(t)
	defer host.close()
	output := virtio.GPUResource3D{ID: 1, Target: 2, Format: 67, Width: 1, Height: 1, Depth: 1, ArraySize: 1}
	positions := virtio.GPUResource3D{ID: 2, Target: 0, Width: 24}
	if err := host.createResource(output); err != nil {
		t.Fatal(err)
	}
	if err := host.createResource(positions); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, positions.Width)
	for index, value := range []float32{-1, -1, 3, -1, -1, 3} {
		binary.LittleEndian.PutUint32(data[index*4:], math.Float32bits(value))
	}

	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err != nil {
			return err
		}
		defer host.gl.deleteProgram(program)
		target := host.resources[output.ID]
		host.gl.bindFramebuffer(glFramebuffer, target.framebuffer)
		host.gl.viewport(0, 0, 1, 1)
		host.gl.useProgram(program)
		host.gl.uniform1f(uniformLocation(host.gl, program, "uWinsysAdjustY"), 1)
		host.gl.bindVertexArray(host.vao)
		host.gl.bindBuffer(glArrayBuffer, host.resources[positions.ID].buffer)
		host.gl.bufferSubData(glArrayBuffer, 0, len(data), glPointer(data))
		host.gl.vertexAttribPtr(0, 2, glFloat, false, 8, 0)
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
	if got, want := pixels, []byte{0, 128, 255, 255}; string(got) != string(want) {
		t.Fatalf("masked FACE declaration with generic varying pixel BGRA = %v, want %v", got, want)
	}
}

func TestGlmarkArithmeticTGSICompilesInDarwinHostContext(t *testing.T) {
	const fragment = `FRAG
DCL OUT[0], COLOR
DCL IN[0], POSITION
DCL TEMP[0..7], ARRAY(1)
DCL TEMP[8..11], ARRAY(2)
DCL CONST[0..7]
DCL ADDR[0]
DCL SAMP[0..1]
DCL SVIEW[0], SHADOW2D, FLOAT
DCL SVIEW[1], 2D, FLOAT
IMM[0] FLT32 {0.25, 0.5, 1.0, 2.0}
IMM[1] UINT32 {0, 1, 2, 3}
  0: MOV TEMP[0], IMM[0]
  1: RCP TEMP[1], TEMP[0]
  2: POW TEMP[2], TEMP[0], IMM[0].wwww
  3: FLR TEMP[3], TEMP[1]
  4: FRC TEMP[4], TEMP[1]
  5: FSLT TEMP[5], TEMP[0], TEMP[1]
  6: FSGE TEMP[6], TEMP[1], TEMP[0]
 7: FSEQ TEMP[7], TEMP[0], TEMP[0]
  8: FSNE TEMP[7], TEMP[0], TEMP[1]
 9: SSG TEMP[7], TEMP[0]
10: NOT TEMP[1], IMM[1]
 11: F2I TEMP[1], TEMP[0]
 12: USNE TEMP[1], IMM[1], TEMP[1]
 13: EX2 TEMP[7], TEMP[0]
 14: SIN TEMP[7], TEMP[0]
 15: LRP TEMP[6], TEMP[0], TEMP[2], TEMP[4]
 16: AND TEMP[7], IMM[0], IMM[0]
 17: DIV_SAT TEMP[6], TEMP[2], TEMP[1]
 18: UCMP TEMP[7], IMM[0], TEMP[6], IN[0]
 19: ARL ADDR[0].x, IMM[0]
 20: MOV TEMP[0], CONST[ADDR[0].x+1]
 21: ISGE TEMP[1], IMM[1], IMM[1]
 22: USEQ TEMP[1], IMM[1], IMM[1]
 23: UADD TEMP[1], IMM[1], IMM[1]
 24: UMUL TEMP[1], TEMP[1], IMM[1]
 25: SHL TEMP[1], TEMP[1], IMM[1]
 26: USHR TEMP[1], TEMP[1], IMM[1]
 27: ISHR TEMP[1], TEMP[1], IMM[1]
 28: OR TEMP[1], TEMP[1], IMM[1]
 29: XOR TEMP[1], TEMP[1], IMM[1]
 30: SGE TEMP[1], TEMP[0], TEMP[1]
 31: DP2 TEMP[1], TEMP[0], TEMP[1]
 32: LG2 TEMP[1], TEMP[0]
 33: BGNLOOP :39
 34: UIF IMM[0] :37
 35: BRK
 36: ELSE :38
 37: CONT
 38: ENDIF
 39: ENDLOOP :33
 40: KILL_IF TEMP[0]
 41: ADD OUT[0], TEMP[6], TEMP[7]
 42: TEX TEMP[1], TEMP[0], SAMP[0], SHADOW2D
 43: COS TEMP[1], TEMP[0]
 44: DDX TEMP[1], TEMP[0]
 45: DDY TEMP[1], TEMP[0]
 46: F2U TEMP[1], TEMP[0]
 47: I2F TEMP[1], TEMP[0]
 48: U2F TEMP[1], TEMP[0]
 49: TRUNC TEMP[1], TEMP[0]
 50: INEG TEMP[1], IMM[1]
 51: ISLT TEMP[1], IMM[1], TEMP[1]
 52: UMAX TEMP[1], IMM[1], TEMP[1]
 53: TXP TEMP[1], TEMP[0], SAMP[1], 2D
 54: TXB TEMP[1], TEMP[0], SAMP[1], 2D
 55: TXL TEMP[1], TEMP[0], SAMP[1], 2D
 56: MOV TEMP[ADDR[0].x](1).xy, TEMP[0]
 57: CEIL TEMP[1], TEMP[0]
 58: IDIV TEMP[1], TEMP[1], IMM[1]
 59: IMIN TEMP[1], TEMP[1], IMM[1]
 60: UMIN TEMP[1], TEMP[1], IMM[1]
 61: IABS TEMP[1], TEMP[1]
 62: ISSG TEMP[1], TEMP[1]
 63: TXD TEMP[1], TEMP[0], TEMP[0], TEMP[0], SAMP[1], 2D
 64: IMAX TEMP[1], TEMP[1], IMM[1]
 65: USGE TEMP[1], TEMP[1], IMM[1]
 66: USLT TEMP[1], TEMP[1], IMM[1]
 67: TG4 TEMP[1], TEMP[0], IMM[1].xxxx, SAMP[1], 2D
 68: TG4 TEMP[1], TEMP[0], IMM[1].xxxx, SAMP[1], 2D, IMM[1].xyxx
 69: MOV TEMP[ADDR[0].x+8](2), TEMP[0]
 70: MOV TEMP[1], TEMP[ADDR[0].x+8](2)
 71: SLT TEMP[1], TEMP[0], TEMP[1]
 72: KILL
 73: END`
	_, fragmentGLSL, err := translateTGSI(fragment)
	if err != nil {
		t.Fatal(err)
	}
	const vertexGLSL = `#version 410 core
void main() {
	vec2 position = vec2(float((gl_VertexID << 1) & 2), float(gl_VertexID & 2));
	gl_Position = vec4(position * 2.0 - 1.0, 0.0, 1.0);
}`
	host := newDarwinTestHost(t)
	defer host.close()
	if err := host.dispatch(func() error {
		program, err := host.gl.compileProgram(vertexGLSL, fragmentGLSL)
		if err == nil {
			host.gl.deleteProgram(program)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
