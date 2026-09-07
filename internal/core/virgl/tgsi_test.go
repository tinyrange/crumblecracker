package virgl

import (
	"fmt"
	"strings"
	"testing"
)

func TestTranslateTGSIRejectsUnsupportedOpcode(t *testing.T) {
	_, _, err := translateTGSI("VERT\nDCL OUT[0], POSITION\n 0: EXPLODE OUT[0]\n 1: END\n")
	if err == nil {
		t.Fatal("unsupported TGSI opcode was accepted")
	}
}

func TestTranslateTGSIRoundInstruction(t *testing.T) {
	const shader = `FRAG
DCL OUT[0], COLOR
DCL TEMP[0]
IMM[0] FLT32 {0.5, 1.5, 2.5, -1.5}
0: ROUND TEMP[0], IMM[0]
1: MOV OUT[0], TEMP[0]
2: END`
	_, _, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTranslateTGSIBitfieldInstructions(t *testing.T) {
	const shader = `FRAG
DCL OUT[0], COLOR
DCL TEMP[0..2]
IMM[0] UINT32 {305419896, 2271560481, 8, 12}
0: IBFE TEMP[0], IMM[0].xxxx, IMM[0].zzzz, IMM[0].wwww
1: UBFE TEMP[1], IMM[0].yyyy, IMM[0].zzzz, IMM[0].wwww
2: BFI TEMP[2], IMM[0].xxxx, IMM[0].yyyy, IMM[0].zzzz, IMM[0].wwww
3: MOV OUT[0], TEMP[2]
4: END`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"intBitsToFloat(bitfieldExtract(floatBitsToInt(immediate0.xxxx)",
		"uintBitsToFloat(bitfieldExtract(floatBitsToUint(immediate0.yyyy)",
		"uintBitsToFloat(bitfieldInsert(floatBitsToUint(immediate0.xxxx), floatBitsToUint(immediate0.yyyy)",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated bitfield shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIUsesIsnanForFloatSelfComparison(t *testing.T) {
	const shader = `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL TEMP[0..1]
0: FSNE TEMP[0], IN[0], IN[0]
1: FSNE TEMP[1], IN[0], TEMP[0]
2: MOV OUT[0], TEMP[0]
3: END`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"intBitsToFloat(-ivec4(isnan(attribute0)))",
		"intBitsToFloat(-ivec4(notEqual(attribute0, temporary[0])))",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated float comparison shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIIndirectSamplerInstruction(t *testing.T) {
	const shader = `FRAG
DCL OUT[0], COLOR
DCL SAMP[0..1]
DCL SVIEW[0..1], 2D, FLOAT
DCL TEMP[0]
DCL ADDR[0]
0: TEX TEMP[0], TEMP[0], SAMP[ADDR[0].x], 2D
1: MOV OUT[0], TEMP[0]
2: END`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, sampler := range []string{"fragmentSampler0", "fragmentSampler1"} {
		if !strings.Contains(glsl, "texture("+sampler) {
			t.Fatalf("translated indirect sampler shader does not sample %s:\n%s", sampler, glsl)
		}
	}
}

func TestTranslateTGSIDoubleArithmeticUsesPackedRegisterPairs(t *testing.T) {
	const shader = `FRAG
DCL OUT[0], COLOR
DCL TEMP[0..4]
IMM[0] FLT64 {1.25, 2.0}
0: DADD TEMP[0].xy, IMM[0].xyxy, IMM[0].zwzw
1: DMUL TEMP[1].zw, TEMP[0].xyxy, IMM[0].zwzw
2: D2F TEMP[2].x, TEMP[1].zwzw
3: DSEQ TEMP[3].x, TEMP[1].zwzw, TEMP[1].zwzw
4: MOV OUT[0], TEMP[2]
5: END`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"packDouble2x32(floatBitsToUint((immediate0.xyxy).xy))",
		"temporary[1].zw = uintBitsToFloat(unpackDouble2x32(",
		"temporary[2].x = (vec4(float(packDouble2x32(",
		"intBitsToFloat((packDouble2x32(",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated double shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIAllowsBlankProgrammableStage(t *testing.T) {
	stage, glsl, err := translateTGSI("VERT\n0: END\n")
	if err != nil {
		t.Fatal(err)
	}
	if stage != tgsiVertex || !strings.Contains(glsl, "void main()") {
		t.Fatalf("blank vertex translation stage/source = %d/%q", stage, glsl)
	}
}

func TestTranslateTGSIUsesHostInstancingSystemValues(t *testing.T) {
	shader := `VERT
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
DCL SV[0], INSTANCEID
DCL SV[1], VERTEXID
0: MOV OUT[0], SV[1]
1: MOV OUT[1], SV[0]
2: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, builtin := range []string{"ivec4(gl_InstanceID)", "ivec4(gl_VertexID)"} {
		if !strings.Contains(glsl, builtin) {
			t.Fatalf("translated shader does not use %s:\n%s", builtin, glsl)
		}
	}
}

func TestTranslateTGSIGeometryShaderPreservesIndexedInputsAndEmission(t *testing.T) {
	shader := `GEOM
PROPERTY GS_INPUT_PRIMITIVE TRIANGLES
PROPERTY GS_OUTPUT_PRIMITIVE TRIANGLE_STRIP
PROPERTY GS_MAX_OUTPUT_VERTICES 3
PROPERTY GS_INVOCATIONS 1
DCL IN[][0], POSITION
DCL IN[][1], GENERIC[20]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[20]
IMM[0] INT32 {0, 0, 0, 0}
0: MOV OUT[0], IN[2][0]
1: MOV OUT[1], IN[0][1]
2: EMIT IMM[0].xxxx
3: ENDPRIM
4: END
`
	stage, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	if stage != tgsiGeometry {
		t.Fatalf("translated stage = %d, want geometry", stage)
	}
	for _, expected := range []string{
		"layout(triangles) in;",
		"layout(triangle_strip, max_vertices = 3) out;",
		"in vec4 varying_generic_20GeometryInput[];",
		"geometryPosition = gl_in[2].gl_Position;",
		"varying_generic_20 = varying_generic_20GeometryInput[0];",
		"gl_Position = geometryPosition;",
		"EmitVertex();",
		"EndPrimitive();",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated geometry shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIGeometryStreamsOwnTheirFeedbackOutputs(t *testing.T) {
	shader := `GEOM
PROPERTY GS_INPUT_PRIMITIVE POINTS
PROPERTY GS_OUTPUT_PRIMITIVE POINTS
PROPERTY GS_MAX_OUTPUT_VERTICES 1
PROPERTY GS_INVOCATIONS 1
DCL IN[][0], POSITION
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
IMM[0] INT32 {0, 1, 2, 3}
0: MOV OUT[0], IN[0][0]
1: MOV OUT[1], IN[0][0]
2: EMIT IMM[0].yyyy
3: ENDPRIM IMM[0].yyyy
4: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	glsl, varyings, _, err := addTGSIStreamOutputs(shader, glsl, []tgsiStreamOutput{{
		registerIndex: 1, numComponents: 4, buffer: 0, stream: 1,
	}}, [4]uint32{4})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"layout(stream = 1) out vec4 streamout0;",
		"streamout0 = varying_generic_0;",
		"EmitStreamVertex(1);",
		"EndStreamPrimitive(1);",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("streamed geometry shader lacks %q:\n%s", expected, glsl)
		}
	}
	if len(varyings) != 1 || varyings[0] != "streamout0" {
		t.Fatalf("streamed transform-feedback varyings = %v", varyings)
	}
}

func TestTranslateTGSIReplicatesColorZeroToAllDrawBuffers(t *testing.T) {
	shader := `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL OUT[0], COLOR
DCL CONST[0]
0: MOV OUT[0], CONST[0]
1: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index < 8; index++ {
		declaration := fmt.Sprintf("layout(location = %d) out vec4 fragmentColor%d;", index, index)
		assignment := fmt.Sprintf("fragmentColor%d = fragmentColor0;", index)
		if !strings.Contains(glsl, declaration) || !strings.Contains(glsl, assignment) {
			t.Fatalf("replicated color output %d is incomplete:\n%s", index, glsl)
		}
	}
}

func TestTranslateTGSIMapsFragmentDepthBeforeColorZero(t *testing.T) {
	const shader = `FRAG
PROPERTY FS_COLOR0_WRITES_ALL_CBUFS 1
DCL OUT[0], POSITION
DCL OUT[1], COLOR
IMM[0] FLT32 {1.0, 0.0, 0.0, 0.0}
0: MOV OUT[0].z, IMM[0].x
1: MOV OUT[1], IMM[0]
2: END`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"layout(location = 0) out vec4 fragmentColor0;",
		"fragmentPosition.z = (immediate0.xxxx).z;",
		"gl_FragDepth = fragmentPosition.z;",
		"fragmentColor7 = fragmentColor0;",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated depth-writing fragment shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSISampleBuiltins(t *testing.T) {
	shader := `FRAG
DCL SV[0], SAMPLEID
DCL SV[1], SAMPLEPOS
DCL SV[2], SAMPLEMASK
DCL OUT[0], COLOR
DCL OUT[1], SAMPLEMASK
DCL TEMP[0]
0: ADD TEMP[0], SV[0], SV[1]
1: ADD OUT[0], TEMP[0], SV[2]
2: MOV OUT[1], SV[2]
3: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"gl_SampleID", "gl_SamplePosition", "gl_SampleMaskIn[0]",
		"gl_SampleMask[0] = floatBitsToInt(fragmentSampleMask).x;",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("sample-aware fragment shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSITextureBufferFetches(t *testing.T) {
	shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0..2]
DCL SVIEW[0], BUFFER, FLOAT
DCL SVIEW[1], BUFFER, UINT
DCL SVIEW[2], BUFFER, SINT
DCL TEMP[0..2]
IMM[0] INT32 {1, 0, 0, 0}
0: TXF TEMP[0], IMM[0], SAMP[0], BUFFER
1: TXF TEMP[1], IMM[0], SAMP[1], BUFFER
2: TXF TEMP[2], IMM[0], SAMP[2], BUFFER
3: ADD TEMP[0], TEMP[0], TEMP[1]
4: ADD OUT[0], TEMP[0], TEMP[2]
5: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"uniform samplerBuffer fragmentSampler0;",
		"uniform usamplerBuffer fragmentSampler1;",
		"uniform isamplerBuffer fragmentSampler2;",
		"texelFetch(fragmentSampler0, (floatBitsToInt(immediate0)).x)",
		"texelFetch(fragmentSampler1, (floatBitsToInt(immediate0)).x)",
		"texelFetch(fragmentSampler2, (floatBitsToInt(immediate0)).x)",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("texture-buffer shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSICubeArraySampling(t *testing.T) {
	shader := `FRAG
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
6: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"uniform samplerCubeArray fragmentSampler0;",
		"texture(fragmentSampler0, immediate0)",
		"textureLod(fragmentSampler0, immediate0, (immediate1).x)",
		"texture(fragmentSampler0, immediate0, (immediate1).x)",
		"textureQueryLod(fragmentSampler0, (immediate0).xyz)",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("cube-array shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIMapsClipDistanceOutputsToGLBuiltins(t *testing.T) {
	shader := `VERT
DCL OUT[0], POSITION
DCL OUT[1], CLIPDIST
DCL OUT[2].xy, CLIPDIST[1]
IMM[0] UINT32 {0, 0, 0, 0}
0: MOV OUT[0], IMM[0]
1: MOV OUT[1], IMM[0]
2: MOV OUT[2].xy, IMM[0]
3: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"out float gl_ClipDistance[8];",
		"vec4 clipDistance0;", "vec4 clipDistance1;",
		"clipDistance0 = vec4(0.0);", "clipDistance1 = vec4(0.0);",
		"gl_ClipDistance[0] = clipDistance0.x;",
		"gl_ClipDistance[4] = clipDistance1.x;",
		"gl_ClipDistance[7] = clipDistance1.w;",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIMapsFragmentClipDistanceInputsFromGLBuiltins(t *testing.T) {
	shader := `FRAG
DCL IN[0], CLIPDIST
DCL IN[1].xy, CLIPDIST[1]
DCL OUT[0], COLOR
0: ADD OUT[0], IN[0], IN[1]
1: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"in float gl_ClipDistance[8];",
		"vec4 clipDistance0;", "vec4 clipDistance1;",
		"clipDistance0 = vec4(gl_ClipDistance[0], gl_ClipDistance[1], gl_ClipDistance[2], gl_ClipDistance[3]);",
		"clipDistance1 = vec4(gl_ClipDistance[4], gl_ClipDistance[5], gl_ClipDistance[6], gl_ClipDistance[7]);",
		"fragmentColor0 = (clipDistance0 + clipDistance1);",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIMapsFragmentInterpolationOntoLinkedInterface(t *testing.T) {
	vertexTGSI := `VERT
DCL IN[0]
DCL OUT[0], POSITION
DCL OUT[1], GENERIC[0]
0: MOV OUT[0], IN[0]
1: MOV OUT[1], IN[0]
2: END
`
	fragmentTGSI := `FRAG
DCL IN[0], GENERIC[0], LINEAR, CENTROID
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
	const declaration = "centroid noperspective in vec4 varying_generic_0;"
	if !strings.Contains(fragment, declaration) {
		t.Fatalf("translated fragment shader lacks %q:\n%s", declaration, fragment)
	}
	vertex = linkTGSIInterfaces(vertex, fragment)
	const output = "centroid noperspective out vec4 varying_generic_0;"
	if !strings.Contains(vertex, output) {
		t.Fatalf("linked vertex shader lacks %q:\n%s", output, vertex)
	}
}

func TestTranslateTGSIIntegerSamplerViewsPreserveRawRegisterBits(t *testing.T) {
	for _, test := range []struct {
		returnType string
		sampler    string
		conversion string
	}{
		{"UINT", "usampler2D", "uintBitsToFloat(texture("},
		{"SINT", "isampler2D", "intBitsToFloat(texture("},
	} {
		t.Run(test.returnType, func(t *testing.T) {
			shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 2D, ` + test.returnType + `
DCL TEMP[0]
0: TEX TEMP[0], TEMP[0], SAMP[0], 2D
1: MOV OUT[0], TEMP[0]
2: END
`
			_, glsl, err := translateTGSI(shader)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{"uniform " + test.sampler, test.conversion} {
				if !strings.Contains(glsl, expected) {
					t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
				}
			}
		})
	}
}

func TestTranslateTGSIUsesOneDimensionalSamplerCoordinates(t *testing.T) {
	shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 1D, FLOAT
DCL TEMP[0]
0: TEX TEMP[0], TEMP[0], SAMP[0], 1D
1: TXF TEMP[0], TEMP[0], SAMP[0], 1D
2: MOV OUT[0], TEMP[0]
3: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"uniform sampler1D",
		"texture(fragmentSampler0, (temporary[0]).x)",
		"texelFetch(fragmentSampler0, (floatBitsToInt(temporary[0])).x, (floatBitsToInt(temporary[0])).w)",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIUsesOneDimensionalArraySamplerCoordinates(t *testing.T) {
	shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 1D_ARRAY, FLOAT
DCL TEMP[0]
0: TXF TEMP[0], TEMP[0], SAMP[0], 1D_ARRAY
1: MOV OUT[0], TEMP[0]
2: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"uniform sampler1DArray",
		"texelFetch(fragmentSampler0, (floatBitsToInt(temporary[0])).xy, (floatBitsToInt(temporary[0])).w)",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIUsesRectangleSamplerCoordinates(t *testing.T) {
	shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], RECT, FLOAT
DCL TEMP[0]
0: TEX TEMP[0], TEMP[0], SAMP[0], RECT
1: TXF TEMP[0], TEMP[0], SAMP[0], RECT
2: MOV OUT[0], TEMP[0]
3: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"uniform sampler2D",
		"texture(fragmentSampler0, (temporary[0]).xy / vec2(textureSize(fragmentSampler0, 0)))",
		"texelFetch(fragmentSampler0, (floatBitsToInt(temporary[0])).xy, 0)",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIUsesThreeDimensionalSamplerCoordinates(t *testing.T) {
	shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], 3D, FLOAT
DCL TEMP[0]
0: TEX TEMP[0], TEMP[0], SAMP[0], 3D
1: TXF TEMP[0], TEMP[0], SAMP[0], 3D
2: MOV OUT[0], TEMP[0]
3: END
`
	_, glsl, err := translateTGSI(shader)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"uniform sampler3D",
		"texture(fragmentSampler0, (temporary[0]).xyz)",
		"texelFetch(fragmentSampler0, (floatBitsToInt(temporary[0])).xyz, (floatBitsToInt(temporary[0])).w)",
	} {
		if !strings.Contains(glsl, expected) {
			t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
		}
	}
}

func TestTranslateTGSIQueriesTextureLODBySamplerDimension(t *testing.T) {
	for _, test := range []struct {
		target     string
		coordinate string
	}{
		{target: "1D", coordinate: "(temporary[0]).x"},
		{target: "2D", coordinate: "(temporary[0]).xy"},
		{target: "3D", coordinate: "(temporary[0]).xyz"},
	} {
		t.Run(test.target, func(t *testing.T) {
			shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], ` + test.target + `, FLOAT
DCL TEMP[0]
0: LODQ TEMP[0], TEMP[0], SAMP[0], ` + test.target + `
1: MOV OUT[0], TEMP[0]
2: END
`
			_, glsl, err := translateTGSI(shader)
			if err != nil {
				t.Fatal(err)
			}
			expected := "vec4(textureQueryLod(fragmentSampler0, " + test.coordinate + "), 0.0, 0.0)"
			if !strings.Contains(glsl, expected) {
				t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
			}
		})
	}
}

func TestTranslateTGSIUsesMultisampleSamplerCoordinates(t *testing.T) {
	for _, test := range []struct {
		target      string
		samplerType string
		coordinate  string
	}{
		{"2D_MSAA", "sampler2DMS", "xy"},
		{"2D_ARRAY_MSAA", "sampler2DMSArray", "xyz"},
	} {
		t.Run(test.target, func(t *testing.T) {
			shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], ` + test.target + `, FLOAT
DCL TEMP[0]
0: TXF TEMP[0], TEMP[0], SAMP[0], ` + test.target + `
1: MOV OUT[0], TEMP[0]
2: END
`
			_, glsl, err := translateTGSI(shader)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{
				"uniform " + test.samplerType,
				"texelFetch(fragmentSampler0, (floatBitsToInt(temporary[0]))." + test.coordinate + ", (floatBitsToInt(temporary[0])).w)",
			} {
				if !strings.Contains(glsl, expected) {
					t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
				}
			}
		})
	}
}

func TestTranslateTGSIUsesLayeredStorageForIntegerMultisampleSamplers(t *testing.T) {
	for _, test := range []struct {
		target      string
		returnType  string
		samplerType string
		expected    string
	}{
		{
			target: "2D_MSAA", returnType: "UINT", samplerType: "usampler2DArray",
			expected: "texelFetch(fragmentSampler0, ivec3((floatBitsToInt(temporary[0])).xy, (floatBitsToInt(temporary[0])).w), 0)",
		},
		{
			target: "2D_ARRAY_MSAA", returnType: "SINT", samplerType: "isampler2DArray",
			expected: "texelFetch(fragmentSampler0, ivec3((floatBitsToInt(temporary[0])).xy, (floatBitsToInt(temporary[0])).z * fragmentSampler0SampleCount + (floatBitsToInt(temporary[0])).w), 0)",
		},
	} {
		t.Run(test.target+"_"+test.returnType, func(t *testing.T) {
			shader := `FRAG
DCL OUT[0], COLOR
DCL SAMP[0]
DCL SVIEW[0], ` + test.target + `, ` + test.returnType + `
DCL TEMP[0]
0: TXF TEMP[0], TEMP[0], SAMP[0], ` + test.target + `
1: MOV OUT[0], TEMP[0]
2: END
`
			_, glsl, err := translateTGSI(shader)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range []string{
				"uniform " + test.samplerType,
				"uniform int fragmentSampler0SampleCount",
				test.expected,
			} {
				if !strings.Contains(glsl, expected) {
					t.Fatalf("translated shader lacks %q:\n%s", expected, glsl)
				}
			}
		})
	}
}
