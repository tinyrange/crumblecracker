//go:build darwin && arm64

package hvf

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/arm64vm"
	"github.com/tinyrange/crumblecracker/internal/core/fdt"
	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	managedagent "github.com/tinyrange/crumblecracker/internal/core/managed/agent"
	managedsession "github.com/tinyrange/crumblecracker/internal/core/managed/session"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/serial"
	"github.com/tinyrange/crumblecracker/internal/core/timing"
	"github.com/tinyrange/crumblecracker/internal/core/virgl"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

var debugTiming = strings.TrimSpace(os.Getenv("CCX3_DEBUG_TIMING")) != ""
var exitTiming = newExitTiming()

const (
	execTerminateGrace = 500 * time.Millisecond
	execKillWait       = 2 * time.Second
)

func timingLog(format string, args ...any) {
	if !debugTiming {
		return
	}
	fmt.Fprintf(os.Stderr, "ccx3 timing: "+format+"\n", args...)
}

type exitTimingStats struct {
	mu       sync.Mutex
	dumpOnce sync.Once
	enabled  bool
	buckets  map[string]*exitTimingBucket
}

type exitTimingBucket struct {
	Count      int64 `json:"count"`
	TotalNanos int64 `json:"total_nanos"`
	MaxNanos   int64 `json:"max_nanos"`
}

func newExitTiming() *exitTimingStats {
	return &exitTimingStats{
		enabled: strings.TrimSpace(os.Getenv("CCX3_EXIT_TIMING")) != "",
		buckets: map[string]*exitTimingBucket{},
	}
}

func (s *exitTimingStats) Record(bucket string, start time.Time) {
	s.RecordDuration(bucket, time.Since(start))
}

func (s *exitTimingStats) Enabled() bool {
	return s != nil && s.enabled
}

func (s *exitTimingStats) RecordDuration(bucket string, elapsed time.Duration) {
	if s == nil || !s.enabled || bucket == "" {
		return
	}
	nanos := elapsed.Nanoseconds()
	s.mu.Lock()
	stat := s.buckets[bucket]
	if stat == nil {
		stat = &exitTimingBucket{}
		s.buckets[bucket] = stat
	}
	stat.Count++
	stat.TotalNanos += nanos
	if nanos > stat.MaxNanos {
		stat.MaxNanos = nanos
	}
	s.mu.Unlock()
}

func (s *exitTimingStats) Dump() {
	if s == nil || !s.enabled {
		return
	}
	s.dumpOnce.Do(s.dump)
}

func ExitTimingSnapshot() map[string]exitTimingBucket {
	if exitTiming == nil {
		return nil
	}
	return exitTiming.Snapshot()
}

func (s *exitTimingStats) Snapshot() map[string]exitTimingBucket {
	if s == nil || !s.enabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := make(map[string]exitTimingBucket, len(s.buckets))
	for name, stat := range s.buckets {
		snapshot[name] = *stat
	}
	return snapshot
}

func (s *exitTimingStats) dump() {
	snapshot := s.Snapshot()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccx3 exit timing: marshal: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "ccx3 exit timing: %s\n", payload)
}

const (
	instanceReadyMarker   = vmruntime.InstanceReadyMarker
	initDurationMarker    = vmruntime.InitDurationMarker
	execTimingMarker      = vmruntime.ExecTimingMarker
	commandBeginMarker    = vmruntime.CommandBeginMarker
	commandOutputMarker   = vmruntime.CommandOutputMarker
	commandErrorMarker    = vmruntime.CommandErrorMarker
	commandExitMarkerPref = vmruntime.CommandExitMarkerPref
	arm64VirtualTimerPPI  = 27
)

type serialTranscript = arm64vm.SerialTranscript
type bootEventWriter = arm64vm.BootEventWriter

func newSerialTranscript() *serialTranscript { return arm64vm.NewSerialTranscript() }
func newBootEventWriter(callback func(client.BootEvent) error) *bootEventWriter {
	return arm64vm.NewBootEventWriter(callback)
}
func hasFatalBootText(text string) bool { return arm64vm.HasFatalBootText(text) }
func parseInitDurationMarker(text string) (int, bool) {
	return arm64vm.ParseInitDurationMarker(text)
}

type ContainerRunRequest = vmruntime.RunRequest
type DirectoryShare = vmruntime.DirectoryShare
type ContainerRunResult = vmruntime.RunResult

type ContainerSession struct {
	cancel            context.CancelFunc
	runCtx            context.Context
	doneCh            chan sessionRunResult
	closeDone         <-chan struct{}
	image             *oci.Image
	baseEnv           []string
	workDir           string
	dmesg             bool
	uart              *serial.UART8250
	control           virtio.VsockConn
	transcript        *arm64vm.SerialTranscript
	serialOut         *arm64vm.SerialTranscript
	listener          virtio.VsockListener
	clipboardListener virtio.VsockListener
	displayListener   virtio.VsockListener
	vsock             *virtio.Vsock
	balloon           *virtio.Balloon
	desktop           *virtio.Desktop
	rootFS            virtio.ShareMounter
	fsdevs            []*virtio.FS
	fsCloseErr        *error
	sendMu            sync.Mutex
	shareMu           sync.Mutex
	shares            map[string]client.ShareMount
	imageMounts       map[string]string
	nextID            atomic.Uint64
	activeExecs       *atomic.Int32
	inlineExec        bool
}

type ManagedMetadata struct {
	Root    imagefs.Directory
	BaseEnv []string
	WorkDir string
}

func (s *ContainerSession) ManagedMetadata() ManagedMetadata {
	if s == nil {
		return ManagedMetadata{}
	}
	var root imagefs.Directory
	if s.image != nil {
		root = s.image.RootFS
	}
	return ManagedMetadata{
		Root:    root,
		BaseEnv: append([]string(nil), s.baseEnv...),
		WorkDir: s.workDir,
	}
}

type readyResult struct {
	conn virtio.VsockConn
	err  error
}

type sessionRunResult struct {
	result ContainerRunResult
	err    error
}

func parseExecTimingMarkers(text, id string) map[string]int {
	out := map[string]int{}
	if text == "" || id == "" {
		return out
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, execTimingMarker+id+":") {
			continue
		}
		rest := strings.TrimPrefix(line, execTimingMarker+id+":")
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 {
			continue
		}
		ms, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		out[strings.TrimSpace(parts[0])] = ms
	}
	return out
}

func hasManagedExecBegin(text, id string) bool {
	return vmruntime.HasManagedExecBegin(text, id)
}

func hasManagedExecFirstByte(text, id string) bool {
	return vmruntime.HasManagedExecFirstByte(text, id)
}

func validateGuestUser(user string) error {
	user = strings.TrimSpace(user)
	if user == "" || user == "root" || user == "0" || user == "0:0" {
		return nil
	}
	uidPart, gidPart, hasGID := strings.Cut(user, ":")
	if !validGuestUserComponent(uidPart) {
		return fmt.Errorf("user must be a name or numeric uid, optionally followed by :group or :gid")
	}
	if hasGID && (!validGuestUserComponent(gidPart) || strings.Contains(gidPart, ":")) {
		return fmt.Errorf("user must be a name or numeric uid, optionally followed by :group or :gid")
	}
	return nil
}

func validGuestUserComponent(value string) bool {
	if value == "" || strings.ContainsAny(value, ":\r\n\x00") {
		return false
	}
	numeric := true
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			numeric = false
			break
		}
	}
	return !numeric || isUint32String(value)
}

func isUint32String(value string) bool {
	n := uint64(0)
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
		n = n*10 + uint64(ch-'0')
		if n > uint64(^uint32(0)) {
			return false
		}
	}
	return value != ""
}

func StartContainer(ctx context.Context, req ContainerRunRequest) (*ContainerSession, error) {
	return StartContainerStream(ctx, req, nil)
}

func StartContainerStream(ctx context.Context, req ContainerRunRequest, onEvent func(client.BootEvent) error) (*ContainerSession, error) {
	if req.Persistent {
		return startPersistentContainer(ctx, req, onEvent)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan error, 1)
	doneCh := make(chan sessionRunResult, 1)

	go func() {
		result, err := runContainer(runCtx, req, readyCh)
		doneCh <- sessionRunResult{result: result, err: err}
	}()

	select {
	case err := <-readyCh:
		if err != nil {
			cancel()
			res := <-doneCh
			if res.err != nil {
				return nil, res.err
			}
			return nil, err
		}
		return &ContainerSession{cancel: cancel, doneCh: doneCh}, nil
	case <-ctx.Done():
		cancel()
		res := <-doneCh
		if res.err != nil {
			return nil, res.err
		}
		return nil, ctx.Err()
	}
}

func (s *ContainerSession) Wait() error {
	res := <-s.doneCh
	if s.closeDone != nil {
		<-s.closeDone
	}
	if s.fsCloseErr != nil {
		res.err = errors.Join(res.err, *s.fsCloseErr)
	}
	return res.err
}

func (s *ContainerSession) ConsoleHistory(context.Context) (string, error) {
	if s == nil || s.serialOut == nil {
		return "", nil
	}
	return s.serialOut.String(), nil
}

func (s *ContainerSession) Desktop() *virtio.Desktop {
	if s == nil {
		return nil
	}
	return s.desktop
}

func (s *ContainerSession) AddShare(ctx context.Context, share client.ShareMount) error {
	return s.AddShares(ctx, []client.ShareMount{share})
}

func (s *ContainerSession) AddShares(ctx context.Context, requested []client.ShareMount) error {
	_ = ctx
	if s.rootFS == nil {
		return fmt.Errorf("root filesystem does not support runtime shares")
	}
	s.shareMu.Lock()
	defer s.shareMu.Unlock()
	prospective := make(map[string]client.ShareMount, len(s.shares)+len(requested))
	for key, share := range s.shares {
		prospective[key] = share
	}
	var mountsToAdd []virtio.ShareMount
	var sharesToAdd []client.ShareMount
	releasePrepared := true
	defer func() {
		if !releasePrepared {
			return
		}
		for _, mount := range mountsToAdd {
			if closer, ok := mount.Backend.(interface{ Close() error }); ok {
				_ = closer.Close()
			}
		}
	}()
	for _, rawShare := range requested {
		share := rawShare
		key := strings.TrimSpace(share.Mount)
		if key == "" {
			return fmt.Errorf("share mount path is required")
		}
		if !strings.HasPrefix(key, "/") {
			return fmt.Errorf("share mount path %q must be absolute", key)
		}
		key = path.Clean(key)
		if key == "/" {
			return fmt.Errorf("share mount path / cannot replace the VM root filesystem")
		}
		share.Mount = key
		if existing, ok := prospective[key]; ok {
			if existing == share {
				continue
			}
			return fmt.Errorf("share mount %q already exists", key)
		}
		mount, err := arm64vm.BuildShareMount(0, DirectoryShare{
			Source: share.Source, Mount: share.Mount, Writable: share.Writable, MapOwner: share.MapOwner,
			OwnerUID: share.OwnerUID, OwnerGID: share.OwnerGID, Cache: share.Cache,
		})
		if err != nil {
			return err
		}
		prospective[key] = share
		mountsToAdd = append(mountsToAdd, mount)
		sharesToAdd = append(sharesToAdd, share)
	}
	if len(mountsToAdd) == 0 {
		releasePrepared = false
		return nil
	}
	if batch, ok := s.rootFS.(virtio.ShareBatchMounter); ok {
		if err := batch.AddShares(mountsToAdd); err != nil {
			return err
		}
	} else if len(mountsToAdd) > 1 {
		return fmt.Errorf("root filesystem does not support atomic multi-share mutation")
	} else if err := s.rootFS.AddShare(mountsToAdd[0]); err != nil {
		return err
	}
	if s.shares == nil {
		s.shares = make(map[string]client.ShareMount)
	}
	for _, share := range sharesToAdd {
		s.shares[share.Mount] = share
	}
	releasePrepared = false
	return nil
}

func (s *ContainerSession) VirtioFSStats() []virtio.FSStats {
	if s == nil || len(s.fsdevs) == 0 {
		return nil
	}
	out := make([]virtio.FSStats, 0, len(s.fsdevs))
	for _, fsdev := range s.fsdevs {
		if fsdev == nil {
			continue
		}
		out = append(out, fsdev.Stats())
	}
	return out
}

func (s *ContainerSession) BackingUsage() (current, highWater, physical uint64, err error) {
	if s == nil {
		return 0, 0, 0, nil
	}
	var errs []error
	for i, fsdev := range s.fsdevs {
		if fsdev == nil {
			continue
		}
		deviceCurrent, deviceHighWater, devicePhysical, deviceErr := fsdev.BackingUsage()
		current += deviceCurrent
		highWater += deviceHighWater
		physical += devicePhysical
		if deviceErr != nil {
			errs = append(errs, fmt.Errorf("virtio-fs device %d: %w", i, deviceErr))
		}
	}
	return current, highWater, physical, errors.Join(errs...)
}

func (s *ContainerSession) BackingMetadataUsage() (current, highWater uint64) {
	if s == nil {
		return 0, 0
	}
	for _, fsdev := range s.fsdevs {
		if fsdev == nil {
			continue
		}
		deviceCurrent, deviceHighWater := fsdev.BackingMetadataUsage()
		current += deviceCurrent
		highWater += deviceHighWater
	}
	return current, highWater
}

func (s *ContainerSession) BackingSnapshot() virtio.FSBackingUsageSnapshot {
	if s == nil {
		return virtio.FSBackingUsageSnapshot{}
	}
	if tracker := virtio.SharedFSBackingUsageTracker(s.fsdevs); tracker != nil {
		return tracker.Snapshot()
	}
	data, dataHigh, physical, err := s.BackingUsage()
	metadata, metadataHigh := s.BackingMetadataUsage()
	combined := data + metadata
	if combined < data {
		combined = ^uint64(0)
	}
	return virtio.FSBackingUsageSnapshot{
		DataBytes: data, DataHighWaterBytes: dataHigh,
		MetadataBytes: metadata, MetadataHighWaterBytes: metadataHigh,
		CombinedBytes: combined, CombinedHighWaterBytes: max(combined, dataHigh, metadataHigh),
		PhysicalBytes: physical, ReclaimError: err,
	}
}

func (s *ContainerSession) PersistentFSStatus() []virtio.PersistentFSStatus {
	if s == nil {
		return nil
	}
	var statuses []virtio.PersistentFSStatus
	for _, device := range s.fsdevs {
		if device != nil {
			statuses = append(statuses, device.PersistentFSStatus()...)
		}
	}
	return statuses
}

func (s *ContainerSession) AddPortForward(ctx context.Context, forward client.PortForward) error {
	_, _ = ctx, forward
	return fmt.Errorf("instance network port forwarding is not supported on darwin/arm64")
}

func (s *ContainerSession) AddImage(ctx context.Context, mount string, image *oci.Image) error {
	_ = ctx
	if s.rootFS == nil {
		return fmt.Errorf("root filesystem does not support runtime image mounts")
	}
	key := strings.TrimSpace(mount)
	if key == "" {
		return fmt.Errorf("image mount path is required")
	}
	if image == nil || image.RootFS == nil {
		return fmt.Errorf("image root filesystem is not available")
	}
	s.shareMu.Lock()
	if existing, ok := s.imageMounts[key]; ok {
		s.shareMu.Unlock()
		if existing == image.Name {
			return nil
		}
		return fmt.Errorf("image mount %q already exists", key)
	}
	if _, ok := s.shares[key]; ok {
		s.shareMu.Unlock()
		return fmt.Errorf("mount path %q is already in use", key)
	}
	s.shareMu.Unlock()
	if err := s.rootFS.AddShare(virtio.ShareMount{
		GuestPath: key,
		Backend:   virtio.NewImageFS(image.RootFS, image.RootFSDir),
		Writable:  true,
		CacheMode: "aggressive",
	}); err != nil {
		return err
	}
	s.shareMu.Lock()
	if s.imageMounts == nil {
		s.imageMounts = make(map[string]string)
	}
	s.imageMounts[key] = image.Name
	s.shareMu.Unlock()
	return nil
}

func (s *ContainerSession) Exec(ctx context.Context, req client.ExecRequest) (client.ExecResponse, error) {
	startTime := time.Now()
	if len(req.Command) == 0 {
		return client.ExecResponse{}, fmt.Errorf("exec command is required")
	}
	s.markExecActive()
	defer s.markExecDone()
	user := strings.TrimSpace(req.User)
	if err := validateGuestUser(user); err != nil {
		return client.ExecResponse{}, err
	}

	env := effectiveExecEnv(s.baseEnv, req.Env, req.ReplaceEnv)
	command := append([]string(nil), req.Command...)
	if !req.SkipResolve {
		if s.image == nil || s.image.RootFS == nil {
			return client.ExecResponse{}, fmt.Errorf("running instance does not have a default image root filesystem")
		}
		var err error
		command, err = imagefs.ResolveCommand(s.image.RootFS, req.Command, env)
		if err != nil {
			return client.ExecResponse{}, err
		}
	}
	timingLog("session.Exec ResolveCommand took=%s argv=%q", time.Since(startTime), req.Command)
	workDir := req.WorkDir
	if workDir == "" {
		workDir = s.workDir
	}
	if workDir == "" {
		workDir = "/"
	}
	if !strings.HasPrefix(workDir, "/") {
		return client.ExecResponse{}, fmt.Errorf("workdir must be absolute")
	}
	id := strconv.FormatUint(s.nextID.Add(1), 10)

	execReq := req
	execReq.Kind = "exec"
	if s.inlineExec {
		execReq.Kind = "exec_inline"
	}
	execReq.Command = command
	execReq.Env = env
	execReq.WorkDir = workDir
	execReq.User = user

	start := s.transcript.Len()
	releaseTranscript := s.transcript.RetainFrom(start)
	defer releaseTranscript()
	s.sendMu.Lock()
	var err error
	if s.inlineExec {
		err = managedagent.Send(s.controlWriter(), managedagent.ExecRequest(id, execReq))
	} else {
		err = managedagent.SendExec(s.controlWriter(), id, execReq)
	}
	s.sendMu.Unlock()
	if err != nil {
		return client.ExecResponse{}, err
	}
	timingLog("session.Exec writeControlPayload took=%s argv=%q id=%s", time.Since(startTime), req.Command, id)

	waitCtx, cancelWait := s.operationContext(ctx)
	defer cancelWait()
	beginSegment, err := s.transcript.WaitFor(waitCtx, start, func(text string) bool {
		return hasManagedExecBegin(text, id)
	})
	if err != nil {
		if ctx.Err() != nil {
			s.terminateExecAndWait(id, start)
		}
		return client.ExecResponse{}, s.withControlDebug("wait for exec begin", err)
	}
	timingLog("session.Exec waitForBegin took=%s argv=%q id=%s segment_bytes=%d", time.Since(startTime), req.Command, id, len(beginSegment))
	firstByteSegment, err := s.transcript.WaitFor(waitCtx, start, func(text string) bool {
		return hasManagedExecFirstByte(text, id)
	})
	if err != nil {
		if ctx.Err() != nil {
			s.terminateExecAndWait(id, start)
		}
		return client.ExecResponse{}, s.withControlDebug("wait for exec first byte", err)
	}
	timingLog("session.Exec waitForFirstByte took=%s argv=%q id=%s segment_bytes=%d", time.Since(startTime), req.Command, id, len(firstByteSegment))
	segment, err := s.transcript.WaitForCommand(waitCtx, start, id, func(text string) bool {
		_, _, ok := extractManagedExecResult(text, id, s.dmesg)
		return ok
	})
	if err != nil {
		if ctx.Err() != nil {
			s.terminateExecAndWait(id, start)
		}
		return client.ExecResponse{}, s.withControlDebug("wait for exec result", err)
	}
	timingLog("session.Exec waitForResult took=%s argv=%q id=%s segment_bytes=%d", time.Since(startTime), req.Command, id, len(segment))
	if phases := parseExecTimingMarkers(segment, id); len(phases) > 0 {
		order := []string{"recv", "start_begin", "started", "wait_done", "streams_done", "exit_sent"}
		parts := make([]string, 0, len(order))
		for _, name := range order {
			if ms, ok := phases[name]; ok {
				parts = append(parts, fmt.Sprintf("%s=%dms", name, ms))
			}
		}
		if len(parts) > 0 {
			timingLog("session.Exec guest phases argv=%q id=%s %s", req.Command, id, strings.Join(parts, " "))
		}
	}
	exitCode, output, ok := extractManagedExecResult(segment, id, s.dmesg)
	if !ok {
		return client.ExecResponse{}, fmt.Errorf("exec did not produce a complete result")
	}
	timingLog("session.Exec total=%s argv=%q id=%s exit=%d output_bytes=%d", time.Since(startTime), req.Command, id, exitCode, len(output))
	return client.ExecResponse{ExitCode: exitCode, Output: output}, nil
}

func (s *ContainerSession) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	opCtx, cancel := context.WithCancel(ctx)
	if s == nil || s.runCtx == nil {
		return opCtx, cancel
	}
	stop := context.AfterFunc(s.runCtx, cancel)
	return opCtx, func() {
		stop()
		cancel()
	}
}

func (s *ContainerSession) withControlDebug(op string, err error) error {
	if err == nil {
		return nil
	}
	if s == nil || s.vsock == nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	transcript := ""
	if s.transcript != nil {
		transcript = s.transcript.String()
		if len(transcript) > 4096 {
			transcript = transcript[len(transcript)-4096:]
		}
	}
	return fmt.Errorf("%s: %w\n%s\ncontrol transcript tail:\n%s", op, err, s.vsock.Summary(), transcript)
}

func (s *ContainerSession) Flush(ctx context.Context) error {
	id := strconv.FormatUint(s.nextID.Add(1), 10)
	start := s.transcript.Len()
	releaseTranscript := s.transcript.RetainFrom(start)
	defer releaseTranscript()
	s.sendMu.Lock()
	err := managedagent.Send(s.controlWriter(), managedagent.SyncRequest(id))
	s.sendMu.Unlock()
	if err != nil {
		return err
	}
	segment, err := s.transcript.WaitForCommand(ctx, start, id, func(text string) bool {
		_, _, ok := extractManagedExecResult(text, id, s.dmesg)
		return ok
	})
	if err != nil {
		return err
	}
	code, output, ok := extractManagedExecResult(segment, id, s.dmesg)
	if !ok {
		return fmt.Errorf("sync did not produce a complete result")
	}
	if code != 0 {
		return fmt.Errorf("sync exited with status %d: %s", code, output)
	}
	return nil
}

func (s *ContainerSession) RootSnapshot() (imagefs.Directory, error) {
	return s.RootSnapshotAt("/")
}

func (s *ContainerSession) RootSnapshotAt(guestPath string) (imagefs.Directory, error) {
	if s == nil || s.rootFS == nil {
		return nil, fmt.Errorf("root filesystem cannot be snapshotted")
	}
	if strings.TrimSpace(guestPath) == "" {
		guestPath = "/"
	}
	if guestPath == "/" {
		if snapshotter, ok := s.rootFS.(interface {
			RootSnapshot() (imagefs.Directory, error)
		}); ok {
			return snapshotter.RootSnapshot()
		}
		return nil, fmt.Errorf("root filesystem cannot be snapshotted")
	}
	if snapshotter, ok := s.rootFS.(interface {
		RootSnapshotAt(string) (imagefs.Directory, error)
	}); ok {
		return snapshotter.RootSnapshotAt(guestPath)
	}
	return nil, fmt.Errorf("mount %q cannot be snapshotted", guestPath)
}

func (s *ContainerSession) RootSnapshotContext(ctx context.Context) (imagefs.Directory, error) {
	return s.RootSnapshotAtContext(ctx, "/")
}

func (s *ContainerSession) RootSnapshotAtContext(ctx context.Context, guestPath string) (imagefs.Directory, error) {
	if s == nil || s.rootFS == nil {
		return nil, fmt.Errorf("root filesystem cannot be snapshotted")
	}
	if strings.TrimSpace(guestPath) == "" {
		guestPath = "/"
	}
	if guestPath == "/" {
		if snapshotter, ok := s.rootFS.(interface {
			RootSnapshotContext(context.Context) (imagefs.Directory, error)
		}); ok {
			return snapshotter.RootSnapshotContext(ctx)
		}
		return nil, fmt.Errorf("root filesystem does not support cancelable snapshots")
	}
	if snapshotter, ok := s.rootFS.(interface {
		RootSnapshotAtContext(context.Context, string) (imagefs.Directory, error)
	}); ok {
		return snapshotter.RootSnapshotAtContext(ctx, guestPath)
	}
	return nil, fmt.Errorf("mount %q does not support cancelable snapshots", guestPath)
}

func (s *ContainerSession) ExecStream(ctx context.Context, req client.ExecRequest, inputs <-chan client.ExecInput, onEvent func(client.ExecEvent) error) error {
	execStart := time.Now()
	if (req.Kind == "" || req.Kind == "exec") && len(req.Command) == 0 {
		return fmt.Errorf("exec command is required")
	}
	s.markExecActive()
	defer s.markExecDone()
	user := strings.TrimSpace(req.User)
	if err := validateGuestUser(user); err != nil {
		return err
	}

	env := effectiveExecEnv(s.baseEnv, req.Env, req.ReplaceEnv)
	command := append([]string(nil), req.Command...)
	kind := req.Kind
	if kind == "" {
		kind = "exec"
	}
	start := time.Now()
	if kind == "exec" && !req.SkipResolve {
		if s.image == nil || s.image.RootFS == nil {
			return fmt.Errorf("running instance does not have a default image root filesystem")
		}
		var err error
		command, err = imagefs.ResolveCommand(s.image.RootFS, req.Command, env)
		if err != nil {
			return err
		}
	}
	timing.Since(ctx, "exec.resolve_command", start)
	workDir := req.WorkDir
	if workDir == "" {
		workDir = s.workDir
	}
	if workDir == "" {
		workDir = "/"
	}
	if !strings.HasPrefix(workDir, "/") {
		return fmt.Errorf("workdir must be absolute")
	}

	id := strconv.FormatUint(s.nextID.Add(1), 10)
	start = time.Now()
	execReq := req
	execReq.Kind = kind
	execReq.Command = command
	execReq.Env = env
	execReq.WorkDir = workDir
	execReq.User = user
	timing.Since(ctx, "exec.marshal_request", start)

	transcriptStart := s.transcript.Len()
	reader := s.transcript.RetainReader(transcriptStart)
	defer reader.Close()
	writeStart := time.Now()
	s.sendMu.Lock()
	err := managedagent.Send(s.controlWriter(), managedagent.ExecRequest(id, execReq))
	s.sendMu.Unlock()
	if err != nil {
		return err
	}
	timing.Since(ctx, "exec.write_control_payload", writeStart)

	if inputs != nil {
		go s.forwardExecInputs(ctx, id, inputs)
	} else if len(req.Stdin) == 0 {
		stdinStart := time.Now()
		if err := s.sendStdinClose(id); err != nil {
			return err
		}
		timing.Since(ctx, "exec.send_stdin_close", stdinStart)
	}

	streamStart := time.Now()
	err = s.streamExecEvents(ctx, transcriptStart, id, reader, execStart, onEvent)
	timing.Since(ctx, "exec.stream_events", streamStart)
	timing.Since(ctx, "exec.total", execStart)
	return err
}

func (s *ContainerSession) forwardExecInputs(ctx context.Context, id string, inputs <-chan client.ExecInput) {
	managedagent.ForwardInputs(ctx, id, inputs, s.sendExecMessage)
}

func (s *ContainerSession) sendStdinClose(id string) error {
	return s.sendExecMessage(managedagent.StdinCloseRequest(id))
}

func (s *ContainerSession) sendExecMessage(msg vmruntime.ManagedExecRequest) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return managedagent.Send(s.controlWriter(), msg)
}

func (s *ContainerSession) streamExecEvents(ctx context.Context, start int, id string, reader vmruntime.TranscriptReader, execStart time.Time, onEvent func(client.ExecEvent) error) error {
	guestPhases := map[string]int{}
	return managedsession.StreamExecEvents(ctx, managedsession.StreamExecOptions{
		Transcript: s.transcript,
		Reader:     reader,
		Start:      start,
		ID:         id,
		OnEvent:    onEvent,
		OnCallbackFail: func() {
			s.terminateExecAndWait(id, start)
		},
		OnContextDone: func() {
			s.terminateExecAndWait(id, start)
		},
		OnObserve: func(obs managedsession.StreamExecObservation) {
			switch obs.Kind {
			case "transcript_string":
				timing.Record(ctx, "exec.stream_events.transcript_string", obs.Duration)
			case "append_pending":
				timing.Record(ctx, "exec.stream_events.append_pending", obs.Duration)
			case "line":
				timing.Record(ctx, "exec.stream_events.next_line", obs.Duration)
				if phase, ms, ok := recordExecTimingLine(ctx, obs.Line, id); ok {
					recordExecObservedTiming(ctx, phase, ms, execStart, guestPhases)
				}
			case "parse":
				timing.Record(ctx, "exec.stream_events.parse_line", obs.Duration)
			case "callback":
				timing.Record(ctx, "exec.stream_events.callback", obs.Duration)
			case "wait":
				timing.Record(ctx, "exec.stream_events.sleep", obs.Duration)
			case "done":
				recordExecStreamCounts(ctx, obs.Stats)
				timing.Record(ctx, "exec.stream_events.until_done", obs.Duration)
			}
		},
		Wait: func(context.Context) error {
			select {
			case <-s.runCtx.Done():
				return fmt.Errorf("VM exited during exec")
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
				return nil
			}
		},
	})
}

func (s *ContainerSession) terminateExecAndWait(id string, start int) {
	_ = s.sendExecSignal(id, "TERM")
	if s.waitForExecExit(id, start, execTerminateGrace) {
		return
	}
	_ = s.sendExecSignal(id, "KILL")
	_ = s.waitForExecExit(id, start, execKillWait)
}

func (s *ContainerSession) sendExecSignal(id, name string) error {
	msg, ok := managedagent.InputRequest(id, client.ExecInput{Kind: "signal", Signal: name})
	if !ok {
		return nil
	}
	return s.sendExecMessage(msg)
}

func (s *ContainerSession) waitForExecExit(id string, start int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := s.transcript.WaitForCommand(ctx, start, id, func(text string) bool {
		return strings.Contains(text, vmruntime.CommandExitMarkerPref+id+":")
	})
	return err == nil
}

func recordExecTimingLine(ctx context.Context, line, id string) (string, int, bool) {
	prefix := execTimingMarker + id + ":"
	if !strings.HasPrefix(line, prefix) {
		return "", 0, false
	}
	rest := strings.TrimPrefix(line, prefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	ms, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return "", 0, false
	}
	phase := strings.TrimSpace(parts[0])
	if phase == "" {
		return "", 0, false
	}
	timing.Record(ctx, "exec.guest."+phase, time.Duration(ms)*time.Millisecond)
	return phase, ms, true
}

func recordExecObservedTiming(ctx context.Context, phase string, ms int, execStart time.Time, guestPhases map[string]int) {
	timing.Since(ctx, "exec.host_observed."+phase, execStart)
	if prevPhase, ok := previousExecPhase(phase); ok {
		if prevMS, ok := guestPhases[prevPhase]; ok && ms >= prevMS {
			timing.Record(ctx, "exec.guest_delta."+prevPhase+"_to_"+phase, time.Duration(ms-prevMS)*time.Millisecond)
		}
	}
	guestPhases[phase] = ms
}

func previousExecPhase(phase string) (string, bool) {
	switch phase {
	case "start_begin":
		return "recv", true
	case "start_call":
		return "start_begin", true
	case "started":
		return "start_call", true
	case "wait_begin":
		return "started", true
	case "first_stdout":
		return "started", true
	case "first_stderr":
		return "started", true
	case "wait_done":
		return "wait_begin", true
	case "streams_done":
		return "wait_done", true
	case "exit_sent":
		return "streams_done", true
	default:
		return "", false
	}
}

func recordExecStreamCounts(ctx context.Context, stats managedsession.StreamExecStats) {
	recorder := timing.FromContext(ctx)
	if recorder == nil {
		return
	}
	recordCount(recorder, "exec.stream_events.loop", stats.Loops)
	recordCount(recorder, "exec.stream_events.read", stats.Reads)
	recordCount(recorder, "exec.stream_events.line", stats.Lines)
	recordCount(recorder, "exec.stream_events.matched_line", stats.Matched)
	recordCount(recorder, "exec.stream_events.ignored_line", stats.Ignored)
	recordCount(recorder, "exec.stream_events.sleep_count", stats.Waits)
}

func recordCount(recorder *timing.Recorder, name string, count int) {
	if recorder == nil || count <= 0 {
		return
	}
	recorder.RecordCount(name, count)
}

func (s *ContainerSession) Close() error {
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.closeDone != nil {
		select {
		case <-s.closeDone:
		case <-time.After(15 * time.Second):
			return fmt.Errorf("container session did not stop within 15s")
		}
	}
	if s.control != nil {
		_ = s.control.Close()
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.clipboardListener != nil {
		_ = s.clipboardListener.Close()
	}
	if s.displayListener != nil {
		_ = s.displayListener.Close()
	}
	if s.vsock != nil {
		_ = s.vsock.Close()
	}
	var closeErr error
	if s.desktop != nil && s.desktop.GPU != nil {
		closeErr = errors.Join(closeErr, s.desktop.GPU.Close())
	}
	if s.fsCloseErr != nil {
		closeErr = *s.fsCloseErr
	}
	if s.transcript != nil {
		closeErr = errors.Join(closeErr, s.transcript.Close())
	}
	if s.serialOut != nil && s.serialOut != s.transcript {
		closeErr = errors.Join(closeErr, s.serialOut.Close())
	}
	exitTiming.Dump()
	return closeErr
}

func (s *ContainerSession) SetBalloonMB(target uint64) error {
	if s == nil || s.balloon == nil {
		return fmt.Errorf("virtio balloon is unavailable")
	}
	return s.balloon.SetTargetPages(balloonTargetPages(target))
}

func (s *ContainerSession) BalloonState() (targetMB, actualMB uint64, driverReady bool) {
	if s == nil || s.balloon == nil {
		return 0, 0, false
	}
	target, actual, ready := s.balloon.State()
	return uint64(target) * 4096 >> 20, uint64(actual) * 4096 >> 20, ready
}

func (s *ContainerSession) writeControlPayload(payload []byte) error {
	if s.control != nil {
		return writeFull(s.control, payload)
	}
	if s.uart == nil {
		return fmt.Errorf("control channel is not available")
	}
	return s.uart.InjectRXBytes(payload)
}

func (s *ContainerSession) controlWriter() io.Writer {
	return containerControlWriter{session: s}
}

type containerControlWriter struct {
	session *ContainerSession
}

func (w containerControlWriter) Write(payload []byte) (int, error) {
	if w.session == nil {
		return 0, fmt.Errorf("container session is nil")
	}
	if err := w.session.writeControlPayload(payload); err != nil {
		return 0, err
	}
	return len(payload), nil
}

func writeFull(w io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := w.Write(payload)
		if n > 0 {
			payload = payload[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func (s *ContainerSession) markExecActive() {
	if s.activeExecs != nil {
		s.activeExecs.Add(1)
	}
}

func (s *ContainerSession) markExecDone() {
	if s.activeExecs != nil {
		s.activeExecs.Add(-1)
	}
}

func startPersistentContainer(ctx context.Context, req ContainerRunRequest, onEvent func(client.BootEvent) error) (*ContainerSession, error) {
	mountsOwned := true
	defer func() {
		if mountsOwned {
			_ = vmruntime.CloseShareMounts(req.Mounts)
		}
	}()
	start := time.Now()
	if req.Image == nil && req.RootFS == nil {
		return nil, fmt.Errorf("image or rootfs backend is required")
	}
	if len(req.Kernel) == 0 {
		return nil, fmt.Errorf("kernel is required")
	}
	if req.CPUs <= 0 {
		req.CPUs = 1
	}
	if (strings.TrimSpace(req.SnapshotDir) != "" || strings.TrimSpace(req.RestoreSnapshot) != "") &&
		(req.DisplayWidth != 0 || req.DisplayHeight != 0) {
		return nil, fmt.Errorf("display-enabled VMs do not support startup snapshots")
	}

	user := strings.TrimSpace(req.User)
	if user == "" && req.Image != nil {
		user = strings.TrimSpace(req.Image.Config.User)
	}
	if err := validateGuestUser(user); err != nil {
		return nil, err
	}

	workDir := req.WorkDir
	if workDir == "" && req.Image != nil {
		workDir = req.Image.Config.WorkingDir
	}
	if workDir == "" {
		workDir = "/"
	}
	if !strings.HasPrefix(workDir, "/") {
		return nil, fmt.Errorf("workdir must be absolute")
	}

	var baseEnv []string
	if req.Image != nil {
		baseEnv = append([]string(nil), req.Image.Config.Env...)
	}
	baseEnv = vmruntime.WithDefaultEnv(vmruntime.MergeEnv(baseEnv, req.Env))

	initrd, err := arm64vm.BuildPersistentInitramfs(req, baseEnv, workDir)
	if err != nil {
		return nil, fmt.Errorf("build initramfs: %w", err)
	}
	timing.Since(ctx, "hvf.build_persistent_initramfs", start)
	timingLog("hvf.StartContainer initramfs.Build took=%s size=%d", time.Since(start), len(initrd))
	start = time.Now()

	vm, err := NewVMWithOptions(ctx, VMOptions{CPUs: req.CPUs, NestedVirt: req.NestedVirt})
	if err != nil {
		return nil, err
	}
	timing.Since(ctx, "hvf.new_vm", start)
	timingLog("hvf.StartContainer NewVM took=%s", time.Since(start))
	start = time.Now()

	memorySize := arm64vm.MemorySizeBytes(req.MemoryMB)
	mem, err := vm.MapAnonymousMemory(uintptr(memorySize), IPA(arm64vm.MemoryBase), hvMemoryRead|hvMemoryWrite|hvMemoryExec)
	if err != nil {
		vm.Close()
		return nil, fmt.Errorf("map guest memory: %w", err)
	}
	mmioRecorder := newSnapshotMMIORecorder()
	snapshot := newSnapshotTrigger(req.SnapshotDir, mem, mmioRecorder)
	timing.Since(ctx, "hvf.map_anonymous_memory", start)
	timingLog("hvf.StartContainer MapAnonymousMemory took=%s", time.Since(start))
	start = time.Now()

	serialOut := newSerialTranscript()
	var serialWriter io.Writer = serialOut
	var bootWriter *bootEventWriter
	if onEvent != nil && req.Dmesg {
		bootWriter = newBootEventWriter(onEvent)
		serialWriter = io.MultiWriter(serialOut, bootWriter)
		defer bootWriter.Close()
	}
	var consoleOut bytes.Buffer
	var fsTrace bytes.Buffer
	var runTrace bytes.Buffer
	var uart *serial.UART8250
	if req.Dmesg {
		uart = serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, serialWriter)
		uart.AttachIRQ(vm, arm64vm.UARTSPI)
	}
	console := virtio.NewConsole(arm64vm.ConsoleBase, arm64vm.ConsoleSize, arm64vm.ConsoleIRQ, &consoleOut)
	console.Attach(vm, vm)
	rng := virtio.NewRNG(arm64vm.RNGBase, arm64vm.RNGSize, arm64vm.RNGIRQ)
	rng.Attach(vm, vm)
	balloon := virtio.NewBalloon(arm64vm.BalloonBase, arm64vm.BalloonSize, arm64vm.BalloonIRQ)
	balloon.Attach(vm, vm)
	if targetPages := balloonTargetPages(req.BalloonMB); targetPages != 0 {
		if err := balloon.SetTargetPages(targetPages); err != nil {
			vm.Close()
			return nil, fmt.Errorf("set balloon target: %w", err)
		}
	}
	var netdev *virtio.Net
	if req.NetDevice != nil {
		netdev = req.NetDevice
		netdev.Attach(vm, vm)
	}
	vsockBackend := virtio.NewSimpleVsockBackend()
	var desktop *virtio.Desktop
	var displayDevices []virtio.MMIODevice
	var displayGPU *virtio.GPU
	displayGPUOwned := true
	defer func() {
		if displayGPUOwned && displayGPU != nil {
			_ = displayGPU.Close()
		}
	}()
	var clipboardListener virtio.VsockListener
	var displayListener virtio.VsockListener
	if req.DisplayWidth != 0 || req.DisplayHeight != 0 {
		framebuffer, err := virtio.NewFramebuffer(int(req.DisplayWidth), int(req.DisplayHeight))
		if err != nil {
			vm.Close()
			return nil, fmt.Errorf("create display: %w", err)
		}
		gpu := virtio.NewGPU(arm64vm.GPUBase, arm64vm.GPUSize, arm64vm.GPUIRQ, framebuffer)
		if req.Accelerated3D {
			renderer, err := virgl.NewHostRendererWithShareGroup(req.OpenGLShareContext, req.OpenGLSharePixelFormat)
			if err != nil {
				vm.Close()
				return nil, fmt.Errorf("create experimental VirGL renderer: %w", err)
			}
			gpu = virtio.NewGPUWithRenderer(arm64vm.GPUBase, arm64vm.GPUSize, arm64vm.GPUIRQ, framebuffer, renderer)
		}
		displayGPU = gpu
		keyboard := virtio.NewKeyboardInput(arm64vm.KeyboardBase, arm64vm.KeyboardSize, arm64vm.KeyboardIRQ)
		pointer := virtio.NewAbsolutePointerInput(arm64vm.PointerBase, arm64vm.PointerSize, arm64vm.PointerIRQ, req.DisplayWidth, req.DisplayHeight)
		clipboard := virtio.NewClipboard()
		clipboardListener, err = vsockBackend.Listen(vmruntime.ClipboardPort)
		if err != nil {
			vm.Close()
			return nil, fmt.Errorf("listen for guest clipboard bridge: %w", err)
		}
		displayListener, err = vsockBackend.Listen(vmruntime.DisplayPort)
		if err != nil {
			_ = clipboardListener.Close()
			vm.Close()
			return nil, fmt.Errorf("listen for guest display bridge: %w", err)
		}
		desktop = &virtio.Desktop{
			Framebuffer: framebuffer,
			GPU:         gpu,
			Keyboard:    keyboard,
			Pointer:     pointer,
			Clipboard:   clipboard,
		}
		displayDevices = []virtio.MMIODevice{gpu, keyboard, pointer}
	}
	displayListenersOwned := true
	defer func() {
		if !displayListenersOwned {
			return
		}
		if clipboardListener != nil {
			_ = clipboardListener.Close()
		}
		if displayListener != nil {
			_ = displayListener.Close()
		}
	}()
	listener, err := vsockBackend.Listen(vmruntime.ControlPort)
	if err != nil {
		vm.Close()
		return nil, fmt.Errorf("listen vsock control: %w", err)
	}
	vsock := virtio.NewVsock(arm64vm.VsockBase, arm64vm.VsockSize, arm64vm.VsockIRQ, vmruntime.GuestCID, vsockBackend)
	vsock.Attach(vm, vm)
	for _, device := range displayDevices {
		switch typed := device.(type) {
		case *virtio.GPU:
			typed.Attach(vm, vm)
		case *virtio.Input:
			typed.Attach(vm, vm)
		}
	}
	fsdevs, rootFS, err := arm64vm.BuildFSDevices(req, &fsTrace)
	if err != nil {
		_ = listener.Close()
		vm.Close()
		return nil, err
	}
	mountsOwned = false
	fsDevicesOwned := true
	defer func() {
		if fsDevicesOwned {
			for _, device := range fsdevs {
				if device != nil {
					_ = device.Close()
				}
			}
		}
	}()
	attachFSDeviceTiming(ctx, fsdevs)
	if strings.TrimSpace(req.SnapshotDir) != "" {
		for _, fsdev := range fsdevs {
			fsdev.Async = false
		}
	}
	for _, fsdev := range fsdevs {
		fsdev.Attach(vm, vm)
	}
	snapshot.setDevices(snapshotDevices{
		console: console,
		rng:     rng,
		balloon: balloon,
		vsock:   vsock,
		fsdevs:  fsdevs,
		netdev:  netdev,
	})
	timing.Since(ctx, "hvf.device_setup", start)
	timingLog("hvf.StartContainer device setup took=%s fsdevs=%d", time.Since(start), len(fsdevs))
	start = time.Now()

	deviceNodes := appendContainerDeviceNodes(console, rng, balloon, vsock, netdev)
	if desktop != nil {
		deviceNodes = append(deviceNodes,
			desktop.GPU.DeviceTreeNode(),
			desktop.Keyboard.DeviceTreeNode(),
			desktop.Pointer.DeviceTreeNode(),
		)
	}
	deviceNodes = append(deviceNodes, arm64vm.SnapshotDeviceNode())
	plan, err := arm64vm.PrepareBoot(mem, req.Kernel, initrd, arm64vm.BootConfig{
		MemoryMB:    req.MemoryMB,
		NumCPUs:     req.CPUs,
		Dmesg:       req.Dmesg,
		DisableUART: !req.Dmesg,
		ExtraNodes:  arm64vm.AppendFSNodes(deviceNodes, fsdevs),
		RecordTime: func(name string, duration time.Duration) {
			timing.Record(ctx, "hvf.prepare_boot."+name, duration)
		},
	})
	if err != nil {
		vm.Close()
		return nil, fmt.Errorf("prepare boot: %w", err)
	}
	timing.Since(ctx, "hvf.prepare_boot", start)
	timingLog("hvf.StartContainer PrepareBoot took=%s", time.Since(start))
	start = time.Now()

	if err := vm.ConfigureLinuxBootState(plan.EntryGPA, plan.StackTopGPA, plan.DeviceTreeGPA); err != nil {
		vm.Close()
		return nil, err
	}
	timing.Since(ctx, "hvf.register_setup", start)
	timingLog("hvf.StartContainer register setup took=%s", time.Since(start))
	start = time.Now()

	runCtx, cancel := context.WithCancel(context.Background())
	if clipboardListener != nil {
		go serveClipboardConnections(runCtx, clipboardListener, desktop.Clipboard)
	}
	if displayListener != nil {
		go serveDisplayConnections(runCtx, displayListener, desktop)
	}
	readyCh := make(chan error, 1)
	doneCh := make(chan sessionRunResult, 1)
	closeDone := make(chan struct{})
	controlTranscript := newSerialTranscript()
	controlAcceptCh := make(chan readyResult, 1)
	controlConnCh := make(chan readyResult, 1)
	activeExecs := &atomic.Int32{}
	guestReady := &atomic.Bool{}
	sendReady := func(err error) {
		select {
		case readyCh <- err:
		default:
		}
	}

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			controlAcceptCh <- readyResult{err: err}
			return
		}
		go func() {
			_, _ = io.Copy(controlTranscript, conn)
		}()
		controlAcceptCh <- readyResult{conn: conn}
	}()

	go func() {
		select {
		case res := <-controlAcceptCh:
			if res.err != nil {
				sendReady(res.err)
				return
			}
			text, err := controlTranscript.WaitFor(runCtx, 0, func(text string) bool {
				return strings.Contains(text, instanceReadyMarker)
			})
			if err != nil {
				_ = res.conn.Close()
				sendReady(err)
				return
			}
			if initMS, ok := parseInitDurationMarker(text); ok {
				totalMS := int(time.Since(start).Milliseconds())
				kernelMS := totalMS - initMS
				if kernelMS < 0 {
					kernelMS = 0
				}
				timingLog("hvf.StartContainer kernel-to-init=%dms init=%dms", kernelMS, initMS)
			} else {
				timingLog("hvf.StartContainer init duration marker missing")
			}
			timingLog("hvf.StartContainer guest ready marker took=%s", time.Since(start))
			guestReady.Store(true)
			controlConnCh <- res
			sendReady(nil)
		case <-runCtx.Done():
			sendReady(runCtx.Err())
		}
	}()

	if req.Dmesg {
		go func() {
			text, err := serialOut.WaitFor(runCtx, 0, hasFatalBootText)
			if err != nil || guestReady.Load() {
				return
			}
			sendReady(fmt.Errorf("guest reported boot failure\nserial:\n%s", text))
			cancel()
		}()
	}

	go func() {
		if onEvent != nil {
			_ = onEvent(client.BootEvent{Kind: "status", Message: "waiting for guest to boot"})
		}
	}()

	var fsCloseErr error
	fsDevicesOwned = false
	go func() {
		defer close(closeDone)
		defer func() {
			_ = vm.Close()
			exitTiming.Dump()
		}()
		defer func() {
			var errs []error
			for _, device := range fsdevs {
				if device != nil {
					errs = append(errs, device.Close())
				}
			}
			fsCloseErr = errors.Join(errs...)
		}()
		defer func() {
			_ = vsock.Close()
		}()
		defer cancel()
		runner := newVMRunManager(vm)
		for {
			active := activeExecs.Load() > 0
			runSlice := persistentRunSlice(guestReady.Load(), active)
			runStart := time.Now()
			runRes, err, stalled := runner.Run(runCtx, runSlice)
			timing.Since(ctx, "hvf.run_loop.run_with_cancel", runStart)
			if stalled {
				if active {
					recordCount(timing.FromContext(ctx), "hvf.run_loop.stalled_active_exec", 1)
					timing.Record(ctx, "hvf.run_loop.active_exec_stall_slice", runSlice)
				} else {
					recordCount(timing.FromContext(ctx), "hvf.run_loop.stalled_idle", 1)
				}
				if runCtx.Err() != nil {
					doneCh <- sessionRunResult{err: runCtx.Err()}
					return
				}
				continue
			}
			if err != nil {
				doneCh <- sessionRunResult{err: fmt.Errorf("%w\nrun:\n%sserial:\n%s\nvirtio-fs:\n%s", err, runTrace.String(), serialOut.String(), fsTrace.String())}
				return
			}
			if runRes == nil || runRes.exit == nil {
				doneCh <- sessionRunResult{err: fmt.Errorf("vcpu returned nil exit info")}
				return
			}
			exitInfo := runRes.exit
			vcpuIndex := runRes.index
			if exitInfo.Reason == hvExitReasonVTimerActivated {
				start := time.Now()
				if err := injectVirtualTimerPPI(vm, vcpuIndex); err != nil {
					doneCh <- sessionRunResult{err: fmt.Errorf("inject virtual timer ppi: %w", err)}
					return
				}
				exitTiming.Record("vtimer.inject_ppi", start)
				continue
			}
			if exitInfo.Reason == hvExitReasonCanceled {
				exitTiming.Record("cancelled", time.Now())
				// HVF can occasionally surface a canceled run even when we did not
				// explicitly cancel the vCPU slice. Treat it like a retry instead of
				// tearing down the persistent guest during startup.
				continue
			}
			if exitInfo.Reason != hvExitReasonException {
				doneCh <- sessionRunResult{err: fmt.Errorf("unexpected exit reason %v", exitInfo.Reason)}
				return
			}
			switch DecodeExceptionClass(exitInfo.Exception.Syndrome) {
			case ExceptionClassDataAbortLowerEL:
				if err := handleContainerDataAbort(ctx, vm, vcpuIndex, uart, console, rng, balloon, fsdevs, vsock, netdev, displayDevices, snapshot, mmioRecorder, exitInfo); err != nil {
					doneCh <- sessionRunResult{err: err}
					return
				}
			case ExceptionClassSystemRegister:
				start := time.Now()
				handled, err := vm.HandleSystemInstructionForVCPU(vcpuIndex, exitInfo.Exception.Syndrome)
				exitTiming.Record("system_register", start)
				if err != nil {
					doneCh <- sessionRunResult{err: err}
					return
				}
				if !handled {
					pc, _ := vm.GetProgramCounterForVCPU(vcpuIndex)
					info, _ := DecodeSystemInstruction(exitInfo.Exception.Syndrome)
					doneCh <- sessionRunResult{err: fmt.Errorf("unsupported system instruction trap pc=%#x syndrome=%#x op0=%d op1=%d op2=%d crn=%d crm=%d rt=%d read=%t\nserial:\n%s\nvirtio-fs:\n%s",
						pc, exitInfo.Exception.Syndrome, info.Op0, info.Op1, info.Op2, info.CRn, info.CRm, info.RawRt, info.Read, serialOut.String(), fsTrace.String())}
					return
				}
			case ExceptionClassHVC64:
				start := time.Now()
				halt, err := handleContainerHVC(vm, vcpuIndex)
				exitTiming.Record("hvc.psci", start)
				if err != nil {
					doneCh <- sessionRunResult{err: err}
					return
				}
				if halt {
					doneCh <- sessionRunResult{}
					return
				}
			default:
				pc, _ := vm.GetProgramCounterForVCPU(vcpuIndex)
				doneCh <- sessionRunResult{err: fmt.Errorf("unexpected exception class %#x pc=%#x syndrome=%#x physical=%#x\nserial:\n%s\nvirtio-fs:\n%s",
					DecodeExceptionClass(exitInfo.Exception.Syndrome), pc, exitInfo.Exception.Syndrome, uint64(exitInfo.Exception.PhysicalAddress), serialOut.String(), fsTrace.String())}
				return
			}
		}
	}()

	select {
	case err := <-readyCh:
		timing.Since(ctx, "hvf.wait_guest_ready", start)
		if err != nil {
			cancel()
			_ = listener.Close()
			res := <-doneCh
			<-closeDone
			if res.err != nil {
				return nil, res.err
			}
			if req.Dmesg && serialOut.Len() > 0 {
				return nil, fmt.Errorf("%w\nserial:\n%s", err, serialOut.String())
			}
			return nil, err
		}
		res, ok := <-controlConnCh
		if !ok || res.err != nil || res.conn == nil {
			cancel()
			_ = listener.Close()
			resDone := <-doneCh
			<-closeDone
			if resDone.err != nil {
				return nil, resDone.err
			}
			if res.err != nil {
				return nil, res.err
			}
			return nil, fmt.Errorf("guest control connection became ready without an accepted vsock connection")
		}
		_ = listener.Close()
		timingLog("hvf.StartContainer total ready=%s", time.Since(start))
		shareState := make(map[string]client.ShareMount, len(req.Shares))
		for _, share := range req.Shares {
			shareState[strings.TrimSpace(share.Mount)] = client.ShareMount{
				Source:   share.Source,
				Mount:    share.Mount,
				Writable: share.Writable,
				MapOwner: share.MapOwner,
				OwnerUID: share.OwnerUID,
				OwnerGID: share.OwnerGID,
				Cache:    share.Cache,
			}
		}
		displayListenersOwned = false
		displayGPUOwned = false
		return &ContainerSession{
			cancel:            cancel,
			runCtx:            runCtx,
			doneCh:            doneCh,
			closeDone:         closeDone,
			image:             req.Image,
			baseEnv:           baseEnv,
			workDir:           workDir,
			dmesg:             req.Dmesg,
			control:           res.conn,
			transcript:        controlTranscript,
			serialOut:         serialOut,
			clipboardListener: clipboardListener,
			displayListener:   displayListener,
			vsock:             vsock,
			balloon:           balloon,
			desktop:           desktop,
			rootFS:            rootFS,
			fsdevs:            fsdevs,
			fsCloseErr:        &fsCloseErr,
			shares:            shareState,
			activeExecs:       activeExecs,
		}, nil
	case res := <-doneCh:
		cancel()
		_ = listener.Close()
		<-closeDone
		if res.err != nil {
			return nil, res.err
		}
		return nil, fmt.Errorf("guest exited before control connection became ready")
	case <-ctx.Done():
		cancel()
		_ = listener.Close()
		res := <-doneCh
		<-closeDone
		if res.err != nil {
			return nil, res.err
		}
		if req.Dmesg && serialOut.Len() > 0 {
			return nil, fmt.Errorf("%w\nserial:\n%s", ctx.Err(), serialOut.String())
		}
		return nil, ctx.Err()
	}
}

func persistentRunSlice(ready bool, active bool) time.Duration {
	switch {
	case !ready:
		return 5 * time.Second
	case active:
		return 5 * time.Millisecond
	default:
		return 250 * time.Millisecond
	}
}

func appendContainerDeviceNodes(console *virtio.Console, rng *virtio.RNG, balloon *virtio.Balloon, vsock *virtio.Vsock, netdev *virtio.Net) []fdt.Node {
	nodes := []fdt.Node{console.DeviceTreeNode(), rng.DeviceTreeNode()}
	if balloon != nil {
		nodes = append(nodes, balloon.DeviceTreeNode())
	}
	if vsock != nil {
		nodes = append(nodes, vsock.DeviceTreeNode())
	}
	if netdev != nil {
		nodes = append(nodes, netdev.DeviceTreeNode())
	}
	return nodes
}

func balloonTargetPages(mb uint64) uint32 {
	if mb == 0 {
		return 0
	}
	pages := mb * 1024 * 1024 / 4096
	if pages > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(pages)
}

func RunContainer(ctx context.Context, req ContainerRunRequest) (ContainerRunResult, error) {
	return runContainer(ctx, req, nil)
}

func runContainer(ctx context.Context, req ContainerRunRequest, readyCh chan<- error) (ret ContainerRunResult, retErr error) {
	mountsOwned := true
	defer func() {
		if mountsOwned {
			retErr = errors.Join(retErr, vmruntime.CloseShareMounts(req.Mounts))
		}
	}()
	if req.Image == nil && req.RootFS == nil {
		return ContainerRunResult{}, fmt.Errorf("image or rootfs backend is required")
	}
	if len(req.Kernel) == 0 {
		return ContainerRunResult{}, fmt.Errorf("kernel is required")
	}
	if req.CPUs <= 0 {
		req.CPUs = 1
	}
	user := strings.TrimSpace(req.User)
	if user == "" && req.Image != nil {
		user = strings.TrimSpace(req.Image.Config.User)
	}
	if err := validateGuestUser(user); err != nil {
		return ContainerRunResult{}, err
	}

	var command []string
	switch {
	case req.Image != nil:
		command = req.Image.Command(req.Command)
		if len(command) == 0 {
			command = []string{"/bin/sh"}
		}
	default:
		command = append([]string(nil), req.Command...)
		if len(command) == 0 {
			return ContainerRunResult{}, fmt.Errorf("command is required when running without an image")
		}
	}
	if len(req.Init) == 0 {
		return ContainerRunResult{}, fmt.Errorf("guest init binary is required")
	}

	workDir := req.WorkDir
	if workDir == "" && req.Image != nil {
		workDir = req.Image.Config.WorkingDir
	}
	if workDir == "" {
		workDir = "/"
	}
	if !strings.HasPrefix(workDir, "/") {
		return ContainerRunResult{}, fmt.Errorf("workdir must be absolute")
	}

	var baseEnv []string
	if req.Image != nil {
		baseEnv = req.Image.Config.Env
	}
	env := vmruntime.WithDefaultEnv(vmruntime.MergeEnv(baseEnv, req.Env))

	var err error
	if req.Image != nil {
		command, err = imagefs.ResolveCommand(req.Image.RootFS, command, env)
		if err != nil {
			return ContainerRunResult{}, err
		}
	}
	initrd, err := arm64vm.BuildExecInitramfs(req, command, env, workDir)
	if err != nil {
		return ContainerRunResult{}, fmt.Errorf("build initramfs: %w", err)
	}

	vm, err := NewVMWithOptions(ctx, VMOptions{CPUs: req.CPUs, NestedVirt: req.NestedVirt})
	if err != nil {
		return ContainerRunResult{}, err
	}
	defer func() {
		if err := vm.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()

	memorySize := arm64vm.MemorySizeBytes(req.MemoryMB)
	mem, err := vm.MapAnonymousMemory(uintptr(memorySize), IPA(arm64vm.MemoryBase), hvMemoryRead|hvMemoryWrite|hvMemoryExec)
	if err != nil {
		return ContainerRunResult{}, fmt.Errorf("map guest memory: %w", err)
	}

	var serialOut bytes.Buffer
	var consoleOut bytes.Buffer
	var fsTrace bytes.Buffer
	var runTrace bytes.Buffer
	uart := serial.NewUART8250(arm64vm.DefaultUARTBase, arm64vm.DefaultUARTRegShift, &serialOut)
	uart.AttachIRQ(vm, arm64vm.UARTSPI)
	console := virtio.NewConsole(arm64vm.ConsoleBase, arm64vm.ConsoleSize, arm64vm.ConsoleIRQ, &consoleOut)
	console.Attach(vm, vm)
	rng := virtio.NewRNG(arm64vm.RNGBase, arm64vm.RNGSize, arm64vm.RNGIRQ)
	rng.Attach(vm, vm)
	balloon := virtio.NewBalloon(arm64vm.BalloonBase, arm64vm.BalloonSize, arm64vm.BalloonIRQ)
	balloon.Attach(vm, vm)
	if targetPages := balloonTargetPages(req.BalloonMB); targetPages != 0 {
		if err := balloon.SetTargetPages(targetPages); err != nil {
			return ContainerRunResult{}, fmt.Errorf("set balloon target: %w", err)
		}
	}
	var netdev *virtio.Net
	if req.NetDevice != nil {
		netdev = req.NetDevice
		netdev.Attach(vm, vm)
	}
	var vsock *virtio.Vsock
	fsdevs, _, err := arm64vm.BuildFSDevices(req, &fsTrace)
	if err != nil {
		return ContainerRunResult{}, err
	}
	mountsOwned = false
	defer func() {
		var errs []error
		for _, device := range fsdevs {
			if device != nil {
				errs = append(errs, device.Close())
			}
		}
		retErr = errors.Join(retErr, errors.Join(errs...))
	}()
	attachFSDeviceTiming(ctx, fsdevs)
	for _, fsdev := range fsdevs {
		fsdev.Attach(vm, vm)
	}

	plan, err := arm64vm.PrepareBoot(mem, req.Kernel, initrd, arm64vm.BootConfig{
		MemoryMB:   req.MemoryMB,
		NumCPUs:    req.CPUs,
		Dmesg:      true,
		ExtraNodes: arm64vm.AppendFSNodes(appendContainerDeviceNodes(console, rng, balloon, vsock, netdev), fsdevs),
		RecordTime: func(name string, duration time.Duration) {
			timing.Record(ctx, "hvf.prepare_boot."+name, duration)
		},
	})
	if err != nil {
		return ContainerRunResult{}, fmt.Errorf("prepare boot: %w", err)
	}

	if err := vm.ConfigureLinuxBootState(plan.EntryGPA, plan.StackTopGPA, plan.DeviceTreeGPA); err != nil {
		return ContainerRunResult{}, err
	}

	readySent := false
	stallSamples := 0
	var lastSamplePC uint64
	var lastSampleCPSR uint64
	var pendingResult *ContainerRunResult
	includeTraceOnExit := os.Getenv("CCX3_DEBUG_VIRTIOFS") != ""
	captureResult := func() string {
		transcript := commandTranscript(req.Dmesg, &serialOut, &consoleOut)
		if pendingResult == nil {
			if exitCode, output, ok := extractCommandResult(transcript, req.Dmesg); ok {
				resultTranscript := transcript
				if includeTraceOnExit {
					resultTranscript = resultTranscript + "\n[virtio-fs trace]\n" + fsTrace.String()
				}
				pendingResult = &ContainerRunResult{
					ExitCode:   exitCode,
					Output:     output,
					Transcript: resultTranscript,
				}
			}
		}
		return transcript
	}
	runner := newVMRunManager(vm)
	for {
		runRes, err, stalled := runner.Run(ctx, 5*time.Second)
		if stalled {
			pc, _ := vm.GetReg(hvRegPC)
			cpsr, _ := vm.GetReg(hvRegCPSR)
			if stallSamples < 16 && (stallSamples == 0 || pc != lastSamplePC || cpsr != lastSampleCPSR) {
				fmt.Fprintf(&runTrace, "stall pc=%#x cpsr=%#x transcript_len=%d\n", pc, cpsr, serialOut.Len())
				lastSamplePC = pc
				lastSampleCPSR = cpsr
				stallSamples++
			}
			if ctx.Err() != nil {
				return ContainerRunResult{}, fmt.Errorf("%w\npc=%#x cpsr=%#x\nrun:\n%sserial:\n%s\nvirtio-fs:\n%s", ctx.Err(), pc, cpsr, runTrace.String(), serialOut.String(), fsTrace.String())
			}
			continue
		}
		if err != nil {
			return ContainerRunResult{}, fmt.Errorf("%w\nrun:\n%sserial:\n%s\nvirtio-fs:\n%s", err, runTrace.String(), serialOut.String(), fsTrace.String())
		}

		transcript := captureResult()
		if !readySent && strings.Contains(transcript, commandBeginMarker) {
			readySent = true
			if readyCh != nil {
				readyCh <- nil
				close(readyCh)
				readyCh = nil
			}
		}
		if pendingResult != nil && ctx.Err() != nil {
			return *pendingResult, nil
		}

		if runRes == nil || runRes.exit == nil {
			return ContainerRunResult{}, fmt.Errorf("vcpu returned nil exit info")
		}
		exitInfo := runRes.exit
		vcpuIndex := runRes.index
		if exitInfo.Reason == hvExitReasonVTimerActivated {
			start := time.Now()
			if err := injectVirtualTimerPPI(vm, vcpuIndex); err != nil {
				return ContainerRunResult{}, fmt.Errorf("inject virtual timer ppi: %w", err)
			}
			exitTiming.Record("vtimer.inject_ppi", start)
			continue
		}
		if exitInfo.Reason == hvExitReasonCanceled {
			exitTiming.Record("cancelled", time.Now())
			continue
		}
		if exitInfo.Reason != hvExitReasonException {
			return ContainerRunResult{}, fmt.Errorf("unexpected exit reason %v", exitInfo.Reason)
		}

		switch DecodeExceptionClass(exitInfo.Exception.Syndrome) {
		case ExceptionClassDataAbortLowerEL:
			if err := handleContainerDataAbort(ctx, vm, vcpuIndex, uart, console, rng, balloon, fsdevs, vsock, netdev, nil, nil, nil, exitInfo); err != nil {
				return ContainerRunResult{}, err
			}
		case ExceptionClassSystemRegister:
			start := time.Now()
			handled, err := vm.HandleSystemInstructionForVCPU(vcpuIndex, exitInfo.Exception.Syndrome)
			exitTiming.Record("system_register", start)
			if err != nil {
				return ContainerRunResult{}, err
			}
			if !handled {
				pc, _ := vm.GetProgramCounterForVCPU(vcpuIndex)
				info, _ := DecodeSystemInstruction(exitInfo.Exception.Syndrome)
				return ContainerRunResult{}, fmt.Errorf("unsupported system instruction trap pc=%#x syndrome=%#x op0=%d op1=%d op2=%d crn=%d crm=%d rt=%d read=%t\nrun:\n%sserial:\n%s\nvirtio-fs:\n%s",
					pc, exitInfo.Exception.Syndrome, info.Op0, info.Op1, info.Op2, info.CRn, info.CRm, info.RawRt, info.Read, runTrace.String(), serialOut.String(), fsTrace.String())
			}
		case ExceptionClassHVC64:
			start := time.Now()
			halt, err := handleContainerHVC(vm, vcpuIndex)
			exitTiming.Record("hvc.psci", start)
			if err != nil {
				return ContainerRunResult{}, err
			}
			if halt {
				captureResult()
				if pendingResult != nil {
					return *pendingResult, nil
				}
				return ContainerRunResult{}, fmt.Errorf("guest halted before command completed\nserial:\n%s\nconsole:\n%s\nvirtio-fs:\n%s", serialOut.String(), consoleOut.String(), fsTrace.String())
			}
		default:
			pc, _ := vm.GetProgramCounterForVCPU(vcpuIndex)
			return ContainerRunResult{}, fmt.Errorf("unexpected exception class %#x pc=%#x syndrome=%#x physical=%#x\nrun:\n%sserial:\n%s\nvirtio-fs:\n%s",
				DecodeExceptionClass(exitInfo.Exception.Syndrome), pc, exitInfo.Exception.Syndrome, uint64(exitInfo.Exception.PhysicalAddress), runTrace.String(), serialOut.String(), fsTrace.String())
		}
	}
}

type vmRunManager struct {
	vm      *VM
	resCh   chan runResultVM
	mu      sync.Mutex
	running map[int]bool
}

func newVMRunManager(vm *VM) *vmRunManager {
	return &vmRunManager{
		vm:      vm,
		resCh:   make(chan runResultVM, len(vm.vcpus)*2),
		running: make(map[int]bool),
	}
}

func (r *vmRunManager) Run(ctx context.Context, timeout time.Duration) (*runResultVM, error, bool) {
	r.startRunnableVCPUs()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-r.resCh:
		r.markStopped(res.index)
		return &res, res.err, false
	case <-ctx.Done():
		if err := r.vm.CancelRun(); err != nil {
			return nil, err, false
		}
		return nil, ctx.Err(), false
	case <-timer.C:
		if err := r.vm.CancelRun(); err != nil {
			return nil, err, false
		}
		return nil, nil, true
	}
}

func (r *vmRunManager) startRunnableVCPUs() {
	for _, index := range r.vm.activeVCPUIndexes() {
		r.mu.Lock()
		if r.running[index] {
			r.mu.Unlock()
			continue
		}
		r.running[index] = true
		r.mu.Unlock()
		go func(index int) {
			exitInfo, err := r.vm.RunVCPU(index)
			r.resCh <- runResultVM{index: index, exit: exitInfo, err: err}
		}(index)
	}
}

func (r *vmRunManager) markStopped(index int) {
	r.mu.Lock()
	delete(r.running, index)
	r.mu.Unlock()
}

type runResultVM struct {
	index int
	exit  *VcpuExit
	err   error
}

func handleContainerDataAbort(ctx context.Context, vm *VM, vcpuIndex int, uart *serial.UART8250, console *virtio.Console, rng *virtio.RNG, balloon *virtio.Balloon, fsdevs []*virtio.FS, vsock *virtio.Vsock, netdev *virtio.Net, extra []virtio.MMIODevice, snapshot *snapshotTrigger, mmioRecorder *snapshotMMIORecorder, exitInfo *VcpuExit) error {
	recorder := timing.FromContext(ctx)
	if recorder != nil {
		totalStart := time.Now()
		defer func() {
			recorder.Record("hvf.data_abort.total", time.Since(totalStart))
		}()
	}
	info, err := DecodeDataAbort(exitInfo.Exception.Syndrome)
	if err != nil {
		return err
	}
	addr := uint64(exitInfo.Exception.PhysicalAddress)
	fsdev := findFSDevice(fsdevs, addr, info.SizeBytes)
	extraDevice := findMMIODevice(extra, addr, info.SizeBytes)
	exitTimingEnabled := exitTiming.Enabled()
	writeValue := uint64(0)
	if info.Write {
		var err error
		writeValue, err = readAbortValue(vm, vcpuIndex, info)
		if err != nil {
			return err
		}
		mmioRecorder.record(addr, info.SizeBytes, writeValue)
	}

	switch {
	case snapshot != nil && snapshot.contains(addr, info.SizeBytes):
		return snapshot.handleDataAbort(vm, vcpuIndex, info, writeValue)
	case uart != nil && uart.Contains(addr, info.SizeBytes):
		if info.Write {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.uart.write", start)
			}
			if err := uart.WriteValue(addr, info.SizeBytes, writeValue); err != nil {
				return err
			}
		} else {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.uart.read", start)
			}
			value, err := uart.ReadValue(addr, info.SizeBytes)
			if err != nil {
				return err
			}
			if err := writeAbortValue(vm, vcpuIndex, info, value); err != nil {
				return err
			}
		}
	case console != nil && console.Contains(addr, info.SizeBytes):
		if info.Write {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_console.write", start)
			}
			if err := console.Write(addr, info.SizeBytes, writeValue); err != nil {
				return err
			}
		} else {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_console.read", start)
			}
			value, err := console.Read(addr, info.SizeBytes)
			if err != nil {
				return err
			}
			if err := writeAbortValue(vm, vcpuIndex, info, value); err != nil {
				return err
			}
		}
	case rng != nil && rng.Contains(addr, info.SizeBytes):
		if info.Write {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_rng.write", start)
			}
			if err := rng.Write(addr, info.SizeBytes, writeValue); err != nil {
				return err
			}
		} else {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_rng.read", start)
			}
			value, err := rng.Read(addr, info.SizeBytes)
			if err != nil {
				return err
			}
			if err := writeAbortValue(vm, vcpuIndex, info, value); err != nil {
				return err
			}
		}
	case balloon != nil && balloon.Contains(addr, info.SizeBytes):
		if info.Write {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_balloon.write", start)
			}
			if err := balloon.Write(addr, info.SizeBytes, writeValue); err != nil {
				return err
			}
		} else {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_balloon.read", start)
			}
			value, err := balloon.Read(addr, info.SizeBytes)
			if err != nil {
				return err
			}
			if err := writeAbortValue(vm, vcpuIndex, info, value); err != nil {
				return err
			}
		}
	case fsdev != nil:
		var fsStart time.Time
		if recorder != nil {
			fsStart = time.Now()
		}
		if exitTimingEnabled {
			start := time.Now()
			if info.Write {
				defer exitTiming.Record("data_abort.virtiofs.write", start)
			} else {
				defer exitTiming.Record("data_abort.virtiofs.read", start)
			}
		}
		if err := handleFSDataAbort(vm, vcpuIndex, fsdev, addr, info); err != nil {
			return err
		}
		if recorder != nil {
			recorder.Record("hvf.data_abort.virtio_fs", time.Since(fsStart))
		}
	case vsock != nil && vsock.Contains(addr, info.SizeBytes):
		if info.Write {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_vsock.write", start)
			}
			if err := vsock.Write(addr, info.SizeBytes, writeValue); err != nil {
				return err
			}
		} else {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_vsock.read", start)
			}
			value, err := vsock.Read(addr, info.SizeBytes)
			if err != nil {
				return err
			}
			if err := writeAbortValue(vm, vcpuIndex, info, value); err != nil {
				return err
			}
		}
	case netdev != nil && netdev.Contains(addr, info.SizeBytes):
		if info.Write {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_net.write", start)
			}
			if err := netdev.Write(addr, info.SizeBytes, writeValue); err != nil {
				return err
			}
		} else {
			if exitTimingEnabled {
				start := time.Now()
				defer exitTiming.Record("data_abort.virtio_net.read", start)
			}
			value, err := netdev.Read(addr, info.SizeBytes)
			if err != nil {
				return err
			}
			if err := writeAbortValue(vm, vcpuIndex, info, value); err != nil {
				return err
			}
		}
	case extraDevice != nil:
		if info.Write {
			if err := extraDevice.Write(addr, info.SizeBytes, writeValue); err != nil {
				return err
			}
		} else {
			value, err := extraDevice.Read(addr, info.SizeBytes)
			if err != nil {
				return err
			}
			if err := writeAbortValue(vm, vcpuIndex, info, value); err != nil {
				return err
			}
		}
	case mmioInRange(addr, arm64vm.GICDistributorMin, arm64vm.GICDistributorMax) || mmioInRange(addr, arm64vm.GICRedistributorMin, arm64vm.GICRedistributorMax):
		if exitTimingEnabled {
			start := time.Now()
			if info.Write {
				defer exitTiming.Record("data_abort.gic.write", start)
			} else {
				defer exitTiming.Record("data_abort.gic.read", start)
			}
		}
		value, err := handleGICAccess(vm, vcpuIndex, addr, info)
		if err != nil {
			return err
		}
		if !info.Write {
			if err := writeAbortValue(vm, vcpuIndex, info, value); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unhandled MMIO access addr=%#x size=%d write=%v", addr, info.SizeBytes, info.Write)
	}

	return vm.AdvanceProgramCounterForVCPU(vcpuIndex)
}

func attachFSDeviceTiming(ctx context.Context, fsdevs []*virtio.FS) {
	recorder := timing.FromContext(ctx)
	if recorder == nil && !exitTiming.Enabled() {
		return
	}
	for _, fsdev := range fsdevs {
		if fsdev == nil {
			continue
		}
		fsdev.RecordTiming = func(name string, duration time.Duration) {
			if recorder != nil {
				recorder.Record(name, duration)
			}
			exitTiming.RecordDuration(name, duration)
		}
	}
}

func findFSDevice(fsdevs []*virtio.FS, addr uint64, size int) *virtio.FS {
	for _, fsdev := range fsdevs {
		if fsdev != nil && fsdev.Contains(addr, size) {
			return fsdev
		}
	}
	return nil
}

func findMMIODevice(devices []virtio.MMIODevice, addr uint64, size int) virtio.MMIODevice {
	for _, device := range devices {
		if device != nil && device.Contains(addr, size) {
			return device
		}
	}
	return nil
}

func handleFSDataAbort(vm *VM, vcpuIndex int, fsdev *virtio.FS, addr uint64, info DataAbortInfo) error {
	if info.Write {
		value, err := readAbortValue(vm, vcpuIndex, info)
		if err != nil {
			return err
		}
		return fsdev.Write(addr, info.SizeBytes, value)
	}
	value, err := fsdev.Read(addr, info.SizeBytes)
	if err != nil {
		return err
	}
	return writeAbortValue(vm, vcpuIndex, info, value)
}

func handleContainerHVC(vm *VM, vcpuIndex int) (bool, error) {
	x0, err := vm.GetRegForVCPU(vcpuIndex, hvRegX0)
	if err != nil {
		return false, err
	}

	const (
		psciVersion         = 0x84000000
		psciCpuSuspend      = 0x84000001
		psciCpuOff          = 0x84000002
		psciCpuOn           = 0x84000003
		psciAffinityInfo    = 0x84000004
		psciCpuOn64         = 0xc4000003
		psciAffinityInfo64  = 0xc4000004
		psciMigrateInfoType = 0x84000006
		psciSystemOff       = 0x84000008
		psciSystemReset     = 0x84000009
		psciFeatures        = 0x8400000a
		psciSuccess         = 0
		psciNotSupported    = 0xffffffff
		psciInvalidParams   = 0xfffffffe
		psciAlreadyOn       = 0xfffffffc
		psciTosNotPresent   = 2
		psciAffinityOff     = 1
	)

	var ret uint64
	switch x0 {
	case psciVersion:
		ret = 0x00010000
	case psciMigrateInfoType:
		ret = psciTosNotPresent
	case psciFeatures:
		ret = psciNotSupported
	case psciCpuSuspend:
		ret = psciNotSupported
	case psciCpuOff:
		ret = psciSuccess
	case psciAffinityInfo, psciAffinityInfo64:
		target, err := vm.GetRegForVCPU(vcpuIndex, hvRegX1)
		if err != nil {
			return false, err
		}
		on, err := vm.IsVCPUOnMPIDR(target)
		if err != nil {
			ret = psciInvalidParams
		} else if on {
			ret = psciSuccess
		} else {
			ret = psciAffinityOff
		}
	case psciCpuOn, psciCpuOn64:
		target, err := vm.GetRegForVCPU(vcpuIndex, hvRegX1)
		if err != nil {
			return false, err
		}
		entry, err := vm.GetRegForVCPU(vcpuIndex, hvRegX2)
		if err != nil {
			return false, err
		}
		contextID, err := vm.GetRegForVCPU(vcpuIndex, hvRegX3)
		if err != nil {
			return false, err
		}
		if on, err := vm.IsVCPUOnMPIDR(target); err == nil && on {
			ret = psciAlreadyOn
		} else if err := vm.StartVCPUByMPIDR(target, entry, contextID); err != nil {
			ret = psciInvalidParams
		} else {
			ret = psciSuccess
		}
	case psciSystemOff, psciSystemReset:
		return true, nil
	default:
		return false, fmt.Errorf("unsupported PSCI call %#x", x0)
	}

	return false, vm.SetRegForVCPU(vcpuIndex, hvRegX0, ret)
}

func readAbortValue(vm *VM, vcpuIndex int, info DataAbortInfo) (uint64, error) {
	if info.Target == hvRegXZR {
		return 0, nil
	}
	value, err := vm.GetRegForVCPU(vcpuIndex, info.Target)
	if err != nil {
		return 0, err
	}
	if info.SizeBytes >= 8 {
		return value, nil
	}
	return value & ((uint64(1) << (8 * info.SizeBytes)) - 1), nil
}

func writeAbortValue(vm *VM, vcpuIndex int, info DataAbortInfo, value uint64) error {
	if info.Target == hvRegXZR {
		return nil
	}
	if info.SizeBytes < 8 {
		value &= (uint64(1) << (8 * info.SizeBytes)) - 1
	}
	return vm.SetRegForVCPU(vcpuIndex, info.Target, value)
}

func mmioInRange(addr, start, end uint64) bool {
	return addr >= start && addr < end
}

func handleGICAccess(vm *VM, vcpuIndex int, addr uint64, info DataAbortInfo) (uint64, error) {
	var value uint64
	if info.Write {
		v, err := readAbortValue(vm, vcpuIndex, info)
		if err != nil {
			return 0, err
		}
		value = v
	}

	switch {
	case mmioInRange(addr, arm64vm.GICDistributorMin, arm64vm.GICDistributorMax):
		reg := GICDistributorReg(addr - arm64vm.GICDistributorMin)
		if info.Write {
			err := vm.SetGICDistributorReg(reg, value)
			if err != nil && strings.Contains(err.Error(), "denied") {
				return 0, nil
			}
			return 0, err
		}
		val, err := vm.GetGICDistributorReg(reg)
		if err != nil && strings.Contains(err.Error(), "denied") && reg == 0xffe8 {
			return 0x30, nil
		}
		return val, err
	case mmioInRange(addr, arm64vm.GICRedistributorMin, arm64vm.GICRedistributorMax):
		const redistStride = 0x20000
		redistIndex := int((addr - arm64vm.GICRedistributorMin) / redistStride)
		reg := GICRedistributorReg((addr - arm64vm.GICRedistributorMin) % redistStride)
		if info.Write {
			err := vm.SetGICRedistributorRegForVCPU(redistIndex, reg, value)
			if err != nil && (strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), "bad argument")) {
				return 0, nil
			}
			return 0, err
		}
		val, err := vm.GetGICRedistributorRegForVCPU(redistIndex, reg)
		if err != nil && (strings.Contains(err.Error(), "denied") || strings.Contains(err.Error(), "bad argument")) {
			switch reg {
			case 0x0:
				return 0, nil
			case 0xffe8:
				return 0x30, nil
			case 0x8:
				return 1 << 4, nil
			case 0x14:
				return 0, nil
			default:
				return 0, nil
			}
		}
		return val, err
	default:
		return 0, fmt.Errorf("address %#x outside GIC MMIO ranges", addr)
	}
}

func injectVirtualTimerPPI(vm *VM, vcpuIndex int) error {
	const (
		gicrISENABLER0 = GICRedistributorReg(0x10100)
		gicrISPENDR0   = GICRedistributorReg(0x10200)
		timerMask      = uint64(1) << arm64VirtualTimerPPI
	)

	enabled, err := vm.GetGICRedistributorRegForVCPU(vcpuIndex, gicrISENABLER0)
	if err == nil && enabled&timerMask == 0 {
		if err := vm.SetGICRedistributorRegForVCPU(vcpuIndex, gicrISENABLER0, enabled|timerMask); err != nil {
			return err
		}
	}

	pending, err := vm.GetGICRedistributorRegForVCPU(vcpuIndex, gicrISPENDR0)
	if err != nil {
		return err
	}
	if pending&timerMask != 0 {
		return nil
	}
	return vm.SetGICRedistributorRegForVCPU(vcpuIndex, gicrISPENDR0, timerMask)
}

func commandTranscript(dmesg bool, serialOut, consoleOut fmt.Stringer) string {
	if dmesg || serialOut.String() != "" {
		return serialOut.String()
	}
	return consoleOut.String()
}

func extractCommandResult(serial string, dmesg bool) (int, string, bool) {
	begin := strings.Index(serial, strings.TrimSuffix(commandBeginMarker, ":"))
	exit := strings.Index(serial, commandExitMarkerPref)
	if exit == -1 || (begin != -1 && exit < begin) {
		return 0, "", false
	}

	rest := serial[exit+len(commandExitMarkerPref):]
	lineEnd := strings.IndexByte(rest, '\n')
	if lineEnd == -1 {
		return 0, "", false
	}
	code, err := strconv.Atoi(strings.TrimSpace(rest[:lineEnd]))
	if err != nil {
		return 0, "", false
	}

	output := serial
	if !dmesg {
		outputStart := 0
		if begin >= 0 {
			outputStart = begin + len(commandBeginMarker)
		} else {
			outputStart = oneShotOutputStart(serial[:exit])
		}
		beginOutput := serial[outputStart:]
		if strings.HasPrefix(beginOutput, "\r\n") {
			beginOutput = beginOutput[2:]
		} else if strings.HasPrefix(beginOutput, "\n") {
			beginOutput = beginOutput[1:]
		}
		endOffset := strings.Index(beginOutput, commandExitMarkerPref)
		if endOffset >= 0 {
			output = strings.TrimRight(beginOutput[:endOffset], "\r\n")
		} else {
			output = strings.TrimRight(beginOutput, "\r\n")
		}
		output = cleanCommandOutput(output)
	}
	return code, output, true
}

func oneShotOutputStart(serialBeforeExit string) int {
	lineStart := 0
	outputStart := 0
	for lineStart < len(serialBeforeExit) {
		lineEnd := strings.IndexByte(serialBeforeExit[lineStart:], '\n')
		if lineEnd == -1 {
			lineEnd = len(serialBeforeExit)
		} else {
			lineEnd += lineStart
		}
		line := strings.TrimSpace(serialBeforeExit[lineStart:lineEnd])
		if strings.HasPrefix(line, "ccx3-init:") || strings.Contains(line, "] ccx3-init:") {
			outputStart = lineEnd
			if outputStart < len(serialBeforeExit) && serialBeforeExit[outputStart] == '\n' {
				outputStart++
			}
		}
		lineStart = lineEnd + 1
	}
	return outputStart
}

func extractManagedExecResult(serial, id string, dmesg bool) (int, string, bool) {
	beginMarker := commandBeginMarker + id
	outputPrefix := commandOutputMarker + id + ":"
	errorPrefix := commandErrorMarker + id + ":"
	exitPrefix := commandExitMarkerPref + id + ":"

	begin := strings.Index(serial, beginMarker)
	if begin == -1 {
		return 0, "", false
	}

	var output bytes.Buffer
	lines := strings.Split(serial[begin:], "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if idx := strings.Index(line, outputPrefix); idx >= 0 {
			encoded := line[idx+len(outputPrefix):]
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				continue
			}
			output.Write(data)
			continue
		}
		if idx := strings.Index(line, errorPrefix); idx >= 0 {
			encoded := line[idx+len(errorPrefix):]
			data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				continue
			}
			output.Write(data)
			continue
		}
		if idx := strings.Index(line, exitPrefix); idx >= 0 {
			code, err := strconv.Atoi(strings.TrimSpace(line[idx+len(exitPrefix):]))
			if err != nil {
				return 0, "", false
			}
			if dmesg {
				return code, strings.TrimRight(serial[begin:], "\r\n"), true
			}
			return code, strings.TrimRight(output.String(), "\r\n"), true
		}
	}
	return 0, "", false
}

func effectiveExecEnv(base, overrides []string, replace bool) []string {
	if replace {
		return vmruntime.WithDefaultEnv(overrides)
	}
	return vmruntime.WithDefaultEnv(vmruntime.MergeEnv(base, overrides))
}

func cleanCommandOutput(output string) string {
	lines := strings.Split(output, "\n")
	cleaned := make([]string, 0, len(lines))
	last := ""
	for i, line := range lines {
		if strings.HasPrefix(line, "[") {
			if idx := strings.Index(line, "] "); idx >= 0 {
				lines[i] = line[idx+2:]
			}
		}
		line = strings.TrimSpace(lines[i])
		if line == "" || line == commandBeginMarker || strings.HasPrefix(line, commandExitMarkerPref) || strings.HasPrefix(line, "ccx3-init:") || strings.HasPrefix(line, "reboot:") {
			continue
		}
		if line == last {
			continue
		}
		cleaned = append(cleaned, line)
		last = line
	}
	return strings.Join(cleaned, "\n")
}
