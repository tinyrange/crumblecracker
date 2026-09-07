//go:build linux

package guestagent

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

func CredentialForUser(user string) (*syscall.Credential, error) {
	return CredentialForUserInRoot("", user)
}

func CredentialForUserInRoot(rootDir, user string) (*syscall.Credential, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return nil, nil
	}
	if user == "root" || user == "0" || user == "0:0" {
		return nil, nil
	}
	uidPart, gidPart, hasGID := strings.Cut(user, ":")
	if uidPart == "" || (hasGID && (gidPart == "" || strings.Contains(gidPart, ":"))) {
		return nil, fmt.Errorf("invalid user %q", user)
	}
	uid, err := parseUint32(uidPart)
	name := ""
	gid := uid
	if err != nil {
		var ok bool
		name, uid, gid, ok = passwdIdentityForName(rootDir, uidPart)
		if !ok {
			return nil, fmt.Errorf("unknown user %q", uidPart)
		}
	}
	if hasGID {
		gid, err = groupIDForNameOrID(rootDir, gidPart)
		if err != nil {
			return nil, err
		}
	}
	groups := supplementaryGroupIDs(rootDir, name, gid)
	if uid == 0 && gid == 0 && len(groups) == 0 {
		return nil, nil
	}
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}, nil
}

func EnsureCredentialUser(rootDir string, cred *syscall.Credential) error {
	if cred == nil || cred.Uid == 0 {
		return nil
	}
	rootDir = strings.TrimRight(rootDir, "/")
	uid := fmt.Sprintf("%d", cred.Uid)
	gid := fmt.Sprintf("%d", cred.Gid)
	name := usernameForUID(rootDir, uid)
	if name == "" {
		name = availableUserName(rootDir, "cc")
	}
	homeDir := homeDirForUID(rootDir, uid)
	if homeDir == "" {
		homeDir = "/home/" + name
	}
	group := groupNameForGID(rootDir, gid)
	if group == "" {
		group = availableGroupName(rootDir, name)
		if err := appendGroupEntry(rootDir, group, gid); err != nil {
			return err
		}
	}
	if name != "" && passwdHasUID(rootDir, uid) {
		return ensureCredentialHome(rootDir, homeDir, cred)
	}
	if err := appendPasswdEntry(rootDir, name, uid, gid, homeDir, "/bin/sh"); err != nil {
		return err
	}
	return ensureCredentialHome(rootDir, homeDir, cred)
}

func EnsureCredentialWorkDir(rootDir, workDir string, cred *syscall.Credential) error {
	if strings.TrimSpace(workDir) == "" {
		return nil
	}
	workDir = filepath.Clean(workDir)
	if workDir == "." || workDir == "/" {
		return nil
	}
	if !strings.HasPrefix(workDir, "/home/") {
		return nil
	}
	// /home/cc is the shared default workspace. Its ownership must not depend
	// on whether a root or unprivileged command happens to use it first.
	if workDir == "/home/cc" && (cred == nil || cred.Uid == 0) {
		cred = &syscall.Credential{Uid: 1000, Gid: 1000}
	}
	return ensureCredentialDirectory(rootDir, workDir, cred)
}

// EnsureCredentialArchiveHome provisions only a missing top-level directory
// below /home for an archive destination. Existing paths are left untouched so
// an archive request cannot acquire access by changing their ownership.
func EnsureCredentialArchiveHome(rootDir, destination string, cred *syscall.Credential) error {
	if cred == nil || cred.Uid == 0 {
		return nil
	}
	if strings.TrimSpace(destination) == "" {
		return nil
	}
	destination = filepath.Clean(destination)
	parts := strings.Split(strings.TrimPrefix(destination, "/"), "/")
	if len(parts) < 2 || parts[0] != "home" || parts[1] == "" {
		return nil
	}
	home := rootPath(rootDir, filepath.Join("/home", parts[1]))
	if _, err := os.Lstat(home); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(home, 0o755); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("mkdir %s: %w", home, err)
	}
	if err := os.Chown(home, int(cred.Uid), int(cred.Gid)); err != nil {
		_ = os.Remove(home)
		return fmt.Errorf("chown %s: %w", home, err)
	}
	return nil
}

func ChownPathForUser(rootDir, target, user string) error {
	cred, err := CredentialForUserInRoot(rootDir, user)
	if err != nil || cred == nil {
		return err
	}
	return os.Chown(target, int(cred.Uid), int(cred.Gid))
}

// EnvironmentForCredential replaces root's conventional identity defaults
// when a command is executed as another guest account. Explicit non-default
// values are preserved.
func EnvironmentForCredential(rootDir string, env []string, cred *syscall.Credential) []string {
	out := append([]string(nil), env...)
	if cred == nil || cred.Uid == 0 {
		return out
	}
	uid := fmt.Sprintf("%d", cred.Uid)
	name := usernameForUID(rootDir, uid)
	if name == "" {
		name = "cc"
	}
	home := homeDirForUID(rootDir, uid)
	if home == "" || home == "/" {
		home = "/home/" + name
	}
	out = replaceIdentityEnv(out, "HOME", home, "/root")
	out = replaceIdentityEnv(out, "USER", name, "root")
	out = replaceIdentityEnv(out, "LOGNAME", name, "root")
	return out
}

func replaceIdentityEnv(env []string, key, value, rootDefault string) []string {
	prefix := key + "="
	for i, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			continue
		}
		if strings.TrimPrefix(entry, prefix) == rootDefault {
			env[i] = prefix + value
		}
		return env
	}
	return append(env, prefix+value)
}

func passwdIdentityForName(rootDir, name string) (string, uint32, uint32, bool) {
	for _, line := range colonFileLines(rootPath(rootDir, "/etc/passwd")) {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		uid, uidErr := parseUint32(fields[2])
		gid, gidErr := parseUint32(fields[3])
		if uidErr != nil || gidErr != nil {
			return "", 0, 0, false
		}
		return fields[0], uid, gid, true
	}
	return "", 0, 0, false
}

func groupIDForNameOrID(rootDir, group string) (uint32, error) {
	if gid, err := parseUint32(group); err == nil {
		return gid, nil
	}
	for _, line := range colonFileLines(rootPath(rootDir, "/etc/group")) {
		fields := strings.Split(line, ":")
		if len(fields) < 3 || fields[0] != group {
			continue
		}
		gid, err := parseUint32(fields[2])
		if err != nil {
			break
		}
		return gid, nil
	}
	return 0, fmt.Errorf("unknown group %q", group)
}

func supplementaryGroupIDs(rootDir, name string, primaryGID uint32) []uint32 {
	if name == "" {
		return nil
	}
	seen := make(map[uint32]struct{})
	for _, line := range colonFileLines(rootPath(rootDir, "/etc/group")) {
		fields := strings.Split(line, ":")
		if len(fields) < 4 || !commaListContains(fields[3], name) {
			continue
		}
		gid, err := parseUint32(fields[2])
		if err != nil || gid == primaryGID {
			continue
		}
		seen[gid] = struct{}{}
	}
	groups := make([]uint32, 0, len(seen))
	for gid := range seen {
		groups = append(groups, gid)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	return groups
}

func commaListContains(list, want string) bool {
	for _, value := range strings.Split(list, ",") {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}

func ensureCredentialHome(rootDir, homeDir string, cred *syscall.Credential) error {
	if strings.TrimSpace(homeDir) == "" || homeDir == "/" {
		return nil
	}
	return ensureCredentialDirectory(rootDir, homeDir, cred)
}

func ensureCredentialDirectory(rootDir, dir string, cred *syscall.Credential) error {
	if strings.TrimSpace(dir) == "" || dir == "/" {
		return nil
	}
	path := rootPath(rootDir, dir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	if cred == nil {
		return nil
	}
	if err := os.Chown(path, int(cred.Uid), int(cred.Gid)); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

func usernameForUID(rootDir, uid string) string {
	for _, line := range colonFileLines(rootPath(rootDir, "/etc/passwd")) {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[2] == uid {
			return fields[0]
		}
	}
	return ""
}

func homeDirForUID(rootDir, uid string) string {
	for _, line := range colonFileLines(rootPath(rootDir, "/etc/passwd")) {
		fields := strings.Split(line, ":")
		if len(fields) >= 6 && fields[2] == uid {
			return fields[5]
		}
	}
	return ""
}

func groupNameForGID(rootDir, gid string) string {
	for _, line := range colonFileLines(rootPath(rootDir, "/etc/group")) {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[2] == gid {
			return fields[0]
		}
	}
	return ""
}

func passwdHasUID(rootDir, uid string) bool {
	return usernameForUID(rootDir, uid) != ""
}

func colonFileLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	out := lines[:0]
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func availableUserName(rootDir, base string) string {
	if !nameExists(rootPath(rootDir, "/etc/passwd"), base) {
		return base
	}
	for i := 1000; ; i++ {
		name := base + itoa(i)
		if !nameExists(rootPath(rootDir, "/etc/passwd"), name) {
			return name
		}
	}
}

func availableGroupName(rootDir, base string) string {
	if !nameExists(rootPath(rootDir, "/etc/group"), base) {
		return base
	}
	for i := 1000; ; i++ {
		name := base + itoa(i)
		if !nameExists(rootPath(rootDir, "/etc/group"), name) {
			return name
		}
	}
}

func nameExists(path, name string) bool {
	for _, line := range colonFileLines(path) {
		fields := strings.Split(line, ":")
		if len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

func appendGroupEntry(rootDir, name, gid string) error {
	path := rootPath(rootDir, "/etc/group")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	return appendLine(path, name+":x:"+gid+":")
}

func appendPasswdEntry(rootDir, name, uid, gid, home, shell string) error {
	path := rootPath(rootDir, "/etc/passwd")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	return appendLine(path, strings.Join([]string{name, "x", uid, gid, "ccvm user", home, shell}, ":"))
}

func appendLine(path, line string) error {
	existing, _ := os.ReadFile(path)
	prefix := ""
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		prefix = "\n"
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, prefix+line+"\n"); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func parseUint32(value string) (uint32, error) {
	if value == "" {
		return 0, fmt.Errorf("not numeric")
	}
	n := uint64(0)
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, fmt.Errorf("not numeric")
		}
		n = n*10 + uint64(ch-'0')
		if n > uint64(^uint32(0)) {
			return 0, fmt.Errorf("out of range")
		}
	}
	return uint32(n), nil
}
