package virgl

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/tinyrange/crumblecracker/internal/core/virtio"
)

const (
	captureMagic = "VIRGLCAP\x01"

	captureReset byte = iota + 1
	captureCreateContext
	captureDestroyContext
	captureCreateResource
	captureUnrefResource
	captureTransferToHost
	captureExecute
	captureScanout
)

type captureHost struct {
	next       hostBackend
	file       *os.File
	compressed *gzip.Writer
	writer     *bufio.Writer
	frames     int
	maxFrames  int
	closed     bool
}

func captureHostFromEnvironment(next hostBackend) (hostBackend, error) {
	path := os.Getenv("VMSH_VIRGL_CAPTURE")
	if path == "" {
		return next, nil
	}
	maxFrames := 240
	if value := os.Getenv("VMSH_VIRGL_CAPTURE_FRAMES"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("invalid VMSH_VIRGL_CAPTURE_FRAMES %q", value)
		}
		maxFrames = parsed
	}
	return newCaptureHost(next, path, maxFrames)
}

func newCaptureHost(next hostBackend, path string, maxFrames int) (*captureHost, error) {
	if maxFrames <= 0 {
		return nil, errors.New("VirGL capture frame limit must be positive")
	}
	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create VirGL capture: %w", err)
	}
	if _, err := io.WriteString(file, captureMagic); err != nil {
		file.Close()
		return nil, fmt.Errorf("write VirGL capture header: %w", err)
	}
	compressed, err := gzip.NewWriterLevel(file, gzip.BestSpeed)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("initialize VirGL capture compression: %w", err)
	}
	return &captureHost{
		next: next, file: file, compressed: compressed,
		writer: bufio.NewWriterSize(compressed, 1<<20), maxFrames: maxFrames,
	}, nil
}

func (h *captureHost) writeRecord(kind byte, payload []byte) error {
	if h.closed {
		return nil
	}
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return fmt.Errorf("VirGL capture record is too large: %d bytes", len(payload))
	}
	var header [5]byte
	header[0] = kind
	binary.LittleEndian.PutUint32(header[1:], uint32(len(payload)))
	if _, err := h.writer.Write(header[:]); err != nil {
		return err
	}
	_, err := h.writer.Write(payload)
	return err
}

func (h *captureHost) finishCapture() error {
	if h.closed {
		return nil
	}
	h.closed = true
	if err := h.writer.Flush(); err != nil {
		h.file.Close()
		return err
	}
	if err := h.compressed.Close(); err != nil {
		h.file.Close()
		return err
	}
	return h.file.Close()
}

func (h *captureHost) createContext(id uint32) error {
	if err := h.next.createContext(id); err != nil {
		return err
	}
	return h.writeRecord(captureCreateContext, words(id))
}

func (h *captureHost) destroyContext(id uint32) error {
	if err := h.next.destroyContext(id); err != nil {
		return err
	}
	return h.writeRecord(captureDestroyContext, words(id))
}

func (h *captureHost) createResource(description virtio.GPUResource3D) error {
	if err := h.next.createResource(description); err != nil {
		fmt.Fprintf(os.Stderr, "VirGL capture rejected resource %+v: %v\n", description, err)
		return err
	}
	return h.writeRecord(captureCreateResource, words(
		description.ID, description.Target, description.Format, description.Bind,
		description.Width, description.Height, description.Depth, description.ArraySize,
		description.LastLevel, description.Samples, description.Flags,
	))
}

func (h *captureHost) unrefResource(id uint32) error {
	if err := h.next.unrefResource(id); err != nil {
		return err
	}
	return h.writeRecord(captureUnrefResource, words(id))
}

func (h *captureHost) transferToHost(resource *resource, transfer virtio.GPUTransfer3D) error {
	if err := h.next.transferToHost(resource, transfer); err != nil {
		return err
	}
	return h.recordTransferToHost(resource, transfer)
}

func (h *captureHost) queueBufferTransfer(resource *resource, transfer virtio.GPUTransfer3D) error {
	queue, ok := h.next.(bufferTransferQueuer)
	if !ok {
		return h.transferToHost(resource, transfer)
	}
	if err := queue.queueBufferTransfer(resource, transfer); err != nil {
		return err
	}
	return h.recordTransferToHost(resource, transfer)
}

func (h *captureHost) recordTransferToHost(resource *resource, transfer virtio.GPUTransfer3D) error {
	if h.closed {
		return nil
	}
	data, normalized, err := captureTransferData(resource, transfer)
	if err != nil {
		return err
	}
	var payload bytes.Buffer
	writeUint32s(&payload,
		normalized.ContextID, normalized.ResourceID,
		normalized.Box.X, normalized.Box.Y, normalized.Box.Z,
		normalized.Box.Width, normalized.Box.Height, normalized.Box.Depth,
	)
	_ = binary.Write(&payload, binary.LittleEndian, normalized.Offset)
	writeUint32s(&payload, normalized.Level, normalized.Stride, normalized.LayerStride, uint32(len(data)))
	payload.Write(data)
	return h.writeRecord(captureTransferToHost, payload.Bytes())
}

func captureTransferData(resource *resource, transfer virtio.GPUTransfer3D) ([]byte, virtio.GPUTransfer3D, error) {
	size, err := transferDataSize(resource.description, transfer)
	if err != nil {
		return nil, transfer, err
	}
	end := transfer.Offset + size
	if end < transfer.Offset || end > uint64(len(resource.data)) {
		return nil, transfer, fmt.Errorf("capture VirGL resource %d transfer range %d..%d exceeds %d bytes",
			transfer.ResourceID, transfer.Offset, end, len(resource.data))
	}
	data := append([]byte(nil), resource.data[int(transfer.Offset):int(end)]...)
	transfer.Offset = 0
	transfer.Backing = nil
	return data, transfer, nil
}

func (h *captureHost) transferFromHost(resource *resource, transfer virtio.GPUTransfer3D) error {
	return h.next.transferFromHost(resource, transfer)
}

func (h *captureHost) execute(contextID uint32, commands []command, resources map[uint32]*resource) error {
	executeErr := h.next.execute(contextID, commands, resources)
	if h.closed {
		return executeErr
	}
	stream := encodeCommands(commands)
	var payload bytes.Buffer
	writeUint32s(&payload, contextID, uint32(len(stream)))
	payload.Write(stream)
	captureErr := h.writeRecord(captureExecute, payload.Bytes())
	return errors.Join(executeErr, captureErr)
}

func encodeCommands(commands []command) []byte {
	var result []byte
	for _, command := range commands {
		header := uint32(command.Opcode) | uint32(command.Object)<<8 | uint32(len(command.Payload))<<16
		result = binary.LittleEndian.AppendUint32(result, header)
		for _, value := range command.Payload {
			result = binary.LittleEndian.AppendUint32(result, value)
		}
	}
	return result
}

func (h *captureHost) readScanout(resource *resource, rect image.Rectangle) ([]byte, int, error) {
	return h.next.readScanout(resource, rect)
}

func (h *captureHost) nativeScanout(resource *resource, rect image.Rectangle) (virtio.GPUNativeFrame, bool, error) {
	frame, available, err := h.next.nativeScanout(resource, rect)
	if err != nil || !available || h.closed {
		return frame, available, err
	}
	h.frames++
	if err := h.writeRecord(captureScanout, words(
		resource.description.ID,
		uint32(rect.Min.X), uint32(rect.Min.Y), uint32(rect.Max.X), uint32(rect.Max.Y),
	)); err != nil {
		return virtio.GPUNativeFrame{}, false, err
	}
	if err := h.writer.Flush(); err != nil {
		return virtio.GPUNativeFrame{}, false, err
	}
	if err := h.compressed.Flush(); err != nil {
		return virtio.GPUNativeFrame{}, false, err
	}
	if h.frames >= h.maxFrames {
		if err := h.finishCapture(); err != nil {
			return virtio.GPUNativeFrame{}, false, err
		}
	}
	return frame, available, nil
}

func (h *captureHost) reset() error {
	if err := h.next.reset(); err != nil {
		return err
	}
	return h.writeRecord(captureReset, nil)
}

func (h *captureHost) close() error {
	captureErr := h.finishCapture()
	backendErr := h.next.close()
	return errors.Join(captureErr, backendErr)
}

func words(values ...uint32) []byte {
	result := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(result[index*4:], value)
	}
	return result
}

func writeUint32s(writer io.Writer, values ...uint32) {
	for _, value := range values {
		_ = binary.Write(writer, binary.LittleEndian, value)
	}
}

type captureDecoder struct {
	file       *os.File
	compressed *gzip.Reader
	reader     *bufio.Reader
}

func openCapture(path string) (*captureDecoder, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	header := make([]byte, len(captureMagic))
	if _, err := io.ReadFull(file, header); err != nil || string(header) != captureMagic {
		file.Close()
		return nil, errors.New("invalid VirGL capture header")
	}
	compressed, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	return &captureDecoder{file: file, compressed: compressed, reader: bufio.NewReaderSize(compressed, 1<<20)}, nil
}

func (d *captureDecoder) next() (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(d.reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.LittleEndian.Uint32(header[1:])
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(d.reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

func (d *captureDecoder) close() error {
	return errors.Join(d.compressed.Close(), d.file.Close())
}

// SummarizeCapture prints protocol features relevant to replay compatibility.
func SummarizeCapture(path string, output io.Writer) error {
	decoder, err := openCapture(path)
	if err != nil {
		return err
	}
	defer decoder.close()
	type counters struct {
		resources, flaggedResources, contexts, subcontexts               int
		executes, draws, indexed, nonzeroStart, instanced, startInstance int
		vertexElements, instanceDivisors, checkpoints                    int
		blits, copies, scissors, viewports                               int
		transfers, bufferTransfers, fullBufferTransfers                  int
		transferBytes, bufferTransferBytes, bufferStorageBytes           uint64
	}
	var count counters
	modes := make(map[uint32]int)
	opcodes := make(map[uint32]int)
	objectCommands := make(map[uint32]int)
	redundantBinds := make(map[uint32]int)
	activeSubcontexts := make(map[uint32]uint32)
	boundObjects := make(map[[3]uint32]uint32)
	boundObjectValid := make(map[[3]uint32]bool)
	rasterizers := make(map[uint32]int)
	dsa := make(map[uint32]int)
	vertexFormats := make(map[uint32]int)
	resourceFormats := make(map[uint32]int)
	samplerViewFormats := make(map[uint32]int)
	surfaceFormats := make(map[uint32]int)
	resourceFormatIDs := make(map[uint32][]uint32)
	scanouts := make(map[uint32]int)
	resourceSizes := make(map[uint32][3]uint32)
	resourceFormatByID := make(map[uint32]uint32)
	var blitDetails []string
	blitResources := make(map[uint32]bool)
	transferDetails := make(map[uint32][]string)
	matchingScanoutResources := make(map[uint32]int)
	for {
		kind, payload, err := decoder.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		switch kind {
		case captureCreateContext:
			count.contexts++
		case captureCreateResource:
			if len(payload) != 44 {
				return errors.New("invalid resource record in VirGL capture")
			}
			count.resources++
			format := binary.LittleEndian.Uint32(payload[8:])
			id := binary.LittleEndian.Uint32(payload)
			resourceFormats[format]++
			resourceFormatIDs[format] = append(resourceFormatIDs[format], id)
			resourceFormatByID[id] = format
			if binary.LittleEndian.Uint32(payload[40:]) != 0 {
				count.flaggedResources++
			}
			resourceSizes[id] = [3]uint32{
				binary.LittleEndian.Uint32(payload[4:]),
				binary.LittleEndian.Uint32(payload[16:]),
				binary.LittleEndian.Uint32(payload[20:]),
			}
		case captureUnrefResource:
			if len(payload) == 4 {
				id := binary.LittleEndian.Uint32(payload)
				delete(resourceSizes, id)
				delete(resourceFormatByID, id)
			}
		case captureTransferToHost:
			if len(payload) < 56 {
				return errors.New("invalid transfer record in VirGL capture")
			}
			count.transfers++
			length := uint64(binary.LittleEndian.Uint32(payload[52:]))
			if length > uint64(len(payload)-56) {
				return errors.New("truncated transfer data in VirGL capture")
			}
			count.transferBytes += length
			resourceID := binary.LittleEndian.Uint32(payload[4:])
			if description, ok := resourceSizes[resourceID]; ok && description[0] == 0 {
				count.bufferTransfers++
				count.bufferTransferBytes += length
				count.bufferStorageBytes += uint64(description[1])
				if binary.LittleEndian.Uint32(payload[8:]) == 0 &&
					binary.LittleEndian.Uint32(payload[20:]) == description[1] {
					count.fullBufferTransfers++
				}
			} else if len(transferDetails[resourceID]) < 16 {
				previewLength := int(length)
				if previewLength > 16 {
					previewLength = 16
				}
				transferDetails[resourceID] = append(transferDetails[resourceID], fmt.Sprintf(
					"transfer resource=%d(format=%#x) box=%dx%dx%d@%d,%d,%d level=%d stride=%d layer_stride=%d bytes=%d preview=%x",
					resourceID, resourceFormatByID[resourceID],
					binary.LittleEndian.Uint32(payload[20:]), binary.LittleEndian.Uint32(payload[24:]), binary.LittleEndian.Uint32(payload[28:]),
					binary.LittleEndian.Uint32(payload[8:]), binary.LittleEndian.Uint32(payload[12:]), binary.LittleEndian.Uint32(payload[16:]),
					binary.LittleEndian.Uint32(payload[40:]), binary.LittleEndian.Uint32(payload[44:]), binary.LittleEndian.Uint32(payload[48:]),
					length, payload[56:56+previewLength]))
			}
		case captureExecute:
			if len(payload) < 8 {
				return errors.New("invalid execute record in VirGL capture")
			}
			commands, err := decodeCommands(payload[8:])
			if err != nil {
				return err
			}
			count.executes++
			contextID := binary.LittleEndian.Uint32(payload)
			for _, command := range commands {
				opcodes[uint32(command.Opcode)]++
				if command.Object != 0 {
					objectCommands[uint32(command.Opcode)<<8|uint32(command.Object)]++
				}
				if command.Opcode == 28 && len(command.Payload) == 1 {
					activeSubcontexts[contextID] = command.Payload[0]
				}
				key := [3]uint32{contextID, activeSubcontexts[contextID], uint32(command.Object)}
				if command.Opcode == 1 && len(command.Payload) != 0 &&
					boundObjectValid[key] && boundObjects[key] == command.Payload[0] {
					boundObjectValid[key] = false
				}
				if command.Opcode == 2 && len(command.Payload) == 1 {
					if boundObjectValid[key] && boundObjects[key] == command.Payload[0] {
						redundantBinds[uint32(command.Object)]++
					}
					boundObjects[key] = command.Payload[0]
					boundObjectValid[key] = true
				}
				switch {
				case command.Opcode == 29:
					count.subcontexts++
				case command.Opcode == 1 && command.Object == 2 && len(command.Payload) >= 2:
					rasterizers[command.Payload[1]]++
				case command.Opcode == 1 && command.Object == 3 && len(command.Payload) >= 2:
					dsa[command.Payload[1]]++
				case command.Opcode == 1 && command.Object == 5:
					for index := 1; index+3 < len(command.Payload); index += 4 {
						count.vertexElements++
						vertexFormats[command.Payload[index+3]]++
						if command.Payload[index+1] != 0 {
							count.instanceDivisors++
						}
					}
				case command.Opcode == 1 && command.Object == 6 && len(command.Payload) == 6:
					samplerViewFormats[command.Payload[2]&0x00ffffff]++
				case command.Opcode == 1 && command.Object == 8:
					if len(command.Payload) == 5 {
						surfaceFormats[command.Payload[2]]++
					} else if len(command.Payload) == 2 {
						surfaceFormats[resourceFormatByID[command.Payload[1]]]++
					}
				case command.Opcode == 8 && len(command.Payload) >= 12:
					count.draws++
					modes[command.Payload[2]]++
					if command.Payload[3] != 0 {
						count.indexed++
					}
					if command.Payload[0] != 0 {
						count.nonzeroStart++
					}
					if command.Payload[4] != 1 {
						count.instanced++
					}
					if command.Payload[6] != 0 {
						count.startInstance++
					}
				case command.Opcode == 4:
					count.viewports++
				case command.Opcode == 15:
					count.scissors++
				case command.Opcode == 16:
					count.blits++
					if len(command.Payload) == 21 {
						dst, src := command.Payload[3], command.Payload[12]
						blitResources[src] = true
						blitResources[dst] = true
						blitDetails = append(blitDetails, fmt.Sprintf(
							"blit source=%d(format=%#x) destination=%d(format=%#x) payload=%v",
							src, resourceFormatByID[src], dst, resourceFormatByID[dst], command.Payload))
					}
				case command.Opcode == 17:
					count.copies++
				}
			}
		case captureScanout:
			count.checkpoints++
			if len(payload) == 20 {
				scanouts[binary.LittleEndian.Uint32(payload)]++
				width := binary.LittleEndian.Uint32(payload[12:]) - binary.LittleEndian.Uint32(payload[4:])
				height := binary.LittleEndian.Uint32(payload[16:]) - binary.LittleEndian.Uint32(payload[8:])
				matchingScanoutResources = make(map[uint32]int)
				for id, dimensions := range resourceSizes {
					if dimensions[0] != 0 && dimensions[1] == width && dimensions[2] == height {
						matchingScanoutResources[id]++
					}
				}
			}
		}
	}
	fmt.Fprintf(output, "resources=%d flagged_resources=%d contexts=%d subcontexts=%d executes=%d checkpoints=%d\n",
		count.resources, count.flaggedResources, count.contexts, count.subcontexts, count.executes, count.checkpoints)
	fmt.Fprintf(output, "draws=%d indexed=%d nonzero_start=%d instanced=%d start_instance=%d vertex_elements=%d instance_divisors=%d\n",
		count.draws, count.indexed, count.nonzeroStart, count.instanced, count.startInstance, count.vertexElements, count.instanceDivisors)
	fmt.Fprintf(output, "viewports=%d scissors=%d blits=%d copies=%d\n",
		count.viewports, count.scissors, count.blits, count.copies)
	for _, detail := range blitDetails {
		fmt.Fprintln(output, detail)
	}
	for _, id := range sortedUint32Keys(blitResources) {
		for _, detail := range transferDetails[id] {
			fmt.Fprintln(output, detail)
		}
	}
	fmt.Fprintf(output, "transfers=%d bytes=%d buffer_transfers=%d full_buffer_transfers=%d buffer_bytes=%d buffer_storage_bytes=%d\n",
		count.transfers, count.transferBytes, count.bufferTransfers, count.fullBufferTransfers,
		count.bufferTransferBytes, count.bufferStorageBytes)
	printUint32Counts(output, "modes", modes)
	printUint32Counts(output, "opcodes", opcodes)
	printUint32Counts(output, "object_commands", objectCommands)
	printUint32Counts(output, "redundant_binds", redundantBinds)
	printUint32Counts(output, "rasterizers", rasterizers)
	printUint32Counts(output, "dsa", dsa)
	printUint32Counts(output, "vertex_formats", vertexFormats)
	printUint32Counts(output, "resource_formats", resourceFormats)
	printUint32Counts(output, "sampler_view_formats", samplerViewFormats)
	printUint32Counts(output, "surface_formats", surfaceFormats)
	formats := make([]uint32, 0, len(resourceFormatIDs))
	for format := range resourceFormatIDs {
		formats = append(formats, format)
	}
	sort.Slice(formats, func(i, j int) bool { return formats[i] < formats[j] })
	for _, format := range formats {
		ids := resourceFormatIDs[format]
		if len(ids) <= 16 {
			fmt.Fprintf(output, "resource_format_%#x_ids=%v\n", format, ids)
		}
	}
	for _, id := range sortedUint32KeysFromSizes(resourceSizes) {
		dimensions := resourceSizes[id]
		if dimensions[0] != 0 && dimensions[1] >= 1024 && dimensions[2] >= 768 {
			fmt.Fprintf(output, "active_large_texture=%d target=%#x format=%#x size=%dx%d\n",
				id, dimensions[0], resourceFormatByID[id], dimensions[1], dimensions[2])
		}
	}
	printUint32Counts(output, "scanouts", scanouts)
	printUint32Counts(output, "final_scanout_sized_textures", matchingScanoutResources)
	return nil
}

func sortedUint32KeysFromSizes(values map[uint32][3]uint32) []uint32 {
	keys := make([]uint32, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func printUint32Counts(output io.Writer, label string, counts map[uint32]int) {
	keys := make([]uint32, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	fmt.Fprintf(output, "%s:", label)
	for _, key := range keys {
		fmt.Fprintf(output, " %#x=%d", key, counts[key])
	}
	_, _ = fmt.Fprintln(output)
}

func sortedUint32Keys(values map[uint32]bool) []uint32 {
	keys := make([]uint32, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
