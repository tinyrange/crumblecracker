//go:build linux

package kvm

import (
	"strings"
	"testing"
)

func TestResolveRuntimeExecUser(t *testing.T) {
	tests := []struct {
		name string
		user string
		want string
	}{
		{name: "root", user: "root", want: "0:0"},
		{name: "uid zero", user: "0", want: "0:0"},
		{name: "uid gid zero", user: "0:0", want: "0:0"},
		{name: "uid only", user: "1234", want: "1234:1234"},
		{name: "uid gid", user: "1234:5678", want: "1234:5678"},
		{name: "name only", user: "nobody", want: "nobody"},
		{name: "name and group", user: "nobody:nogroup", want: "nobody:nogroup"},
		{name: "uid and group", user: "65534:nogroup", want: "65534:nogroup"},
		{name: "name and gid", user: "nobody:65534", want: "nobody:65534"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveRuntimeExecUser("linux test", tc.user)
			if err != nil {
				t.Fatalf("ResolveRuntimeExecUser: %v", err)
			}
			if got != tc.want {
				t.Fatalf("user = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveRuntimeExecUserDefaultsToHostIdentity(t *testing.T) {
	got, err := ResolveRuntimeExecUser("linux test", "")
	if err != nil {
		t.Fatalf("ResolveRuntimeExecUser: %v", err)
	}
	if parts := strings.Split(got, ":"); len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		t.Fatalf("default user = %q, want uid:gid", got)
	}
}

func TestResolveRuntimeExecUserRejectsInvalidUsers(t *testing.T) {
	users := []string{
		":1",
		"1:",
		"user:group:extra",
		"4294967296",
		"1:4294967296",
	}
	for _, user := range users {
		t.Run(user, func(t *testing.T) {
			_, err := ResolveRuntimeExecUser("linux test", user)
			if err == nil {
				t.Fatalf("expected error for user %q", user)
			}
		})
	}
}
