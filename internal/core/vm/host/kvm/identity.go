//go:build linux

package kvm

import (
	"fmt"
	"os"
	osuser "os/user"
	"strconv"
	"strings"

	"github.com/tinyrange/crumblecracker/internal/core/imagefs"
	"github.com/tinyrange/crumblecracker/internal/core/vmruntime"
)

func AddRuntimeIdentityFiles(overlay *imagefs.Overlay, uid, gid int) {
	if overlay == nil {
		return
	}
	identity := runtimeHostIdentity(uid, gid)
	addRuntimeIdentityFilesForUser(overlay, identity)
}

func ResolveRuntimeExecUser(runtimeName, user string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		uid := os.Getuid()
		gid := os.Getgid()
		if uid <= 0 {
			return "0:0", nil
		}
		return strconv.Itoa(uid) + ":" + strconv.Itoa(gid), nil
	}
	if user == "root" || user == "0" || user == "0:0" {
		return "0:0", nil
	}
	uidPart, gidPart, hasGID := strings.Cut(user, ":")
	if !validRuntimeUserComponent(uidPart) {
		return "", fmt.Errorf("invalid user %q", user)
	}
	if hasGID {
		if !validRuntimeUserComponent(gidPart) || strings.Contains(gidPart, ":") {
			return "", fmt.Errorf("invalid user %q", user)
		}
		return uidPart + ":" + gidPart, nil
	}
	if _, err := strconv.ParseUint(uidPart, 10, 32); err != nil {
		return uidPart, nil
	}
	return uidPart + ":" + uidPart, nil
}

func validRuntimeUserComponent(value string) bool {
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
	if !numeric {
		return true
	}
	_, err := strconv.ParseUint(value, 10, 32)
	return err == nil
}

type runtimeIdentity struct {
	Name   string
	UID    int
	GID    int
	Gecos  string
	Home   string
	Shell  string
	Groups []runtimeGroup
}

type runtimeGroup struct {
	Name string
	GID  int
}

func runtimeHostIdentity(uid, gid int) runtimeIdentity {
	name := "ccx3"
	home := "/home/ccx3"
	gecos := "ccx3 user"
	shell := "/bin/sh"
	if u, err := osuser.LookupId(strconv.Itoa(uid)); err == nil {
		if u.Username != "" {
			name = u.Username
		}
		if u.Name != "" {
			gecos = u.Name
		}
		if u.HomeDir != "" {
			home = u.HomeDir
		}
		if parsed := runtimeHostPasswdIdentity(uid); parsed.Shell != "" {
			shell = parsed.Shell
			if parsed.Gecos != "" {
				gecos = parsed.Gecos
			}
		}
	}
	groups := []runtimeGroup{runtimeHostGroup(gid, name)}
	for _, groupID := range runtimeHostGroups() {
		if groupID == gid {
			continue
		}
		groups = append(groups, runtimeHostGroup(groupID, name))
	}
	return runtimeIdentity{
		Name:   name,
		UID:    uid,
		GID:    gid,
		Gecos:  gecos,
		Home:   home,
		Shell:  shell,
		Groups: groups,
	}
}

func runtimeHostPasswdIdentity(uid int) runtimeIdentity {
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return runtimeIdentity{}
	}
	uidText := strconv.Itoa(uid)
	for _, line := range strings.Split(string(passwd), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 7 && fields[2] == uidText {
			return runtimeIdentity{Gecos: fields[4], Shell: fields[6]}
		}
	}
	return runtimeIdentity{}
}

func runtimeHostGroups() []int {
	groupIDs, err := os.Getgroups()
	if err != nil {
		return nil
	}
	return groupIDs
}

func runtimeHostGroup(gid int, fallbackName string) runtimeGroup {
	name := fallbackName
	if g, err := osuser.LookupGroupId(strconv.Itoa(gid)); err == nil && g.Name != "" {
		name = g.Name
	}
	return runtimeGroup{Name: name, GID: gid}
}

func addRuntimeIdentityFilesForUser(overlay *imagefs.Overlay, identity runtimeIdentity) {
	if overlay == nil {
		return
	}
	passwd := readRuntimeTextFile(overlay.Root(), "/etc/passwd")
	group := readRuntimeTextFile(overlay.Root(), "/etc/group")

	if strings.TrimSpace(passwd) == "" {
		passwd = "Root:x:0:0:Root:/Root:/bin/sh\n"
	}
	if strings.TrimSpace(group) == "" {
		group = "Root:x:0:\n"
	}

	_ = overlay.AddFile("/etc/passwd", 0o644, []byte(runtimePasswdContent(passwd, identity)))
	_ = overlay.AddFile("/etc/group", 0o644, []byte(runtimeGroupContent(group, identity)))
}

func readRuntimeTextFile(root imagefs.Directory, guestPath string) string {
	entry, err := imagefs.LookupPath(root, guestPath)
	if err != nil || entry.File == nil {
		return ""
	}
	size, _ := entry.File.Stat()
	if size == 0 {
		return ""
	}
	data, err := entry.File.ReadAt(0, uint32(size))
	if err != nil && len(data) == 0 {
		return ""
	}
	return string(data)
}

func runtimePasswdContent(passwd string, identity runtimeIdentity) string {
	lines := strings.Split(strings.TrimSuffix(passwd, "\n"), "\n")
	uidText := strconv.Itoa(identity.UID)
	for i, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) >= 7 && fields[2] == uidText {
			lines[i] = strings.Join([]string{
				identity.Name,
				"x",
				uidText,
				fields[3],
				identity.Gecos,
				identity.Home,
				fields[6],
			}, ":")
			return strings.Join(append(lines, ""), "\n")
		}
	}
	return ensureTrailingNewline(passwd) + runtimePasswdLine(identity)
}

func runtimePasswdLine(identity runtimeIdentity) string {
	return fmt.Sprintf("%s:x:%d:%d:%s:%s:%s\n", identity.Name, identity.UID, identity.GID, identity.Gecos, identity.Home, identity.Shell)
}

func runtimeGroupContent(group string, identity runtimeIdentity) string {
	group = ensureTrailingNewline(group)
	seen := map[string]bool{}
	for _, hostGroup := range identity.Groups {
		line := fmt.Sprintf("%s:x:%d:%s\n", hostGroup.Name, hostGroup.GID, identity.Name)
		if !seen[line] {
			seen[line] = true
			group += line
		}
	}
	return group
}

func ensureTrailingNewline(value string) string {
	if value == "" || strings.HasSuffix(value, "\n") {
		return value
	}
	return value + "\n"
}

func AddRuntimeHostnameFiles(overlay *imagefs.Overlay) {
	hostname := vmruntime.DefaultHostname("")
	_ = overlay.AddFile("/etc/hostname", 0o644, []byte(hostname+"\n"))
	hosts := "127.0.0.1\tlocalhost " + hostname + "\n::1\tlocalhost ip6-localhost ip6-loopback " + hostname + "\n"
	_ = overlay.AddFile("/etc/hosts", 0o644, []byte(hosts))
}
