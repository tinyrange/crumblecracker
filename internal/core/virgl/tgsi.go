package virgl

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	tgsiVertex         = 0
	tgsiFragment       = 1
	tgsiGeometry       = 2
	tgsiTessControl    = 3
	tgsiTessEvaluation = 4
)

const (
	fragmentOutputFloat uint8 = iota
	fragmentOutputUInt
	fragmentOutputSInt
)

const (
	emulatedVertexSystemNone uint8 = iota
	emulatedVertexID
	emulatedInstanceID
)

type hostVertexSystemEmulation struct {
	systemValue uint8
	attribute   uint8
}

type tgsiDeclaration struct {
	index         int
	semantic      string
	interpolation string
	location      string
}

type tgsiStreamOutput struct {
	registerIndex  uint32
	startComponent uint32
	numComponents  uint32
	buffer         uint32
	dstOffset      uint32
	stream         uint32
}

type tgsiSamplerView struct {
	target     string
	returnType string
}

func normalizeTGSISamplerTarget(target string) string {
	switch target {
	case "CUBEARRAY":
		return "CUBE_ARRAY"
	case "SHADOWCUBEARRAY":
		return "SHADOWCUBE_ARRAY"
	default:
		return target
	}
}

func supportedTGSISamplerTarget(target string) bool {
	switch target {
	case "BUFFER", "1D", "1D_ARRAY", "2D", "2D_MSAA", "2D_ARRAY_MSAA", "3D", "RECT",
		"SHADOW2D", "SHADOW2D_ARRAY", "SHADOWRECT", "CUBE", "CUBE_ARRAY", "SHADOWCUBE",
		"SHADOWCUBE_ARRAY", "2D_ARRAY":
		return true
	default:
		return false
	}
}

func tgsiGatherTarget(target string) bool {
	switch target {
	case "2D", "2D_ARRAY", "RECT", "CUBE", "CUBE_ARRAY", "SHADOW2D", "SHADOW2D_ARRAY",
		"SHADOWRECT", "SHADOWCUBE", "SHADOWCUBE_ARRAY":
		return true
	default:
		return false
	}
}

type tgsiTempArray struct {
	first int
	last  int
}

type tgsiShader struct {
	stage                   uint32
	inputs                  map[int]tgsiDeclaration
	systemValues            map[int]tgsiDeclaration
	outputs                 map[int]tgsiDeclaration
	maxConstants            [16]int
	maxTemporary            int
	maxAddress              int
	maxSampler              int
	maxSamplerView          int
	samplerViews            map[int]tgsiSamplerView
	tempArrays              map[int]tgsiTempArray
	immediates              []string
	immediateInts           [][4]int32
	instructions            []string
	loopDepth               int
	deferredDiscard         bool
	geometryInputPrimitive  string
	geometryOutputPrimitive string
	geometryMaxVertices     int
	geometryInvocations     int
	tessControlVertices     int
	tessEvaluationPrimitive int
	tessEvaluationSpacing   int
	tessEvaluationClockwise bool
	tessEvaluationPointMode bool
	fragmentColor0WritesAll bool
	usesDoubleRoundEven     bool
	usesDoubleEquality      bool
	usesCubeGradientFix     bool
}

var (
	tgsiDeclarationPattern          = regexp.MustCompile(`^DCL (IN|OUT|SV|CONST|TEMP|ADDR|SAMP)\[(\d+)(?:\.\.(\d+))?\](?:\.[xyzw]+)?(?:,\s*([^,]+))?(?:,\s*(.*))?$`)
	tgsiConstantDeclarationPattern  = regexp.MustCompile(`^DCL CONST\[(\d+)\]\[(\d+)(?:\.\.(\d+))?\](?:\.[xyzw]+)?`)
	tgsiImmediatePattern            = regexp.MustCompile(`^IMM\[(\d+)\]\s+(\w+)\s+\{([^}]+)\}$`)
	tgsiSamplerViewPattern          = regexp.MustCompile(`^DCL SVIEW\[(\d+)(?:\.\.(\d+))?\],\s*([A-Z0-9_]+),\s*([A-Z0-9_]+)$`)
	tgsiTempArrayDeclarationPattern = regexp.MustCompile(`^DCL TEMP\[(\d+)\.\.(\d+)\],\s*ARRAY\((\d+)\)`)
	tgsiInstructionPattern          = regexp.MustCompile(`^\s*\d+:\s+([A-Z0-9_]+)(?:\s+(.+))?$`)
	tgsiRegisterPattern             = regexp.MustCompile(`^(IN|OUT|SV|CONST|TEMP|IMM)\[(\d+)\](?:\.([xyzw]+))?$`)
	tgsiGeometryInputPattern        = regexp.MustCompile(`^IN\[(\d+)\]\[(\d+)\](?:\.([xyzw]+))?$`)
	tgsiIndirectDimensionalInput    = regexp.MustCompile(`^IN\[ADDR\[(\d+)\]\.([xyzw])\]\[(\d+)\](?:\.([xyzw]+))?$`)
	tgsiGeometryInputDeclaration    = regexp.MustCompile(`^DCL IN\[\]\[(\d+)(?:\.\.(\d+))?\](?:\.[xyzw]+)?(?:,\s*([^,]+))?(?:,\s*(.*))?$`)
	tgsiTessInputSystemPattern      = regexp.MustCompile(`^IN\[SV\[(\d+)\]\.([xyzw])\]\[(\d+)\](?:\.([xyzw]+))?$`)
	tgsiTessOutputPattern           = regexp.MustCompile(`^OUT\[(?:(\d+)|SV\[(\d+)\]\.([xyzw]))\]\[(\d+)\](?:\.([xyzw]+))?$`)
	tgsiIndirectTessOutputPattern   = regexp.MustCompile(`^OUT\[ADDR\[(\d+)\]\.([xyzw])\]\[(\d+)\](?:\.([xyzw]+))?$`)
	tgsiTessOutputDeclaration       = regexp.MustCompile(`^DCL OUT\[\]\[(\d+)(?:\.\.(\d+))?\](?:\.[xyzw]+)?(?:,\s*([^,]+))?(?:,\s*(.*))?$`)
	tgsiPropertyPattern             = regexp.MustCompile(`^PROPERTY\s+([A-Z0-9_]+)\s+([A-Z0-9_]+)$`)
	tgsiConstantRegisterPattern     = regexp.MustCompile(`^CONST\[(\d+)\]\[(\d+)\](?:\.([xyzw]+))?$`)
	tgsiIndirectPattern             = regexp.MustCompile(`^(CONST|TEMP)\[ADDR\[(\d+)\]\.([xyzw])([+-]\d+)?\](?:\((\d+)\))?(?:\.([xyzw]+))?$`)
	tgsiConstantIndirectPattern     = regexp.MustCompile(`^CONST\[(\d+)\]\[ADDR\[(\d+)\]\.([xyzw])([+-]\d+)?\](?:\(\d+\))?(?:\.([xyzw]+))?$`)
	tgsiAddressPattern              = regexp.MustCompile(`^ADDR\[(\d+)\](?:\.([xyzw]+))?$`)
	tgsiControlLabel                = regexp.MustCompile(`(?:^|\s+):\d+$`)
	tgsiSamplerPattern              = regexp.MustCompile(`^SAMP\[(\d+)\]$`)
	tgsiIndirectSamplerPattern      = regexp.MustCompile(`^SAMP\[ADDR\[(\d+)\]\.([xyzw])([+-]\d+)?\]$`)
)

func translateTGSI(source string) (uint32, string, error) {
	shader := tgsiShader{
		inputs:         make(map[int]tgsiDeclaration),
		systemValues:   make(map[int]tgsiDeclaration),
		outputs:        make(map[int]tgsiDeclaration),
		samplerViews:   make(map[int]tgsiSamplerView),
		tempArrays:     make(map[int]tgsiTempArray),
		maxTemporary:   -1,
		maxAddress:     -1,
		maxSampler:     -1,
		maxSamplerView: -1,
	}
	for index := range shader.maxConstants {
		shader.maxConstants[index] = -1
	}
	scanner := bufio.NewScanner(strings.NewReader(source))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if match := tgsiPropertyPattern.FindStringSubmatch(line); match != nil {
			switch match[1] {
			case "FS_COLOR0_WRITES_ALL_CBUFS":
				shader.fragmentColor0WritesAll = match[2] == "1"
			case "GS_INPUT_PRIMITIVE":
				shader.geometryInputPrimitive = match[2]
			case "GS_OUTPUT_PRIMITIVE":
				shader.geometryOutputPrimitive = match[2]
			case "GS_MAX_OUTPUT_VERTICES":
				shader.geometryMaxVertices, _ = strconv.Atoi(match[2])
			case "GS_INVOCATIONS":
				shader.geometryInvocations, _ = strconv.Atoi(match[2])
			case "TCS_VERTICES_OUT":
				shader.tessControlVertices, _ = strconv.Atoi(match[2])
			case "TES_PRIM_MODE":
				shader.tessEvaluationPrimitive, _ = strconv.Atoi(match[2])
			case "TES_SPACING":
				shader.tessEvaluationSpacing, _ = strconv.Atoi(match[2])
			case "TES_VERTEX_ORDER_CW":
				shader.tessEvaluationClockwise = match[2] == "1"
			case "TES_POINT_MODE":
				shader.tessEvaluationPointMode = match[2] == "1"
			}
			continue
		}
		switch line {
		case "VERT":
			shader.stage = tgsiVertex
			continue
		case "FRAG":
			shader.stage = tgsiFragment
			continue
		case "GEOM":
			shader.stage = tgsiGeometry
			continue
		case "TESS_CTRL":
			shader.stage = tgsiTessControl
			continue
		case "TESS_EVAL":
			shader.stage = tgsiTessEvaluation
			continue
		}
		if match := tgsiGeometryInputDeclaration.FindStringSubmatch(line); match != nil {
			first, _ := strconv.Atoi(match[1])
			last := first
			if match[2] != "" {
				last, _ = strconv.Atoi(match[2])
			}
			semantic, interpolation, location := parseTGSIDeclarationModifiers(match[3], match[4])
			for index := first; index <= last; index++ {
				shader.inputs[index] = tgsiDeclaration{index: index, semantic: semantic, interpolation: interpolation, location: location}
			}
			continue
		}
		if match := tgsiTessOutputDeclaration.FindStringSubmatch(line); match != nil {
			if shader.stage != tgsiTessControl {
				return 0, "", fmt.Errorf("TGSI line %d dimensional output is invalid in stage %d", lineNumber, shader.stage)
			}
			first, _ := strconv.Atoi(match[1])
			last := first
			if match[2] != "" {
				last, _ = strconv.Atoi(match[2])
			}
			semantic, interpolation, location := parseTGSIDeclarationModifiers(match[3], match[4])
			for index := first; index <= last; index++ {
				shader.outputs[index] = tgsiDeclaration{index: index, semantic: semantic, interpolation: interpolation, location: location}
			}
			continue
		}
		if match := tgsiConstantDeclarationPattern.FindStringSubmatch(line); match != nil {
			buffer, _ := strconv.Atoi(match[1])
			if buffer >= len(shader.maxConstants) {
				return 0, "", fmt.Errorf("TGSI line %d constant buffer %d is unsupported", lineNumber, buffer)
			}
			last, _ := strconv.Atoi(match[2])
			if match[3] != "" {
				last, _ = strconv.Atoi(match[3])
			}
			shader.maxConstants[buffer] = max(shader.maxConstants[buffer], last)
			continue
		}
		if match := tgsiTempArrayDeclarationPattern.FindStringSubmatch(line); match != nil {
			first, _ := strconv.Atoi(match[1])
			last, _ := strconv.Atoi(match[2])
			array, _ := strconv.Atoi(match[3])
			if first > last || array == 0 {
				return 0, "", fmt.Errorf("TGSI line %d temporary array declaration is invalid", lineNumber)
			}
			shader.tempArrays[array] = tgsiTempArray{first: first, last: last}
			shader.maxTemporary = max(shader.maxTemporary, last)
			continue
		}
		if match := tgsiDeclarationPattern.FindStringSubmatch(line); match != nil {
			first, _ := strconv.Atoi(match[2])
			last := first
			if match[3] != "" {
				last, _ = strconv.Atoi(match[3])
			}
			semantic, interpolation, location := parseTGSIDeclarationModifiers(match[4], match[5])
			switch match[1] {
			case "IN":
				shader.inputs[first] = tgsiDeclaration{index: first, semantic: semantic, interpolation: interpolation, location: location}
			case "OUT":
				shader.outputs[first] = tgsiDeclaration{index: first, semantic: semantic, interpolation: interpolation, location: location}
			case "SV":
				for index := first; index <= last; index++ {
					shader.systemValues[index] = tgsiDeclaration{index: index, semantic: semantic, interpolation: interpolation, location: location}
				}
			case "CONST":
				shader.maxConstants[0] = max(shader.maxConstants[0], last)
			case "TEMP":
				shader.maxTemporary = max(shader.maxTemporary, last)
			case "ADDR":
				shader.maxAddress = max(shader.maxAddress, last)
			case "SAMP":
				shader.maxSampler = max(shader.maxSampler, last)
			}
			continue
		}
		if match := tgsiSamplerViewPattern.FindStringSubmatch(line); match != nil {
			first, _ := strconv.Atoi(match[1])
			last := first
			if match[2] != "" {
				last, _ = strconv.Atoi(match[2])
			}
			target := normalizeTGSISamplerTarget(match[3])
			if !supportedTGSISamplerTarget(target) ||
				(match[4] != "FLOAT" && match[4] != "UINT" && match[4] != "SINT") ||
				(strings.HasPrefix(target, "SHADOW") && match[4] != "FLOAT") {
				return 0, "", fmt.Errorf("TGSI line %d sampler view %s/%s is unsupported", lineNumber, match[3], match[4])
			}
			for index := first; index <= last; index++ {
				shader.samplerViews[index] = tgsiSamplerView{target: target, returnType: match[4]}
			}
			shader.maxSamplerView = max(shader.maxSamplerView, last)
			continue
		}
		if match := tgsiImmediatePattern.FindStringSubmatch(line); match != nil {
			index, _ := strconv.Atoi(match[1])
			if index != len(shader.immediates) {
				return 0, "", fmt.Errorf("TGSI line %d immediate index %d is not contiguous", lineNumber, index)
			}
			values := splitTGSIList(match[3])
			wantValues := 4
			if match[2] == "FLT64" {
				wantValues = 2
			}
			if len(values) != wantValues {
				return 0, "", fmt.Errorf("TGSI line %d %s immediate has %d components, want %d", lineNumber, match[2], len(values), wantValues)
			}
			switch match[2] {
			case "UINT32":
				shader.immediates = append(shader.immediates,
					"uintBitsToFloat(uvec4("+strings.Join(values, ", ")+"))")
				shader.immediateInts = append(shader.immediateInts, parseTGSIIntegerImmediate(values))
			case "INT32":
				shader.immediates = append(shader.immediates,
					"intBitsToFloat(ivec4("+strings.Join(values, ", ")+"))")
				shader.immediateInts = append(shader.immediateInts, parseTGSIIntegerImmediate(values))
			case "FLT32":
				components := make([]string, len(values))
				for index, value := range values {
					if strings.HasPrefix(strings.ToLower(value), "0x") {
						components[index] = "uintBitsToFloat(uint(" + value + "))"
					} else {
						components[index] = value
					}
				}
				shader.immediates = append(shader.immediates, "vec4("+strings.Join(components, ", ")+")")
				shader.immediateInts = append(shader.immediateInts, [4]int32{})
			case "FLT64":
				var words [4]uint32
				for index, value := range values {
					parsed, err := strconv.ParseFloat(value, 64)
					if err != nil {
						return 0, "", fmt.Errorf("TGSI line %d FLT64 immediate component %q is invalid", lineNumber, value)
					}
					packed := math.Float64bits(parsed)
					words[index*2], words[index*2+1] = uint32(packed), uint32(packed>>32)
				}
				shader.immediates = append(shader.immediates, fmt.Sprintf(
					"uintBitsToFloat(uvec4(%du, %du, %du, %du))", words[0], words[1], words[2], words[3]))
				shader.immediateInts = append(shader.immediateInts, [4]int32{})
			default:
				return 0, "", fmt.Errorf("TGSI line %d immediate type %s is unsupported", lineNumber, match[2])
			}
			continue
		}
		if match := tgsiInstructionPattern.FindStringSubmatch(line); match != nil {
			if match[1] == "END" {
				continue
			}
			operands := match[2]
			switch match[1] {
			case "BGNLOOP", "ENDLOOP", "ELSE":
				operands = tgsiControlLabel.ReplaceAllString(operands, "")
			case "IF", "UIF":
				operands = tgsiControlLabel.ReplaceAllString(operands, "")
			}
			statement, err := shader.translateInstruction(match[1], splitTGSIList(operands))
			if err != nil {
				return 0, "", fmt.Errorf("TGSI line %d: %w", lineNumber, err)
			}
			shader.instructions = append(shader.instructions, statement)
			continue
		}
		return 0, "", fmt.Errorf("TGSI line %d is unsupported: %q", lineNumber, line)
	}
	if err := scanner.Err(); err != nil {
		return 0, "", err
	}
	glsl, err := shader.glsl()
	return shader.stage, glsl, err
}

func addTGSIStreamOutputs(tgsi, glsl string, outputs []tgsiStreamOutput, strides [4]uint32) (string, []string, bool, error) {
	if len(outputs) == 0 {
		return glsl, nil, false, nil
	}
	shader := tgsiShader{outputs: make(map[int]tgsiDeclaration)}
	scanner := bufio.NewScanner(strings.NewReader(tgsi))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch line {
		case "VERT":
			shader.stage = tgsiVertex
		case "FRAG":
			shader.stage = tgsiFragment
		case "GEOM":
			shader.stage = tgsiGeometry
		case "TESS_EVAL":
			shader.stage = tgsiTessEvaluation
		}
		match := tgsiDeclarationPattern.FindStringSubmatch(line)
		if match == nil || match[1] != "OUT" {
			continue
		}
		first, _ := strconv.Atoi(match[2])
		last := first
		if match[3] != "" {
			last, _ = strconv.Atoi(match[3])
		}
		semantic, interpolation, location := parseTGSIDeclarationModifiers(match[4], match[5])
		for index := first; index <= last; index++ {
			shader.outputs[index] = tgsiDeclaration{index: index, semantic: semantic, interpolation: interpolation, location: location}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, false, err
	}
	if shader.stage != tgsiVertex && shader.stage != tgsiGeometry && shader.stage != tgsiTessEvaluation {
		return "", nil, false, fmt.Errorf("stream output requires a vertex, tessellation evaluation, or geometry shader, got stage %d", shader.stage)
	}

	prepared := make([]tgsiStreamOutput, len(outputs))
	copy(prepared, outputs)
	sort.SliceStable(prepared, func(left, right int) bool {
		if prepared[left].buffer != prepared[right].buffer {
			return prepared[left].buffer < prepared[right].buffer
		}
		return prepared[left].dstOffset < prepared[right].dstOffset
	})

	var declarations, assignments strings.Builder
	names := make(map[tgsiStreamOutput]string, len(outputs))
	bufferStreams := [4]int32{-1, -1, -1, -1}
	for index, output := range outputs {
		if output.buffer >= uint32(len(strides)) || output.stream > 3 || output.numComponents == 0 ||
			output.numComponents > 4 || output.startComponent+output.numComponents > 4 {
			return "", nil, false, fmt.Errorf("invalid stream output %+v", output)
		}
		if bufferStreams[output.buffer] >= 0 && bufferStreams[output.buffer] != int32(output.stream) {
			return "", nil, false, fmt.Errorf("stream output buffer %d mixes streams %d and %d", output.buffer, bufferStreams[output.buffer], output.stream)
		}
		bufferStreams[output.buffer] = int32(output.stream)
		if _, ok := shader.outputs[int(output.registerIndex)]; !ok {
			return "", nil, false, fmt.Errorf("stream output register %d is not declared", output.registerIndex)
		}
		name := fmt.Sprintf("streamout%d", index)
		names[output] = name
		typeName := "float"
		if output.numComponents > 1 {
			typeName = fmt.Sprintf("vec%d", output.numComponents)
		}
		if output.stream == 0 {
			fmt.Fprintf(&declarations, "out %s %s;\n", typeName, name)
		} else {
			fmt.Fprintf(&declarations, "layout(stream = %d) out %s %s;\n", output.stream, typeName, name)
		}
		swizzle := "xyzw"[output.startComponent : output.startComponent+output.numComponents]
		expression := shader.outputName(int(output.registerIndex))
		if output.numComponents != 4 || output.startComponent != 0 {
			expression += "." + swizzle
		}
		fmt.Fprintf(&assignments, "    %s = %s;\n", name, expression)
	}
	glsl = strings.Replace(glsl, "#version 410 core\n", "#version 410 core\n"+declarations.String(), 1)
	if shader.stage == tgsiGeometry && strings.Contains(glsl, "/* tgsi-streamout */") {
		glsl = strings.ReplaceAll(glsl, "/* tgsi-streamout */", assignments.String())
	} else if marker := "    gl_Position.y *= uWinsysAdjustY;\n"; strings.Contains(glsl, marker) {
		glsl = strings.Replace(glsl, marker, assignments.String()+marker, 1)
	} else if end := strings.LastIndex(glsl, "\n}"); end >= 0 {
		glsl = glsl[:end] + "\n" + assignments.String() + glsl[end:]
	} else {
		return "", nil, false, errors.New("translated shader has no main-function terminator")
	}

	var varyings []string
	appendSkip := func(components uint32) {
		for components != 0 {
			count := min(components, uint32(4))
			varyings = append(varyings, fmt.Sprintf("gl_SkipComponents%d", count))
			components -= count
		}
	}
	outputIndex := 0
	maxBuffer := prepared[len(prepared)-1].buffer
	for buffer := uint32(0); buffer <= maxBuffer; buffer++ {
		if buffer != 0 {
			varyings = append(varyings, "gl_NextBuffer")
		}
		cursor := uint32(0)
		for outputIndex < len(prepared) && prepared[outputIndex].buffer == buffer {
			output := prepared[outputIndex]
			if output.dstOffset < cursor {
				return "", nil, false, fmt.Errorf("stream output buffer %d overlaps component %d", buffer, output.dstOffset)
			}
			appendSkip(output.dstOffset - cursor)
			varyings = append(varyings, names[output])
			cursor = output.dstOffset + output.numComponents
			outputIndex++
		}
		if cursor > strides[buffer] {
			return "", nil, false, fmt.Errorf("stream output buffer %d uses %d components, stride is %d", buffer, cursor, strides[buffer])
		}
		appendSkip(strides[buffer] - cursor)
	}
	// Apple's OpenGL 4.1 implementation reports the required 64-component
	// interleaved limit, but drops every transformed vertex after the first
	// when one native transform-feedback buffer has exactly that stride. Split
	// the final vec4 into a second native buffer; the host draw path stitches
	// both buffers back into the single VirGL target after capture.
	maxInterleavedWorkaround := strides[0] == 64 && strides[1] == 0 && strides[2] == 0 && strides[3] == 0
	if maxInterleavedWorkaround {
		last := prepared[len(prepared)-1]
		maxInterleavedWorkaround = last.buffer == 0 && last.dstOffset == 60 && last.numComponents == 4
		if maxInterleavedWorkaround {
			lastName := names[last]
			for index := len(varyings) - 1; index >= 0; index-- {
				if varyings[index] == lastName {
					varyings = append(varyings[:index], append([]string{"gl_NextBuffer", lastName}, varyings[index+1:]...)...)
					break
				}
			}
		}
	}
	return glsl, varyings, maxInterleavedWorkaround, nil
}

func splitTGSIList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	raw := strings.Split(value, ",")
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		result = append(result, strings.TrimSpace(item))
	}
	return result
}

func parseTGSIIntegerImmediate(values []string) (result [4]int32) {
	for index, value := range values {
		if strings.HasPrefix(value, "-") {
			parsed, _ := strconv.ParseInt(value, 0, 32)
			result[index] = int32(parsed)
		} else {
			parsed, _ := strconv.ParseUint(value, 0, 32)
			result[index] = int32(parsed)
		}
	}
	return result
}

func (s *tgsiShader) geometryStreamOperand(raw string) (int32, error) {
	match := tgsiRegisterPattern.FindStringSubmatch(raw)
	if match == nil || match[1] != "IMM" {
		return 0, fmt.Errorf("geometry stream operand %q is not an immediate", raw)
	}
	index, _ := strconv.Atoi(match[2])
	if index >= len(s.immediateInts) {
		return 0, fmt.Errorf("geometry stream immediate %d is not declared", index)
	}
	component := 0
	if match[3] != "" {
		component = strings.IndexByte("xyzw", match[3][0])
	}
	stream := s.immediateInts[index][component]
	if stream < 0 || stream > 3 {
		return 0, fmt.Errorf("geometry stream %d is outside 0..3", stream)
	}
	return stream, nil
}

func (s *tgsiShader) translateInstruction(opcode string, operands []string) (string, error) {
	saturate := strings.HasSuffix(opcode, "_SAT")
	if saturate {
		opcode = strings.TrimSuffix(opcode, "_SAT")
	}
	switch opcode {
	case "BARRIER":
		if s.stage != tgsiTessControl || len(operands) != 0 {
			return "", fmt.Errorf("opcode BARRIER has %d operands in stage %d", len(operands), s.stage)
		}
		return "barrier();", nil
	case "EMIT":
		if s.stage != tgsiGeometry || len(operands) != 1 {
			return "", fmt.Errorf("opcode EMIT has %d operands in stage %d", len(operands), s.stage)
		}
		stream, err := s.geometryStreamOperand(operands[0])
		if err != nil {
			return "", err
		}
		emit := "EmitVertex();"
		if stream != 0 {
			emit = fmt.Sprintf("EmitStreamVertex(%d);", stream)
		}
		return s.geometryEmitStatement() + "\n/* tgsi-streamout */\n" + emit, nil
	case "ENDPRIM":
		if s.stage != tgsiGeometry || (len(operands) != 0 && len(operands) != 1) {
			return "", fmt.Errorf("opcode ENDPRIM has %d operands in stage %d", len(operands), s.stage)
		}
		stream := int32(0)
		var err error
		if len(operands) == 1 {
			stream, err = s.geometryStreamOperand(operands[0])
			if err != nil {
				return "", err
			}
		}
		if stream != 0 {
			return fmt.Sprintf("EndStreamPrimitive(%d);", stream), nil
		}
		return "EndPrimitive();", nil
	case "BGNLOOP":
		if len(operands) != 0 {
			return "", fmt.Errorf("opcode BGNLOOP has %d operands, want 0", len(operands))
		}
		s.loopDepth++
		return "while (true) {", nil
	case "ENDLOOP":
		if len(operands) != 0 {
			return "", fmt.Errorf("opcode ENDLOOP has %d operands, want 0", len(operands))
		}
		if s.loopDepth == 0 {
			return "", errors.New("ENDLOOP has no matching BGNLOOP")
		}
		s.loopDepth--
		if !s.deferredDiscard {
			return "}", nil
		}
		if s.loopDepth != 0 {
			return "if (tgsiKilled) break;\n}", nil
		}
		return "}\nif (tgsiKilled) discard;", nil
	case "BRK":
		if len(operands) != 0 {
			return "", fmt.Errorf("opcode BRK has %d operands, want 0", len(operands))
		}
		return "break;", nil
	case "CONT":
		if len(operands) != 0 {
			return "", fmt.Errorf("opcode CONT has %d operands, want 0", len(operands))
		}
		return "continue;", nil
	case "ELSE":
		if len(operands) != 0 {
			return "", fmt.Errorf("opcode ELSE has %d operands, want 0", len(operands))
		}
		return "} else {", nil
	case "ENDIF":
		if len(operands) != 0 {
			return "", fmt.Errorf("opcode ENDIF has %d operands, want 0", len(operands))
		}
		return "}", nil
	case "IF", "UIF":
		if len(operands) != 1 {
			return "", fmt.Errorf("opcode %s has %d operands, want 1", opcode, len(operands))
		}
		condition, _, err := s.register(operands[0], false)
		if err != nil {
			return "", err
		}
		if opcode == "UIF" {
			return "if (floatBitsToUint((" + condition + ").x) != 0u) {", nil
		}
		return "if ((" + condition + ").x != 0.0) {", nil
	case "ARL", "UARL":
		if len(operands) != 2 {
			return "", fmt.Errorf("opcode %s has %d operands, want 2", opcode, len(operands))
		}
		destination, err := s.addressRegister(operands[0])
		if err != nil {
			return "", err
		}
		source, _, err := s.register(operands[1], false)
		if err != nil {
			return "", err
		}
		expression := "ivec4(floor(" + source + "))"
		if opcode == "UARL" {
			expression = "floatBitsToInt(" + source + ")"
		}
		address := tgsiAddressPattern.FindStringSubmatch(operands[0])
		if mask := address[2]; mask != "" {
			expression = "(" + expression + ")." + mask
		}
		return destination + " = " + expression + ";", nil
	case "KILL_IF":
		if len(operands) != 1 {
			return "", fmt.Errorf("opcode KILL_IF has %d operands, want 1", len(operands))
		}
		source, _, err := s.register(operands[0], false)
		if err != nil {
			return "", err
		}
		condition := "any(lessThan((" + source + ") + vec4(0.0), vec4(0.0)))"
		if s.loopDepth != 0 {
			s.deferredDiscard = true
			return "if (" + condition + ") { tgsiKilled = true; break; }", nil
		}
		return "if (" + condition + ") discard;", nil
	case "KILL":
		if len(operands) != 0 {
			return "", fmt.Errorf("opcode KILL has %d operands, want 0", len(operands))
		}
		if s.loopDepth != 0 {
			s.deferredDiscard = true
			return "tgsiKilled = true; break;", nil
		}
		return "discard;", nil
	}
	if statement, handled, err := s.translateDoubleInstruction(opcode, operands, saturate); handled {
		return statement, err
	}
	twoSourceTexture := opcode == "TEX2" || opcode == "TXB2" || opcode == "TXL2"
	gradientTexture := opcode == "TXD"
	gatherTexture := opcode == "TG4"
	if opcode == "TEX" || opcode == "TXP" || opcode == "TXB" || opcode == "TXL" || opcode == "TXF" || opcode == "TXQ" || opcode == "LODQ" || twoSourceTexture || gradientTexture || gatherTexture {
		targetIndex, samplerIndexOperand := 3, 2
		if twoSourceTexture {
			targetIndex, samplerIndexOperand = 4, 3
		} else if gradientTexture {
			targetIndex, samplerIndexOperand = 5, 4
		} else if gatherTexture {
			targetIndex, samplerIndexOperand = 4, 3
		}
		target := ""
		if len(operands) > targetIndex {
			target = normalizeTGSISamplerTarget(operands[targetIndex])
		}
		operandCountOK := len(operands) == targetIndex+1
		hasTextureOffset := !gatherTexture && len(operands) == targetIndex+2
		if hasTextureOffset {
			operandCountOK = true
		}
		if gatherTexture && len(operands) == targetIndex+2 {
			operandCountOK = true
		}
		if !operandCountOK ||
			!supportedTGSISamplerTarget(target) ||
			(target == "BUFFER" && opcode != "TXF" && opcode != "TXQ") ||
			(opcode == "TXP" && (target == "CUBE" || target == "CUBE_ARRAY" || target == "1D_ARRAY" || target == "2D_ARRAY")) ||
			((opcode == "TXP" || opcode == "TXB" || opcode == "TXL") && (target == "RECT" || target == "CUBE_ARRAY")) ||
			(opcode == "LODQ" && (target == "RECT" || target == "2D_MSAA" || target == "2D_ARRAY_MSAA")) ||
			(hasTextureOffset && (opcode == "TXQ" || opcode == "LODQ" || target == "BUFFER" ||
				target == "CUBE" || target == "CUBE_ARRAY" || target == "SHADOWCUBE" || target == "SHADOWCUBE_ARRAY" ||
				target == "2D_MSAA" || target == "2D_ARRAY_MSAA")) ||
			(opcode != "TXF" && opcode != "TXQ" && (target == "2D_MSAA" || target == "2D_ARRAY_MSAA")) ||
			(opcode == "TXF" && (strings.HasPrefix(target, "SHADOW") || target == "CUBE" || target == "CUBE_ARRAY")) ||
			(gatherTexture && (!tgsiGatherTarget(target) || (len(operands) == targetIndex+2 &&
				(target == "CUBE" || target == "CUBE_ARRAY" || target == "SHADOWCUBE" || target == "SHADOWCUBE_ARRAY")))) {
			return "", fmt.Errorf("opcode %s operands %q are unsupported", opcode, operands)
		}
		destination, mask, err := s.register(operands[0], true)
		if err != nil {
			return "", err
		}
		coordinate, _, err := s.register(operands[1], false)
		if err != nil {
			return "", err
		}
		extra := ""
		if twoSourceTexture {
			extra, _, err = s.register(operands[2], false)
			if err != nil {
				return "", err
			}
		}
		gradientX, gradientY := "", ""
		if gradientTexture {
			gradientX, _, err = s.register(operands[2], false)
			if err != nil {
				return "", err
			}
			gradientY, _, err = s.register(operands[3], false)
			if err != nil {
				return "", err
			}
		}
		textureOffset := ""
		if hasTextureOffset {
			textureOffset, _, err = s.register(operands[targetIndex+1], false)
			if err != nil {
				return "", err
			}
		}
		gatherComponent := ""
		gatherOffset := ""
		if gatherTexture {
			gatherComponent, _, err = s.register(operands[2], false)
			if err != nil {
				return "", err
			}
			if len(operands) == targetIndex+2 {
				gatherOffset, _, err = s.register(operands[targetIndex+1], false)
				if err != nil {
					return "", err
				}
			}
		}
		sampler := tgsiSamplerPattern.FindStringSubmatch(operands[samplerIndexOperand])
		if sampler == nil {
			indirect := tgsiIndirectSamplerPattern.FindStringSubmatch(operands[samplerIndexOperand])
			if indirect == nil {
				return "", fmt.Errorf("invalid texture sampler %q", operands[samplerIndexOperand])
			}
			addressIndex, _ := strconv.Atoi(indirect[1])
			if addressIndex > s.maxAddress {
				return "", fmt.Errorf("address register %d is not declared", addressIndex)
			}
			lastSampler := max(s.maxSampler, s.maxSamplerView)
			if lastSampler < 0 {
				return "", errors.New("indirect texture sampler has no declared samplers")
			}
			index := fmt.Sprintf("address[%d].%s%s", addressIndex, indirect[2], indirect[3])
			staticOpcode := opcode
			if saturate {
				staticOpcode += "_SAT"
			}
			var statement strings.Builder
			fmt.Fprintf(&statement, "{\nswitch (%s) {\n", index)
			for samplerIndex := 0; samplerIndex <= lastSampler; samplerIndex++ {
				staticOperands := append([]string(nil), operands...)
				staticOperands[samplerIndexOperand] = fmt.Sprintf("SAMP[%d]", samplerIndex)
				translated, err := s.translateInstruction(staticOpcode, staticOperands)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&statement, "case %d:\n%s\nbreak;\n", samplerIndex, translated)
			}
			statement.WriteString("default:\nbreak;\n}\n}")
			return statement.String(), nil
		}
		samplerIndex, _ := strconv.Atoi(sampler[1])
		samplerName := tgsiSamplerName(s.stage, samplerIndex)
		var expression string
		view := s.samplerViews[samplerIndex]
		buffer := target == "BUFFER" || view.target == "BUFFER"
		oneD := target == "1D" || view.target == "1D"
		oneDArray := target == "1D_ARRAY" || view.target == "1D_ARRAY"
		threeD := target == "3D" || view.target == "3D"
		multisample2D := target == "2D_MSAA" || view.target == "2D_MSAA"
		multisampleArray := target == "2D_ARRAY_MSAA" || view.target == "2D_ARRAY_MSAA"
		integerView := view.returnType == "UINT" || view.returnType == "SINT"
		rectangle := target == "RECT" || view.target == "RECT" || target == "SHADOWRECT" || view.target == "SHADOWRECT"
		shadowTarget := target
		if strings.HasPrefix(view.target, "SHADOW") {
			shadowTarget = view.target
		}
		shadow := strings.HasPrefix(shadowTarget, "SHADOW")
		shadow2D := shadowTarget == "SHADOW2D" || shadowTarget == "SHADOWRECT"
		shadow2DArray := shadowTarget == "SHADOW2D_ARRAY"
		shadowCube := shadowTarget == "SHADOWCUBE"
		shadowCubeArray := shadowTarget == "SHADOWCUBE_ARRAY"
		cube := target == "CUBE" || view.target == "CUBE"
		cubeArray := target == "CUBE_ARRAY" || view.target == "CUBE_ARRAY"
		array := target == "2D_ARRAY" || view.target == "2D_ARRAY" || target == "SHADOW2D_ARRAY" || view.target == "SHADOW2D_ARRAY"
		if textureOffset != "" {
			offsetBits := "floatBitsToInt(" + textureOffset + ")"
			offsetArgument := "ivec2((" + offsetBits + ").xy)"
			if oneD {
				offsetArgument = "int((" + offsetBits + ").x)"
			} else if threeD {
				offsetArgument = "ivec3((" + offsetBits + ").xyz)"
			}
			coordinateArgument := fmt.Sprintf("(%s).xy", coordinate)
			switch {
			case oneD:
				coordinateArgument = fmt.Sprintf("(%s).x", coordinate)
			case rectangle:
				coordinateArgument = fmt.Sprintf("(%s).xy / vec2(textureSize(%s, 0))", coordinate, samplerName)
			case shadow2DArray:
				coordinateArgument = coordinate
			case shadow:
				coordinateArgument = fmt.Sprintf("(%s).xyz", coordinate)
			case oneDArray:
				coordinateArgument = fmt.Sprintf("(%s).xy", coordinate)
			case threeD || array:
				coordinateArgument = fmt.Sprintf("(%s).xyz", coordinate)
			}
			switch opcode {
			case "TEX", "TEX2":
				expression = fmt.Sprintf("textureOffset(%s, %s, %s)", samplerName, coordinateArgument, offsetArgument)
			case "TXP":
				projectiveCoordinate := fmt.Sprintf("vec3((%s).xy, (%s).w)", coordinate, coordinate)
				if oneD {
					projectiveCoordinate = fmt.Sprintf("vec2((%s).x, (%s).w)", coordinate, coordinate)
				} else if rectangle {
					projectiveCoordinate = fmt.Sprintf("vec3((%s).xy / vec2(textureSize(%s, 0)), (%s).w)", coordinate, samplerName, coordinate)
				} else if shadow || threeD {
					projectiveCoordinate = coordinate
				}
				expression = fmt.Sprintf("textureProjOffset(%s, %s, %s)", samplerName, projectiveCoordinate, offsetArgument)
			case "TXB":
				expression = fmt.Sprintf("textureOffset(%s, %s, %s, (%s).w)", samplerName, coordinateArgument, offsetArgument, coordinate)
			case "TXB2":
				expression = fmt.Sprintf("textureOffset(%s, %s, %s, (%s).x)", samplerName, coordinateArgument, offsetArgument, extra)
			case "TXL", "TXL2":
				lod := fmt.Sprintf("(%s).w", coordinate)
				if opcode == "TXL2" {
					lod = fmt.Sprintf("(%s).x", extra)
				} else {
					crossover := tgsiSamplerLODCrossoverName(s.stage, samplerIndex)
					lod = fmt.Sprintf("(%s - (%s <= %s ? %s : 0.0))", lod, lod, crossover, crossover)
				}
				expression = fmt.Sprintf("textureLodOffset(%s, %s, %s, %s)", samplerName, coordinateArgument, lod, offsetArgument)
			case "TXD":
				gradientXArgument := fmt.Sprintf("(%s).xy", gradientX)
				gradientYArgument := fmt.Sprintf("(%s).xy", gradientY)
				if oneD {
					gradientXArgument = fmt.Sprintf("(%s).x", gradientX)
					gradientYArgument = fmt.Sprintf("(%s).x", gradientY)
				} else if threeD {
					gradientXArgument = fmt.Sprintf("(%s).xyz", gradientX)
					gradientYArgument = fmt.Sprintf("(%s).xyz", gradientY)
				} else if rectangle {
					gradientXArgument = fmt.Sprintf("(%s).xy / vec2(textureSize(%s, 0))", gradientX, samplerName)
					gradientYArgument = fmt.Sprintf("(%s).xy / vec2(textureSize(%s, 0))", gradientY, samplerName)
				}
				expression = fmt.Sprintf("textureGradOffset(%s, %s, %s, %s, %s)", samplerName, coordinateArgument, gradientXArgument, gradientYArgument, offsetArgument)
			case "TXF":
				integerCoordinate := fmt.Sprintf("floatBitsToInt(%s)", coordinate)
				fetchCoordinate := fmt.Sprintf("(%s).xy", integerCoordinate)
				if oneD {
					fetchCoordinate = fmt.Sprintf("(%s).x", integerCoordinate)
				} else if threeD || array {
					fetchCoordinate = fmt.Sprintf("(%s).xyz", integerCoordinate)
				}
				expression = fmt.Sprintf("texelFetchOffset(%s, %s, (%s).w, %s)", samplerName, fetchCoordinate, integerCoordinate, offsetArgument)
			default:
				return "", fmt.Errorf("opcode %s cannot use a texture offset", opcode)
			}
			if shadow {
				expression = "vec4(" + expression + ")"
			}
		} else {
			switch opcode {
			case "TXQ":
				lod := fmt.Sprintf("floatBitsToInt(%s).x", coordinate)
				size := fmt.Sprintf("textureSize(%s, %s)", samplerName, lod)
				if buffer || rectangle || multisample2D || multisampleArray {
					size = fmt.Sprintf("textureSize(%s)", samplerName)
				}
				levels := tgsiSamplerLevelCountName(s.stage, samplerIndex)
				switch {
				case buffer || oneD:
					expression = fmt.Sprintf("intBitsToFloat(ivec4(%s, 0, 0, %s))", size, levels)
				case oneDArray:
					expression = fmt.Sprintf("intBitsToFloat(ivec4(%s, 0, %s))", size, levels)
				case threeD || array || cubeArray || multisampleArray:
					expression = fmt.Sprintf("intBitsToFloat(ivec4(%s, %s))", size, levels)
				default:
					expression = fmt.Sprintf("intBitsToFloat(ivec4(%s, 0, %s))", size, levels)
				}
			case "TEX", "TEX2":
				if cubeArray {
					expression = fmt.Sprintf("texture(%s, %s)", samplerName, coordinate)
				} else if oneD {
					expression = fmt.Sprintf("texture(%s, (%s).x)", samplerName, coordinate)
				} else if rectangle {
					expression = fmt.Sprintf("texture(%s, (%s).xy / vec2(textureSize(%s, 0)))", samplerName, coordinate, samplerName)
				} else if shadowCubeArray {
					expression = fmt.Sprintf("vec4(texture(%s, %s, (%s).x))", samplerName, coordinate, extra)
				} else if shadow2DArray || shadowCube {
					expression = fmt.Sprintf("vec4(texture(%s, %s))", samplerName, coordinate)
				} else if shadow {
					expression = fmt.Sprintf("vec4(texture(%s, (%s).xyz))", samplerName, coordinate)
				} else if oneDArray {
					expression = fmt.Sprintf("texture(%s, (%s).xy)", samplerName, coordinate)
				} else if threeD || cube || array {
					expression = fmt.Sprintf("texture(%s, (%s).xyz)", samplerName, coordinate)
				} else {
					expression = fmt.Sprintf("texture(%s, (%s).xy)", samplerName, coordinate)
				}
			case "TXP":
				if oneD {
					expression = fmt.Sprintf("textureProj(%s, vec2((%s).x, (%s).w))", samplerName, coordinate, coordinate)
				} else if rectangle {
					expression = fmt.Sprintf("texture(%s, ((%s).xy / (%s).w) / vec2(textureSize(%s, 0)))", samplerName, coordinate, coordinate, samplerName)
				} else if shadow {
					expression = fmt.Sprintf("vec4(textureProj(%s, %s))", samplerName, coordinate)
				} else if threeD {
					expression = fmt.Sprintf("textureProj(%s, %s)", samplerName, coordinate)
				} else {
					expression = fmt.Sprintf("textureProj(%s, vec3((%s).xy, (%s).w))", samplerName, coordinate, coordinate)
				}
			case "TXB":
				if oneD {
					expression = fmt.Sprintf("texture(%s, (%s).x, (%s).w)", samplerName, coordinate, coordinate)
				} else if rectangle {
					expression = fmt.Sprintf("texture(%s, (%s).xy / vec2(textureSize(%s, 0)), (%s).w)", samplerName, coordinate, samplerName, coordinate)
				} else if shadow2DArray || shadowCube {
					expression = fmt.Sprintf("vec4(texture(%s, %s, (%s).w))", samplerName, coordinate, coordinate)
				} else if shadow {
					expression = fmt.Sprintf("vec4(texture(%s, (%s).xyz, (%s).w))", samplerName, coordinate, coordinate)
				} else if oneDArray {
					expression = fmt.Sprintf("texture(%s, (%s).xy, (%s).w)", samplerName, coordinate, coordinate)
				} else if threeD || cube || array {
					expression = fmt.Sprintf("texture(%s, (%s).xyz, (%s).w)", samplerName, coordinate, coordinate)
				} else {
					expression = fmt.Sprintf("texture(%s, (%s).xy, (%s).w)", samplerName, coordinate, coordinate)
				}
			case "TXB2":
				if shadowCubeArray {
					expression = fmt.Sprintf("vec4(texture(%s, %s, (%s).x, (%s).y))", samplerName, coordinate, extra, extra)
				} else if cubeArray {
					expression = fmt.Sprintf("texture(%s, %s, (%s).x)", samplerName, coordinate, extra)
				} else if shadow2DArray || shadowCube {
					expression = fmt.Sprintf("vec4(texture(%s, %s, (%s).x))", samplerName, coordinate, extra)
				} else if shadow2D {
					expression = fmt.Sprintf("vec4(texture(%s, (%s).xyz, (%s).x))", samplerName, coordinate, extra)
				} else if oneD {
					expression = fmt.Sprintf("texture(%s, (%s).x, (%s).x)", samplerName, coordinate, extra)
				} else if oneDArray {
					expression = fmt.Sprintf("texture(%s, (%s).xy, (%s).x)", samplerName, coordinate, extra)
				} else if threeD || cube || array {
					expression = fmt.Sprintf("texture(%s, (%s).xyz, (%s).x)", samplerName, coordinate, extra)
				} else {
					expression = fmt.Sprintf("texture(%s, (%s).xy, (%s).x)", samplerName, coordinate, extra)
				}
			case "TXL":
				lod := fmt.Sprintf("(%s).w", coordinate)
				crossover := tgsiSamplerLODCrossoverName(s.stage, samplerIndex)
				lod = fmt.Sprintf("(%s - (%s <= %s ? %s : 0.0))", lod, lod, crossover, crossover)
				if oneD {
					expression = fmt.Sprintf("textureLod(%s, (%s).x, %s)", samplerName, coordinate, lod)
				} else if rectangle {
					expression = fmt.Sprintf("textureLod(%s, (%s).xy / vec2(textureSize(%s, 0)), 0.0)", samplerName, coordinate, samplerName)
				} else if shadow2DArray || shadowCube {
					expression = fmt.Sprintf("vec4(textureLod(%s, %s, %s))", samplerName, coordinate, lod)
				} else if shadow {
					expression = fmt.Sprintf("vec4(textureLod(%s, (%s).xyz, %s))", samplerName, coordinate, lod)
				} else if oneDArray {
					expression = fmt.Sprintf("textureLod(%s, (%s).xy, %s)", samplerName, coordinate, lod)
				} else if threeD || cube || array {
					expression = fmt.Sprintf("textureLod(%s, (%s).xyz, %s)", samplerName, coordinate, lod)
				} else {
					expression = fmt.Sprintf("textureLod(%s, (%s).xy, %s)", samplerName, coordinate, lod)
				}
			case "TXL2":
				if shadowCubeArray {
					expression = fmt.Sprintf("vec4(textureLod(%s, %s, (%s).x, (%s).y))", samplerName, coordinate, extra, extra)
				} else if cubeArray {
					expression = fmt.Sprintf("textureLod(%s, %s, (%s).x)", samplerName, coordinate, extra)
				} else if shadow2DArray || shadowCube {
					expression = fmt.Sprintf("vec4(textureLod(%s, %s, (%s).x))", samplerName, coordinate, extra)
				} else if shadow2D {
					expression = fmt.Sprintf("vec4(textureLod(%s, (%s).xyz, (%s).x))", samplerName, coordinate, extra)
				} else if oneD {
					expression = fmt.Sprintf("textureLod(%s, (%s).x, (%s).x)", samplerName, coordinate, extra)
				} else if oneDArray {
					expression = fmt.Sprintf("textureLod(%s, (%s).xy, (%s).x)", samplerName, coordinate, extra)
				} else if threeD || cube || array {
					expression = fmt.Sprintf("textureLod(%s, (%s).xyz, (%s).x)", samplerName, coordinate, extra)
				} else {
					expression = fmt.Sprintf("textureLod(%s, (%s).xy, (%s).x)", samplerName, coordinate, extra)
				}
			case "TXD":
				if oneD {
					expression = fmt.Sprintf("textureGrad(%s, (%s).x, (%s).x, (%s).x)", samplerName, coordinate, gradientX, gradientY)
				} else if rectangle {
					expression = fmt.Sprintf("textureGrad(%s, (%s).xy / vec2(textureSize(%s, 0)), (%s).xy / vec2(textureSize(%s, 0)), (%s).xy / vec2(textureSize(%s, 0)))", samplerName, coordinate, samplerName, gradientX, samplerName, gradientY, samplerName)
				} else if shadow2DArray {
					expression = fmt.Sprintf("vec4(textureGrad(%s, %s, (%s).xy, (%s).xy))", samplerName, coordinate, gradientX, gradientY)
				} else if shadowCube {
					expression = fmt.Sprintf("vec4(textureGrad(%s, %s, (%s).xyz, (%s).xyz))", samplerName, coordinate, gradientX, gradientY)
				} else if shadow {
					expression = fmt.Sprintf("vec4(textureGrad(%s, (%s).xyz, (%s).xy, (%s).xy))", samplerName, coordinate, gradientX, gradientY)
				} else if oneDArray {
					expression = fmt.Sprintf("textureGrad(%s, (%s).xy, (%s).x, (%s).x)", samplerName, coordinate, gradientX, gradientY)
				} else if cube {
					s.usesCubeGradientFix = true
					expression = fmt.Sprintf("(tgsiCubePositiveZ((%[2]s).xyz) ? textureLod(%[1]s, (%[2]s).xyz, tgsiCubeGradientLOD((%[2]s).xyz, (%[3]s).xyz, (%[4]s).xyz, vec2(textureSize(%[1]s, 0)))) : textureGrad(%[1]s, (%[2]s).xyz, (%[3]s).xyz, (%[4]s).xyz))", samplerName, coordinate, gradientX, gradientY)
				} else if threeD {
					expression = fmt.Sprintf("textureGrad(%s, (%s).xyz, (%s).xyz, (%s).xyz)", samplerName, coordinate, gradientX, gradientY)
				} else if cubeArray {
					expression = fmt.Sprintf("textureGrad(%s, %s, (%s).xyz, (%s).xyz)", samplerName, coordinate, gradientX, gradientY)
				} else if array {
					expression = fmt.Sprintf("textureGrad(%s, (%s).xyz, (%s).xy, (%s).xy)", samplerName, coordinate, gradientX, gradientY)
				} else {
					expression = fmt.Sprintf("textureGrad(%s, (%s).xy, (%s).xy, (%s).xy)", samplerName, coordinate, gradientX, gradientY)
				}
			case "TG4":
				component := "floatBitsToInt(" + gatherComponent + ").x"
				coordinateOffset := ""
				if gatherOffset != "" {
					// Apple's GLSL compiler incorrectly requires textureGatherOffset's
					// offset to be constant. Shifting the normalized base coordinate by
					// an integer number of level-zero texels is the same gather footprint
					// and preserves Gallium's permitted dynamic-offset behavior.
					coordinateOffset = " + vec2(floatBitsToInt(" + gatherOffset + ").xy) / vec2(textureSize(" + samplerName + ", 0).xy)"
				}
				switch shadowTarget {
				case "SHADOW2D", "SHADOWRECT":
					coordinates := fmt.Sprintf("(%s).xy", coordinate)
					if shadowTarget == "SHADOWRECT" {
						coordinates += fmt.Sprintf(" / vec2(textureSize(%s, 0))", samplerName)
					}
					expression = fmt.Sprintf("textureGather(%s, %s%s, (%s).z)", samplerName, coordinates, coordinateOffset, coordinate)
				case "SHADOW2D_ARRAY":
					expression = fmt.Sprintf("textureGather(%s, vec3((%s).xy%s, (%s).z), (%s).w)", samplerName, coordinate, coordinateOffset, coordinate, coordinate)
				case "SHADOWCUBE":
					expression = fmt.Sprintf("textureGather(%s, (%s).xyz, (%s).w)", samplerName, coordinate, coordinate)
				case "SHADOWCUBE_ARRAY":
					expression = fmt.Sprintf("textureGather(%s, %s, (%s).x)", samplerName, coordinate, gatherComponent)
				default:
					if rectangle {
						expression = fmt.Sprintf("textureGather(%s, (%s).xy / vec2(textureSize(%s, 0))%s, %s)", samplerName, coordinate, samplerName, coordinateOffset, component)
					} else if cubeArray {
						expression = fmt.Sprintf("textureGather(%s, %s, %s)", samplerName, coordinate, component)
					} else if cube {
						expression = fmt.Sprintf("textureGather(%s, (%s).xyz, %s)", samplerName, coordinate, component)
					} else if array {
						expression = fmt.Sprintf("textureGather(%s, vec3((%s).xy%s, (%s).z), %s)", samplerName, coordinate, coordinateOffset, coordinate, component)
					} else {
						expression = fmt.Sprintf("textureGather(%s, (%s).xy%s, %s)", samplerName, coordinate, coordinateOffset, component)
					}
				}
			case "TXF":
				integerCoordinate := fmt.Sprintf("floatBitsToInt(%s)", coordinate)
				if buffer {
					expression = fmt.Sprintf("texelFetch(%s, (%s).x)", samplerName, integerCoordinate)
				} else if oneD {
					expression = fmt.Sprintf("texelFetch(%s, (%s).x, (%s).w)", samplerName, integerCoordinate, integerCoordinate)
				} else if rectangle {
					expression = fmt.Sprintf("texelFetch(%s, (%s).xy, 0)", samplerName, integerCoordinate)
				} else if multisampleArray && integerView {
					expression = fmt.Sprintf("texelFetch(%s, ivec3((%s).xy, (%s).z * %s + (%s).w), 0)", samplerName, integerCoordinate, integerCoordinate, tgsiSamplerSampleCountName(s.stage, samplerIndex), integerCoordinate)
				} else if multisample2D && integerView {
					expression = fmt.Sprintf("texelFetch(%s, ivec3((%s).xy, (%s).w), 0)", samplerName, integerCoordinate, integerCoordinate)
				} else if multisampleArray {
					expression = fmt.Sprintf("texelFetch(%s, (%s).xyz, (%s).w)", samplerName, integerCoordinate, integerCoordinate)
				} else if multisample2D {
					expression = fmt.Sprintf("texelFetch(%s, (%s).xy, (%s).w)", samplerName, integerCoordinate, integerCoordinate)
				} else if oneDArray {
					expression = fmt.Sprintf("texelFetch(%s, (%s).xy, (%s).w)", samplerName, integerCoordinate, integerCoordinate)
				} else if threeD || array {
					expression = fmt.Sprintf("texelFetch(%s, (%s).xyz, (%s).w)", samplerName, integerCoordinate, integerCoordinate)
				} else {
					expression = fmt.Sprintf("texelFetch(%s, (%s).xy, (%s).w)", samplerName, integerCoordinate, integerCoordinate)
				}
			case "LODQ":
				var queryCoordinate string
				if oneD {
					queryCoordinate = fmt.Sprintf("(%s).x", coordinate)
				} else if threeD || cube || cubeArray {
					queryCoordinate = fmt.Sprintf("(%s).xyz", coordinate)
				} else {
					queryCoordinate = fmt.Sprintf("(%s).xy", coordinate)
				}
				expression = fmt.Sprintf("vec4(textureQueryLod(%s, %s), 0.0, 0.0)", samplerName, queryCoordinate)
			}
		}
		if opcode != "TXQ" && (multisample2D || multisampleArray) {
			expression = fmt.Sprintf("tgsiSamplerViewSwizzle(%s, %s)", expression,
				tgsiSamplerViewSwizzleName(s.stage, samplerIndex))
		}
		if opcode == "TXQ" {
			// TXQ returns signed integer resource dimensions as raw TGSI register bits.
		} else if view.returnType == "UINT" {
			expression = "uintBitsToFloat(" + expression + ")"
		} else if view.returnType == "SINT" {
			expression = "intBitsToFloat(" + expression + ")"
		}
		if saturate {
			expression = "clamp(" + expression + ", 0.0, 1.0)"
		}
		if mask != "" {
			expression = "(" + expression + ")." + mask
		}
		return destination + " = " + expression + ";", nil
	}
	arities := map[string]int{
		"MOV": 2, "RSQ": 2, "RCP": 2, "FLR": 2, "FRC": 2, "CEIL": 2, "TRUNC": 2, "ROUND": 2, "EX2": 2, "LG2": 2, "SIN": 2, "COS": 2, "DDX": 2, "DDY": 2, "SSG": 2, "ISSG": 2, "IABS": 2, "NOT": 2, "INEG": 2,
		"F2I": 2, "F2U": 2, "I2F": 2, "U2F": 2,
		"ADD": 3, "MUL": 3, "DIV": 3, "DP2": 3, "DP3": 3, "DP4": 3, "MAX": 3, "MIN": 3,
		"POW": 3, "FSLT": 3, "FSGE": 3, "SLT": 3, "SGE": 3, "FSEQ": 3, "FSNE": 3, "ISGE": 3, "ISLT": 3, "USEQ": 3, "USNE": 3, "USGE": 3, "USLT": 3, "UMAX": 3, "UMIN": 3,
		"AND": 3, "OR": 3, "XOR": 3, "UADD": 3, "UMUL": 3, "UDIV": 3, "UMOD": 3, "IDIV": 3, "IMIN": 3, "IMAX": 3, "SHL": 3, "USHR": 3, "ISHR": 3,
		"IBFE": 4, "UBFE": 4,
		"MAD": 4, "LRP": 4, "UCMP": 4,
		"BFI": 5,
	}
	arity, ok := arities[opcode]
	if !ok {
		return "", fmt.Errorf("opcode %s is unsupported", opcode)
	}
	if len(operands) != arity {
		return "", fmt.Errorf("opcode %s has %d operands, want %d", opcode, len(operands), arity)
	}
	destination, mask, err := s.register(operands[0], true)
	if err != nil {
		return "", err
	}
	sources := make([]string, 0, len(operands)-1)
	for _, operand := range operands[1:] {
		source, _, err := s.register(operand, false)
		if err != nil {
			return "", err
		}
		sources = append(sources, source)
	}
	var expression string
	switch opcode {
	case "MOV":
		expression = sources[0]
	case "RSQ":
		expression = "vec4(inversesqrt((" + sources[0] + ").x))"
	case "RCP":
		expression = "vec4(1.0 / (" + sources[0] + ").x)"
	case "FLR":
		expression = "floor(" + sources[0] + ")"
	case "FRC":
		expression = "fract(" + sources[0] + ")"
	case "CEIL":
		expression = "ceil(" + sources[0] + ")"
	case "TRUNC":
		expression = "trunc(" + sources[0] + ")"
	case "ROUND":
		expression = "roundEven(" + sources[0] + ")"
	case "EX2":
		expression = "vec4(exp2((" + sources[0] + ").x))"
	case "LG2":
		expression = "vec4(log2((" + sources[0] + ").x))"
	case "SIN":
		expression = "vec4(sin((" + sources[0] + ").x))"
	case "COS":
		expression = "vec4(cos((" + sources[0] + ").x))"
	case "DDX":
		expression = "dFdx(" + sources[0] + ")"
	case "DDY":
		expression = "dFdy(" + sources[0] + ")"
	case "SSG":
		expression = "sign(" + sources[0] + ")"
	case "ISSG":
		expression = "intBitsToFloat(sign(floatBitsToInt(" + sources[0] + ")))"
	case "IABS":
		expression = "intBitsToFloat(abs(floatBitsToInt(" + sources[0] + ")))"
	case "NOT":
		expression = "uintBitsToFloat(~floatBitsToUint(" + sources[0] + "))"
	case "INEG":
		expression = "intBitsToFloat(-floatBitsToInt(" + sources[0] + "))"
	case "F2I":
		expression = "intBitsToFloat(ivec4(" + sources[0] + "))"
	case "F2U":
		expression = "uintBitsToFloat(uvec4(" + sources[0] + "))"
	case "I2F":
		expression = "vec4(floatBitsToInt(" + sources[0] + "))"
	case "U2F":
		expression = "vec4(floatBitsToUint(" + sources[0] + "))"
	case "ADD":
		expression = "(" + sources[0] + " + " + sources[1] + ")"
	case "MUL":
		expression = "(" + sources[0] + " * " + sources[1] + ")"
	case "DIV":
		expression = "(" + sources[0] + " / " + sources[1] + ")"
	case "MAX":
		expression = "max(" + sources[0] + ", " + sources[1] + ")"
	case "MIN":
		expression = "min(" + sources[0] + ", " + sources[1] + ")"
	case "DP3":
		expression = "vec4(dot((" + sources[0] + ").xyz, (" + sources[1] + ").xyz))"
	case "DP2":
		expression = "vec4(dot((" + sources[0] + ").xy, (" + sources[1] + ").xy))"
	case "DP4":
		expression = "vec4(dot(" + sources[0] + ", " + sources[1] + "))"
	case "POW":
		expression = "vec4(pow((" + sources[0] + ").x, (" + sources[1] + ").x))"
	case "FSLT":
		expression = "intBitsToFloat(-ivec4(lessThan(" + sources[0] + ", " + sources[1] + ")))"
	case "FSGE":
		expression = "intBitsToFloat(-ivec4(greaterThanEqual(" + sources[0] + ", " + sources[1] + ")))"
	case "SLT":
		expression = "vec4(lessThan(" + sources[0] + ", " + sources[1] + "))"
	case "SGE":
		expression = "vec4(greaterThanEqual(" + sources[0] + ", " + sources[1] + "))"
	case "FSEQ":
		expression = "intBitsToFloat(-ivec4(equal(" + sources[0] + ", " + sources[1] + ")))"
	case "FSNE":
		if sources[0] == sources[1] {
			// Mesa lowers isnan(value) to FSNE value, value. Apple's GLSL
			// optimizer folds the translated self-comparison to false despite
			// IEEE NaN semantics, while the dedicated builtin remains correct.
			expression = "intBitsToFloat(-ivec4(isnan(" + sources[0] + ")))"
		} else {
			expression = "intBitsToFloat(-ivec4(notEqual(" + sources[0] + ", " + sources[1] + ")))"
		}
	case "ISGE":
		expression = "intBitsToFloat(-ivec4(greaterThanEqual(" +
			"floatBitsToInt(" + sources[0] + "), floatBitsToInt(" + sources[1] + "))))"
	case "ISLT":
		expression = "intBitsToFloat(-ivec4(lessThan(" +
			"floatBitsToInt(" + sources[0] + "), floatBitsToInt(" + sources[1] + "))))"
	case "USEQ":
		expression = "intBitsToFloat(-ivec4(equal(" +
			"floatBitsToUint(" + sources[0] + "), floatBitsToUint(" + sources[1] + "))))"
	case "USNE":
		expression = "intBitsToFloat(-ivec4(notEqual(" +
			"floatBitsToUint(" + sources[0] + "), floatBitsToUint(" + sources[1] + "))))"
	case "USGE":
		expression = "intBitsToFloat(-ivec4(greaterThanEqual(" +
			"floatBitsToUint(" + sources[0] + "), floatBitsToUint(" + sources[1] + "))))"
	case "USLT":
		expression = "intBitsToFloat(-ivec4(lessThan(" +
			"floatBitsToUint(" + sources[0] + "), floatBitsToUint(" + sources[1] + "))))"
	case "UMAX":
		expression = "uintBitsToFloat(max(floatBitsToUint(" + sources[0] + "), floatBitsToUint(" + sources[1] + ")))"
	case "UMIN":
		expression = "uintBitsToFloat(min(floatBitsToUint(" + sources[0] + "), floatBitsToUint(" + sources[1] + ")))"
	case "AND":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") & floatBitsToUint(" + sources[1] + "))"
	case "OR":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") | floatBitsToUint(" + sources[1] + "))"
	case "XOR":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") ^ floatBitsToUint(" + sources[1] + "))"
	case "UADD":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") + floatBitsToUint(" + sources[1] + "))"
	case "UMUL":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") * floatBitsToUint(" + sources[1] + "))"
	case "UDIV":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") / floatBitsToUint(" + sources[1] + "))"
	case "UMOD":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") % floatBitsToUint(" + sources[1] + "))"
	case "IDIV":
		expression = "intBitsToFloat(floatBitsToInt(" + sources[0] + ") / floatBitsToInt(" + sources[1] + "))"
	case "IMIN":
		expression = "intBitsToFloat(min(floatBitsToInt(" + sources[0] + "), floatBitsToInt(" + sources[1] + ")))"
	case "IMAX":
		expression = "intBitsToFloat(max(floatBitsToInt(" + sources[0] + "), floatBitsToInt(" + sources[1] + ")))"
	case "SHL":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") << floatBitsToUint(" + sources[1] + "))"
	case "USHR":
		expression = "uintBitsToFloat(floatBitsToUint(" + sources[0] + ") >> floatBitsToUint(" + sources[1] + "))"
	case "ISHR":
		expression = "intBitsToFloat(floatBitsToInt(" + sources[0] + ") >> floatBitsToInt(" + sources[1] + "))"
	case "IBFE":
		expression = "intBitsToFloat(bitfieldExtract(floatBitsToInt(" + sources[0] + "), " +
			"(floatBitsToInt(" + sources[1] + ")).x, (floatBitsToInt(" + sources[2] + ")).x))"
	case "UBFE":
		expression = "uintBitsToFloat(bitfieldExtract(floatBitsToUint(" + sources[0] + "), " +
			"(floatBitsToInt(" + sources[1] + ")).x, (floatBitsToInt(" + sources[2] + ")).x))"
	case "MAD":
		expression = "((" + sources[0] + " * " + sources[1] + ") + " + sources[2] + ")"
	case "LRP":
		expression = "mix(" + sources[2] + ", " + sources[1] + ", " + sources[0] + ")"
	case "UCMP":
		expression = "mix(" + sources[2] + ", " + sources[1] +
			", notEqual(floatBitsToUint(" + sources[0] + "), uvec4(0)))"
	case "BFI":
		expression = "uintBitsToFloat(bitfieldInsert(floatBitsToUint(" + sources[0] + "), " +
			"floatBitsToUint(" + sources[1] + "), (floatBitsToInt(" + sources[2] + ")).x, " +
			"(floatBitsToInt(" + sources[3] + ")).x))"
	}
	if saturate {
		expression = "clamp(" + expression + ", 0.0, 1.0)"
	}
	if mask != "" {
		expression = "(" + expression + ")." + mask
	}
	return destination + " = " + expression + ";", nil
}

// VirGL transports double values in pairs of ordinary 32-bit TGSI register
// components. Mesa scalarizes double operations before emitting TGSI: an XY or
// ZW destination is one IEEE-754 double, and source swizzles select the matching
// pair. Keep the register file as vec4 so ordinary MOV instructions can copy
// those raw words, and only unpack while evaluating a double opcode.
func (s *tgsiShader) doubleSource(raw string) (string, error) {
	negative := strings.HasPrefix(raw, "-")
	if negative {
		raw = strings.TrimPrefix(raw, "-")
	}
	source, _, err := s.register(raw, false)
	if err != nil {
		return "", err
	}
	expression := "packDouble2x32(floatBitsToUint((" + source + ").xy))"
	if negative {
		expression = "(-" + expression + ")"
	}
	return expression, nil
}

func (s *tgsiShader) scalarSource(raw, kind string) (string, error) {
	negative := strings.HasPrefix(raw, "-")
	if negative {
		raw = strings.TrimPrefix(raw, "-")
	}
	source, _, err := s.register(raw, false)
	if err != nil {
		return "", err
	}
	var expression string
	switch kind {
	case "float":
		expression = "(" + source + ").x"
	case "int":
		expression = "floatBitsToInt(" + source + ").x"
	case "uint":
		expression = "floatBitsToUint(" + source + ").x"
	default:
		return "", fmt.Errorf("invalid scalar source kind %q", kind)
	}
	if negative {
		expression = "(-" + expression + ")"
	}
	return expression, nil
}

func (s *tgsiShader) doubleAssignment(raw, expression string, saturate bool) (string, error) {
	destination, mask, err := s.register(raw, true)
	if err != nil {
		return "", err
	}
	if mask != "xy" && mask != "zw" {
		return "", fmt.Errorf("double destination %q must select one complete XY or ZW word pair", raw)
	}
	if saturate {
		expression = "clamp(" + expression + ", 0.0LF, 1.0LF)"
	}
	return destination + " = uintBitsToFloat(unpackDouble2x32(" + expression + "));", nil
}

func (s *tgsiShader) scalarAssignment(raw, expression string, saturate bool) (string, error) {
	destination, mask, err := s.register(raw, true)
	if err != nil {
		return "", err
	}
	if saturate {
		expression = "clamp(" + expression + ", 0.0, 1.0)"
	}
	value := "vec4(" + expression + ")"
	if mask != "" {
		value = "(" + value + ")." + mask
	}
	return destination + " = " + value + ";", nil
}

func (s *tgsiShader) translateDoubleInstruction(opcode string, operands []string, saturate bool) (string, bool, error) {
	arities := map[string]int{
		"DABS": 2, "DNEG": 2, "DRCP": 2, "DSQRT": 2, "DFRAC": 2,
		"DRSQ": 2, "DTRUNC": 2, "DCEIL": 2, "DFLR": 2, "DROUND": 2,
		"DSSG": 2, "D2F": 2, "D2I": 2, "D2U": 2,
		"F2D": 2, "I2D": 2, "U2D": 2,
		"DADD": 3, "DMUL": 3, "DMAX": 3, "DMIN": 3, "DDIV": 3,
		"DSEQ": 3, "DSLT": 3, "DSNE": 3, "DSGE": 3, "DLDEXP": 3,
		"DMAD": 4, "DFMA": 4, "DFRACEXP": 3,
	}
	arity, handled := arities[opcode]
	if !handled {
		return "", false, nil
	}
	if len(operands) != arity {
		return "", true, fmt.Errorf("opcode %s has %d operands, want %d", opcode, len(operands), arity)
	}
	if s.stage == tgsiFragment && s.maxTemporary >= 128 && (opcode == "DSEQ" || opcode == "DSNE") &&
		!strings.HasPrefix(operands[1], "-") && !strings.HasPrefix(operands[2], "-") {
		left, _, leftErr := s.register(operands[1], false)
		if leftErr != nil {
			return "", true, leftErr
		}
		right, _, rightErr := s.register(operands[2], false)
		if rightErr != nil {
			return "", true, rightErr
		}
		s.usesDoubleEquality = true
		equal := "tgsiDoubleEqual(floatBitsToUint((" + left + ").xy), floatBitsToUint((" + right + ").xy))"
		if opcode == "DSNE" {
			equal = "!(" + equal + ")"
		}
		statement, err := s.scalarAssignment(operands[0], "intBitsToFloat(("+equal+") ? -1 : 0)", saturate)
		return statement, true, err
	}

	if opcode == "F2D" || opcode == "I2D" || opcode == "U2D" {
		kind := map[string]string{"F2D": "float", "I2D": "int", "U2D": "uint"}[opcode]
		source, err := s.scalarSource(operands[1], kind)
		if err != nil {
			return "", true, err
		}
		statement, err := s.doubleAssignment(operands[0], "double("+source+")", saturate)
		return statement, true, err
	}

	sourceIndex := 1
	if opcode == "DFRACEXP" {
		sourceIndex = 2
	}
	first, err := s.doubleSource(operands[sourceIndex])
	if err != nil {
		return "", true, err
	}
	if opcode == "D2F" {
		statement, err := s.scalarAssignment(operands[0], "float("+first+")", saturate)
		return statement, true, err
	}
	if opcode == "D2I" || opcode == "D2U" {
		conversion := "int"
		bitcast := "intBitsToFloat"
		if opcode == "D2U" {
			conversion = "uint"
			bitcast = "uintBitsToFloat"
		}
		statement, err := s.scalarAssignment(operands[0], bitcast+"("+conversion+"("+first+"))", saturate)
		return statement, true, err
	}
	if opcode == "DFRACEXP" {
		fractionDestination, fractionMask, err := s.register(operands[0], true)
		if err != nil {
			return "", true, err
		}
		if fractionMask != "xy" && fractionMask != "zw" {
			return "", true, fmt.Errorf("double destination %q must select one complete XY or ZW word pair", operands[0])
		}
		exponentDestination, exponentMask, err := s.register(operands[1], true)
		if err != nil {
			return "", true, err
		}
		exponentValue := "vec4(intBitsToFloat(tgsiDoubleExponent))"
		if exponentMask != "" {
			exponentValue = "(" + exponentValue + ")." + exponentMask
		}
		statement := "{ int tgsiDoubleExponent; double tgsiDoubleFraction = frexp(" + first + ", tgsiDoubleExponent); " +
			fractionDestination + " = uintBitsToFloat(unpackDouble2x32(tgsiDoubleFraction)); " +
			exponentDestination + " = " + exponentValue + "; }"
		return statement, true, nil
	}

	var expression string
	switch opcode {
	case "DABS":
		expression = "abs(" + first + ")"
	case "DNEG":
		expression = "(-" + first + ")"
	case "DRCP":
		expression = "(1.0LF / " + first + ")"
	case "DSQRT":
		expression = "sqrt(" + first + ")"
	case "DFRAC":
		expression = "fract(" + first + ")"
	case "DRSQ":
		expression = "inversesqrt(" + first + ")"
	case "DTRUNC":
		expression = "trunc(" + first + ")"
	case "DCEIL":
		expression = "ceil(" + first + ")"
	case "DFLR":
		expression = "floor(" + first + ")"
	case "DROUND":
		s.usesDoubleRoundEven = true
		expression = "tgsiRoundEven(" + first + ")"
	case "DSSG":
		expression = "sign(" + first + ")"
	case "DLDEXP":
		exponent, err := s.scalarSource(operands[2], "int")
		if err != nil {
			return "", true, err
		}
		expression = "ldexp(" + first + ", " + exponent + ")"
	default:
		sources := make([]string, 0, len(operands)-1)
		for _, operand := range operands[1:] {
			source, err := s.doubleSource(operand)
			if err != nil {
				return "", true, err
			}
			sources = append(sources, source)
		}
		switch opcode {
		case "DADD":
			expression = "(" + sources[0] + " + " + sources[1] + ")"
		case "DMUL":
			expression = "(" + sources[0] + " * " + sources[1] + ")"
		case "DMAX":
			expression = "max(" + sources[0] + ", " + sources[1] + ")"
		case "DMIN":
			expression = "min(" + sources[0] + ", " + sources[1] + ")"
		case "DDIV":
			expression = "(" + sources[0] + " / " + sources[1] + ")"
		case "DMAD":
			expression = "(" + sources[0] + " * " + sources[1] + " + " + sources[2] + ")"
		case "DFMA":
			expression = "fma(" + sources[0] + ", " + sources[1] + ", " + sources[2] + ")"
		case "DSEQ", "DSLT", "DSNE", "DSGE":
			comparison := map[string]string{"DSEQ": "==", "DSLT": "<", "DSNE": "!=", "DSGE": ">="}[opcode]
			statement, err := s.scalarAssignment(operands[0], "intBitsToFloat(("+sources[0]+" "+comparison+" "+sources[1]+") ? -1 : 0)", saturate)
			return statement, true, err
		}
	}
	statement, err := s.doubleAssignment(operands[0], expression, saturate)
	return statement, true, err
}

func (s *tgsiShader) register(raw string, destination bool) (string, string, error) {
	negative := strings.HasPrefix(raw, "-")
	if negative {
		if destination {
			return "", "", fmt.Errorf("destination %q is negative", raw)
		}
		raw = strings.TrimPrefix(raw, "-")
	}
	if match := tgsiIndirectDimensionalInput.FindStringSubmatch(raw); match != nil {
		if destination || (s.stage != tgsiGeometry && s.stage != tgsiTessControl && s.stage != tgsiTessEvaluation) {
			return "", "", fmt.Errorf("indirect dimensional input register %q is invalid in stage %d", raw, s.stage)
		}
		addressIndex, _ := strconv.Atoi(match[1])
		if addressIndex > s.maxAddress {
			return "", "", fmt.Errorf("address register %d is not declared", addressIndex)
		}
		index, _ := strconv.Atoi(match[3])
		declaration, ok := s.inputs[index]
		if !ok {
			return "", "", fmt.Errorf("dimensional input register %d is not declared", index)
		}
		vertex := fmt.Sprintf("address[%d].%s", addressIndex, match[2])
		name := s.dimensionalInputName(declaration, index, vertex)
		if swizzle := match[4]; swizzle != "" {
			if len(swizzle) < 4 {
				swizzle += strings.Repeat(swizzle[len(swizzle)-1:], 4-len(swizzle))
			}
			name += "." + swizzle
		}
		if negative {
			name = "(-" + name + ")"
		}
		return name, "", nil
	}
	if match := tgsiGeometryInputPattern.FindStringSubmatch(raw); match != nil {
		if destination || (s.stage != tgsiGeometry && s.stage != tgsiTessControl && s.stage != tgsiTessEvaluation) {
			return "", "", fmt.Errorf("geometry input register %q is invalid in stage %d", raw, s.stage)
		}
		vertex, _ := strconv.Atoi(match[1])
		index, _ := strconv.Atoi(match[2])
		declaration, ok := s.inputs[index]
		if !ok {
			return "", "", fmt.Errorf("geometry input register %d is not declared", index)
		}
		name := s.dimensionalInputName(declaration, index, strconv.Itoa(vertex))
		if swizzle := match[3]; swizzle != "" {
			if len(swizzle) < 4 {
				swizzle += strings.Repeat(swizzle[len(swizzle)-1:], 4-len(swizzle))
			}
			name += "." + swizzle
		}
		if negative {
			name = "(-" + name + ")"
		}
		return name, "", nil
	}
	if match := tgsiTessInputSystemPattern.FindStringSubmatch(raw); match != nil {
		if destination || (s.stage != tgsiTessControl && s.stage != tgsiTessEvaluation) {
			return "", "", fmt.Errorf("tessellation input register %q is invalid in stage %d", raw, s.stage)
		}
		systemIndex, _ := strconv.Atoi(match[1])
		declaration, ok := s.systemValues[systemIndex]
		if !ok || declaration.semantic != "INVOCATIONID" {
			return "", "", fmt.Errorf("tessellation input index uses unsupported system value %d", systemIndex)
		}
		index, _ := strconv.Atoi(match[3])
		input, ok := s.inputs[index]
		if !ok {
			return "", "", fmt.Errorf("tessellation input register %d is not declared", index)
		}
		name := s.dimensionalInputName(input, index, "gl_InvocationID")
		if swizzle := match[4]; swizzle != "" {
			if len(swizzle) < 4 {
				swizzle += strings.Repeat(swizzle[len(swizzle)-1:], 4-len(swizzle))
			}
			name += "." + swizzle
		}
		if negative {
			name = "(-" + name + ")"
		}
		return name, "", nil
	}
	if match := tgsiTessOutputPattern.FindStringSubmatch(raw); match != nil {
		if s.stage != tgsiTessControl {
			return "", "", fmt.Errorf("tessellation output register %q is invalid in stage %d", raw, s.stage)
		}
		vertex := match[1]
		if vertex == "" {
			systemIndex, _ := strconv.Atoi(match[2])
			declaration, ok := s.systemValues[systemIndex]
			if !ok || declaration.semantic != "INVOCATIONID" {
				return "", "", fmt.Errorf("tessellation output index uses unsupported system value %d", systemIndex)
			}
			vertex = "gl_InvocationID"
		}
		index, _ := strconv.Atoi(match[4])
		declaration, ok := s.outputs[index]
		if !ok {
			return "", "", fmt.Errorf("tessellation output register %d is not declared", index)
		}
		name := s.tessControlOutputNameAt(declaration, index, vertex)
		mask := match[5]
		if destination && mask != "" {
			name += "." + mask
		} else if !destination && mask != "" {
			if len(mask) < 4 {
				mask += strings.Repeat(mask[len(mask)-1:], 4-len(mask))
			}
			name += "." + mask
			mask = ""
		}
		if negative {
			name = "(-" + name + ")"
		}
		return name, mask, nil
	}
	if match := tgsiIndirectTessOutputPattern.FindStringSubmatch(raw); match != nil {
		if s.stage != tgsiTessControl {
			return "", "", fmt.Errorf("indirect tessellation output register %q is invalid in stage %d", raw, s.stage)
		}
		addressIndex, _ := strconv.Atoi(match[1])
		if addressIndex > s.maxAddress {
			return "", "", fmt.Errorf("address register %d is not declared", addressIndex)
		}
		index, _ := strconv.Atoi(match[3])
		declaration, ok := s.outputs[index]
		if !ok {
			return "", "", fmt.Errorf("tessellation output register %d is not declared", index)
		}
		vertex := fmt.Sprintf("address[%d].%s", addressIndex, match[2])
		name := s.tessControlOutputNameAt(declaration, index, vertex)
		mask := match[4]
		if destination && mask != "" {
			name += "." + mask
		} else if !destination && mask != "" {
			if len(mask) < 4 {
				mask += strings.Repeat(mask[len(mask)-1:], 4-len(mask))
			}
			name += "." + mask
			mask = ""
		}
		if negative {
			name = "(-" + name + ")"
		}
		return name, mask, nil
	}
	if match := tgsiConstantIndirectPattern.FindStringSubmatch(raw); match != nil {
		if destination {
			return "", "", fmt.Errorf("constant destination %q is invalid", raw)
		}
		buffer, _ := strconv.Atoi(match[1])
		addressIndex, _ := strconv.Atoi(match[2])
		if buffer >= len(s.maxConstants) || s.maxConstants[buffer] < 0 {
			return "", "", fmt.Errorf("constant buffer %d is not declared", buffer)
		}
		if addressIndex > s.maxAddress {
			return "", "", fmt.Errorf("address register %d is not declared", addressIndex)
		}
		index := fmt.Sprintf("address[%d].%s", addressIndex, match[3])
		if match[4] != "" {
			index += match[4]
		}
		name := fmt.Sprintf("%s[%s]", s.constantName(buffer), index)
		if s.usesNativeConstantBlock(buffer) {
			name = "uintBitsToFloat(" + name + ")"
		}
		if swizzle := match[5]; swizzle != "" {
			if len(swizzle) < 4 {
				swizzle += strings.Repeat(swizzle[len(swizzle)-1:], 4-len(swizzle))
			}
			name += "." + swizzle
		}
		if negative {
			name = "(-" + name + ")"
		}
		return name, "", nil
	}
	if match := tgsiIndirectPattern.FindStringSubmatch(raw); match != nil {
		if destination && match[1] != "TEMP" {
			return "", "", fmt.Errorf("indirect destination %q is not a temporary", raw)
		}
		addressIndex, _ := strconv.Atoi(match[2])
		if addressIndex > s.maxAddress {
			return "", "", fmt.Errorf("address register %d is not declared", addressIndex)
		}
		offset := match[4]
		index := fmt.Sprintf("address[%d].%s", addressIndex, match[3])
		if offset != "" {
			index += offset
		}
		name := ""
		if match[1] == "CONST" {
			name = fmt.Sprintf("%s[%s]", s.constantName(0), index)
			if s.usesNativeConstantBlock(0) {
				name = "uintBitsToFloat(" + name + ")"
			}
		} else {
			arrayID, _ := strconv.Atoi(match[5])
			array, isArray := s.tempArrays[arrayID]
			if !destination && isArray {
				for temporary := array.last; temporary >= array.first; temporary-- {
					if name == "" {
						name = fmt.Sprintf("temporary[%d]", temporary)
						continue
					}
					name = fmt.Sprintf("(%s == %d ? temporary[%d] : %s)", index, temporary, temporary, name)
				}
			} else {
				name = fmt.Sprintf("temporary[%s]", index)
			}
		}
		if swizzle := match[6]; swizzle != "" {
			if destination {
				return name + "." + swizzle, swizzle, nil
			}
			if len(swizzle) < 4 {
				swizzle += strings.Repeat(swizzle[len(swizzle)-1:], 4-len(swizzle))
			}
			name += "." + swizzle
		}
		if negative {
			name = "(-" + name + ")"
		}
		return name, "", nil
	}
	if match := tgsiConstantRegisterPattern.FindStringSubmatch(raw); match != nil {
		if destination {
			return "", "", fmt.Errorf("constant destination %q is invalid", raw)
		}
		buffer, _ := strconv.Atoi(match[1])
		index, _ := strconv.Atoi(match[2])
		if buffer >= len(s.maxConstants) || index > s.maxConstants[buffer] {
			return "", "", fmt.Errorf("constant buffer %d register %d is not declared", buffer, index)
		}
		name := fmt.Sprintf("%s[%d]", s.constantName(buffer), index)
		if s.usesNativeConstantBlock(buffer) {
			name = "uintBitsToFloat(" + name + ")"
		}
		if swizzle := match[3]; swizzle != "" {
			if len(swizzle) < 4 {
				swizzle += strings.Repeat(swizzle[len(swizzle)-1:], 4-len(swizzle))
			}
			name += "." + swizzle
		}
		if negative {
			name = "(-" + name + ")"
		}
		return name, "", nil
	}
	match := tgsiRegisterPattern.FindStringSubmatch(raw)
	if match == nil {
		return "", "", fmt.Errorf("invalid register %q", raw)
	}
	index, _ := strconv.Atoi(match[2])
	mask := match[3]
	var name string
	switch match[1] {
	case "IN":
		name = s.inputName(index)
	case "OUT":
		name = s.outputName(index)
	case "SV":
		declaration, ok := s.systemValues[index]
		if !ok {
			return "", "", fmt.Errorf("system value %d is not declared", index)
		}
		switch declaration.semantic {
		case "INSTANCEID":
			name = "intBitsToFloat(ivec4(gl_InstanceID))"
		case "VERTEXID":
			name = "intBitsToFloat(ivec4(gl_VertexID))"
		case "PRIMID":
			if s.stage != tgsiGeometry && s.stage != tgsiTessControl && s.stage != tgsiTessEvaluation {
				return "", "", fmt.Errorf("system value PRIMID is unsupported in stage %d", s.stage)
			}
			if s.stage == tgsiGeometry {
				name = "intBitsToFloat(ivec4(gl_PrimitiveIDIn))"
			} else {
				name = "intBitsToFloat(ivec4(gl_PrimitiveID))"
			}
		case "INVOCATIONID":
			if s.stage != tgsiGeometry && s.stage != tgsiTessControl {
				return "", "", fmt.Errorf("system value INVOCATIONID is unsupported in stage %d", s.stage)
			}
			name = "intBitsToFloat(ivec4(gl_InvocationID))"
		case "TESSCOORD":
			if s.stage != tgsiTessEvaluation {
				return "", "", fmt.Errorf("system value TESSCOORD is unsupported in stage %d", s.stage)
			}
			name = "vec4(gl_TessCoord, 0.0)"
		case "SAMPLEID":
			if s.stage != tgsiFragment {
				return "", "", fmt.Errorf("system value SAMPLEID is unsupported in stage %d", s.stage)
			}
			name = "intBitsToFloat(ivec4(gl_SampleID))"
		case "SAMPLEPOS":
			if s.stage != tgsiFragment {
				return "", "", fmt.Errorf("system value SAMPLEPOS is unsupported in stage %d", s.stage)
			}
			name = "vec4(gl_SamplePosition, 0.0, 0.0)"
		case "SAMPLEMASK":
			if s.stage != tgsiFragment {
				return "", "", fmt.Errorf("system value SAMPLEMASK is unsupported in stage %d", s.stage)
			}
			name = "intBitsToFloat(ivec4(gl_SampleMaskIn[0]))"
		default:
			return "", "", fmt.Errorf("system value %s is unsupported", declaration.semantic)
		}
	case "CONST":
		name = fmt.Sprintf("%s[%d]", s.constantName(0), index)
	case "TEMP":
		name = fmt.Sprintf("temporary[%d]", index)
	case "IMM":
		name = fmt.Sprintf("immediate%d", index)
	}
	if !destination && mask != "" {
		if len(mask) < 4 {
			mask += strings.Repeat(mask[len(mask)-1:], 4-len(mask))
		}
		name += "." + mask
		mask = ""
	} else if destination && mask != "" {
		name += "." + mask
	}
	if negative {
		name = "(-" + name + ")"
	}
	return name, mask, nil
}

func (s *tgsiShader) constantName(buffer int) string {
	return tgsiConstantName(s.stage, buffer)
}

func tgsiConstantName(stage uint32, buffer int) string {
	switch stage {
	case tgsiFragment:
		return fmt.Sprintf("uFragmentConstants%d", buffer)
	case tgsiGeometry:
		return fmt.Sprintf("uGeometryConstants%d", buffer)
	case tgsiTessControl:
		return fmt.Sprintf("uTessControlConstants%d", buffer)
	case tgsiTessEvaluation:
		return fmt.Sprintf("uTessEvaluationConstants%d", buffer)
	}
	return fmt.Sprintf("uVertexConstants%d", buffer)
}

func tgsiConstantBlockName(stage uint32, buffer int) string {
	return tgsiConstantName(stage, buffer) + "Block"
}

func tgsiConstantTextureName(stage uint32, buffer int) string {
	return tgsiConstantName(stage, buffer) + "Texture"
}

const tgsiNativeConstantBlockRegisters = 1024

func (s *tgsiShader) usesNativeConstantBlock(buffer int) bool {
	return buffer >= 0 && buffer < len(s.maxConstants) &&
		s.maxConstants[buffer]+1 >= tgsiNativeConstantBlockRegisters
}

func (s *tgsiShader) addressRegister(raw string) (string, error) {
	match := tgsiAddressPattern.FindStringSubmatch(raw)
	if match == nil {
		return "", fmt.Errorf("invalid address register %q", raw)
	}
	index, _ := strconv.Atoi(match[1])
	if index > s.maxAddress {
		return "", fmt.Errorf("address register %d is not declared", index)
	}
	name := fmt.Sprintf("address[%d]", index)
	if mask := match[2]; mask != "" {
		name += "." + mask
	}
	return name, nil
}

func (s *tgsiShader) inputName(index int) string {
	declaration := s.inputs[index]
	if s.stage == tgsiFragment {
		if clipIndex, ok := clipDistanceSemanticIndex(declaration.semantic); ok {
			return fmt.Sprintf("clipDistance%d", clipIndex)
		}
		if declaration.semantic == "POSITION" {
			return "gl_FragCoord"
		}
		if declaration.semantic == "FACE" {
			return "vec4(gl_FrontFacing ? 1.0 : -1.0)"
		}
		if declaration.semantic == "PCOORD" {
			return "vec4(gl_PointCoord, 0.0, 1.0)"
		}
		if declaration.semantic == "VIEWPORT_INDEX" {
			return "intBitsToFloat(ivec4(gl_ViewportIndex))"
		}
		if declaration.semantic == "LAYER" {
			return "intBitsToFloat(ivec4(gl_Layer))"
		}
		if declaration.semantic == "" || isInterpolationQualifier(declaration.semantic) {
			return fmt.Sprintf("varying%d", declarationRank(s.inputs, index, false))
		}
		return semanticName(declaration.semantic, index)
	}
	if s.stage == tgsiGeometry {
		return semanticName(declaration.semantic, index) + "GeometryInput"
	}
	if s.stage == tgsiTessControl || s.stage == tgsiTessEvaluation {
		switch declaration.semantic {
		case "TESSOUTER":
			return "vec4(gl_TessLevelOuter[0], gl_TessLevelOuter[1], gl_TessLevelOuter[2], gl_TessLevelOuter[3])"
		case "TESSINNER":
			return "vec4(gl_TessLevelInner[0], gl_TessLevelInner[1], 0.0, 0.0)"
		}
		suffix := "TessControlInput"
		if s.stage == tgsiTessEvaluation {
			suffix = "TessEvaluationInput"
		}
		return semanticName(declaration.semantic, index) + suffix
	}
	return fmt.Sprintf("attribute%d", index)
}

func (s *tgsiShader) outputName(index int) string {
	declaration := s.outputs[index]
	if s.stage == tgsiVertex && declaration.semantic == "POSITION" {
		return "gl_Position"
	}
	if s.stage == tgsiGeometry && declaration.semantic == "POSITION" {
		return "geometryPosition"
	}
	if s.stage == tgsiTessEvaluation && declaration.semantic == "POSITION" {
		return "gl_Position"
	}
	if s.stage == tgsiTessControl {
		switch declaration.semantic {
		case "TESSOUTER":
			return "tessLevelOuter"
		case "TESSINNER":
			return "tessLevelInner"
		case "PATCH":
			return semanticName(declaration.semantic, index)
		}
	}
	if (s.stage == tgsiGeometry || s.stage == tgsiTessEvaluation) && declaration.semantic == "VIEWPORT_INDEX" {
		return "viewportIndex"
	}
	if (s.stage == tgsiGeometry || s.stage == tgsiTessEvaluation) && declaration.semantic == "LAYER" {
		return "layerIndex"
	}
	if s.stage == tgsiVertex {
		if clipIndex, ok := clipDistanceSemanticIndex(declaration.semantic); ok {
			return fmt.Sprintf("clipDistance%d", clipIndex)
		}
	}
	if s.stage == tgsiFragment && declaration.semantic == "POSITION" {
		return "fragmentPosition"
	}
	if s.stage == tgsiFragment {
		if colorIndex, ok := fragmentColorSemanticIndex(declaration.semantic); ok {
			return fmt.Sprintf("fragmentColor%d", colorIndex)
		}
	}
	if s.stage == tgsiFragment && declaration.semantic == "SAMPLEMASK" {
		return "fragmentSampleMask"
	}
	if (s.stage == tgsiVertex || s.stage == tgsiGeometry || s.stage == tgsiTessControl || s.stage == tgsiTessEvaluation) && (declaration.semantic == "" || isInterpolationQualifier(declaration.semantic)) {
		return fmt.Sprintf("varying%d", declarationRank(s.outputs, index, true))
	}
	return semanticName(declaration.semantic, index)
}

func (s *tgsiShader) dimensionalInputName(declaration tgsiDeclaration, index int, vertex string) string {
	switch declaration.semantic {
	case "POSITION":
		return fmt.Sprintf("gl_in[%s].gl_Position", vertex)
	case "PSIZE":
		return fmt.Sprintf("vec4(gl_in[%s].gl_PointSize)", vertex)
	}
	if clipIndex, ok := clipDistanceSemanticIndex(declaration.semantic); ok {
		base := clipIndex * 4
		return fmt.Sprintf("vec4(gl_in[%s].gl_ClipDistance[%d], gl_in[%s].gl_ClipDistance[%d], gl_in[%s].gl_ClipDistance[%d], gl_in[%s].gl_ClipDistance[%d])",
			vertex, base, vertex, base+1, vertex, base+2, vertex, base+3)
	}
	return fmt.Sprintf("%s[%s]", s.inputName(index), vertex)
}

func (s *tgsiShader) tessControlOutputName(declaration tgsiDeclaration, index int) string {
	return s.tessControlOutputNameAt(declaration, index, "gl_InvocationID")
}

func (s *tgsiShader) tessControlOutputNameAt(declaration tgsiDeclaration, index int, vertex string) string {
	if declaration.semantic == "POSITION" {
		return "gl_out[" + vertex + "].gl_Position"
	}
	if declaration.semantic == "PSIZE" {
		return "gl_out[" + vertex + "].gl_PointSize"
	}
	return s.outputName(index) + "[" + vertex + "]"
}

func (s *tgsiShader) geometryEmitStatement() string {
	var statement strings.Builder
	for _, declaration := range s.outputs {
		if declaration.semantic == "POSITION" {
			statement.WriteString("gl_Position = geometryPosition;\n")
			statement.WriteString("gl_Position.y *= uWinsysAdjustY;\n")
			break
		}
	}
	for index := 0; index <= maxDeclarationIndex(s.outputs); index++ {
		declaration, ok := s.outputs[index]
		if !ok {
			continue
		}
		if declaration.semantic == "PSIZE" {
			fmt.Fprintf(&statement, "gl_PointSize = %s.x;\n", s.outputName(index))
		}
		if declaration.semantic == "VIEWPORT_INDEX" {
			fmt.Fprintf(&statement, "gl_ViewportIndex = floatBitsToInt(%s).x;\n", s.outputName(index))
		}
		if declaration.semantic == "LAYER" {
			fmt.Fprintf(&statement, "gl_Layer = floatBitsToInt(%s).x;\n", s.outputName(index))
		}
		if clipIndex, isClipDistance := clipDistanceSemanticIndex(declaration.semantic); isClipDistance {
			name := s.outputName(index)
			for component, swizzle := range "xyzw" {
				fmt.Fprintf(&statement, "gl_ClipDistance[%d] = %s.%c;\n", clipIndex*4+component, name, swizzle)
			}
		}
	}
	return strings.TrimSuffix(statement.String(), "\n")
}

func clipDistanceSemanticIndex(semantic string) (int, bool) {
	semantic = strings.TrimSpace(semantic)
	if semantic == "CLIPDIST" {
		return 0, true
	}
	if !strings.HasPrefix(semantic, "CLIPDIST[") || !strings.HasSuffix(semantic, "]") {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(semantic, "CLIPDIST["), "]"))
	return index, err == nil && index >= 0 && index < 2
}

func fragmentColorSemanticIndex(semantic string) (int, bool) {
	semantic = strings.TrimSpace(semantic)
	if semantic == "COLOR" {
		return 0, true
	}
	if !strings.HasPrefix(semantic, "COLOR[") || !strings.HasSuffix(semantic, "]") {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(semantic, "COLOR["), "]"))
	return index, err == nil && index >= 0 && index < 8
}

func declarationRank(declarations map[int]tgsiDeclaration, index int, _ bool) int {
	rank := 0
	for candidate := 0; candidate < index; candidate++ {
		declaration, ok := declarations[candidate]
		if !ok || declaration.semantic == "FACE" || declaration.semantic == "POSITION" {
			continue
		}
		rank++
	}
	return rank
}

func isInterpolationQualifier(value string) bool {
	switch value {
	case "CONSTANT", "LINEAR", "PERSPECTIVE", "COLOR":
		return true
	default:
		return false
	}
}

func parseTGSIDeclarationModifiers(first, remainder string) (semantic, interpolation, location string) {
	modifiers := []string{strings.TrimSpace(first)}
	if remainder != "" {
		modifiers = append(modifiers, splitTGSIList(remainder)...)
	}
	for index, modifier := range modifiers {
		modifier = strings.TrimSpace(modifier)
		switch modifier {
		case "":
		case "COLOR":
			if index == 0 {
				semantic = modifier
			} else {
				interpolation = modifier
			}
		case "CONSTANT", "LINEAR", "PERSPECTIVE":
			interpolation = modifier
		case "CENTROID", "SAMPLE":
			location = modifier
		default:
			if semantic == "" {
				semantic = modifier
			}
		}
	}
	return semantic, interpolation, location
}

func tgsiInterpolationQualifier(declaration tgsiDeclaration) string {
	var qualifiers []string
	switch declaration.location {
	case "CENTROID":
		qualifiers = append(qualifiers, "centroid")
	case "SAMPLE":
		qualifiers = append(qualifiers, "sample")
	}
	switch declaration.interpolation {
	case "CONSTANT":
		qualifiers = append(qualifiers, "flat")
	case "LINEAR":
		qualifiers = append(qualifiers, "noperspective")
	case "PERSPECTIVE", "COLOR", "":
	default:
		return ""
	}
	return strings.Join(qualifiers, " ")
}

func semanticName(semantic string, fallback int) string {
	semantic = strings.TrimSpace(semantic)
	if semantic == "" {
		return fmt.Sprintf("varying%d", fallback)
	}
	replacer := strings.NewReplacer("[", "_", "]", "", ".", "_")
	return "varying_" + strings.ToLower(replacer.Replace(semantic))
}

type glslVarying struct {
	name      string
	qualifier string
}

func pointSpriteFragmentSource(fragment string, coordinates uint32) string {
	for coordinates != 0 {
		index := bits.TrailingZeros32(coordinates)
		name := fmt.Sprintf("varying_generic_%d", index)
		declaration := regexp.MustCompile(`(?m)^\s*(?:(?:flat|smooth|noperspective)\s+)?in\s+vec4\s+` +
			regexp.QuoteMeta(name) + `\s*;\s*\n?`)
		if declaration.MatchString(fragment) {
			fragment = declaration.ReplaceAllString(fragment, "")
			fragment = regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\b`).
				ReplaceAllString(fragment, "vec4(gl_PointCoord, 0.0, 1.0)")
		}
		coordinates &^= 1 << index
	}
	return fragment
}

func typedFragmentOutputSource(fragment string, classes [8]uint8, dualSource bool) string {
	var finalizers strings.Builder
	for index := range classes {
		name := fmt.Sprintf("fragmentColor%d", index)
		declaration := regexp.MustCompile(`(?m)^layout\(location\s*=\s*\d+\)\s+out\s+vec4\s+` +
			regexp.QuoteMeta(name) + `\s*;\s*$`)
		if !declaration.MatchString(fragment) {
			continue
		}
		location, outputIndex := index, -1
		if dualSource && index < 2 {
			location, outputIndex = 0, index
		}
		layout := fmt.Sprintf("layout(location = %d", location)
		if outputIndex >= 0 {
			layout += fmt.Sprintf(", index = %d", outputIndex)
		}
		layout += ")"
		if classes[index] == fragmentOutputFloat {
			fragment = declaration.ReplaceAllString(fragment,
				fmt.Sprintf("%s out vec4 %s;", layout, name))
			continue
		}

		rawName := name + "Raw"
		fragment = regexp.MustCompile(`\b`+regexp.QuoteMeta(name)+`\b`).ReplaceAllString(fragment, rawName)
		rawDeclaration := regexp.MustCompile(`(?m)^layout\(location\s*=\s*\d+\)\s+out\s+vec4\s+` +
			regexp.QuoteMeta(rawName) + `\s*;\s*$`)
		outputType, conversion := "uvec4", "floatBitsToUint"
		if classes[index] == fragmentOutputSInt {
			outputType, conversion = "ivec4", "floatBitsToInt"
		}
		fragment = rawDeclaration.ReplaceAllString(fragment,
			fmt.Sprintf("vec4 %s = vec4(0.0);\n%s out %s %s;", rawName, layout, outputType, name))
		fmt.Fprintf(&finalizers, "    %s = %s(%s);\n", name, conversion, rawName)
	}
	if finalizers.Len() == 0 {
		return fragment
	}
	if closing := strings.LastIndex(fragment, "}"); closing >= 0 {
		fragment = fragment[:closing] + finalizers.String() + fragment[closing:]
	}
	return fragment
}

func uniformOnlyLargeFragmentTGSI(source string) bool {
	largeConstants := false
	colorOutput := false
	for _, rawLine := range strings.Split(source, "\n") {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "DCL IN[") || strings.HasPrefix(line, "DCL SV[") ||
			strings.HasPrefix(line, "DCL SAMP[") {
			return false
		}
		if strings.HasPrefix(line, "DCL OUT[") {
			if colorOutput || !strings.HasPrefix(line, "DCL OUT[0], COLOR") {
				return false
			}
			colorOutput = true
		}
		if match := tgsiConstantDeclarationPattern.FindStringSubmatch(line); match != nil {
			last := match[2]
			if match[3] != "" {
				last = match[3]
			}
			maximum, _ := strconv.Atoi(last)
			largeConstants = largeConstants || maximum >= 1023
		}
		if match := tgsiInstructionPattern.FindStringSubmatch(line); match != nil {
			switch match[1] {
			case "KILL", "KILL_IF", "DDX", "DDY", "TEX", "TEX2", "TXB", "TXB2", "TXD", "TXF", "TXL", "TXL2", "TXP", "TG4", "LODQ":
				return false
			}
		}
	}
	return largeConstants && colorOutput
}

var tgsiFragmentOutputDeclaration = regexp.MustCompile(`(?m)^layout\(location\s*=\s*0\)\s+out\s+vec4\s+fragmentColor0\s*;\s*$`)

func hoistUniformFragmentSource(vertex, fragment string) (string, string, bool) {
	if !tgsiFragmentOutputDeclaration.MatchString(fragment) ||
		len(regexp.MustCompile(`(?m)^layout\(location\s*=\s*\d+\)\s+out\s+vec4\s+fragmentColor\d+\s*;\s*$`).FindAllString(fragment, -1)) != 1 {
		return vertex, fragment, false
	}
	body := strings.TrimPrefix(fragment, "#version 410 core\n")
	body = tgsiFragmentOutputDeclaration.ReplaceAllString(body, "vec4 vmshHoistedFragmentColor = vec4(0.0);")
	body = regexp.MustCompile(`\bfragmentColor0\b`).ReplaceAllString(body, "vmshHoistedFragmentColor")
	body = regexp.MustCompile(`\bimmediate(\d+)\b`).ReplaceAllString(body, "vmshFragmentImmediate$1")
	body = regexp.MustCompile(`\btgsi([A-Za-z0-9_]+)\b`).ReplaceAllString(body, "vmshFragmentTGSI$1")
	for buffer := 0; buffer < 16; buffer++ {
		constant := tgsiConstantName(tgsiFragment, buffer)
		block := tgsiConstantBlockName(tgsiFragment, buffer)
		texture := tgsiConstantTextureName(tgsiFragment, buffer)
		declaration := regexp.MustCompile(`layout\(std140\) uniform ` + regexp.QuoteMeta(block) +
			` \{ uvec4 ` + regexp.QuoteMeta(constant) + `\[\d+\]; \};`)
		if !declaration.MatchString(body) {
			continue
		}
		body = declaration.ReplaceAllString(body, "uniform usamplerBuffer "+texture+";")
		reference := regexp.MustCompile(`\b` + regexp.QuoteMeta(constant) + `\[(address\[\d+\]\.[xyzw]|\d+)\]`)
		body = reference.ReplaceAllString(body, "texelFetch("+texture+", $1)")
	}
	body = strings.Replace(body, "void main() {", "void vmshRunHoistedFragment() {", 1)
	if !strings.Contains(body, "void vmshRunHoistedFragment() {") {
		return vertex, fragment, false
	}
	const varying = "varying_vmsh_hoisted_fragment_color"
	declarations := "flat out vec4 " + varying + ";\n" + body
	vertex = strings.Replace(vertex, "#version 410 core\n", "#version 410 core\n"+declarations, 1)
	closing := strings.LastIndex(vertex, "}")
	if closing < 0 {
		return vertex, fragment, false
	}
	vertex = vertex[:closing] + "    vmshRunHoistedFragment();\n    " + varying + " = vmshHoistedFragmentColor;\n" + vertex[closing:]
	fragment = "#version 410 core\nflat in vec4 " + varying + ";\n" +
		"layout(location = 0) out vec4 fragmentColor0;\n" +
		"void main() { fragmentColor0 = " + varying + "; }\n"
	return vertex, fragment, true
}

func emulateVertexSystemValueSource(vertex string, emulation hostVertexSystemEmulation) string {
	if emulation.systemValue == emulatedVertexSystemNone {
		return vertex
	}
	attribute := fmt.Sprintf("attribute%d", emulation.attribute)
	declaration := regexp.MustCompile(`(?m)^layout\(location\s*=\s*` +
		strconv.Itoa(int(emulation.attribute)) + `\)\s+in\s+vec4\s+` +
		regexp.QuoteMeta(attribute) + `\s*;\s*$`)
	if !declaration.MatchString(vertex) {
		return vertex
	}
	replacement := "layout(location = " + strconv.Itoa(int(emulation.attribute)) + ") in vec4 vmshVertexSystemValue;\n" +
		"layout(std140) uniform VMSHVertexAttributeBlock { vec4 vmshVertexAttribute[4096]; };\n" +
		"#define " + attribute + " vmshVertexAttribute[floatBitsToInt(vmshVertexSystemValue.y)]"
	vertex = declaration.ReplaceAllString(vertex, replacement)
	builtin := "gl_VertexID"
	if emulation.systemValue == emulatedInstanceID {
		builtin = "gl_InstanceID"
	}
	return strings.ReplaceAll(vertex, builtin, "floatBitsToInt(vmshVertexSystemValue.x)")
}

func typedVertexInputSource(vertex string, signedMask, unsignedMask uint16) string {
	for index := 0; index < 16; index++ {
		mask := uint16(1) << index
		if signedMask&mask == 0 && unsignedMask&mask == 0 {
			continue
		}
		attribute := fmt.Sprintf("attribute%d", index)
		declaration := regexp.MustCompile(`(?m)^layout\(location\s*=\s*` + strconv.Itoa(index) +
			`\)\s+in\s+vec4\s+` + regexp.QuoteMeta(attribute) + `\s*;\s*$`)
		if !declaration.MatchString(vertex) {
			continue
		}
		inputType, conversion := "uvec4", "uintBitsToFloat"
		if signedMask&mask != 0 {
			inputType, conversion = "ivec4", "intBitsToFloat"
		}
		replacement := fmt.Sprintf("layout(location = %d) in %s %sInteger;\n#define %s %s(%sInteger)",
			index, inputType, attribute, attribute, conversion, attribute)
		vertex = declaration.ReplaceAllString(vertex, replacement)
	}
	return vertex
}

func linkTGSIInterfaces(vertex, fragment string) string {
	vertexOutputs := glslVaryings(vertex, "out")
	fragmentInputs := glslVaryings(fragment, "in")
	fragmentOutputs := glslVaryings(fragment, "out")
	matched := make(map[string]bool, len(fragmentInputs))

	for _, output := range vertexOutputs {
		for _, input := range fragmentInputs {
			inputSourceName := input.name
			for _, suffix := range []string{"GeometryInput", "TessControlInput", "TessEvaluationInput"} {
				inputSourceName = strings.TrimSuffix(inputSourceName, suffix)
			}
			if output.name == inputSourceName {
				if output.name != input.name {
					vertex = regexp.MustCompile(`\b`+regexp.QuoteMeta(output.name)+`\b`).ReplaceAllString(vertex, input.name)
				}
				matched[input.name] = true
				if input.qualifier != "" && output.qualifier != input.qualifier {
					vertex = setGLSLVaryingQualifier(vertex, "out", output.name, input.qualifier)
				}
				break
			}
		}
	}
	// A stage may write a generic that its immediate consumer does not read,
	// while that consumer independently writes the same TGSI semantic for the
	// following stage. GLSL uses one program-wide name namespace for these
	// interfaces, so leave the live downstream varying alone and give the dead
	// upstream output a distinct name.
	for _, output := range vertexOutputs {
		if matched[output.name] {
			continue
		}
		for _, downstream := range fragmentOutputs {
			if output.name != downstream.name {
				continue
			}
			vertex = regexp.MustCompile(`\b`+regexp.QuoteMeta(output.name)+`\b`).
				ReplaceAllString(vertex, output.name+"UpstreamUnused")
			break
		}
	}
	fallback := regexp.MustCompile(`^varying\d+$`)
	for _, output := range vertexOutputs {
		if matched[output.name] || !fallback.MatchString(output.name) {
			continue
		}
		for _, input := range fragmentInputs {
			if matched[input.name] {
				continue
			}
			vertex = regexp.MustCompile(`\b`+regexp.QuoteMeta(output.name)+`\b`).
				ReplaceAllString(vertex, input.name)
			if input.qualifier != "" {
				vertex = setGLSLVaryingQualifier(vertex, "out", input.name, input.qualifier)
			}
			matched[input.name] = true
			break
		}
	}

	var declarations strings.Builder
	var initializers strings.Builder
	for _, input := range fragmentInputs {
		if matched[input.name] {
			continue
		}
		if input.qualifier != "" {
			declarations.WriteString(input.qualifier)
			declarations.WriteByte(' ')
		}
		fmt.Fprintf(&declarations, "out vec4 %s;\n", input.name)
		fmt.Fprintf(&initializers, "    %s = vec4(0.0);\n", input.name)
	}
	if declarations.Len() == 0 {
		return vertex
	}
	vertex = strings.Replace(vertex, "#version 410 core\n", "#version 410 core\n"+declarations.String(), 1)
	return strings.Replace(vertex, "void main() {\n", "void main() {\n"+initializers.String(), 1)
}

func setGLSLVaryingQualifier(source, direction, name, qualifier string) string {
	declaration := regexp.MustCompile(`(?m)^\s*(?:(?:flat|smooth|noperspective|centroid|sample)\s+)*` +
		regexp.QuoteMeta(direction) + `\s+vec4\s+` + regexp.QuoteMeta(name) + `\s*;\s*$`)
	return declaration.ReplaceAllString(source, qualifier+" "+direction+" vec4 "+name+";")
}

func glslVaryings(source, direction string) []glslVarying {
	var varyings []glslVarying
	for _, line := range strings.Split(source, "\n") {
		fields := strings.Fields(line)
		directionIndex := -1
		for index, field := range fields {
			if field == direction {
				directionIndex = index
				break
			}
		}
		if directionIndex < 0 || directionIndex+2 >= len(fields) || fields[directionIndex+1] != "vec4" {
			continue
		}
		name := strings.TrimSuffix(fields[directionIndex+2], ";")
		name = strings.TrimSuffix(name, "[]")
		if !strings.HasPrefix(name, "varying") {
			continue
		}
		varyings = append(varyings, glslVarying{
			name:      name,
			qualifier: strings.Join(fields[:directionIndex], " "),
		})
	}
	return varyings
}

func (s *tgsiShader) glsl() (string, error) {
	var source strings.Builder
	source.WriteString("#version 410 core\n")
	if s.stage == tgsiVertex {
		// VirGL applications commonly render coplanar geometry in multiple
		// passes with different vertex programs.  GLSL only guarantees identical
		// window-space positions across those programs when gl_Position is
		// invariant; without it, distant surfaces can fail an EQUAL/LEQUAL depth
		// comparison as alternating triangles.
		source.WriteString("invariant gl_Position;\n")
		source.WriteString("uniform float uWinsysAdjustY;\n")
		for index := 0; index <= maxDeclarationIndex(s.outputs); index++ {
			declaration, ok := s.outputs[index]
			_, isClipDistance := clipDistanceSemanticIndex(declaration.semantic)
			if ok && isClipDistance {
				source.WriteString("out float gl_ClipDistance[8];\n")
				break
			}
		}
	} else if s.stage == tgsiFragment {
		for index := 0; index <= maxDeclarationIndex(s.inputs); index++ {
			declaration, ok := s.inputs[index]
			_, isClipDistance := clipDistanceSemanticIndex(declaration.semantic)
			if ok && isClipDistance {
				source.WriteString("in float gl_ClipDistance[8];\n")
				break
			}
		}
	} else if s.stage == tgsiGeometry {
		input, ok := geometryInputLayout(s.geometryInputPrimitive)
		if !ok {
			return "", fmt.Errorf("geometry input primitive %q is unsupported", s.geometryInputPrimitive)
		}
		output, ok := geometryOutputLayout(s.geometryOutputPrimitive)
		if !ok || s.geometryMaxVertices < 1 || s.geometryMaxVertices > 256 || s.geometryInvocations < 0 || s.geometryInvocations > 32 {
			return "", fmt.Errorf("geometry layout %q/%q max_vertices=%d invocations=%d is unsupported",
				s.geometryInputPrimitive, s.geometryOutputPrimitive, s.geometryMaxVertices, s.geometryInvocations)
		}
		if s.geometryInvocations > 1 {
			fmt.Fprintf(&source, "layout(%s, invocations = %d) in;\n", input, s.geometryInvocations)
		} else {
			fmt.Fprintf(&source, "layout(%s) in;\n", input)
		}
		fmt.Fprintf(&source, "layout(%s, max_vertices = %d) out;\n", output, s.geometryMaxVertices)
		source.WriteString("uniform float uWinsysAdjustY;\n")
	} else if s.stage == tgsiTessControl {
		if s.tessControlVertices < 1 || s.tessControlVertices > 32 {
			return "", fmt.Errorf("tessellation control output vertex count %d is unsupported", s.tessControlVertices)
		}
		fmt.Fprintf(&source, "layout(vertices = %d) out;\n", s.tessControlVertices)
	} else if s.stage == tgsiTessEvaluation {
		primitive, ok := tessEvaluationPrimitive(s.tessEvaluationPrimitive)
		if !ok || s.tessEvaluationSpacing < 0 || s.tessEvaluationSpacing > 2 {
			return "", fmt.Errorf("tessellation evaluation layout primitive=%d spacing=%d is unsupported", s.tessEvaluationPrimitive, s.tessEvaluationSpacing)
		}
		spacing := []string{"fractional_odd_spacing", "fractional_even_spacing", "equal_spacing"}[s.tessEvaluationSpacing]
		order := "ccw"
		if s.tessEvaluationClockwise {
			order = "cw"
		}
		pointMode := ""
		if s.tessEvaluationPointMode {
			pointMode = ", point_mode"
		}
		fmt.Fprintf(&source, "layout(%s, %s, %s%s) in;\n", primitive, spacing, order, pointMode)
		source.WriteString("uniform float uWinsysAdjustY;\n")
	}
	for index := 0; index <= maxDeclarationIndex(s.inputs); index++ {
		declaration, ok := s.inputs[index]
		if !ok {
			continue
		}
		if s.stage == tgsiFragment && (declaration.semantic == "POSITION" ||
			declaration.semantic == "FACE" || declaration.semantic == "PCOORD" ||
			declaration.semantic == "VIEWPORT_INDEX" || declaration.semantic == "LAYER") {
			continue
		}
		if s.stage == tgsiTessControl || s.stage == tgsiTessEvaluation {
			switch declaration.semantic {
			case "POSITION", "PSIZE", "CLIPDIST", "TESSOUTER", "TESSINNER":
				continue
			case "PATCH":
				fmt.Fprintf(&source, "patch in vec4 %s;\n", semanticName(declaration.semantic, index))
				continue
			default:
				fmt.Fprintf(&source, "in vec4 %s[];\n", s.inputName(index))
				continue
			}
		}
		if s.stage == tgsiFragment {
			if _, isClipDistance := clipDistanceSemanticIndex(declaration.semantic); isClipDistance {
				fmt.Fprintf(&source, "vec4 %s;\n", s.inputName(index))
				continue
			}
		}
		if s.stage == tgsiVertex {
			fmt.Fprintf(&source, "layout(location = %d) in vec4 %s;\n", index, s.inputName(index))
		} else if s.stage == tgsiGeometry {
			if declaration.semantic == "POSITION" || declaration.semantic == "PSIZE" {
				continue
			}
			if _, isClipDistance := clipDistanceSemanticIndex(declaration.semantic); isClipDistance {
				continue
			}
			qualifier := tgsiInterpolationQualifier(declaration)
			if qualifier != "" {
				qualifier += " "
			}
			fmt.Fprintf(&source, "%sin vec4 %s[];\n", qualifier, s.inputName(index))
		} else {
			qualifier := tgsiInterpolationQualifier(declaration)
			if qualifier != "" {
				qualifier += " "
			}
			fmt.Fprintf(&source, "%sin vec4 %s;\n", qualifier, s.inputName(index))
		}
	}
	for index := 0; index <= maxDeclarationIndex(s.outputs); index++ {
		declaration, ok := s.outputs[index]
		if !ok || ((s.stage == tgsiVertex || s.stage == tgsiTessEvaluation) && declaration.semantic == "POSITION") {
			continue
		}
		if s.stage == tgsiTessControl {
			switch declaration.semantic {
			case "POSITION", "PSIZE", "CLIPDIST":
				continue
			case "TESSOUTER":
				source.WriteString("vec4 tessLevelOuter;\n")
				continue
			case "TESSINNER":
				source.WriteString("vec4 tessLevelInner;\n")
				continue
			case "PATCH":
				fmt.Fprintf(&source, "patch out vec4 %s;\n", s.outputName(index))
				continue
			default:
				fmt.Fprintf(&source, "out vec4 %s[];\n", s.outputName(index))
				continue
			}
		}
		if s.stage == tgsiGeometry && declaration.semantic == "POSITION" {
			fmt.Fprintf(&source, "vec4 %s;\n", s.outputName(index))
			continue
		}
		if s.stage == tgsiVertex {
			if _, ok := clipDistanceSemanticIndex(declaration.semantic); ok {
				fmt.Fprintf(&source, "vec4 %s;\n", s.outputName(index))
				continue
			}
		}
		if s.stage == tgsiGeometry {
			if _, ok := clipDistanceSemanticIndex(declaration.semantic); ok || declaration.semantic == "PSIZE" {
				fmt.Fprintf(&source, "vec4 %s;\n", s.outputName(index))
				continue
			}
		}
		if (s.stage == tgsiGeometry || s.stage == tgsiTessEvaluation) &&
			(declaration.semantic == "VIEWPORT_INDEX" || declaration.semantic == "LAYER") {
			fmt.Fprintf(&source, "vec4 %s;\n", s.outputName(index))
			continue
		}
		if s.stage == tgsiFragment {
			if colorIndex, ok := fragmentColorSemanticIndex(declaration.semantic); ok {
				fmt.Fprintf(&source, "layout(location = %d) out vec4 %s;\n", colorIndex, s.outputName(index))
				continue
			}
			if declaration.semantic == "POSITION" {
				fmt.Fprintf(&source, "vec4 %s;\n", s.outputName(index))
				continue
			}
		}
		if s.stage == tgsiFragment && declaration.semantic == "SAMPLEMASK" {
			fmt.Fprintf(&source, "vec4 %s;\n", s.outputName(index))
		} else {
			fmt.Fprintf(&source, "out vec4 %s;\n", s.outputName(index))
		}
	}
	if s.stage == tgsiFragment && s.fragmentColor0WritesAll {
		color0Found := false
		for _, declaration := range s.outputs {
			if index, ok := fragmentColorSemanticIndex(declaration.semantic); ok && index == 0 {
				color0Found = true
				break
			}
		}
		if !color0Found {
			return "", errors.New("FS_COLOR0_WRITES_ALL_CBUFS requires color output zero")
		}
		for index := 1; index < 8; index++ {
			fmt.Fprintf(&source, "layout(location = %d) out vec4 fragmentColor%d;\n", index, index)
		}
	}
	if s.usesDoubleRoundEven {
		source.WriteString(`double tgsiRoundEven(double value) {
	double lower = floor(value);
	double fraction = value - lower;
	double nearest = floor(value + 0.5LF);
	double evenTie = lower + 2.0LF * fract(abs(lower) * 0.5LF);
	return fraction == 0.5LF ? evenTie : nearest;
}
`)
	}
	if s.usesDoubleEquality {
		source.WriteString(`bool tgsiDoubleEqual(uvec2 left, uvec2 right) {
	bool leftNaN = (left.y & 0x7ff00000u) == 0x7ff00000u && ((left.y & 0xfffffu) != 0u || left.x != 0u);
	bool rightNaN = (right.y & 0x7ff00000u) == 0x7ff00000u && ((right.y & 0xfffffu) != 0u || right.x != 0u);
	bool bothZero = left.x == 0u && (left.y & 0x7fffffffu) == 0u && right.x == 0u && (right.y & 0x7fffffffu) == 0u;
	return !leftNaN && !rightNaN && (all(equal(left, right)) || bothZero);
}
`)
	}
	if s.usesCubeGradientFix {
		source.WriteString(`bool tgsiCubePositiveZ(vec3 coordinate) {
	return coordinate.z > 0.0 && coordinate.z >= abs(coordinate.x) && coordinate.z >= abs(coordinate.y);
}

float tgsiCubeGradientLOD(vec3 coordinate, vec3 gradientX, vec3 gradientY, vec2 size) {
	float major = coordinate.z;
	float scale = 0.5 / (major * major);
	vec2 projectedX = vec2(
		gradientX.x * major - coordinate.x * gradientX.z,
		-gradientX.y * major + coordinate.y * gradientX.z) * scale;
	vec2 projectedY = vec2(
		gradientY.x * major - coordinate.x * gradientY.z,
		-gradientY.y * major + coordinate.y * gradientY.z) * scale;
	float footprint = max(length(projectedX * size), length(projectedY * size));
	return log2(max(footprint, 1.0 / 65536.0));
}
`)
	}
	for buffer, maxConstant := range s.maxConstants {
		if maxConstant >= 0 {
			if s.usesNativeConstantBlock(buffer) {
				fmt.Fprintf(&source, "layout(std140) uniform %s { uvec4 %s[%d]; };\n",
					tgsiConstantBlockName(s.stage, buffer), s.constantName(buffer), maxConstant+1)
			} else {
				fmt.Fprintf(&source, "uniform vec4 %s[%d];\n", s.constantName(buffer), maxConstant+1)
			}
		}
	}
	multisampleViewSwizzle := false
	for _, view := range s.samplerViews {
		if view.target == "2D_MSAA" || view.target == "2D_ARRAY_MSAA" {
			multisampleViewSwizzle = true
			break
		}
	}
	if multisampleViewSwizzle {
		source.WriteString(`vec4 tgsiSamplerViewSwizzle(vec4 value, ivec4 swizzle) {
	vec4 result;
	for (int component = 0; component < 4; component++) {
		int sourceComponent = swizzle[component];
		result[component] = sourceComponent < 4 ? value[sourceComponent] : (sourceComponent == 5 ? 1.0 : 0.0);
	}
	return result;
}
	uvec4 tgsiSamplerViewSwizzle(uvec4 value, ivec4 swizzle) {
		uvec4 result;
		for (int component = 0; component < 4; component++) {
			int sourceComponent = swizzle[component];
			result[component] = sourceComponent < 4 ? value[sourceComponent] : (sourceComponent == 5 ? 1u : 0u);
		}
		return result;
	}
	ivec4 tgsiSamplerViewSwizzle(ivec4 value, ivec4 swizzle) {
		ivec4 result;
		for (int component = 0; component < 4; component++) {
			int sourceComponent = swizzle[component];
			result[component] = sourceComponent < 4 ? value[sourceComponent] : (sourceComponent == 5 ? 1 : 0);
		}
		return result;
	}
`)
	}
	for index := 0; index <= max(s.maxSampler, s.maxSamplerView); index++ {
		samplerType := "sampler2D"
		view := s.samplerViews[index]
		if view.target == "BUFFER" {
			samplerType = "samplerBuffer"
		} else if view.target == "1D" {
			samplerType = "sampler1D"
		} else if view.target == "1D_ARRAY" {
			samplerType = "sampler1DArray"
		} else if view.target == "3D" {
			samplerType = "sampler3D"
		} else if view.target == "2D_MSAA" {
			if view.returnType == "UINT" || view.returnType == "SINT" {
				samplerType = "sampler2DArray"
			} else {
				samplerType = "sampler2DMS"
			}
		} else if view.target == "2D_ARRAY_MSAA" {
			if view.returnType == "UINT" || view.returnType == "SINT" {
				samplerType = "sampler2DArray"
			} else {
				samplerType = "sampler2DMSArray"
			}
		} else if view.target == "SHADOW2D" {
			samplerType = "sampler2DShadow"
		} else if view.target == "SHADOW2D_ARRAY" {
			samplerType = "sampler2DArrayShadow"
		} else if view.target == "SHADOWRECT" {
			// Rectangle resources use ordinary 2D storage on Apple and the
			// translated sampling coordinates are normalized accordingly.
			samplerType = "sampler2DShadow"
		} else if view.target == "CUBE" {
			samplerType = "samplerCube"
		} else if view.target == "CUBE_ARRAY" {
			samplerType = "samplerCubeArray"
		} else if view.target == "SHADOWCUBE" {
			samplerType = "samplerCubeShadow"
		} else if view.target == "SHADOWCUBE_ARRAY" {
			samplerType = "samplerCubeArrayShadow"
		} else if view.target == "2D_ARRAY" {
			samplerType = "sampler2DArray"
		}
		if view.returnType == "UINT" {
			samplerType = "u" + samplerType
		} else if view.returnType == "SINT" {
			samplerType = "i" + samplerType
		}
		fmt.Fprintf(&source, "uniform %s %s;\n", samplerType, tgsiSamplerName(s.stage, index))
		fmt.Fprintf(&source, "uniform float %s;\n", tgsiSamplerLODCrossoverName(s.stage, index))
		fmt.Fprintf(&source, "uniform int %s;\n", tgsiSamplerSampleCountName(s.stage, index))
		fmt.Fprintf(&source, "uniform int %s;\n", tgsiSamplerLevelCountName(s.stage, index))
		if view.target == "2D_MSAA" || view.target == "2D_ARRAY_MSAA" {
			fmt.Fprintf(&source, "uniform ivec4 %s;\n", tgsiSamplerViewSwizzleName(s.stage, index))
		}
	}
	for index, immediate := range s.immediates {
		fmt.Fprintf(&source, "const vec4 immediate%d = %s;\n", index, immediate)
	}
	source.WriteString("void main() {\n")
	if s.maxTemporary >= 0 {
		fmt.Fprintf(&source, "    vec4 temporary[%d];\n", s.maxTemporary+1)
	}
	if s.maxAddress >= 0 {
		fmt.Fprintf(&source, "    ivec4 address[%d];\n", s.maxAddress+1)
	}
	if s.deferredDiscard {
		source.WriteString("    bool tgsiKilled = false;\n")
	}
	if s.stage == tgsiTessControl {
		for _, declaration := range s.outputs {
			switch declaration.semantic {
			case "TESSOUTER":
				source.WriteString("    tessLevelOuter = vec4(1.0);\n")
			case "TESSINNER":
				source.WriteString("    tessLevelInner = vec4(1.0);\n")
			}
		}
	}
	if s.stage == tgsiVertex || s.stage == tgsiGeometry {
		for index := 0; index <= maxDeclarationIndex(s.outputs); index++ {
			declaration, ok := s.outputs[index]
			if !ok {
				continue
			}
			if _, isClipDistance := clipDistanceSemanticIndex(declaration.semantic); isClipDistance {
				fmt.Fprintf(&source, "    %s = vec4(0.0);\n", s.outputName(index))
			}
		}
	}
	writeFragmentClipInputs := func() {
		for index := 0; index <= maxDeclarationIndex(s.inputs); index++ {
			declaration, ok := s.inputs[index]
			clipIndex, isClipDistance := clipDistanceSemanticIndex(declaration.semantic)
			if !ok || !isClipDistance {
				continue
			}
			name := s.inputName(index)
			fmt.Fprintf(&source, "    %s = vec4(gl_ClipDistance[%d], gl_ClipDistance[%d], gl_ClipDistance[%d], gl_ClipDistance[%d]);\n",
				name, clipIndex*4, clipIndex*4+1, clipIndex*4+2, clipIndex*4+3)
		}
	}
	if s.stage == tgsiFragment {
		writeFragmentClipInputs()
	}
	for _, instruction := range s.instructions {
		fmt.Fprintf(&source, "    %s\n", instruction)
	}
	if s.stage == tgsiTessControl {
		for _, declaration := range s.outputs {
			switch declaration.semantic {
			case "TESSOUTER":
				for index := 0; index < 4; index++ {
					fmt.Fprintf(&source, "    gl_TessLevelOuter[%d] = tessLevelOuter.%c;\n", index, "xyzw"[index])
				}
			case "TESSINNER":
				for index := 0; index < 2; index++ {
					fmt.Fprintf(&source, "    gl_TessLevelInner[%d] = tessLevelInner.%c;\n", index, "xy"[index])
				}
			}
		}
	}
	if s.stage == tgsiFragment && s.fragmentColor0WritesAll {
		for index := 1; index < 8; index++ {
			fmt.Fprintf(&source, "    fragmentColor%d = fragmentColor0;\n", index)
		}
	}
	if s.stage == tgsiFragment {
		for index := 0; index <= maxDeclarationIndex(s.outputs); index++ {
			if declaration, ok := s.outputs[index]; ok {
				switch declaration.semantic {
				case "POSITION":
					fmt.Fprintf(&source, "    gl_FragDepth = %s.z;\n", s.outputName(index))
				case "SAMPLEMASK":
					fmt.Fprintf(&source, "    gl_SampleMask[0] = floatBitsToInt(%s).x;\n", s.outputName(index))
				}
			}
		}
	}
	if s.stage == tgsiVertex {
		for index := 0; index <= maxDeclarationIndex(s.outputs); index++ {
			if declaration, ok := s.outputs[index]; ok && declaration.semantic == "PSIZE" {
				fmt.Fprintf(&source, "    gl_PointSize = %s.x;\n", s.outputName(index))
				break
			}
		}
		for index := 0; index <= maxDeclarationIndex(s.outputs); index++ {
			declaration, ok := s.outputs[index]
			clipIndex, isClipDistance := clipDistanceSemanticIndex(declaration.semantic)
			if !ok || !isClipDistance {
				continue
			}
			name := s.outputName(index)
			for component, swizzle := range "xyzw" {
				fmt.Fprintf(&source, "    gl_ClipDistance[%d] = %s.%c;\n", clipIndex*4+component, name, swizzle)
			}
		}
		source.WriteString("    gl_Position.y *= uWinsysAdjustY;\n")
	}
	if s.stage == tgsiTessEvaluation {
		for index := 0; index <= maxDeclarationIndex(s.outputs); index++ {
			declaration, ok := s.outputs[index]
			if !ok {
				continue
			}
			switch declaration.semantic {
			case "VIEWPORT_INDEX":
				fmt.Fprintf(&source, "    gl_ViewportIndex = floatBitsToInt(%s).x;\n", s.outputName(index))
			case "LAYER":
				fmt.Fprintf(&source, "    gl_Layer = floatBitsToInt(%s).x;\n", s.outputName(index))
			}
		}
		source.WriteString("    gl_Position.y *= uWinsysAdjustY;\n")
	}
	source.WriteString("}\n")
	return source.String(), nil
}

func tgsiSamplerName(stage uint32, index int) string {
	if stage == tgsiVertex {
		return fmt.Sprintf("vertexSampler%d", index)
	}
	if stage == tgsiGeometry {
		return fmt.Sprintf("geometrySampler%d", index)
	}
	if stage == tgsiTessControl {
		return fmt.Sprintf("tessControlSampler%d", index)
	}
	if stage == tgsiTessEvaluation {
		return fmt.Sprintf("tessEvaluationSampler%d", index)
	}
	return fmt.Sprintf("fragmentSampler%d", index)
}

func geometryInputLayout(primitive string) (string, bool) {
	switch primitive {
	case "POINTS":
		return "points", true
	case "LINES":
		return "lines", true
	case "LINES_ADJACENCY":
		return "lines_adjacency", true
	case "TRIANGLES":
		return "triangles", true
	case "TRIANGLES_ADJACENCY":
		return "triangles_adjacency", true
	default:
		return "", false
	}
}

func tessEvaluationPrimitive(primitive int) (string, bool) {
	switch primitive {
	case 1:
		return "isolines", true
	case 4:
		return "triangles", true
	case 7:
		return "quads", true
	default:
		return "", false
	}
}

func geometryOutputLayout(primitive string) (string, bool) {
	switch primitive {
	case "POINTS":
		return "points", true
	case "LINE_STRIP":
		return "line_strip", true
	case "TRIANGLE_STRIP":
		return "triangle_strip", true
	default:
		return "", false
	}
}

func tgsiSamplerLODCrossoverName(stage uint32, index int) string {
	return tgsiSamplerName(stage, index) + "LODCrossover"
}

func tgsiSamplerSampleCountName(stage uint32, index int) string {
	return tgsiSamplerName(stage, index) + "SampleCount"
}

func tgsiSamplerLevelCountName(stage uint32, index int) string {
	return tgsiSamplerName(stage, index) + "LevelCount"
}

func tgsiSamplerViewSwizzleName(stage uint32, index int) string {
	return tgsiSamplerName(stage, index) + "ViewSwizzle"
}

func maxDeclarationIndex(declarations map[int]tgsiDeclaration) int {
	result := -1
	for index := range declarations {
		result = max(result, index)
	}
	return result
}
