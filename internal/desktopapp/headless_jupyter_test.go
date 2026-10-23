package desktopapp

import (
	"archive/tar"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHeadlessJupyterValidation(t *testing.T) {
	server, _ := testHeadlessServer(t)
	config := server.config
	for _, tc := range []struct {
		name    string
		port    int
		user    string
		network bool
		valid   bool
	}{
		{"loopback", 18888, "jovyan", true, true},
		{"privileged", 80, "jovyan", true, false},
		{"overflow", 65536, "jovyan", true, false},
		{"root", 18888, "root", true, false},
		{"offline", 18888, "jovyan", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := headlessDefaults(config, t.TempDir())
			req.ImageID = "img_test"
			req.User = tc.user
			req.Jupyter = &headlessJupyterRequest{tc.port}
			req.Network = headlessNetwork{Enabled: tc.network}
			if got := req.validate(config); (got == nil) != tc.valid {
				t.Fatalf("validation: %v", got)
			}
		})
	}
}

func TestHeadlessDesktopInspectionRequiresAuthorizationAndPresentedWindow(t *testing.T) {
	server, _ := testHeadlessServer(t)
	id := startHeadlessTestVM(t, server)
	for _, route := range []string{"screenshot", "diagnostics"} {
		request := httptest.NewRequest("GET", "http://"+server.host+"/v1/vms/"+id+"/"+route, nil)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", route, response.Code)
		}
	}
	response := testHeadlessRequest(server, "GET", "/v1/vms/"+id+"/screenshot", "", "")
	if response.Code != 409 {
		t.Fatalf("capture before presentation: %d", response.Code)
	}
}

func TestLocalArchiveMustHaveExactlyOneNativeLinuxImage(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch string
		entries        int
		valid          bool
	}{
		{"native", "linux", runtime.GOARCH, 1, true},
		{"foreign", "linux", "foreign", 1, false},
		{"other-os", "windows", runtime.GOARCH, 1, false},
		{"multiple", "linux", runtime.GOARCH, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, err := os.Create(filepath.Join(t.TempDir(), "image.tar"))
			if err != nil {
				t.Fatal(err)
			}
			writer := tar.NewWriter(file)
			entries := []map[string]string{}
			for i := 0; i < tc.entries; i++ {
				entries = append(entries, map[string]string{"Config": "config.json"})
			}
			manifest, _ := json.Marshal(entries)
			config, _ := json.Marshal(map[string]string{"os": tc.os, "architecture": tc.arch})
			for name, data := range map[string][]byte{"manifest.json": manifest, "config.json": config} {
				if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
					t.Fatal(err)
				}
				if _, err := writer.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			file.Close()
			if err := validateArchivePlatform(file.Name()); (err == nil) != tc.valid {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

func TestLocalArchiveRequestRejectsRelativePaths(t *testing.T) {
	s, _ := testHeadlessServer(t)
	for _, reference := range []string{"docker-archive:relative.tar", "docker-archive:", "docker-archive:/tmp/a.tar#other"} {
		body, _ := json.Marshal(map[string]string{"reference": reference})
		response := testHeadlessRequest(s, "POST", "/v1/images/pull", string(body), "")
		if response.Code == 202 {
			t.Fatalf("accepted %s", reference)
		}
	}
	// Spaces in an absolute local path are not shell arguments and must survive.
	reference := "docker-archive:" + filepath.Join(t.TempDir(), "candidate with spaces.tar")
	body, _ := json.Marshal(map[string]string{"reference": reference})
	op := waitHeadlessOperation(t, s, testHeadlessRequest(s, "POST", "/v1/images/pull", string(body), ""))
	data, _ := json.Marshal(op.Result)
	if !strings.Contains(string(data), "candidate with spaces.tar") {
		t.Fatal("archive path changed")
	}
}
