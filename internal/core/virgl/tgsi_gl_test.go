//go:build darwin || ((linux || windows) && amd64)

package virgl

import "testing"

func TestTessellationTGSICompilesInHostContext(t *testing.T) {
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
	host := newReplayTestHost(t)
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
