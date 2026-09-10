package desktopapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type headlessFake struct {
	mu                   sync.Mutex
	pulls, starts, stops int
	env                  map[string]string
	shares               []headlessShare
	closeGlass           chan struct{}
	failStop             atomic.Bool
}

func (f *headlessFake) Check(context.Context) headlessVirt {
	return headlessVirt{Supported: true, Accessible: true, Backend: "hvf"}
}
func (f *headlessFake) Pull(ctx context.Context, r headlessPullRequest, report func(headlessTransfer)) (headlessImage, error) {
	f.mu.Lock()
	f.pulls++
	f.mu.Unlock()
	report(headlessTransfer{ID: "layer1", Kind: "image_layer", Name: "layer1", State: "downloading", Total: headlessPtr(int64(100)), Completed: 50, NetworkBytes: 50, Branch: "image", PlanningComplete: true})
	return headlessImage{ID: "img_test", Reference: r.Reference, Kernel: headlessKernel{ID: "kernel_test"}}, nil
}
func (f *headlessFake) Start(ctx context.Context, id string, r headlessVMRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.env = r.Env
	f.shares = r.Shares
	return nil
}
func (f *headlessFake) Glass(ctx context.Context, id string, r headlessGlassRequest, ready func(int, int)) error {
	ready(r.Width, r.Height)
	select {
	case <-ctx.Done():
	case <-f.closeGlass:
	}
	return nil
}
func (f *headlessFake) Stop(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.failStop.Load() {
		return context.DeadlineExceeded
	}
	return nil
}
func (f *headlessFake) Status(context.Context, string) (string, error) { return "running", nil }
func (f *headlessFake) Shutdown(ctx context.Context) error {
	if f.failStop.Load() {
		return context.DeadlineExceeded
	}
	return nil
}
func testHeadlessServer(t *testing.T) (*headlessServer, *headlessFake) {
	t.Helper()
	config, err := normalizeConfig(Config{ProductName: "test", Kind: "ndappx", DefaultImage: "example.org/image:tag", DefaultMemoryMB: 256, DefaultCPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	fake := &headlessFake{closeGlass: make(chan struct{}, 1)}
	s := newHeadlessServer(config, t.TempDir(), "127.0.0.1:49152", "test-secret", fake)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		fake.failStop.Store(false)
		if err := s.shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return s, fake
}

var headlessTestSequence atomic.Uint64

func testHeadlessRequest(s *headlessServer, method, path, body, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+s.host+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+s.token)
	r.Header.Set("Content-Type", "application/json")
	if key == "" {
		key = fmt.Sprintf("00000000-0000-4000-8000-%012x", headlessTestSequence.Add(1))
	}
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func waitHeadlessOperation(t *testing.T, s *headlessServer, w *httptest.ResponseRecorder) headlessOperation {
	t.Helper()
	if w.Code != 202 {
		t.Fatalf("accept: %d %s", w.Code, w.Body.String())
	}
	var accepted headlessAcceptance
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		poll := testHeadlessRequest(s, "GET", "/v1/operations/"+accepted.ID, "", "")
		var op headlessOperation
		if err := json.Unmarshal(poll.Body.Bytes(), &op); err != nil {
			t.Fatal(err)
		}
		if op.State == "succeeded" || op.State == "failed" || op.State == "cancelled" {
			return op
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return headlessOperation{}
}
func startHeadlessTestVM(t *testing.T, s *headlessServer) string {
	t.Helper()
	pull := waitHeadlessOperation(t, s, testHeadlessRequest(s, "POST", "/v1/images/pull", `{"reference":"example.org/image:tag"}`, ""))
	if pull.State != "succeeded" {
		t.Fatalf("pull: %+v", pull)
	}
	start := waitHeadlessOperation(t, s, testHeadlessRequest(s, "POST", "/v1/vms", `{"image_id":"img_test","env":{"NEURODESKTOP_VERSION":"2026-07-11","VALUE":"quotes \" $HOME\nsecond line"},"shares":[{"host_path":"/tmp/project","guest_path":"/data"}]}`, ""))
	if start.State != "succeeded" {
		t.Fatalf("start: %+v", start)
	}
	return *start.Resource
}
func TestHeadlessRoutesAuthenticationAndValidation(t *testing.T) {
	s, _ := testHeadlessServer(t)
	for _, tc := range []struct {
		method, path, body string
		status             int
	}{{"GET", "/v1/info", "", 200}, {"GET", "/v1/virtualization", "", 200}, {"GET", "/missing", "", 404}, {"GET", "/v1/images/pull", "", 405}, {"POST", "/v1/images/pull", `{"reference":"x","reference":"y"}`, 400}, {"POST", "/v1/images/pull", `{"reference":null}`, 400}, {"POST", "/v1/images/pull", `{"reference":"x","extra":true}`, 400}, {"POST", "/v1/images/pull", `{} {}`, 400}, {"POST", "/v1/images/pull", `{"reference":"x","timeout_seconds":-1}`, 400}} {
		w := testHeadlessRequest(s, tc.method, tc.path, tc.body, "")
		if w.Code != tc.status {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	for _, tc := range []struct {
		auth, host, origin string
		status             int
	}{{"", "127.0.0.1:49152", "", 401}, {"Bearer wrong", "127.0.0.1:49152", "", 401}, {"Bearer test-secret", "evil.test", "", 403}, {"Bearer test-secret", "127.0.0.1:49152", "http://localhost", 403}} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/v1/info", nil)
		r.Header.Set("Authorization", tc.auth)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("auth: got %d want %d", w.Code, tc.status)
		}
	}
}
func TestHeadlessIdempotencyAndNativeLifecycle(t *testing.T) {
	s, f := testHeadlessServer(t)
	id := startHeadlessTestVM(t, s)
	f.mu.Lock()
	if f.env["VALUE"] != "quotes \" $HOME\nsecond line" || len(f.shares) != 1 || f.shares[0].GuestPath != "/data" {
		t.Errorf("guest options lost: %v %v", f.env, f.shares)
	}
	f.mu.Unlock()
	key := "11111111-1111-4111-8111-111111111111"
	request := testHeadlessRequest(s, "POST", "/v1/vms/"+id+"/glass", `{}`, key)
	op := waitHeadlessOperation(t, s, request)
	if op.State != "succeeded" {
		t.Fatalf("glass: %+v", op)
	}
	replay := testHeadlessRequest(s, "POST", "/v1/vms/"+id+"/glass", `{}`, key)
	if request.Body.String() != replay.Body.String() {
		t.Fatal("retry did not replay original response")
	}
	if w := testHeadlessRequest(s, "POST", "/v1/vms/"+id+"/glass", `{"title":"Other"}`, key); w.Code != 409 {
		t.Fatalf("conflicting retry: %d", w.Code)
	}
	if w := testHeadlessRequest(s, "POST", "/v1/vms/"+id+"/glass", `{}`, ""); w.Code != 409 {
		t.Fatalf("second window: %d", w.Code)
	}
	f.closeGlass <- struct{}{}
	s.mu.Lock()
	done := s.vms[id].done
	s.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Glass did not detach")
	}
	s.mu.Lock()
	if s.vms[id].State != "running" || s.vms[id].Glass.State != "closed" {
		t.Error("window close changed VM lifetime")
	}
	s.mu.Unlock()
	op = waitHeadlessOperation(t, s, testHeadlessRequest(s, "POST", "/v1/vms/"+id+"/glass", `{}`, ""))
	if op.State != "succeeded" {
		t.Fatalf("reopen: %+v", op)
	}
	op = waitHeadlessOperation(t, s, testHeadlessRequest(s, "POST", "/v1/vms/"+id+"/stop", `{}`, ""))
	if op.State != "succeeded" {
		t.Fatalf("stop: %+v", op)
	}
	f.mu.Lock()
	if f.starts != 1 || f.stops != 1 {
		t.Errorf("unexpected boot/stop counts: %d %d", f.starts, f.stops)
	}
	f.mu.Unlock()
}
func TestHeadlessShutdownTimeoutRetainsOwnership(t *testing.T) {
	s, f := testHeadlessServer(t)
	id := startHeadlessTestVM(t, s)
	f.failStop.Store(true)
	stop := waitHeadlessOperation(t, s, testHeadlessRequest(s, "POST", "/v1/vms/"+id+"/stop", `{}`, ""))
	if stop.Error == nil || stop.Error.Code != "shutdown_timeout" {
		t.Fatalf("stop: %+v", stop)
	}
	if w := testHeadlessRequest(s, "POST", "/v1/vms", `{"image_id":"img_test"}`, ""); w.Code != 409 {
		t.Fatalf("reused storage after incomplete stop: %d", w.Code)
	}
	if w := testHeadlessRequest(s, "POST", "/v1/shutdown", `{}`, ""); w.Code != 504 {
		t.Fatalf("shutdown: %d", w.Code)
	}
	if w := testHeadlessRequest(s, "POST", "/v1/images/pull", `{"reference":"example.org/image"}`, ""); w.Code != 503 {
		t.Fatalf("mutation while draining: %d", w.Code)
	}
	f.failStop.Store(false)
	if w := testHeadlessRequest(s, "POST", "/v1/shutdown", `{}`, ""); w.Code != 200 {
		t.Fatalf("retry shutdown: %d", w.Code)
	}
}
func TestHeadlessConcurrentRetryStartsOnePull(t *testing.T) {
	s, f := testHeadlessServer(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := testHeadlessRequest(s, "POST", "/v1/images/pull", `{"reference":"example.org/image"}`, "22222222-2222-4222-8222-222222222222")
			if w.Code != 202 {
				t.Errorf("pull: %d", w.Code)
			}
		}()
	}
	wg.Wait()
	s.jobs.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pulls != 1 {
		t.Errorf("duplicate pulls: %d", f.pulls)
	}
}
func TestHeadlessProgressParallelRatesCacheAndStall(t *testing.T) {
	p := newHeadlessProgressTracker()
	now := time.Unix(1000, 0)
	p.update(headlessTransfer{ID: "image", Kind: "image_layer", Name: "layer", State: "downloading", Total: headlessPtr(int64(1000)), Branch: "image", PlanningComplete: true}, now)
	p.update(headlessTransfer{ID: "kernel", Kind: "kernel", Name: "kernel.apk", State: "downloading", Total: headlessPtr(int64(200)), Branch: "kernel", PlanningComplete: true}, now)
	p.update(headlessTransfer{ID: "cache", Kind: "image_layer", State: "cached", Completed: 10000, Total: headlessPtr(int64(10000)), Branch: "image"}, now)
	p.update(headlessTransfer{ID: "image", Kind: "image_layer", Name: "layer", State: "downloading", Completed: 100, NetworkBytes: 100, Total: headlessPtr(int64(1000)), Branch: "image"}, now.Add(time.Second))
	p.update(headlessTransfer{ID: "kernel", Kind: "kernel", Name: "kernel.apk", State: "downloading", Completed: 50, NetworkBytes: 50, Total: headlessPtr(int64(200)), Branch: "kernel"}, now.Add(time.Second))
	snapshot := p.snapshot(now.Add(time.Second))
	if snapshot.Completed != 150 || *snapshot.Total != 1200 || *snapshot.Rate != 150 || *snapshot.ETA != 7 {
		t.Fatalf("parallel snapshot: %+v", snapshot)
	}
	snapshot = p.snapshot(now.Add(7 * time.Second))
	if *snapshot.Rate != 0 || snapshot.ETA != nil {
		t.Fatalf("stall: %+v", snapshot)
	}
	p.planned["kernel"] = false
	snapshot = p.snapshot(now.Add(8 * time.Second))
	if snapshot.Total != nil || snapshot.ETA != nil {
		t.Fatal("undiscovered work presented as known")
	}
}
