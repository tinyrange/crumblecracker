//go:build linux

package guestagent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestEnsureCredentialUserCreatesMissingHomeForExistingPasswdUser(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test uses the current unprivileged uid to avoid requiring chown privileges")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	uid := os.Getuid()
	gid := os.Getgid()
	passwd := fmt.Sprintf("ubuntu:x:%d:%d:Ubuntu:/home/ubuntu:/bin/sh\n", uid, gid)
	if err := os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatalf("write passwd: %v", err)
	}
	group := fmt.Sprintf("ubuntu:x:%d:\n", gid)
	if err := os.WriteFile(filepath.Join(root, "etc", "group"), []byte(group), 0o644); err != nil {
		t.Fatalf("write group: %v", err)
	}

	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if err := EnsureCredentialUser(root, cred); err != nil {
		t.Fatalf("ensure user: %v", err)
	}
	home := filepath.Join(root, "home", "ubuntu")
	info, err := os.Stat(home)
	if err != nil {
		t.Fatalf("stat home: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("home is not a directory: %s", home)
	}
}

func TestEnsureCredentialWorkDirCreatesHomeSubdirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test uses the current unprivileged uid to avoid requiring chown privileges")
	}
	root := t.TempDir()
	cred := &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
	if err := EnsureCredentialWorkDir(root, "/home/ubuntu", cred); err != nil {
		t.Fatalf("ensure workdir: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "home", "ubuntu"))
	if err != nil {
		t.Fatalf("stat workdir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("workdir is not a directory")
	}
	if err := EnsureCredentialWorkDir(root, "/home/ubuntu ", cred); err != nil {
		t.Fatalf("ensure spaced workdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "home", "ubuntu ")); err != nil {
		t.Fatalf("stat spaced workdir: %v", err)
	}
	if err := EnsureCredentialWorkDir(root, "/work/project", cred); err != nil {
		t.Fatalf("ensure non-home workdir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "work", "project")); !os.IsNotExist(err) {
		t.Fatalf("non-home workdir stat err = %v, want not created", err)
	}
}

func TestEnsureCredentialWorkDirCreatesRootCommandDirectory(t *testing.T) {
	if os.Geteuid() != 0 && (os.Geteuid() != 1000 || os.Getegid() != 1000) {
		t.Skip("test process cannot assign the default workspace identity")
	}
	root := t.TempDir()
	if err := EnsureCredentialWorkDir(root, "/home/cc", nil); err != nil {
		t.Fatalf("ensure root workdir: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "home", "cc"))
	if err != nil {
		t.Fatalf("stat root workdir: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("root workdir is not a directory")
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1000 || stat.Gid != 1000 {
		t.Fatalf("root command workspace owner = %d:%d, want 1000:1000", stat.Uid, stat.Gid)
	}
}

func TestEnsureCredentialArchiveHomeCreatesOnlyMissingWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 1000, 1000
	}
	cred := &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}
	if err := EnsureCredentialArchiveHome(root, "/home/cc/project/file", cred); err != nil {
		t.Fatalf("create archive workspace: %v", err)
	}
	workspace, err := os.Stat(filepath.Join(root, "home", "cc"))
	if err != nil {
		t.Fatal(err)
	}
	stat := workspace.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != uid || int(stat.Gid) != gid {
		t.Fatalf("workspace owner = %d:%d, want %d:%d", stat.Uid, stat.Gid, uid, gid)
	}
	if _, err := os.Stat(filepath.Join(root, "home", "cc", "project")); !os.IsNotExist(err) {
		t.Fatalf("archive destination parent was created eagerly: %v", err)
	}
	if err := EnsureCredentialArchiveHome(root, "/home/release /file", cred); err != nil {
		t.Fatalf("create spaced archive workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "home", "release ")); err != nil {
		t.Fatalf("stat spaced archive workspace: %v", err)
	}

	protected := filepath.Join(root, "home", "protected")
	if err := os.Mkdir(protected, 0o700); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(protected)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureCredentialArchiveHome(root, "/home/protected/file", cred); err != nil {
		t.Fatalf("inspect existing workspace: %v", err)
	}
	after, err := os.Stat(protected)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() || before.Sys().(*syscall.Stat_t).Uid != after.Sys().(*syscall.Stat_t).Uid || before.Sys().(*syscall.Stat_t).Gid != after.Sys().(*syscall.Stat_t).Gid {
		t.Fatal("existing archive workspace metadata changed")
	}
}

func TestCredentialForUser(t *testing.T) {
	root, err := CredentialForUser("root")
	if err != nil || root != nil {
		t.Fatalf("root credential = %+v, %v; want nil, nil", root, err)
	}
	cred, err := CredentialForUser("1000:1001")
	if err != nil {
		t.Fatalf("CredentialForUser: %v", err)
	}
	if cred == nil || cred.Uid != 1000 || cred.Gid != 1001 {
		t.Fatalf("credential = %+v, want 1000:1001", cred)
	}
	if _, err := CredentialForUser("1000:"); err == nil {
		t.Fatalf("CredentialForUser accepted empty gid")
	}
	if _, err := CredentialForUser("not-a-uid"); err == nil {
		t.Fatalf("CredentialForUser accepted an unknown user")
	}
}

func TestCredentialForUserResolvesGuestAccountNames(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	passwd := "root:x:0:0:root:/root:/bin/sh\n" +
		"nobody:x:65534:65534:nobody:/nonexistent:/sbin/nologin\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatalf("write passwd: %v", err)
	}
	group := "root:x:0:\n" +
		"dialout:x:20:nobody\n" +
		"diagnostics:x:27:nobody\n" +
		"nogroup:x:65534:\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "group"), []byte(group), 0o644); err != nil {
		t.Fatalf("write group: %v", err)
	}

	cred, err := CredentialForUserInRoot(root, "nobody")
	if err != nil {
		t.Fatalf("resolve nobody: %v", err)
	}
	if cred == nil || cred.Uid != 65534 || cred.Gid != 65534 || !equalUint32s(cred.Groups, []uint32{20, 27}) {
		t.Fatalf("nobody credential = %+v, want uid/gid 65534 with supplementary groups 20,27", cred)
	}

	cred, err = CredentialForUserInRoot(root, "nobody:diagnostics")
	if err != nil {
		t.Fatalf("resolve nobody:diagnostics: %v", err)
	}
	if cred == nil || cred.Uid != 65534 || cred.Gid != 27 || !equalUint32s(cred.Groups, []uint32{20}) {
		t.Fatalf("overridden credential = %+v, want uid 65534, gid 27, supplementary group 20", cred)
	}
}

func TestEnvironmentForCredentialReplacesRootDefaults(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatalf("mkdir etc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte("node:x:1000:1000:Node:/home/node:/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write passwd: %v", err)
	}
	cred := &syscall.Credential{Uid: 1000, Gid: 1000}
	env := EnvironmentForCredential(root, []string{"PATH=/bin", "HOME=/root", "USER=root", "CUSTOM=1"}, cred)
	want := map[string]string{"PATH": "/bin", "HOME": "/home/node", "USER": "node", "LOGNAME": "node", "CUSTOM": "1"}
	got := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("identity environment = %#v, want %#v", got, want)
	}

	env = EnvironmentForCredential(root, []string{"HOME=/workspace", "USER=builder", "LOGNAME=builder"}, cred)
	if !reflect.DeepEqual(env, []string{"HOME=/workspace", "USER=builder", "LOGNAME=builder"}) {
		t.Fatalf("explicit identity environment was replaced: %#v", env)
	}
}

func equalUint32s(got, want []uint32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
