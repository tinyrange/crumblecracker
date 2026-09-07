package vm

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/kernel/alpine"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"github.com/tinyrange/crumblecracker/internal/core/virtio"
	"github.com/tinyrange/crumblecracker/internal/protocol"
)

const (
	runtimeBootImage    = "alpine-runtime-boot-test"
	runtimeBootAltImage = "alpine-runtime-boot-test-alt"
)

type runtimeBootEnv struct {
	backend        Backend
	images         *oci.Store
	kernelRoot     string
	guestInitCache string
	imageName      string
	altImageName   string
	memoryMB       uint64
	caps           client.CapabilitiesResponse
}

type runtimeImageAdder interface {
	AddImage(context.Context, string, *oci.Image) error
}

func TestRuntimeBootsLinuxAndRunsOneShotCommand(t *testing.T) {
	env := newRuntimeBootEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()

	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:    env.imageName,
		MemoryMB: minimumGuestMemoryMB,
		CPUs:     1,
		Command: []string{
			"sh",
			"-lc",
			"set -eu; printf 'runtime-one-shot\n'; cat /proc/sys/kernel/ostype; test -r /proc/1/cmdline; cat /etc/alpine-release; printf 'machine=%s\n' \"$(uname -m)\"",
		},
	})
	requireRunResponse(t, resp, err, 0)
	requireGuestOutput(t, resp.Output, "runtime-one-shot", "Linux", "machine=")
}

func TestRuntimeLinuxSerialConsoleCanBeOpened(t *testing.T) {
	env := newRuntimeBootEnv(t)
	command := []string{
		"sh",
		"-lc",
		"set -eu; exec 3<>/dev/ttyS0; test -t 3; printf 'serial-console-open\\n'",
	}

	t.Run("one-shot", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
		defer cancel()
		resp, err := env.backend.Run(ctx, client.RunRequest{
			Image:    env.imageName,
			MemoryMB: env.memoryMB,
			CPUs:     1,
			User:     "root",
			Command:  command,
		})
		requireRunResponse(t, resp, err, 0)
		requireGuestOutput(t, resp.Output, "serial-console-open")
	})

	t.Run("persistent", func(t *testing.T) {
		inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{})
		defer inst.Close()
		resp := execInRuntimeRequest(t, inst, client.ExecRequest{
			Command: command,
			User:    "root",
		})
		requireGuestOutput(t, resp.Output, "serial-console-open")
	})
}

func TestRuntimePersistentImageHomeSurvivesRestart(t *testing.T) {
	env := newRuntimeBootEnv(t)
	req := client.CreateInstanceRequest{
		PersistentMounts: []client.PersistentMount{{
			Name:  "runtime-home",
			Mount: "/root",
		}},
	}
	inst := startRuntimeInstance(t, env, req)
	resp := execInRuntimeRequest(t, inst, client.ExecRequest{
		User: "root",
		Command: []string{"sh", "-lc", `
set -eu
printf 'persistent-home\n' > /root/state
ln /root/state /root/state-link
truncate -s 8388608 /root/sparse
printf tail | dd of=/root/sparse bs=1 seek=8388604 conv=notrunc 2>/dev/null
mkdir /root/directory
printf nested > '/root/directory/name '
sync
`},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("prepare persistent home: %s", resp.Output)
	}
	if err := inst.Close(); err != nil {
		t.Fatalf("close first persistent guest: %v", err)
	}

	inst = startRuntimeInstance(t, env, req)
	resp = execInRuntimeRequest(t, inst, client.ExecRequest{
		User: "root",
		Command: []string{"sh", "-lc", `
set -eu
test "$(cat /root/state)" = persistent-home
test "$(stat -c %i /root/state)" = "$(stat -c %i /root/state-link)"
test "$(stat -c %h /root/state)" = 2
test "$(stat -c %s /root/sparse)" = 8388608
test "$(dd if=/root/sparse bs=1 skip=8388604 count=4 2>/dev/null)" = tail
test "$(cat '/root/directory/name ')" = nested
mv /root/state /root/renamed
rm /root/state-link
sync
`},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("verify persistent home after first restart: %s", resp.Output)
	}
	statusProvider, ok := inst.(interface {
		PersistentFSStatus() []virtio.PersistentFSStatus
	})
	if !ok {
		t.Fatalf("persistent runtime instance %T does not expose home status", inst)
	}
	statuses := statusProvider.PersistentFSStatus()
	if len(statuses) != 1 || statuses[0].Name != "runtime-home" || statuses[0].Mount != "/root" || statuses[0].DurableSequence == 0 {
		t.Fatalf("persistent runtime status = %+v", statuses)
	}
	if err := inst.Close(); err != nil {
		t.Fatalf("close second persistent guest: %v", err)
	}

	inst = startRuntimeInstance(t, env, req)
	defer inst.Close()
	resp = execInRuntimeRequest(t, inst, client.ExecRequest{
		User:    "root",
		Command: []string{"sh", "-lc", "set -eu; test ! -e /root/state; test ! -e /root/state-link; test \"$(cat /root/renamed)\" = persistent-home"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("verify persistent home after rename restart: %s", resp.Output)
	}
}

func TestRuntimePersistentImageHomeSurvivesHostProcessKill(t *testing.T) {
	if os.Getenv("CC_PERSISTENT_HOME_CRASH_HELPER") == "1" {
		runPersistentHomeCrashHelper(t)
		return
	}
	env := newRuntimeBootEnv(t)
	helperTimeout := min(runtimeBootTimeout(), 45*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimePersistentImageHomeSurvivesHostProcessKill$")
	cmd.Env = append(os.Environ(),
		"CC_PERSISTENT_HOME_CRASH_HELPER=1",
		"CC_PERSISTENT_HOME_CRASH_IMAGES="+env.images.Root(),
		"CC_PERSISTENT_HOME_CRASH_KERNEL="+env.kernelRoot,
		"CC_PERSISTENT_HOME_CRASH_GUEST_INIT="+env.guestInitCache,
		"CCX3_OCI_SHARED_CACHE_DIR="+os.Getenv("CCX3_OCI_SHARED_CACHE_DIR"),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if scanner.Text() == "CC_PERSISTENT_HOME_CRASH_READY" {
			ready = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !ready {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("crash helper exited before ready:\n%s", stderr.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("crash helper exited cleanly after hard kill")
	}

	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		PersistentMounts: []client.PersistentMount{{Name: "crash-home", Mount: "/root"}},
	})
	defer inst.Close()
	resp := execInRuntimeRequest(t, inst, client.ExecRequest{
		User:    "root",
		Command: []string{"sh", "-lc", "set -eu; test \"$(cat /root/committed)\" = survived-hard-kill"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("verify home after host process kill: %s", resp.Output)
	}
}

func runPersistentHomeCrashHelper(t *testing.T) {
	store := oci.NewStore(os.Getenv("CC_PERSISTENT_HOME_CRASH_IMAGES"))
	backend := NewRuntimeBackend(
		alpine.NewManager(os.Getenv("CC_PERSISTENT_HOME_CRASH_KERNEL")),
		store,
		os.Getenv("CC_PERSISTENT_HOME_CRASH_GUEST_INIT"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()
	inst, err := backend.Start(ctx, client.CreateInstanceRequest{
		Image:    runtimeBootImage,
		MemoryMB: 768,
		CPUs:     1,
		PersistentMounts: []client.PersistentMount{{
			Name:  "crash-home",
			Mount: "/root",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := inst.Exec(ctx, client.ExecRequest{
		User:    "root",
		Command: []string{"sh", "-lc", "set -eu; printf survived-hard-kill | dd of=/root/committed conv=fsync 2>/dev/null"},
	})
	if err != nil || resp.ExitCode != 0 {
		t.Fatalf("write committed home state: response=%+v error=%v", resp, err)
	}
	fmt.Println("CC_PERSISTENT_HOME_CRASH_READY")
	select {}
}

func TestRuntimeLinuxExposesMatchingModuleSymbolVersions(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{})
	defer inst.Close()

	resp := execInRuntime(t, inst, []string{
		"sh",
		"-lc",
		`set -eu
release=$(uname -r)
symvers=/usr/src/linux-headers-$release/Module.symvers
test -s "$symvers"
test "$(readlink -f /lib/modules/$release/build/Module.symvers)" = "$symvers"
crc=$(awk '$2 == "module_layout" { print $1; exit }' "$symvers")
test "${crc#0x}" != "$crc"
test "${#crc}" -eq 10
printf 'release=%s bytes=%s\n' "$release" "$(wc -c < "$symvers")"`,
	})
	requireGuestOutput(t, resp.Output, "release=", "bytes=")
}

func TestRuntimeLinuxImageFSPreservesTrailingSpaceNames(t *testing.T) {
	env := newRuntimeBootEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()

	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:    env.imageName,
		MemoryMB: env.memoryMB,
		CPUs:     1,
		User:     "root",
		Command: []string{
			"sh", "-lc",
			"set -eu; mkdir /work; printf A > /work/collision; printf B > '/work/collision '; test \"$(cat /work/collision)\" = A; test \"$(cat '/work/collision ')\" = B; test \"$(stat -c %i /work/collision)\" != \"$(stat -c %i '/work/collision ')\"",
		},
	})
	requireRunResponse(t, resp, err, 0)
}

func TestRuntimeLinuxImageFSLargeDirectoryListing(t *testing.T) {
	const files = 30000
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "imagefs-large-directory", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()

	execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", fmt.Sprintf(`set -eu; mkdir /many; i=0; while [ "$i" -lt %d ]; do : >"/many/entry-$i"; i=$((i+1)); done`, files)},
		User:    "root",
	})
	listings := []struct{ name, script string }{
		{"find", fmt.Sprintf(`set -eu; test "$(find /many -maxdepth 1 -type f | wc -l)" -eq %d`, files)},
		{"ls", `set -eu; ls -1 /many >/dev/null`},
	}
	for _, listing := range listings {
		started := time.Now()
		execInRuntimeRequest(t, inst, client.ExecRequest{Command: []string{"sh", "-lc", listing.script}, User: "root"})
		elapsed := time.Since(started)
		if elapsed > 15*time.Second {
			t.Fatalf("%s of %d imageFS entries took %s", listing.name, files, elapsed)
		}
		t.Logf("%s of %d imageFS entries completed in %s", listing.name, files, elapsed)
	}
	execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", `set -eu
test "$(stat -c %h /many/entry-9999)" -eq 1
ln /many/entry-9999 /many/alias
test "$(stat -c %h /many/entry-9999)" -eq 2
rm /many/alias
test "$(stat -c %h /many/entry-9999)" -eq 1
chmod 640 /many/entry-9999
test "$(stat -c %a /many/entry-9999)" = 640
printf payload >/many/entry-9999
test "$(stat -c %s /many/entry-9999)" -eq 7
mv /many/entry-9999 /many/renamed
test ! -e /many/entry-9999
test "$(cat /many/renamed)" = payload`},
		User: "root",
	})
}

func TestRuntimeLinuxImageFSKernelPermissionAndDirectorySemantics(t *testing.T) {
	t.Setenv("CCX3_VM_TEST_GUEST_INIT_CACHE_DIR", t.TempDir())
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "imagefs-posix-semantics", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()

	setup := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; printf 'cc:x:1000:1000:cc:/home/cc:/bin/sh\n' >>/etc/passwd; printf 'cc:x:1000:\nshared:x:2345:cc\n' >>/etc/group; mkdir /work; chown 1000:1000 /work; mkdir /work/shared; chown root:2345 /work/shared; chmod 2770 /work/shared; printf root >/work/root-only; chmod 0600 /work/root-only"},
		User:    "root",
	})
	if setup.ExitCode != 0 {
		t.Fatalf("set up imageFS semantics fixture: %s", setup.Output)
	}

	user := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; printf group-access >/work/shared/file; mkdir /work/shared/child; test \"$(stat -c %g /work/shared/file)\" = 2345; test \"$(stat -c %g /work/shared/child)\" = 2345; test $(( $(stat -c %a /work/shared/child) / 1000 % 10 & 2 )) -eq 2; printf owned >/work/owned; chgrp 2345 /work/owned; test \"$(stat -c %g /work/owned)\" = 2345; if chgrp 999 /work/owned 2>/dev/null; then exit 90; fi; if printf denied >/work/root-only 2>/dev/null; then exit 91; fi"},
		User:    "cc",
	})
	if user.ExitCode != 0 {
		t.Fatalf("supplementary-group or setgid behavior failed: %s", user.Output)
	}

	privilegeBits := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; printf tool >/work/shared/tool; chmod 6755 /work/shared/tool; printf changed >>/work/shared/tool; test $(( $(stat -c %a /work/shared/tool) / 1000 )) -eq 0"},
		User:    "cc",
	})
	if privilegeBits.ExitCode != 0 {
		t.Fatalf("privilege bits survived an unprivileged write: %s", privilegeBits.Output)
	}
}

func TestRuntimeCancelsInteractiveShellWithBlockedForegroundCommand(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "cancel-interactive-shell", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()
	ctx, cancel := context.WithCancel(context.Background())
	inputs := make(chan client.ExecInput, 1)
	inputs <- client.ExecInput{Kind: "stdin", Data: []byte("trap '' TERM INT\nprintf 'ready\\n'\nsleep 60\n")}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- inst.ExecStream(ctx, client.ExecRequest{Command: []string{"/bin/sh"}}, inputs, func(event client.ExecEvent) error {
			if event.Kind == "stdout" && strings.Contains(event.Output, "ready") {
				select {
				case <-started:
				default:
					close(started)
				}
			}
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(runtimeExecTimeout()):
		cancel()
		t.Fatal("interactive shell did not start its foreground command")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interactive shell cancellation = %v, want context canceled", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("interactive shell cancellation did not terminate and reap the foreground command")
	}
}

func TestRuntimeContainsDetachedCommandWithoutGuestEnvironmentMarkers(t *testing.T) {
	t.Setenv("CCX3_VM_TEST_GUEST_INIT_CACHE_DIR", t.TempDir())
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "detached-command-containment", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()

	started := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-c", `set -eu; test "${CC_EXEC_FAMILY-unset}" = user-value; env -u CC_EXEC_FAMILY setsid sh -c 'trap "" TERM HUP INT; exec sleep 30' >/dev/null 2>&1 & echo $! >/tmp/detached.pid`},
		Env:     []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "CC_EXEC_FAMILY=user-value"},
		User:    "root",
	})
	if started.ExitCode != 0 {
		t.Fatalf("start detached workload: %s", started.Output)
	}
	checked := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", `set -eu; pid=$(cat /tmp/detached.pid); i=0; while kill -0 "$pid" 2>/dev/null && test "$i" -lt 100; do i=$((i+1)); sleep .01; done; ! kill -0 "$pid" 2>/dev/null`},
		User:    "root",
	})
	if checked.ExitCode != 0 {
		t.Fatalf("detached workload survived command completion: %s", checked.Output)
	}
}

func TestRuntimeArchiveControlsInitializeUserHomeAndPreserveHardLinks(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "archive-user-home", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()
	rootFirst := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"/bin/true"},
		WorkDir: "/home/cc",
		User:    "root",
	})
	if rootFirst.ExitCode != 0 {
		t.Fatalf("root command in shared workspace exited with %d: %s", rootFirst.ExitCode, rootFirst.Output)
	}
	runRuntimeControl(t, inst, client.ExecRequest{
		Kind:      "fs_extract",
		Path:      "/home/cc/first-copy",
		Directory: true,
		Stdin:     tarPayload(t, map[string]string{"payload.txt": "first-copy"}),
		User:      "1000:1000",
	}, 0)
	first := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; test \"$(stat -c %u:%g /home/cc)\" = 1000:1000; cat /home/cc/first-copy/payload.txt"},
		User:    "1000:1000",
	})
	requireGuestOutput(t, first.Output, "first-copy")

	created := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; mkdir -p /home/cc/links; printf linked >/home/cc/links/first; ln /home/cc/links/first /home/cc/links/second"},
		User:    "1000:1000",
	})
	if created.ExitCode != 0 {
		t.Fatalf("create guest hard links exited with %d: %s", created.ExitCode, created.Output)
	}
	archive := runRuntimeControl(t, inst, client.ExecRequest{Kind: "fs_archive", Path: "/home/cc/links", User: "1000:1000"}, 0)
	runRuntimeControl(t, inst, client.ExecRequest{
		Kind: "fs_extract", Path: "/home/cc/copied-links", Directory: true, Stdin: archive, User: "1000:1000",
	}, 0)
	verified := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; a=$(stat -c %i /home/cc/copied-links/links/first); b=$(stat -c %i /home/cc/copied-links/links/second); test \"$a\" = \"$b\"; test \"$(stat -c %h /home/cc/copied-links/links/first)\" -eq 2; printf change >/home/cc/copied-links/links/first; test \"$(cat /home/cc/copied-links/links/second)\" = change"},
		User:    "1000:1000",
	})
	if verified.ExitCode != 0 {
		t.Fatalf("copied hard-link topology check exited with %d: %s", verified.ExitCode, verified.Output)
	}
}

func TestRuntimeArchiveTransferKeepsSparseFilesSparse(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "archive-sparse-file", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()
	created := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; mkdir /work; chown 1000:1000 /work"}, User: "root",
	})
	if created.ExitCode != 0 {
		t.Fatalf("prepare sparse guest tree: %s", created.Output)
	}
	created = execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; mkdir -p /work/sparse-source; dd if=/dev/zero of=/work/sparse-source/file bs=1 count=0 seek=67108864 2>/dev/null; printf tail | dd of=/work/sparse-source/file bs=1 seek=67108860 conv=notrunc 2>/dev/null"},
		User:    "1000:1000",
	})
	if created.ExitCode != 0 {
		t.Fatalf("create sparse guest file: %s", created.Output)
	}
	archive := runRuntimeControl(t, inst, client.ExecRequest{
		Kind: "fs_archive", Path: "/work/sparse-source", User: "1000:1000",
	}, 0)
	if len(archive) > 1<<20 {
		t.Fatalf("mounted sparse archive expanded to %d bytes", len(archive))
	}
	runRuntimeControl(t, inst, client.ExecRequest{
		Kind: "fs_extract", Path: "/work/sparse-copy", Directory: true, Stdin: archive, User: "1000:1000",
	}, 0)
	verified := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; file=/work/sparse-copy/sparse-source/file; test \"$(stat -c %s \"$file\")\" = 67108864; test \"$(tail -c 4 \"$file\")\" = tail; test $(( $(stat -c %b \"$file\") * 512 )) -lt 1048576"},
		User:    "1000:1000",
	})
	if verified.ExitCode != 0 {
		t.Fatalf("sparse artifact round trip expanded or corrupted the file: %s", verified.Output)
	}
}

func TestRuntimeArchiveTransferPreservesMountedXattrs(t *testing.T) {
	sourceDir := t.TempDir()
	sourceFile := filepath.Join(sourceDir, "file")
	if err := os.WriteFile(sourceFile, []byte("xattr-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantValue := []byte{0, 1, 0xff}
	if err := setRuntimeTestXattr(sourceFile, "user.vmsh-test", wantValue); err != nil {
		t.Skipf("host test filesystem does not support user xattrs: %v", err)
	}
	info, err := os.Lstat(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	if err := writeRuntimePathTar(&input, sourceDir, filepath.Base(sourceDir), info); err != nil {
		t.Fatal(err)
	}

	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "archive-xattrs", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()
	prepared := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "mkdir /work; chown 1000:1000 /work"}, User: "root",
	})
	if prepared.ExitCode != 0 {
		t.Fatalf("prepare mounted xattr destination: %s", prepared.Output)
	}
	runRuntimeControl(t, inst, client.ExecRequest{
		Kind: "fs_extract", Path: "/work/xattr-copy", Directory: true, Stdin: input.Bytes(), User: "1000:1000",
	}, 0)
	archive := runRuntimeControl(t, inst, client.ExecRequest{
		Kind: "fs_archive", Path: "/work/xattr-copy", User: "1000:1000",
	}, 0)
	wantKey := "VMSH.xattr." + base64.RawURLEncoding.EncodeToString([]byte("user.vmsh-test"))
	reader := tar.NewReader(bytes.NewReader(archive))
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		encoded, ok := header.PAXRecords[wantKey]
		if !ok {
			continue
		}
		value, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(value, wantValue) {
			t.Fatalf("mounted xattr value = %v, want %v", value, wantValue)
		}
		found = true
	}
	if !found {
		t.Fatal("mounted artifact archive omitted the user xattr")
	}
}

func TestRuntimeArchiveControlEnforcesSparseLogicalLimits(t *testing.T) {
	const maxFile = int64(64 << 20)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	header := &tar.Header{
		Name: "oversized", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: tar.FormatPAX,
		PAXRecords: map[string]string{
			"VMSH.sparse.size":      strconv.FormatInt(maxFile+1, 10),
			"VMSH.sparse.numblocks": "1",
			"VMSH.sparse.map":       strconv.FormatInt(maxFile, 10) + ",1",
		},
	}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte{'x'})
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "archive-sparse-limits", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()
	runRuntimeControl(t, inst, client.ExecRequest{
		Kind: "fs_extract", Path: "/work/oversized", Stdin: archive.Bytes(), User: "root",
		ArchiveLimits: &client.ArchiveLimits{MaxEntries: 10, MaxFileBytes: maxFile, MaxExpandedBytes: maxFile},
	}, 1)
	probe := execInRuntimeRequest(t, inst, client.ExecRequest{Command: []string{"/bin/sh", "-c", "test ! -e /work/oversized"}, User: "root"})
	if probe.ExitCode != 0 {
		t.Fatalf("rejected sparse archive left a destination behind: %s", probe.Output)
	}
}

func TestRuntimeMountedImageFSRejectsUnsafeExchangeAndHonorsPOSIXLocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runtime helper archive requires a POSIX host")
	}
	helperDir := t.TempDir()
	renameat2Syscall := 316
	if runtime.GOARCH == "arm64" {
		renameat2Syscall = 276
	} else if runtime.GOARCH != "amd64" {
		t.Skipf("renameat2 test helper does not define the syscall number for %s", runtime.GOARCH)
	}
	source := fmt.Sprintf(`package main
import ("fmt"; "os"; "syscall"; "time"; "unsafe")
func main() {
	if os.Args[1] == "noreplace" {
		old, new := "/work/noreplace-old", "/work/noreplace-new"
		os.Remove(old); os.Remove(new); os.WriteFile(old, []byte("SOURCE"), 0600)
		op, _ := syscall.BytePtrFromString(old); np, _ := syscall.BytePtrFromString(new)
		atFDCWD := ^uintptr(99)
		_, _, errno := syscall.Syscall6(%d, atFDCWD, uintptr(unsafe.Pointer(op)), atFDCWD, uintptr(unsafe.Pointer(np)), 1, 0)
		if errno != 0 { panic(errno) }
		if _, err := os.Stat(old); !os.IsNotExist(err) { os.Exit(6) }
		data, err := os.ReadFile(new); if err != nil || string(data) != "SOURCE" { os.Exit(7) }
		fmt.Println("noreplace-renamed"); return
	}
	if os.Args[1] == "noreplace-existing" {
		a, b, c := "/work/sequence-a", "/work/sequence-b", "/work/sequence-c"
		os.Remove(a); os.Remove(b); os.Remove(c); os.WriteFile(a, []byte("A"), 0600)
		ap, _ := syscall.BytePtrFromString(a); bp, _ := syscall.BytePtrFromString(b); cp, _ := syscall.BytePtrFromString(c)
		atFDCWD := ^uintptr(99)
		_, _, first := syscall.Syscall6(%d, atFDCWD, uintptr(unsafe.Pointer(ap)), atFDCWD, uintptr(unsafe.Pointer(bp)), 1, 0)
		if first != 0 { panic(first) }
		os.WriteFile(c, []byte("C"), 0600)
		_, _, second := syscall.Syscall6(%d, atFDCWD, uintptr(unsafe.Pointer(bp)), atFDCWD, uintptr(unsafe.Pointer(cp)), 1, 0)
		if second != syscall.EEXIST { fmt.Printf("second=%%v\n", second); os.Exit(10) }
		bd, _ := os.ReadFile(b); cd, _ := os.ReadFile(c)
		if string(bd) != "A" || string(cd) != "C" { os.Exit(11) }
		fmt.Println("noreplace-existing-preserved"); return
	}
	if os.Args[1] == "hardlinks" {
		a, b := "/work/link-a", "/work/link-b"; os.Remove(a); os.Remove(b)
		if err := os.WriteFile(a, []byte("x"), 0600); err != nil { panic(err) }
		if err := os.Link(a, b); err != nil { panic(err) }
		info, err := os.Stat(a); if err != nil || info.Sys().(*syscall.Stat_t).Nlink != 2 { os.Exit(8) }
		if err := os.Remove(b); err != nil { panic(err) }
		info, err = os.Stat(a); if err != nil || info.Sys().(*syscall.Stat_t).Nlink != 1 { os.Exit(9) }
		fmt.Println("hardlink-counts-current"); return
	}
 if os.Args[1] == "opendir" {
  path := "/work/open-directory"
  if err := os.Mkdir(path, 0700); err != nil { panic(err) }
  dir, err := os.Open(path); if err != nil { panic(err) }
  if err := os.Remove(path); err != nil { panic(err) }
  if _, err := dir.Stat(); err != nil { panic(err) }
  fmt.Println("open-directory-alive"); return
 }
 if os.Args[1] == "exchange" {
  left, right := "/work/exchange-left", "/work/exchange-right"
  os.WriteFile(left, []byte("LEFT"), 0600); os.WriteFile(right, []byte("RIGHT"), 0600)
  lp, _ := syscall.BytePtrFromString(left); rp, _ := syscall.BytePtrFromString(right)
  atFDCWD := ^uintptr(99)
  _, _, errno := syscall.Syscall6(%d, atFDCWD, uintptr(unsafe.Pointer(lp)), atFDCWD, uintptr(unsafe.Pointer(rp)), 2, 0)
  if errno == 0 { os.Exit(4) }
  l, _ := os.ReadFile(left); r, _ := os.ReadFile(right)
  if string(l) != "LEFT" || string(r) != "RIGHT" { os.Exit(5) }
  fmt.Println("exchange-blocked"); return
 }
 f, err := os.OpenFile(os.Args[2], os.O_CREATE|os.O_RDWR, 0600); if err != nil { panic(err) }
 lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
 err = syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock)
 if os.Args[1] == "hold" { if err != nil { panic(err) }; fmt.Println("locked"); time.Sleep(30*time.Second); return }
 if err == syscall.EAGAIN || err == syscall.EACCES { fmt.Println("blocked"); return }
 if err != nil { panic(err) }
 os.Exit(3)
}`, renameat2Syscall, renameat2Syscall, renameat2Syscall, renameat2Syscall)
	sourcePath := filepath.Join(helperDir, "main.go")
	binaryPath := filepath.Join(helperDir, "lock-helper")
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-trimpath", "-o", binaryPath, sourcePath)
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build POSIX lock helper: %v\n%s", err, output)
	}
	info, err := os.Lstat(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	var payload bytes.Buffer
	if err := writeRuntimePathTar(&payload, binaryPath, "lock-helper", info); err != nil {
		t.Fatal(err)
	}

	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		ID: "imagefs-posix-locks", Image: env.imageName, MemoryMB: env.memoryMB, CPUs: 1,
	})
	defer inst.Close()
	runRuntimeControl(t, inst, client.ExecRequest{
		Kind: "fs_extract", Path: "/work/lock-bin", Directory: true, Stdin: payload.Bytes(), User: "root",
	}, 0)
	exchange := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exec /work/lock-bin/lock-helper exchange"}, User: "root",
	})
	if exchange.ExitCode != 0 {
		t.Fatalf("mounted RENAME_EXCHANGE did not fail safely: exit=%d output=%s", exchange.ExitCode, exchange.Output)
	}
	noreplace := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exec /work/lock-bin/lock-helper noreplace"}, User: "root",
	})
	if noreplace.ExitCode != 0 {
		t.Fatalf("mounted RENAME_NOREPLACE failed for an absent target: exit=%d output=%s", noreplace.ExitCode, noreplace.Output)
	}
	contextInputs := make(chan client.ExecInput, 2)
	var contextControl strings.Builder
	var contextExit *int
	commandSent := false
	if err := inst.ExecStream(context.Background(), client.ExecRequest{
		Command: []string{"/bin/sh", "-c", "printf 'ready\\n' >&3; IFS= read -r command; eval \"$command\"; status=$?; printf 'complete:%s\\n' \"$status\" >&3; exit \"$status\""},
		User:    "root", ControlFD: true,
	}, contextInputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "control":
			data := event.Data
			if len(data) == 0 {
				data = []byte(event.Output)
			}
			contextControl.Write(data)
			if !commandSent && strings.Contains(contextControl.String(), "ready\n") {
				commandSent = true
				contextInputs <- client.ExecInput{Kind: "stdin", Data: []byte("/work/lock-bin/lock-helper noreplace-existing\n")}
				close(contextInputs)
			}
		case "exit":
			code := event.ExitCode
			contextExit = &code
		}
		return nil
	}); err != nil {
		t.Fatalf("persistent root rename sequence: %v; control=%q", err, contextControl.String())
	}
	if !commandSent || contextExit == nil || *contextExit != 0 || contextControl.String() != "ready\ncomplete:0\n" {
		t.Fatalf("persistent root rename completion: sent=%t exit=%v control=%q", commandSent, contextExit, contextControl.String())
	}
	hardlinks := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exec /work/lock-bin/lock-helper hardlinks"}, User: "root",
	})
	if hardlinks.ExitCode != 0 {
		t.Fatalf("mounted hard-link attributes remained stale: exit=%d output=%s", hardlinks.ExitCode, hardlinks.Output)
	}
	openDirectory := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exec /work/lock-bin/lock-helper opendir"}, User: "root",
	})
	if openDirectory.ExitCode != 0 {
		t.Fatalf("open directory descriptor did not survive rmdir: exit=%d output=%s", openDirectory.ExitCode, openDirectory.Output)
	}
	holdCtx, holdCancel := context.WithCancel(context.Background())
	defer holdCancel()
	inputs := make(chan client.ExecInput, 1)
	inputs <- client.ExecInput{Kind: "stdin_close"}
	locked := make(chan struct{})
	holdDone := make(chan error, 1)
	var holdOutput strings.Builder
	go func() {
		holdDone <- inst.ExecStream(holdCtx, client.ExecRequest{
			Command: []string{"/bin/sh", "-c", "exec /work/lock-bin/lock-helper hold /work/lock-target"}, User: "root",
		}, inputs, func(event client.ExecEvent) error {
			data := event.Data
			if len(data) == 0 {
				data = []byte(event.Output)
			}
			holdOutput.Write(data)
			if event.Kind == "stdout" && strings.Contains(holdOutput.String(), "locked\n") {
				select {
				case <-locked:
				default:
					close(locked)
				}
			}
			return nil
		})
	}()
	select {
	case <-locked:
	case err := <-holdDone:
		t.Fatalf("lock holder exited before acquiring its lock: %v output=%s", err, holdOutput.String())
	case <-time.After(runtimeExecTimeout()):
		holdCancel()
		t.Fatal("lock holder did not acquire its POSIX lock")
	}
	probe := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"/bin/sh", "-c", "exec /work/lock-bin/lock-helper try /work/lock-target"}, User: "root",
	})
	if probe.ExitCode != 0 {
		t.Fatalf("conflicting POSIX lock was not blocked: exit=%d output=%s", probe.ExitCode, probe.Output)
	}
	holdCancel()
	select {
	case <-holdDone:
	case <-time.After(3 * time.Second):
		t.Fatal("lock holder did not stop after cancellation")
	}
}

func TestRuntimeBootsLinuxWithVirtioBalloon(t *testing.T) {
	if runtime.GOOS != "darwin" && !(runtime.GOOS == "linux" && runtime.GOARCH == "amd64") {
		t.Skip("virtio balloon support is implemented by the Darwin arm64 and Linux amd64 backends")
	}
	env := newRuntimeBootEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()

	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:     env.imageName,
		MemoryMB:  env.memoryMB,
		BalloonMB: 64,
		CPUs:      1,
		Command: []string{
			"sh",
			"-lc",
			`set -eu
found=0
for dev in /sys/bus/virtio/devices/virtio*; do
	test -e "$dev/device" || continue
	if test "$(cat "$dev/device")" = "0x0005"; then
		found=1
		test "$(basename "$(readlink "$dev/driver")")" = "virtio_balloon"
		break
	fi
done
test "$found" = 1
printf 'virtio-balloon-ok\n'`,
		},
	})
	requireRunResponse(t, resp, err, 0)
}

func TestRuntimeNamedBlankImagePreservesInitialBalloonTarget(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("dynamic balloon recovery is currently implemented by Linux amd64 KVM")
	}
	env := newRuntimeBootEnv(t)
	manager := NewManagerWithBackend(env.backend)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()
	const id = "blank-image-balloon"
	state, err := manager.StartBlank(ctx, client.StartInstanceRequest{
		ID: id, Image: env.imageName, MemoryMB: 768, BalloonMB: 384, CPUs: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.ShutdownAll(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for state.BalloonStatus != "converged" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		state = manager.StatusOf(id)
	}
	if state.BalloonMB != 384 || state.BalloonActualMB != 384 || state.BalloonStatus != "converged" {
		t.Fatalf("named blank-image balloon state = %+v", state)
	}
}

func TestRuntimeRetargetsBalloonWhileGuestRemainsRunning(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("dynamic balloon recovery is currently implemented by Linux amd64 KVM")
	}
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{MemoryMB: 768})
	defer inst.Close()
	controller, ok := inst.(interface{ SetBalloonMB(uint64) error })
	if !ok {
		t.Fatal("running Linux VM does not expose dynamic balloon control")
	}
	if err := controller.SetBalloonMB(384); err != nil {
		t.Fatal(err)
	}
	shrunk := execInRuntime(t, inst, []string{"sh", "-lc", `
for attempt in $(seq 1 100); do
	memory_kb=$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)
	if test "$memory_kb" -lt 600000; then printf '%s\n' "$memory_kb"; exit 0; fi
	sleep 0.02
done
exit 1
`})
	if shrunk.ExitCode != 0 {
		t.Fatalf("guest did not relinquish memory: %s", shrunk.Output)
	}
	if err := controller.SetBalloonMB(0); err != nil {
		t.Fatal(err)
	}
	restored := execInRuntime(t, inst, []string{"sh", "-lc", `
for attempt in $(seq 1 100); do
	memory_kb=$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)
	if test "$memory_kb" -gt 700000; then printf '%s\n' "$memory_kb"; exit 0; fi
	sleep 0.02
done
exit 1
`})
	if restored.ExitCode != 0 {
		t.Fatalf("guest memory was not restored after pressure: %s", restored.Output)
	}
}

func TestRuntimeOneShotReportsNonZeroExitAndStderr(t *testing.T) {
	env := newRuntimeBootEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()

	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:    env.imageName,
		MemoryMB: env.memoryMB,
		CPUs:     1,
		Command: []string{
			"sh",
			"-lc",
			"printf 'stdout-before-fail\n'; printf 'stderr-before-fail\n' >&2; exit 37",
		},
	})
	requireRunResponse(t, resp, err, 37)
	requireGuestOutput(t, resp.Output, "stdout-before-fail", "stderr-before-fail")
}

func TestRuntimeOneShotDmesgIncludesTranscript(t *testing.T) {
	env := newRuntimeBootEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()

	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:    env.imageName,
		MemoryMB: env.memoryMB,
		CPUs:     1,
		Dmesg:    true,
		Command: []string{
			"sh",
			"-lc",
			"set -eu; printf 'runtime-dmesg-one-shot\n'; true",
		},
	})
	requireRunResponse(t, resp, err, 0)
	requireGuestOutput(t, resp.Output, "runtime-dmesg-one-shot", "ccx3-init")
}

func TestRuntimeBootsLinuxWithExt4Root(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("rootfs image mode is implemented by the Linux KVM backends")
	}
	t.Setenv("CCX3_ROOTFS_IMAGE_TYPE", "ext4")
	env := newRuntimeBootEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()

	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:    env.imageName,
		MemoryMB: env.memoryMB,
		CPUs:     1,
		Command: []string{
			"sh",
			"-lc",
			"set -eu; awk '$5 == \"/\" { for (i = 1; i <= NF; i++) if ($i == \"-\") { print $(i+1); exit } }' /proc/self/mountinfo",
		},
	})
	requireRunResponse(t, resp, err, 0)
	fields := strings.Fields(resp.Output)
	if len(fields) == 0 || fields[0] != "ext4" {
		t.Fatalf("root filesystem type output = %q, want ext4\noutput:\n%s", strings.TrimSpace(resp.Output), resp.Output)
	}
}

func TestRuntimeBootsOpenBSDBuiltinImage(t *testing.T) {
	ctx, inst := bootManagedBSDRuntimeContract(t, managedBSDRuntimeBootCase{
		name:     "OpenBSD",
		envVar:   "CC_TEST_OPENBSD_KVM",
		image:    "@openbsd",
		memoryMB: 768,
		timeout:  120 * time.Second,
		guestOS:  "OpenBSD",
		label:    "openbsd",
	})

	inputs := make(chan client.ExecInput, 2)
	var interactiveControl strings.Builder
	var sentInteractiveInput bool
	var interactiveExit *int
	if err := inst.ExecStream(ctx, client.ExecRequest{
		Command:   []string{"sh", "-c", "printf 'ready:%s\\n' \"$PWD\" >&3; IFS= read -r line; eval \"$line\""},
		WorkDir:   "/tmp",
		ControlFD: true,
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "control":
			interactiveControl.WriteString(event.Output)
			if !sentInteractiveInput && strings.Contains(event.Output, "ready:/tmp") {
				sentInteractiveInput = true
				inputs <- client.ExecInput{Kind: "stdin", Data: []byte("printf 'done:%s\\n' \"$PWD\" >&3\n")}
				close(inputs)
			}
		case "exit":
			code := event.ExitCode
			interactiveExit = &code
		}
		return nil
	}); err != nil {
		t.Fatalf("OpenBSD runtime interactive control fd exec: %v", err)
	}
	if !sentInteractiveInput {
		t.Fatalf("OpenBSD interactive control fd never reported ready; control=%q", interactiveControl.String())
	}
	if interactiveExit == nil || *interactiveExit != 0 {
		t.Fatalf("OpenBSD interactive control fd exit = %v, want 0", interactiveExit)
	}
	if got := interactiveControl.String(); !strings.Contains(got, "ready:/tmp") || !strings.Contains(got, "done:/tmp") {
		t.Fatalf("OpenBSD interactive control fd output = %q, want ready and done", got)
	}

	resp, err := inst.Exec(ctx, client.ExecRequest{
		Command: []string{"sh", "-c", "set -eu; test \"$(date +%z)\" = +0000; pkg_info -q >/tmp/pkg-list; cc --version >/tmp/cc-version; test -s /tmp/cc-version"},
		WorkDir: "/tmp",
	})
	requireRunResponse(t, resp, err, 0)
}

func TestRuntimeBootsFreeBSDBuiltinImage(t *testing.T) {
	ctx, inst := bootManagedBSDRuntimeContract(t, managedBSDRuntimeBootCase{
		name:     "FreeBSD",
		envVar:   "CC_TEST_FREEBSD_KVM",
		image:    "@freebsd",
		memoryMB: 1024,
		timeout:  180 * time.Second,
		guestOS:  "FreeBSD",
		label:    "freebsd",
	})
	resp, err := inst.Exec(ctx, client.ExecRequest{
		Command: []string{"sh", "-c", "set -eu; printf A > /tmp/collision; printf B > '/tmp/collision '; test \"$(cat /tmp/collision)\" = A; test \"$(cat '/tmp/collision ')\" = B; mkdir /tmp/work '/tmp/work '; : > /tmp/work/plain; : > '/tmp/work '/spaced"},
		WorkDir: "/tmp",
	})
	requireRunResponse(t, resp, err, 0)
	resp, err = inst.Exec(ctx, client.ExecRequest{
		Command: []string{"sh", "-c", "test -f spaced; test ! -e plain"},
		WorkDir: "/tmp/work ",
	})
	requireRunResponse(t, resp, err, 0)
}

func TestRuntimeBootsNetBSDBuiltinImage(t *testing.T) {
	ctx, inst := bootManagedBSDRuntimeContract(t, managedBSDRuntimeBootCase{
		name:     "NetBSD",
		envVar:   "CC_TEST_NETBSD_KVM",
		image:    "@netbsd",
		memoryMB: 1024,
		timeout:  240 * time.Second,
		guestOS:  "NetBSD",
		label:    "netbsd",
	})

	resp, err := inst.Exec(ctx, client.ExecRequest{
		Command: []string{"sh", "-c", "set -eu; pkg_info -V >/tmp/pkg-version; test -s /tmp/pkg-version; cc --version >/tmp/cc-version; test -s /tmp/cc-version"},
		WorkDir: "/tmp",
	})
	requireRunResponse(t, resp, err, 0)
}

type managedBSDRuntimeBootCase struct {
	name     string
	envVar   string
	image    string
	memoryMB uint64
	timeout  time.Duration
	guestOS  string
	label    string
}

func bootManagedBSDRuntimeContract(t *testing.T, tc managedBSDRuntimeBootCase) (context.Context, Instance) {
	t.Helper()
	if os.Getenv(tc.envVar) == "" {
		t.Skipf("set %s=1 to run %s runtime boot test", tc.envVar, tc.name)
	}
	if err := Supports(); err != nil {
		t.Skipf("VM runtime unsupported on this host: %v", err)
	}
	cacheRoot := runtimeBootCacheRoot(t)
	backend := NewRuntimeBackend(nil, nil, filepath.Join(cacheRoot, "guestinit"))
	ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
	t.Cleanup(cancel)
	inst, err := backend.StartStream(ctx, client.CreateInstanceRequest{
		Image:    tc.image,
		MemoryMB: tc.memoryMB,
	}, nil)
	if err != nil {
		t.Fatalf("boot %s runtime: %v", tc.name, err)
	}
	t.Cleanup(func() { _ = inst.Close() })

	resp, err := inst.Exec(ctx, client.ExecRequest{
		Command: []string{"sh", "-c", fmt.Sprintf("printf '%s-runtime:'; printf %%s \"$(uname -s)\"; printf ':copy:'; cat", tc.label)},
		WorkDir: "/tmp",
		Stdin:   []byte("stdin-ok"),
	})
	if err != nil {
		t.Fatalf("%s runtime exec: %v", tc.name, err)
	}
	normalized := strings.ReplaceAll(strings.TrimSpace(resp.Output), "\n", "")
	expectedOutput := fmt.Sprintf("%s-runtime:%s:copy:stdin-ok", tc.label, tc.guestOS)
	if resp.ExitCode != 0 || normalized != expectedOutput {
		t.Fatalf("%s exec response = code %d output %q, want %q", tc.name, resp.ExitCode, resp.Output, expectedOutput)
	}

	const escalationProbe = "/root/cc-nonroot-escalation-probe"
	nonRoot, err := inst.Exec(ctx, client.ExecRequest{
		Command: []string{"sh", "-c", "touch " + escalationProbe},
		WorkDir: "/",
		User:    "1000:1000",
	})
	requireRunResponse(t, nonRoot, err, 126)
	probe, err := inst.Exec(ctx, client.ExecRequest{
		Command: []string{"sh", "-c", "test ! -e " + escalationProbe},
		WorkDir: "/",
		User:    "root",
	})
	requireRunResponse(t, probe, err, 0)

	var controlOutput string
	var controlExit *int
	if err := inst.ExecStream(ctx, client.ExecRequest{
		Command:   []string{"sh", "-c", fmt.Sprintf("printf '%s-control:%%s\\n' \"$PWD\" >&3", tc.label)},
		WorkDir:   "/tmp",
		ControlFD: true,
	}, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "control":
			controlOutput += event.Output
		case "exit":
			code := event.ExitCode
			controlExit = &code
		}
		return nil
	}); err != nil {
		t.Fatalf("%s runtime control fd exec: %v", tc.name, err)
	}
	if controlExit == nil || *controlExit != 0 {
		t.Fatalf("%s control fd exit = %v, want 0", tc.name, controlExit)
	}
	expectedControl := fmt.Sprintf("%s-control:/tmp", tc.label)
	if strings.TrimSpace(controlOutput) != expectedControl {
		t.Fatalf("%s control fd output = %q, want %s", tc.name, controlOutput, expectedControl)
	}

	inputs := make(chan client.ExecInput, 2)
	var ttyControl strings.Builder
	var ttyExit *int
	resizeSent := false
	if err := inst.ExecStream(ctx, client.ExecRequest{
		Command:   []string{"sh", "-c", "test -t 0 || exit 91; printf 'tty-ready\\n' >&3; IFS= read -r command; eval \"$command\""},
		TTY:       true,
		ControlFD: true,
		Cols:      80,
		Rows:      24,
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "control":
			ttyControl.WriteString(event.Output)
			if !resizeSent && strings.Contains(ttyControl.String(), "tty-ready\n") {
				resizeSent = true
				inputs <- client.ExecInput{Kind: "resize", Cols: 91, Rows: 37}
				inputs <- client.ExecInput{Kind: "stdin", Data: []byte("stty size >&3\n")}
				close(inputs)
			}
		case "exit":
			code := event.ExitCode
			ttyExit = &code
		}
		return nil
	}); err != nil {
		t.Fatalf("%s runtime TTY exec: %v", tc.name, err)
	}
	if !resizeSent || ttyExit == nil || *ttyExit != 0 {
		t.Fatalf("%s TTY state resize_sent=%t exit=%v control=%q", tc.name, resizeSent, ttyExit, ttyControl.String())
	}
	if got, want := strings.TrimSpace(ttyControl.String()), "tty-ready\n37 91"; got != want {
		t.Fatalf("%s TTY control output = %q, want %q", tc.name, got, want)
	}

	flusher, ok := inst.(instanceFlushProvider)
	if !ok {
		t.Fatalf("%s runtime does not expose flush", tc.name)
	}
	if err := flusher.Flush(ctx); err != nil {
		t.Fatalf("%s runtime flush: %v", tc.name, err)
	}
	return ctx, inst
}

func TestRuntimeRejectsInvalidRequests(t *testing.T) {
	env := newRuntimeBootEnv(t)
	for _, tc := range []struct {
		name string
		req  client.RunRequest
	}{
		{
			name: "missing image",
			req: client.RunRequest{
				MemoryMB: env.memoryMB,
				CPUs:     1,
				Command:  []string{"sh", "-lc", "true"},
			},
		},
		{
			name: "relative workdir",
			req: client.RunRequest{
				Image:    env.imageName,
				MemoryMB: env.memoryMB,
				CPUs:     1,
				WorkDir:  "relative",
				Command:  []string{"sh", "-lc", "true"},
			},
		},
		{
			name: "invalid user",
			req: client.RunRequest{
				Image:    env.imageName,
				MemoryMB: env.memoryMB,
				CPUs:     1,
				User:     "daemon:",
				Command:  []string{"sh", "-lc", "true"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
			defer cancel()
			resp, err := env.backend.Run(ctx, tc.req)
			if err == nil && resp.ExitCode == 0 {
				t.Fatalf("invalid request unexpectedly succeeded: %+v", resp)
			}
		})
	}
}

func TestRuntimeRunsAsNamedGuestUser(t *testing.T) {
	env := newRuntimeBootEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()
	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:    env.imageName,
		MemoryMB: env.memoryMB,
		CPUs:     1,
		User:     "daemon",
		Command:  []string{"sh", "-lc", `test "$(id -un)" = daemon`},
	})
	if err != nil {
		t.Fatalf("run as named guest user: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("run as named guest user exit code = %d, output = %q", resp.ExitCode, resp.Output)
	}
}

func TestRuntimeBootsPersistentLinuxAndExecsCommands(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{})
	defer inst.Close()

	first := execInRuntime(t, inst, []string{
		"sh",
		"-lc",
		"set -eu; printf 'runtime-persistent\n'; cat /proc/sys/kernel/ostype; test -d /sys/kernel; cat /etc/alpine-release",
	})
	requireGuestOutput(t, first.Output, "runtime-persistent", "Linux")

	second := execInRuntime(t, inst, []string{
		"sh",
		"-lc",
		"set -eu; printf '%s\n' $((21 + 21)); printf persisted >/tmp/runtime-test; cat /tmp/runtime-test",
	})
	requireGuestOutput(t, second.Output, "42", "persisted")
}

func TestRuntimeRestoresLinuxFromStartupSnapshotOnWindowsARM64(t *testing.T) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "arm64" {
		t.Skip("Windows arm64 WHP startup snapshots are host-specific")
	}
	env := newRuntimeBootEnv(t)
	snapshotRoot := t.TempDir()
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		SnapshotDir: snapshotRoot,
	})
	if err := inst.Close(); err != nil {
		t.Fatalf("close snapshot capture instance: %v", err)
	}
	snapshotPath := requireSingleStartupSnapshot(t, snapshotRoot)
	restored := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		RestoreSnapshot: snapshotPath,
	})
	t.Cleanup(func() { _ = restored.Close() })
	resp := execInRuntime(t, restored, []string{"sh", "-lc", "printf 'snapshot-restored:'; uname -m"})
	requireGuestOutput(t, resp.Output, "snapshot-restored:")
}

func requireSingleStartupSnapshot(t *testing.T, root string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, "snapshot-*"))
	if err != nil {
		t.Fatalf("glob startup snapshots: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("snapshot count = %d, want 1 under %s", len(matches), root)
	}
	for _, name := range []string{"manifest.json", "memory.bin"} {
		info, err := os.Stat(filepath.Join(matches[0], name))
		if err != nil {
			t.Fatalf("stat snapshot %s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("snapshot %s is empty", name)
		}
	}
	return matches[0]
}

func TestRuntimePersistentLinuxStreamsStdin(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{})
	defer inst.Close()

	output := execStreamInRuntime(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; while IFS= read -r line; do printf 'line:%s\n' \"$line\"; done"},
	}, execStreamInput("alpha\n", "beta\n"), 0)
	requireGuestOutput(t, output, "line:alpha", "line:beta")
}

func TestRuntimePersistentLinuxTTYStreamsControlFD(t *testing.T) {
	guestInitCache := t.TempDir()
	t.Setenv("CCX3_VM_TEST_GUEST_INIT_CACHE_DIR", guestInitCache)
	if err := os.MkdirAll(guestInitCache, 0o755); err != nil {
		t.Fatalf("create stale guest init cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(guestInitCache, "guest-init-linux-"+runtime.GOARCH), []byte("\x7fELFstale-agent"), 0o755); err != nil {
		t.Fatalf("seed stale guest init cache: %v", err)
	}
	env := newRuntimeBootEnv(t)
	manager := NewManagerWithBackend(env.backend)
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()
	const instanceID = "tty-control"
	hostWorkDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize TTY workdir: %v", err)
	}
	guestWorkDir := "/host" + hostWorkDir
	share := client.ShareMount{
		Source:   "/",
		Mount:    "/host",
		Writable: true,
		MapOwner: true,
		OwnerUID: 1000,
		OwnerGID: 1000,
		Cache:    "strict",
	}
	if _, err := manager.StartBlank(ctx, client.StartInstanceRequest{
		ID:       instanceID,
		Image:    env.imageName,
		Shares:   []client.ShareMount{share},
		MemoryMB: env.memoryMB,
		CPUs:     1,
	}); err != nil {
		t.Fatalf("start managed Linux TTY instance: %v", err)
	}
	t.Cleanup(func() { _ = manager.ShutdownAll(context.Background()) })
	inputs := make(chan client.ExecInput, 2)
	var control strings.Builder
	var output strings.Builder
	var exit *int
	stage := 0
	persistentCommand := `__vmsh_uid="$(id -u 2>/dev/null || printf '')"
__vmsh_passwd="$(awk -F: -v u="$__vmsh_uid" '$3 == u { print $1 ":" $6; exit }' /etc/passwd 2>/dev/null || true)"
if [ -n "$__vmsh_passwd" ]; then
  USER="${__vmsh_passwd%%:*}"
  LOGNAME="$USER"
  HOME="${__vmsh_passwd#*:}"
  export USER LOGNAME HOME
fi
unset __vmsh_uid __vmsh_passwd
stty -echo 2>/dev/null || true
alias ls >/dev/null 2>&1 || { ls --color=always -C -w ${COLUMNS:-80} >/dev/null 2>&1 && alias ls='ls --color=always -C -w ${COLUMNS:-80}'; } || { ls -G -C >/dev/null 2>&1 && alias ls='ls -G -C'; } || true

__vmsh_control_fd=3
__vmsh_report() {
  printf '%s\t%s\t%s\n' "$1" "$2" "$PWD" >&$__vmsh_control_fd
}
__vmsh_run() {
  stty echo 2>/dev/null || true
  eval " $1"
  __vmsh_status=$?
  stty -echo 2>/dev/null || true
  __vmsh_report done "$__vmsh_status"
}
__vmsh_report ready 0
while IFS= read -r __vmsh_line; do eval "$__vmsh_line"; done`
	err = manager.RunStreamIn(ctx, instanceID, client.RunRequest{
		Image:   env.imageName,
		Command: []string{"sh", "-lc", persistentCommand},
		Env: []string{
			"HOME=/home/cc",
			"USER=cc",
			"LOGNAME=cc",
			"TERM=dumb",
			"LS_COLORS=" + os.Getenv("LS_COLORS"),
			"NO_COLOR=1",
			"CLICOLOR=1",
		},
		WorkDir:   guestWorkDir,
		User:      "1000:1000",
		Shares:    []client.ShareMount{share},
		TTY:       true,
		ControlFD: true,
		Cols:      120,
		Rows:      30,
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "output":
			output.WriteString(event.Output)
		case "control":
			control.WriteString(event.Output)
			if stage == 0 && strings.Contains(event.Output, "ready\t0\t"+guestWorkDir) {
				stage = 1
				inputs <- client.ExecInput{Kind: "stdin", Data: []byte("__vmsh_run \"printf 'first\\n' >&3\"\n")}
			} else if stage == 1 && strings.Contains(event.Output, "done\t0\t"+guestWorkDir) {
				stage = 2
				inputs <- client.ExecInput{Kind: "resize", Cols: 132, Rows: 41}
				inputs <- client.ExecInput{Kind: "stdin", Data: []byte("__vmsh_run \"stty size >&3\"\n")}
				close(inputs)
			}
		case "exit":
			code := event.ExitCode
			exit = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Linux TTY control fd exec: %v; stdout=%q control=%q", err, output.String(), control.String())
	}
	if stage != 2 || exit == nil || *exit != 0 {
		t.Fatalf("Linux TTY control fd state: stage=%d exit=%v stdout=%q control=%q", stage, exit, output.String(), control.String())
	}
	if got, want := strings.TrimSpace(control.String()), "ready\t0\t"+guestWorkDir+"\nfirst\ndone\t0\t"+guestWorkDir+"\n41 132\ndone\t0\t"+guestWorkDir; got != want {
		t.Fatalf("Linux TTY control fd output = %q", got)
	}
}

func TestRuntimeRestoresPersistentFromStartupSnapshot(t *testing.T) {
	supported := (runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")) ||
		(runtime.GOOS == "darwin" && runtime.GOARCH == "arm64")
	if !supported {
		t.Skip("startup snapshots are implemented on Linux amd64/arm64 and Darwin arm64")
	}
	env := newRuntimeBootEnv(t)
	snapshotRoot := t.TempDir()

	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		SnapshotDir: snapshotRoot,
		MemoryMB:    256,
		CPUs:        1,
	})
	captureConsole := runtimeConsoleHistory(inst)
	if err := inst.Close(); err != nil {
		t.Fatalf("close snapshot capture instance: %v", err)
	}

	snapshotPath := singleSnapshotPath(t, snapshotRoot, captureConsole)
	shareRoot := t.TempDir()
	share := client.ShareMount{Source: shareRoot, Mount: "/host", Writable: true, MapOwner: true, OwnerUID: 1000, OwnerGID: 1000, Cache: "strict"}
	for restore := 0; restore < 3; restore++ {
		restored := startRuntimeInstance(t, env, client.CreateInstanceRequest{
			RestoreSnapshot: snapshotPath,
			MemoryMB:        256,
			CPUs:            1,
			Shares:          []client.ShareMount{share},
		})
		execPersistentControlInRuntime(t, restored)
		if err := restored.Close(); err != nil {
			t.Fatalf("close restored instance %d: %v", restore, err)
		}
	}
}

func TestRuntimeSnapshotAppliesCurrentBalloonTarget(t *testing.T) {
	if !((runtime.GOOS == "linux" && runtime.GOARCH == "amd64") || (runtime.GOOS == "darwin" && runtime.GOARCH == "arm64")) {
		t.Skip("virtio balloon startup snapshots are implemented on Linux amd64 and Darwin arm64")
	}
	env := newRuntimeBootEnv(t)
	snapshotRoot := t.TempDir()

	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		SnapshotDir: snapshotRoot,
		MemoryMB:    256,
		BalloonMB:   160,
		CPUs:        1,
	})
	captureConsole := runtimeConsoleHistory(inst)
	if err := inst.Close(); err != nil {
		t.Fatalf("close snapshot capture instance: %v", err)
	}
	snapshotPath := singleSnapshotPath(t, snapshotRoot, captureConsole)

	data, err := os.ReadFile(filepath.Join(snapshotPath, "manifest.json"))
	if err != nil {
		t.Fatalf("read snapshot manifest: %v", err)
	}
	var manifest struct {
		Devices map[string]struct {
			NumPages    uint32 `json:"num_pages"`
			ActualPages uint32 `json:"actual_pages"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode snapshot manifest: %v", err)
	}
	balloon, ok := manifest.Devices["balloon"]
	if !ok || balloon.NumPages == 0 {
		t.Fatalf("snapshot has no balloon target: %+v", balloon)
	}
	if balloon.ActualPages < balloon.NumPages {
		t.Fatalf("snapshot balloon actual pages = %d, want at least target %d", balloon.ActualPages, balloon.NumPages)
	}

	restored := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		RestoreSnapshot: snapshotPath,
		MemoryMB:        256,
		BalloonMB:       64,
		CPUs:            1,
	})
	defer restored.Close()
	resp := execInRuntime(t, restored, []string{"sh", "-lc", `
for attempt in $(seq 1 100); do
	memory_kb=$(awk '/^MemTotal:/ { print $2 }' /proc/meminfo)
	if test "$memory_kb" -ge 143360; then
		printf 'balloon-retargeted:%s\n' "$memory_kb"
		exit 0
	fi
	sleep 0.02
done
printf 'balloon target was not applied; MemTotal=' >&2
awk '/^MemTotal:/ { print $2 " kB" }' /proc/meminfo >&2
exit 1
`})
	var memoryKB uint64
	if _, err := fmt.Sscanf(strings.TrimSpace(resp.Output), "balloon-retargeted:%d", &memoryKB); err != nil {
		t.Fatalf("parse restored guest memory from %q: %v", resp.Output, err)
	}
	if memoryKB < 143360 {
		t.Fatalf("restored guest memory = %d kB, want at least 143360 kB after applying the current balloon target", memoryKB)
	}
}

func TestRuntimePersistentRejectsRuntimeMountConflicts(t *testing.T) {
	env := newRuntimeBootEnv(t)
	sourceA := t.TempDir()
	sourceB := t.TempDir()
	mustWriteFile(t, filepath.Join(sourceA, "a.txt"), "a")
	mustWriteFile(t, filepath.Join(sourceB, "b.txt"), "b")

	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{})
	defer inst.Close()
	adder, ok := inst.(runtimeImageAdder)
	if !ok {
		t.Fatalf("runtime instance does not support image mounts")
	}

	share := client.ShareMount{Source: sourceA, Mount: "/mnt/runtime", Writable: true}
	addShareWithTimeout(t, inst, share)
	addShareWithTimeout(t, inst, share)

	conflictingShare := share
	conflictingShare.Source = sourceB
	addShareExpectError(t, inst, conflictingShare)

	addImageExpectError(t, adder, share.Mount, env.openImage(t, env.imageName))

	imageMount := "/.ccx3/images/conflict"
	addImageWithTimeout(t, adder, imageMount, env.openImage(t, env.imageName))
	addImageWithTimeout(t, adder, imageMount, env.openImage(t, env.imageName))

	addImageExpectError(t, adder, imageMount, env.openImage(t, env.altImageName))

	imagePathShare := client.ShareMount{Source: sourceB, Mount: imageMount, Writable: false}
	addShareExpectError(t, inst, imagePathShare)
}

func TestRuntimePersistentCancelAndCloseDuringLongRunningExec(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{})
	closed := false
	defer func() {
		if !closed {
			_ = inst.Close()
		}
	}()

	execCtx, cancelExec := context.WithCancel(context.Background())
	defer cancelExec()
	ready := make(chan struct{})
	done := make(chan error, 1)
	var readyOnce sync.Once
	var output bytes.Buffer
	go func() {
		done <- inst.ExecStream(execCtx, client.ExecRequest{
			Command: []string{"sh", "-lc", "set -eu; printf 'long-running-ready\n'; sleep 30; printf 'should-not-print\n'"},
		}, nil, func(event client.ExecEvent) error {
			if event.Kind == "stdout" || event.Kind == "stderr" {
				writeExecEventOutput(&output, event)
				if strings.Contains(output.String(), "long-running-ready") {
					readyOnce.Do(func() { close(ready) })
				}
			}
			return nil
		})
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("long-running exec returned before readiness marker: %v\noutput:\n%s", err, output.String())
	case <-time.After(runtimeExecTimeout()):
		t.Fatalf("timed out waiting for long-running exec readiness\noutput:\n%s", output.String())
	}

	cancelExec()
	if err := inst.Close(); err != nil {
		t.Fatalf("close persistent guest during canceled exec: %v", err)
	}
	closed = true

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled exec error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for canceled exec to return\noutput:\n%s", output.String())
	}
	requireGuestOutput(t, output.String(), "long-running-ready")
	if strings.Contains(output.String(), "should-not-print") {
		t.Fatalf("long-running exec continued after cancellation\noutput:\n%s", output.String())
	}
}

func TestRuntimeGuestPoweroffTerminatesSessionAndActiveCommand(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{})
	defer inst.Close()
	execDone := make(chan error, 1)
	go func() {
		_, err := inst.Exec(context.Background(), client.ExecRequest{Command: []string{"poweroff", "-f"}, User: "root"})
		execDone <- err
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- inst.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("guest poweroff was classified as a crash: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guest poweroff left the VM session running")
	}
	select {
	case <-execDone:
	case <-time.After(time.Second):
		t.Fatal("guest poweroff left the initiating command running")
	}
}

func TestRuntimePersistentLifecycleRemainsResponsiveDuringBulkOutput(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{})
	defer inst.Close()

	const producers = 8
	start := make(chan struct{})
	done := make(chan error, producers)
	for range producers {
		go func() {
			<-start
			done <- inst.ExecStream(context.Background(), client.ExecRequest{
				Command: []string{"sh", "-lc", "head -c 4194304 /dev/zero"},
			}, nil, func(client.ExecEvent) error { return nil })
		}()
	}
	close(start)
	time.Sleep(25 * time.Millisecond)

	controlCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := inst.Exec(controlCtx, client.ExecRequest{Command: []string{"/bin/true"}})
	if err != nil {
		t.Fatalf("control command was starved by bulk output: %v", err)
	}
	if response.ExitCode != 0 {
		t.Fatalf("control command exit = %d", response.ExitCode)
	}
	for range producers {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("bulk output command: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("bulk output command did not complete")
		}
	}
}

func TestRuntimePersistentLinuxExercisesRuntimeFeatures(t *testing.T) {
	env := newRuntimeBootEnv(t)
	readOnlyShare := t.TempDir()
	writableShare := t.TempDir()
	lateShare := t.TempDir()
	mustWriteFile(t, filepath.Join(readOnlyShare, "host.txt"), "read-only-share\n")
	mustWriteFile(t, filepath.Join(lateShare, "late.txt"), "late-share\n")

	bootEvents := &bootEventRecorder{}
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		Shares: []client.ShareMount{
			{Source: readOnlyShare, Mount: "/mnt/ro", Writable: false},
			{Source: writableShare, Mount: "/mnt/rw", Writable: true, Cache: "aggressive"},
		},
	}, bootEvents.Record)
	defer inst.Close()
	bootEvents.RequireKind(t, "status")

	shareEnv := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{
			"sh",
			"-lc",
			"set -eu; printf 'runtime-features\n'; pwd; printf 'env=%s/%s\n' \"$RUNTIME_BASE\" \"$RUNTIME_EXTRA\"; cat /mnt/ro/host.txt; if printf denied >/mnt/ro/denied 2>/tmp/ro.err; then exit 41; fi; printf guest-write >/mnt/rw/guest.txt; cat /mnt/rw/guest.txt",
		},
		Env:     []string{"RUNTIME_BASE=base", "RUNTIME_EXTRA=extra"},
		WorkDir: "/tmp",
	})
	requireGuestOutput(t, shareEnv.Output, "runtime-features", "/tmp", "env=base/extra", "read-only-share", "guest-write")
	if got := mustReadHostFile(t, filepath.Join(writableShare, "guest.txt")); got != "guest-write" {
		t.Fatalf("writable share did not receive guest write: %q", got)
	}

	addShareWithTimeout(t, inst, client.ShareMount{Source: lateShare, Mount: "/mnt/late", Writable: true})
	late := execInRuntime(t, inst, []string{
		"sh",
		"-lc",
		"set -eu; cat /mnt/late/late.txt; printf late-write >/mnt/late/from-guest.txt",
	})
	requireGuestOutput(t, late.Output, "late-share")
	if got := mustReadHostFile(t, filepath.Join(lateShare, "from-guest.txt")); got != "late-write" {
		t.Fatalf("runtime share did not receive guest write: %q", got)
	}

	user := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; printf 'uid=%s gid=%s\n' \"$(id -u)\" \"$(id -g)\"; printf user-write >/tmp/user-owned; test -s /tmp/user-owned"},
		User:    "1234:1235",
	})
	requireGuestOutput(t, user.Output, "uid=1234 gid=1235")

	rootMount := execInRuntime(t, inst, []string{
		"sh",
		"-lc",
		"set -eu; awk '$4 == \"/\" && $5 == \"/\" { found = 1 } END { exit found ? 0 : 1 }' /proc/self/mountinfo; df -k / >/tmp/root-df; test -s /tmp/root-df; printf 'root-mount-ok\n'",
	})
	requireGuestOutput(t, rootMount.Output, "root-mount-ok")

	alt := runInRuntimeInstance(t, env, inst, client.RunRequest{
		Image:   env.altImageName,
		Command: []string{"sh", "-lc", "set -eu; test -r /etc/alpine-release; printf 'image-run-in-instance\n'; pwd"},
		WorkDir: "/",
	})
	requireGuestOutput(t, alt.Output, "image-run-in-instance", "/")

	altStream := execInRuntimeInstanceStream(t, env, inst, client.ExecRequest{
		Image:   env.altImageName,
		Command: []string{"sh", "-lc", "set -eu; printf 'image-exec-stream\n'; cat /etc/alpine-release"},
	})
	requireGuestOutput(t, altStream, "image-exec-stream")

	streamOutput := execStreamInRuntime(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; while IFS= read -r line; do printf 'line:%s\n' \"$line\"; done"},
	}, execStreamInput("alpha\n", "beta\n"), 0)
	requireGuestOutput(t, streamOutput, "line:alpha", "line:beta")

	if runtime.GOOS != "windows" {
		runRuntimeControl(t, inst, client.ExecRequest{Kind: "fs_mkdir", Path: "/runtime-control", User: "0:0"}, 0)
		runRuntimeControl(t, inst, client.ExecRequest{Kind: "fs_write", Path: "/runtime-control/file.txt", Stdin: []byte("control-write"), User: "0:0"}, 0)
		archive := runRuntimeControl(t, inst, client.ExecRequest{Kind: "fs_archive", Path: "/runtime-control", User: "0:0"}, 0)
		requireTarFile(t, archive, "runtime-control/file.txt", "control-write")
		runRuntimeControl(t, inst, client.ExecRequest{
			Kind:      "fs_extract",
			Path:      "/runtime-control",
			Directory: true,
			Stdin:     tarPayload(t, map[string]string{"extract.txt": "extracted"}),
			User:      "0:0",
		}, 0)
		extracted := execInRuntime(t, inst, []string{"sh", "-lc", "set -eu; cat /runtime-control/extract.txt"})
		requireGuestOutput(t, extracted.Output, "extracted")
	}

	writtenRoot := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; printf snapshot-root >/runtime-snapshot.txt; sync"},
		User:    "0:0",
	})
	if writtenRoot.ExitCode != 0 {
		t.Fatalf("write root snapshot fixture exited with %d", writtenRoot.ExitCode)
	}
	flushRuntimeInstance(t, inst)
	rootSnapshot, err := inst.(rootSnapshotProvider).RootSnapshot()
	if err != nil {
		t.Fatalf("snapshot guest root filesystem: %v", err)
	}
	if got := readImageFile(t, rootSnapshot, "/runtime-snapshot.txt"); got != "snapshot-root" {
		t.Fatalf("root snapshot mismatch: %q", got)
	}

	imageSnapshot, err := inst.(imageSnapshotProvider).SnapshotImage(env.altImageName)
	if err != nil {
		t.Fatalf("snapshot mounted image: %v", err)
	}
	if got := readImageFile(t, imageSnapshot, "/etc/alpine-release"); strings.TrimSpace(got) == "" {
		t.Fatalf("mounted image snapshot did not include Alpine release")
	}

	stats := inst.(virtioFSStatsProvider).VirtioFSStats()
	if len(stats) == 0 {
		t.Fatalf("expected virtio-fs stats after filesystem activity")
	}
	var fuseRequests uint64
	for _, stat := range stats {
		fuseRequests += stat.FUSERequests
	}
	if fuseRequests == 0 {
		t.Fatalf("expected virtio-fs FUSE requests after filesystem activity: %+v", stats)
	}

	ttyOutput := execStreamInRuntime(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", "set -eu; stty size; printf 'tty-ok\n'"},
		TTY:     true,
		Cols:    77,
		Rows:    33,
	}, nil, 0)
	requireGuestOutput(t, ttyOutput, "33 77", "tty-ok")

	signalOutput := execStreamSignalOnOutput(t, inst)
	requireGuestOutput(t, signalOutput, "signal-ready", "got-term")
}

func TestRuntimeBootsLinuxWithVirtioDevicesNetworkAndSMP(t *testing.T) {
	env := newRuntimeBootEnv(t)
	cpus := 1
	if runtimeBootContains(env.caps.ResourceLimits, "cpus") {
		cpus = 2
	}
	networkEnabled := runtimeBootContains(env.caps.NetworkModes, "user")
	var network *client.NetworkConfig
	if networkEnabled {
		network = &client.NetworkConfig{Enabled: true}
	}

	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		CPUs:    cpus,
		Network: network,
	})
	defer inst.Close()

	command := "set -eu; printf 'runtime-devices\n'; cpus=$(grep -c '^processor' /proc/cpuinfo); test \"$cpus\" -ge " + strconv.Itoa(cpus) + "; printf 'cpus=%s\n' \"$cpus\"; dd if=/dev/hwrng of=/tmp/runtime-rng bs=16 count=1 2>/dev/null; test \"$(wc -c </tmp/runtime-rng)\" = 16; printf 'rng=ok\n'"
	if networkEnabled {
		command += "; test -d /sys/class/net/eth0; mac=$(cat /sys/class/net/eth0/address); test -n \"$mac\"; grep -q '^nameserver 10.42.0.1$' /etc/resolv.conf; printf 'net=%s\n' \"$mac\""
	}
	resp := execInRuntimeRequest(t, inst, client.ExecRequest{
		Command: []string{"sh", "-lc", command},
		User:    "0:0",
	})
	requireGuestOutput(t, resp.Output, "runtime-devices", "cpus=", "rng=ok")
	if networkEnabled {
		requireGuestOutput(t, resp.Output, "net=")
		ipProvider, ok := inst.(networkIPv4Provider)
		if !ok {
			t.Fatalf("network-enabled instance does not report an IPv4 address")
		}
		if ip := strings.TrimSpace(ipProvider.NetworkIPv4()); ip == "" {
			t.Fatalf("network-enabled instance reported an empty IPv4 address")
		}
	}
}

func TestRuntimeIsolatedGuestCanReachAllowedServiceProxyPort(t *testing.T) {
	env := newRuntimeBootEnv(t)
	if !runtimeBootContains(env.caps.NetworkModes, "user") {
		t.Skip("runtime backend does not support user networking")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen host proxy fixture: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/probe" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, "proxy-ok\n")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("host proxy fixture serve: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()
	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:    env.imageName,
		MemoryMB: env.memoryMB,
		CPUs:     1,
		Network: &client.NetworkConfig{
			Enabled:                  true,
			AllowInternet:            true,
			BlockHostAccess:          true,
			AllowedServiceProxyPorts: []int{port},
		},
		Command: []string{
			"sh",
			"-lc",
			fmt.Sprintf("set -eu; busybox wget -T 5 -qO- http://10.42.0.100:%d/probe", port),
		},
	})
	requireRunResponse(t, resp, err, 0)
	requireGuestOutput(t, resp.Output, "proxy-ok")
}

func TestRuntimeIsolatedAllowedServiceProxyStreamsFlushedResponse(t *testing.T) {
	env := newRuntimeBootEnv(t)
	if !runtimeBootContains(env.caps.NetworkModes, "user") {
		t.Skip("runtime backend does not support user networking")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen streaming host proxy fixture: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/events" {
				http.NotFound(w, r)
				return
			}
			flusher, ok := w.(http.Flusher)
			if !ok {
				http.Error(w, "streaming unsupported", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: alpha\n\n")
			flusher.Flush()
			time.Sleep(100 * time.Millisecond)
			_, _ = io.WriteString(w, "data: omega\n\n")
			flusher.Flush()
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("streaming host proxy fixture serve: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()
	resp, err := env.backend.Run(ctx, client.RunRequest{
		Image:    env.imageName,
		MemoryMB: env.memoryMB,
		CPUs:     1,
		Network: &client.NetworkConfig{
			Enabled:                  true,
			AllowInternet:            true,
			BlockHostAccess:          true,
			AllowedServiceProxyPorts: []int{port},
		},
		Command: []string{
			"sh",
			"-lc",
			fmt.Sprintf("set -eu; busybox wget -T 10 -qO- http://10.42.0.100:%d/events >/tmp/events; grep -q 'data: alpha' /tmp/events; grep -q 'data: omega' /tmp/events; printf 'stream-ok\\n'", port),
		},
	})
	requireRunResponse(t, resp, err, 0)
	requireGuestOutput(t, resp.Output, "stream-ok")
}

func TestRuntimeIsolatedRunningGuestCanReachDynamicallyAllowedServiceProxyPort(t *testing.T) {
	env := newRuntimeBootEnv(t)
	if !runtimeBootContains(env.caps.NetworkModes, "user") {
		t.Skip("runtime backend does not support user networking")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen dynamic host proxy fixture: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/probe" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, "dynamic-proxy-ok\n")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("dynamic host proxy fixture serve: %v", err)
		}
	})

	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{
		Network: &client.NetworkConfig{
			Enabled:         true,
			AllowInternet:   true,
			BlockHostAccess: true,
		},
	})
	defer inst.Close()
	allower, ok := inst.(serviceProxyPortAllower)
	if !ok {
		t.Fatalf("runtime instance does not support dynamic service proxy allowlist")
	}
	if err := allower.AllowServiceProxyPort(context.Background(), port); err != nil {
		t.Fatalf("allow service proxy port: %v", err)
	}

	resp := execInRuntime(t, inst, []string{
		"sh",
		"-lc",
		fmt.Sprintf("set -eu; busybox wget -T 5 -qO- http://10.42.0.100:%d/probe", port),
	})
	requireGuestOutput(t, resp.Output, "dynamic-proxy-ok")
}

func TestRuntimePersistentLinuxConsoleHistoryWithDmesg(t *testing.T) {
	env := newRuntimeBootEnv(t)
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{Dmesg: true})
	defer inst.Close()

	execInRuntime(t, inst, []string{"sh", "-lc", "true"})
	historyProvider, ok := inst.(consoleHistoryProvider)
	if !ok {
		t.Fatalf("runtime instance does not expose console history")
	}
	history, err := historyProvider.ConsoleHistory(context.Background())
	if err != nil {
		t.Fatalf("read console history: %v", err)
	}
	requireGuestOutput(t, history, "ccx3-init")
}

func TestRuntimeBootsLinuxWithNestedVirtualizationWhenSupported(t *testing.T) {
	env := newRuntimeBootEnv(t)
	if !env.caps.SupportsNestedVirt {
		t.Skipf("nested virtualization is not supported on %s", env.caps.Host)
	}
	inst := startRuntimeInstance(t, env, client.CreateInstanceRequest{NestedVirt: true})
	defer inst.Close()

	resp := execInRuntime(t, inst, []string{
		"sh",
		"-lc",
		"set -eu; printf 'runtime-nested\n'; cat /proc/sys/kernel/ostype; test -r /proc/cpuinfo",
	})
	requireGuestOutput(t, resp.Output, "runtime-nested", "Linux")
}

func newRuntimeBootEnv(t *testing.T) *runtimeBootEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping VM runtime boot test in short mode")
	}
	if err := Supports(); err != nil {
		t.Skipf("VM runtime unsupported on this host: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), runtimePrepareTimeout())
	defer cancel()

	root := runtimeBootRepoRoot()
	cacheRoot := runtimeBootCacheRoot(t)
	t.Setenv("CCX3_OCI_SHARED_CACHE_DIR", filepath.Join(cacheRoot, "oci-shared"))
	for _, name := range []string{
		"CCX3_WORKER_BOOT_SOCKET",
		"CCX3_WORKER_FS_SOCKET",
		"CCX3_WORKER_NET_SOCKET",
		"CCX3_WORKER_NET_IPV4",
		"CCX3_WORKER_NET_MAC",
	} {
		t.Setenv(name, "")
	}

	kernelRoot := filepath.Join(cacheRoot, "kernel")
	kernelManager := alpine.NewManager(kernelRoot)
	if err := kernelManager.EnsureWithProgress(ctx, nil); err != nil {
		t.Fatalf("prepare Alpine Linux kernel: %v", err)
	}

	store := oci.NewStore(filepath.Join(t.TempDir(), "images"))
	architecture := runtime.GOARCH
	fixtureName := "alpine.simg"
	if architecture == "arm64" {
		fixtureName = "alpine-arm64.simg"
	}
	fixture := filepath.Join(root, "fixtures", fixtureName)
	for _, name := range []string{runtimeBootImage, runtimeBootAltImage} {
		if _, err := store.Pull(ctx, name, fixture, oci.PullOptions{Architecture: architecture}); err != nil {
			t.Fatalf("import Alpine SIMG fixture as %s: %v", name, err)
		}
	}

	guestInitCache := filepath.Join(cacheRoot, "guestinit")
	if override := strings.TrimSpace(os.Getenv("CCX3_VM_TEST_GUEST_INIT_CACHE_DIR")); override != "" {
		guestInitCache = override
	}
	return &runtimeBootEnv{
		backend:        NewRuntimeBackend(kernelManager, store, guestInitCache),
		images:         store,
		kernelRoot:     kernelRoot,
		guestInitCache: guestInitCache,
		imageName:      runtimeBootImage,
		altImageName:   runtimeBootAltImage,
		memoryMB:       768,
		caps:           HostCapabilities(),
	}
}

func (e *runtimeBootEnv) openImage(t *testing.T, name string) *oci.Image {
	t.Helper()
	image, err := e.images.Open(name)
	if err != nil {
		t.Fatalf("open image %s: %v", name, err)
	}
	if image == nil || image.RootFS == nil {
		t.Fatalf("image %s has no root filesystem", name)
	}
	return image
}

func startRuntimeInstance(t *testing.T, env *runtimeBootEnv, req client.CreateInstanceRequest, onEvent ...func(client.BootEvent) error) Instance {
	t.Helper()
	if strings.TrimSpace(req.Image) == "" {
		req.Image = env.imageName
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = env.memoryMB
	}
	if req.CPUs == 0 {
		req.CPUs = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeBootTimeout())
	defer cancel()
	var events func(client.BootEvent) error
	if len(onEvent) > 0 {
		events = onEvent[0]
	}
	inst, err := env.backend.StartStream(ctx, req, events)
	if err != nil {
		t.Fatalf("boot persistent Linux guest: %v", err)
	}
	return inst
}

func singleSnapshotPath(t *testing.T, root, console string) string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read snapshot root: %v", err)
	}
	var snapshots []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "snapshot-") {
			snapshots = append(snapshots, filepath.Join(root, entry.Name()))
		}
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshot count = %d, want 1\nconsole:\n%s", len(snapshots), console)
	}
	if _, err := os.Stat(filepath.Join(snapshots[0], "manifest.json")); err != nil {
		t.Fatalf("stat snapshot manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(snapshots[0], "memory.bin")); err != nil {
		t.Fatalf("stat snapshot memory: %v", err)
	}
	return snapshots[0]
}

func runInRuntimeInstance(t *testing.T, env *runtimeBootEnv, inst Instance, req client.RunRequest) client.ExecResponse {
	t.Helper()
	if req.MemoryMB == 0 {
		req.MemoryMB = env.memoryMB
	}
	if req.CPUs == 0 {
		req.CPUs = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	resp, err := env.backend.RunInInstance(ctx, inst, env.imageName, req)
	if err != nil {
		t.Fatalf("run image %q in existing instance: %v\nconsole:\n%s", req.Image, err, runtimeConsoleHistory(inst))
	}
	if resp.ExitCode != 0 {
		t.Fatalf("run image %q exited with %d\noutput:\n%s", req.Image, resp.ExitCode, resp.Output)
	}
	return resp
}

func execInRuntimeInstanceStream(t *testing.T, env *runtimeBootEnv, inst Instance, req client.ExecRequest) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	var output bytes.Buffer
	var exitCode *int
	err := env.backend.ExecInInstanceStream(ctx, inst, env.imageName, req, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "stderr", "control":
			writeExecEventOutput(&output, event)
		case "exit":
			code := event.ExitCode
			exitCode = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream exec image %q in existing instance: %v\noutput:\n%s\nconsole:\n%s", req.Image, err, output.String(), runtimeConsoleHistory(inst))
	}
	if exitCode == nil {
		t.Fatalf("stream exec image %q did not report an exit\noutput:\n%s", req.Image, output.String())
	}
	if *exitCode != 0 {
		t.Fatalf("stream exec image %q exited with %d\noutput:\n%s", req.Image, *exitCode, output.String())
	}
	return output.String()
}

func execInRuntime(t *testing.T, inst Instance, command []string) client.ExecResponse {
	t.Helper()
	return execInRuntimeRequest(t, inst, client.ExecRequest{Command: command})
}

func execInRuntimeRequest(t *testing.T, inst Instance, req client.ExecRequest) client.ExecResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	resp, err := inst.Exec(ctx, req)
	if err != nil {
		t.Fatalf("run guest command %q: %v\nconsole:\n%s", req.Command, err, runtimeConsoleHistory(inst))
	}
	if resp.ExitCode != 0 {
		t.Fatalf("guest command %q exited with %d\noutput:\n%s", req.Command, resp.ExitCode, resp.Output)
	}
	return resp
}

func execStreamInput(chunks ...string) <-chan client.ExecInput {
	inputs := make(chan client.ExecInput, len(chunks))
	for _, chunk := range chunks {
		inputs <- client.ExecInput{Kind: "stdin", Input: chunk}
	}
	close(inputs)
	return inputs
}

func execStreamInRuntime(t *testing.T, inst Instance, req client.ExecRequest, inputs <-chan client.ExecInput, wantExit int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	var output bytes.Buffer
	var exitCode *int
	err := inst.ExecStream(ctx, req, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "stderr", "control":
			writeExecEventOutput(&output, event)
		case "exit":
			code := event.ExitCode
			exitCode = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream guest exec %q: %v\noutput:\n%s\nconsole:\n%s", req.Command, err, output.String(), runtimeConsoleHistory(inst))
	}
	if exitCode == nil {
		t.Fatalf("stream guest exec %q did not report an exit\noutput:\n%s", req.Command, output.String())
	}
	if *exitCode != wantExit {
		t.Fatalf("stream guest exec %q exited with %d, want %d\noutput:\n%s", req.Command, *exitCode, wantExit, output.String())
	}
	return output.String()
}

func execPersistentControlInRuntime(t *testing.T, inst Instance) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	inputs := make(chan client.ExecInput, 1)
	ready := false
	var control bytes.Buffer
	var output bytes.Buffer
	var exitCode *int
	err := inst.ExecStream(ctx, client.ExecRequest{
		Command:   []string{"/bin/sh", "-c", "stty -echo; printf 'ready\\n' >&3; IFS= read -r command; eval \"$command\"; status=$?; printf 'complete:%s\\n' \"$status\" >&3; exit \"$status\""},
		ControlFD: true,
		TTY:       true,
		WorkDir:   "/host",
		User:      "1000",
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "control":
			writeExecEventOutput(&control, event)
			if !ready {
				ready = true
				inputs <- client.ExecInput{Kind: "stdin", Data: []byte("printf 'warm-command\\n'; test \"$PWD\" = /host\n")}
				close(inputs)
			}
		case "stdout":
			writeExecEventOutput(&output, event)
		case "exit":
			code := event.ExitCode
			exitCode = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("persistent restored guest shell: %v; control=%q output=%q console=%q", err, control.String(), output.String(), runtimeConsoleHistory(inst))
	}
	if !ready || exitCode == nil || *exitCode != 0 || control.String() != "ready\ncomplete:0\n" || strings.ReplaceAll(output.String(), "\r\n", "\n") != "warm-command\n" {
		t.Fatalf("persistent restored guest shell ready=%t exit=%v control=%q output=%q", ready, exitCode, control.String(), output.String())
	}
}

func execStreamSignalOnOutput(t *testing.T, inst Instance) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	inputs := make(chan client.ExecInput, 1)
	var output bytes.Buffer
	var exitCode *int
	signaled := false
	err := inst.ExecStream(ctx, client.ExecRequest{
		Command: []string{"sh", "-lc", "trap 'echo got-term; exit 7' TERM; echo signal-ready; while :; do sleep 1; done"},
	}, inputs, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "stderr":
			writeExecEventOutput(&output, event)
			if !signaled && strings.Contains(output.String(), "signal-ready") {
				signaled = true
				inputs <- client.ExecInput{Kind: "signal", Signal: "TERM"}
				close(inputs)
			}
		case "exit":
			code := event.ExitCode
			exitCode = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("stream guest signal exec: %v\noutput:\n%s\nconsole:\n%s", err, output.String(), runtimeConsoleHistory(inst))
	}
	if exitCode == nil {
		t.Fatalf("stream guest signal exec did not report an exit\noutput:\n%s", output.String())
	}
	if *exitCode != 7 {
		t.Fatalf("stream guest signal exec exited with %d, want 7\noutput:\n%s", *exitCode, output.String())
	}
	return output.String()
}

func runRuntimeControl(t *testing.T, inst Instance, req client.ExecRequest, wantExit int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	var output bytes.Buffer
	var exitCode *int
	err := inst.ExecStream(ctx, req, nil, func(event client.ExecEvent) error {
		switch event.Kind {
		case "stdout", "stderr":
			writeExecEventOutput(&output, event)
		case "exit":
			code := event.ExitCode
			exitCode = &code
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run guest control request %q path %q: %v\noutput:\n%s\nconsole:\n%s", req.Kind, req.Path, err, output.String(), runtimeConsoleHistory(inst))
	}
	if exitCode == nil {
		t.Fatalf("guest control request %q path %q did not report an exit\noutput:\n%s", req.Kind, req.Path, output.String())
	}
	if *exitCode != wantExit {
		t.Fatalf("guest control request %q path %q exited with %d, want %d\noutput:\n%s", req.Kind, req.Path, *exitCode, wantExit, output.String())
	}
	return output.Bytes()
}

func addShareWithTimeout(t *testing.T, inst Instance, share client.ShareMount) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	if err := inst.AddShare(ctx, share); err != nil {
		t.Fatalf("add runtime share %q: %v", share.Mount, err)
	}
}

func addShareExpectError(t *testing.T, inst Instance, share client.ShareMount) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	err := inst.AddShare(ctx, share)
	if err == nil {
		t.Fatalf("add runtime share %q unexpectedly succeeded", share.Mount)
	}
	return err
}

func addImageWithTimeout(t *testing.T, adder runtimeImageAdder, mount string, image *oci.Image) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	if err := adder.AddImage(ctx, mount, image); err != nil {
		t.Fatalf("add runtime image at %q: %v", mount, err)
	}
}

func addImageExpectError(t *testing.T, adder runtimeImageAdder, mount string, image *oci.Image) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	err := adder.AddImage(ctx, mount, image)
	if err == nil {
		t.Fatalf("add runtime image at %q unexpectedly succeeded", mount)
	}
	return err
}

func flushRuntimeInstance(t *testing.T, inst Instance) {
	t.Helper()
	flusher, ok := inst.(instanceFlushProvider)
	if !ok {
		t.Fatalf("runtime instance does not support flushing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), runtimeExecTimeout())
	defer cancel()
	if err := flusher.Flush(ctx); err != nil {
		t.Fatalf("flush guest filesystem state: %v", err)
	}
}

type bootEventRecorder struct {
	mu     sync.Mutex
	events []client.BootEvent
}

func (r *bootEventRecorder) Record(event client.BootEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *bootEventRecorder) RequireKind(t *testing.T, kind string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		for _, event := range r.events {
			if event.Kind == kind {
				r.mu.Unlock()
				return
			}
		}
		events := append([]client.BootEvent(nil), r.events...)
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("boot events did not include kind %q: %+v", kind, events)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeExecEventOutput(output *bytes.Buffer, event client.ExecEvent) {
	if len(event.Data) > 0 {
		output.Write(event.Data)
		return
	}
	output.WriteString(event.Output)
}

func runtimeConsoleHistory(inst Instance) string {
	historyProvider, ok := inst.(consoleHistoryProvider)
	if !ok {
		return ""
	}
	history, _ := historyProvider.ConsoleHistory(context.Background())
	return history
}

func requireRunResponse(t *testing.T, resp client.ExecResponse, err error, wantExit int) {
	t.Helper()
	if err != nil {
		t.Fatalf("run guest command: %v\noutput:\n%s", err, resp.Output)
	}
	if resp.ExitCode != wantExit {
		t.Fatalf("guest command exited with %d, want %d\noutput:\n%s", resp.ExitCode, wantExit, resp.Output)
	}
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustReadHostFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readImageFile(t *testing.T, root imagefs.Directory, guestPath string) string {
	t.Helper()
	entry, err := imagefs.LookupPath(root, guestPath)
	if err != nil {
		t.Fatalf("lookup snapshot path %s: %v", guestPath, err)
	}
	if entry.File == nil {
		t.Fatalf("snapshot path %s is not a file", guestPath)
	}
	size, _ := entry.File.Stat()
	data, err := entry.File.ReadAt(0, uint32(size))
	if err != nil {
		t.Fatalf("read snapshot path %s: %v", guestPath, err)
	}
	return string(data)
}

func tarPayload(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, contents := range files {
		header := &tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(contents)),
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write tar header %s: %v", name, err)
		}
		if _, err := io.WriteString(tw, contents); err != nil {
			t.Fatalf("write tar contents %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar payload: %v", err)
	}
	return buf.Bytes()
}

func requireTarFile(t *testing.T, archive []byte, name, want string) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	var names []string
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar archive: %v", err)
		}
		names = append(names, header.Name)
		if header.Name != name {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar file %s: %v", name, err)
		}
		if string(data) != want {
			t.Fatalf("tar file %s = %q, want %q", name, string(data), want)
		}
		return
	}
	t.Fatalf("tar archive did not contain %s; entries: %s", name, strings.Join(names, ", "))
}

func requireGuestOutput(t *testing.T, output string, fragments ...string) {
	t.Helper()
	// Runtime tests validate markers emitted by guest commands through
	// unstructured stdout/stderr streams. Whole-output equality would bind these
	// tests to unrelated shell or runtime chatter.
	for _, fragment := range fragments {
		if !strings.Contains(output, fragment) {
			t.Fatalf("guest output does not contain %q\noutput:\n%s", fragment, output)
		}
	}
}

func runtimeBootContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func runtimeBootRepoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("locate test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func runtimeBootCacheRoot(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"CCX3_VM_TEST_CACHE_DIR", "CCX3_HVF_TEST_CACHE_DIR"} {
		if dir := strings.TrimSpace(os.Getenv(name)); dir != "" {
			return dir
		}
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil || cacheRoot == "" {
		return filepath.Join(os.TempDir(), "ccx3-runtime-boot-test")
	}
	return filepath.Join(cacheRoot, "ccx3", "runtime-boot-test")
}

func runtimePrepareTimeout() time.Duration {
	return envDuration([]string{"CCX3_VM_TEST_PREPARE_TIMEOUT", "CCX3_HVF_TEST_PREPARE_TIMEOUT"}, 10*time.Minute)
}

func runtimeBootTimeout() time.Duration {
	return envDuration([]string{"CCX3_VM_TEST_BOOT_TIMEOUT", "CCX3_HVF_TEST_BOOT_TIMEOUT"}, 3*time.Minute)
}

func runtimeExecTimeout() time.Duration {
	return envDuration([]string{"CCX3_VM_TEST_EXEC_TIMEOUT", "CCX3_HVF_TEST_EXEC_TIMEOUT"}, time.Minute)
}

func envDuration(names []string, fallback time.Duration) time.Duration {
	for _, name := range names {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		duration, err := time.ParseDuration(raw)
		if err == nil {
			return duration
		}
		seconds, err := strconv.ParseFloat(raw, 64)
		if err == nil && seconds > 0 {
			return time.Duration(seconds * float64(time.Second))
		}
		fmt.Fprintf(os.Stderr, "invalid %s duration %q; using %s\n", name, raw, fallback)
		return fallback
	}
	return fallback
}
