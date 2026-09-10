package desktopapp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tinyrange/crumblecracker/internal/core/oci"
	"io"
	"mime"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type headlessReplay struct {
	request string
	done    chan struct{}
	status  int
	header  http.Header
	body    []byte
}
type headlessServer struct {
	mu           sync.Mutex
	driver       headlessDriver
	config       Config
	defaults     headlessVMRequest
	token, host  string
	closing      bool
	operations   map[string]*headlessOperation
	trackers     map[string]*headlessProgressTracker
	images       map[string]headlessImage
	vms          map[string]*headlessVM
	replays      map[string]*headlessReplay
	ctx          context.Context
	cancel       context.CancelFunc
	jobs         sync.WaitGroup
	shutdownGate chan struct{}
	stopped      chan struct{}
	stopOnce     sync.Once
}

func newHeadlessServer(config Config, storage, host, token string, driver headlessDriver) *headlessServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &headlessServer{config: config, defaults: headlessDefaults(config, storage), host: host, token: token, driver: driver, operations: map[string]*headlessOperation{}, trackers: map[string]*headlessProgressTracker{}, images: map[string]headlessImage{}, vms: map[string]*headlessVM{}, replays: map[string]*headlessReplay{}, ctx: ctx, cancel: cancel, shutdownGate: make(chan struct{}, 1), stopped: make(chan struct{})}
}
func headlessID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}
func headlessJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func headlessHTTPError(w http.ResponseWriter, status int, code, message string) {
	headlessJSON(w, status, map[string]any{"error": headlessFailure(code, message)})
}
func headlessErrorStatus(e *headlessError) int {
	switch e.Code {
	case "invalid_request":
		return 400
	case "not_found":
		return 404
	case "resource_in_use", "invalid_state":
		return 409
	case "unsupported_configuration", "image_not_prepared", "virt_unavailable", "display_unavailable":
		return 422
	case "timeout", "shutdown_timeout":
		return 504
	default:
		return 500
	}
}
func (s *headlessServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth := r.Header.Get("Authorization")
	if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+s.token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		headlessHTTPError(w, 401, "unauthorized", "Valid bearer token required")
		return
	}
	if r.Host != s.host || len(r.Header.Values("Origin")) > 0 {
		headlessHTTPError(w, 403, "forbidden", "Only local process requests are accepted")
		return
	}
	if r.URL.RawQuery != "" || strings.Contains(r.URL.EscapedPath(), "%") {
		headlessHTTPError(w, 400, "invalid_request", "Queries and escaped paths are not supported")
		return
	}
	route, method := headlessRoute(r.URL.Path)
	if route == "" {
		headlessHTTPError(w, 404, "not_found", "Endpoint not found")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		headlessHTTPError(w, 405, "method_not_allowed", "Method not allowed")
		return
	}
	if method == "GET" {
		if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
			headlessHTTPError(w, 400, "invalid_request", "GET requests have no body")
			return
		}
		s.read(w, r, route)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		headlessHTTPError(w, 415, "unsupported_media_type", "Use application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			headlessHTTPError(w, 413, "request_too_large", "JSON body exceeds 1 MiB")
		} else {
			headlessHTTPError(w, 400, "invalid_request", "Cannot read JSON body")
		}
		return
	}
	canonical, err := headlessCanonical(body)
	if err != nil {
		headlessHTTPError(w, 400, "invalid_request", err.Error())
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(r.Header.Values("Idempotency-Key")) != 1 || !headlessUUID.MatchString(key) {
		headlessHTTPError(w, 400, "invalid_request", "Idempotency-Key must be a UUID")
		return
	}
	request := r.Method + " " + r.URL.Path + " " + string(canonical)
	s.mu.Lock()
	if previous := s.replays[key]; previous != nil {
		s.mu.Unlock()
		if previous.request != request {
			headlessHTTPError(w, 409, "idempotency_conflict", "Idempotency key already used for another request")
			return
		}
		select {
		case <-previous.done:
			for k, values := range previous.header {
				w.Header()[k] = values
			}
			w.WriteHeader(previous.status)
			_, _ = w.Write(previous.body)
		case <-r.Context().Done():
		}
		return
	}
	if s.closing && route != "shutdown" {
		s.mu.Unlock()
		headlessHTTPError(w, 503, "shutting_down", "Backend is shutting down")
		return
	}
	replay := &headlessReplay{request: request, done: make(chan struct{})}
	s.replays[key] = replay
	s.mu.Unlock()
	out := &headlessResponse{header: make(http.Header)}
	s.mutate(out, r, route, canonical)
	replay.status = out.status
	replay.header = out.header.Clone()
	replay.body = append([]byte(nil), out.body.Bytes()...)
	close(replay.done)
	for k, values := range out.header {
		w.Header()[k] = values
	}
	w.WriteHeader(out.status)
	_, _ = w.Write(out.body.Bytes())
	if route == "shutdown" && out.status == 200 {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		s.stopOnce.Do(func() { close(s.stopped) })
	}
}

type headlessResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *headlessResponse) Header() http.Header    { return w.header }
func (w *headlessResponse) WriteHeader(status int) { w.status = status }
func (w *headlessResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(p)
}
func headlessRoute(p string) (string, string) {
	switch p {
	case "/v1/info":
		return "info", "GET"
	case "/v1/virtualization":
		return "virt", "GET"
	case "/v1/images/pull":
		return "pull", "POST"
	case "/v1/vms":
		return "start", "POST"
	case "/v1/shutdown":
		return "shutdown", "POST"
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) == 3 && parts[0] == "v1" && parts[2] != "" {
		if parts[1] == "operations" {
			return "operation", "GET"
		}
		if parts[1] == "vms" {
			return "vm", "GET"
		}
	}
	if len(parts) == 4 && parts[0] == "v1" && parts[1] == "vms" && parts[2] != "" {
		switch parts[3] {
		case "glass", "stop":
			return parts[3], "POST"
		}
	}
	return "", ""
}

// Validate recursively before decoding typed DTOs: encoding/json alone accepts
// duplicate keys and silently treats null as a scalar's zero value.
func headlessCanonical(body []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	depth := 0
	var value func() (any, error)
	value = func() (any, error) {
		depth++
		defer func() { depth-- }()
		if depth > 64 {
			return nil, fmt.Errorf("JSON nesting exceeds 64 levels")
		}
		t, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("Invalid JSON")
		}
		if t == nil {
			return nil, fmt.Errorf("Null request values are not supported")
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return t, nil
		}
		switch delim {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, fmt.Errorf("Invalid JSON")
				}
				key, ok := k.(string)
				if !ok {
					return nil, fmt.Errorf("Invalid JSON key")
				}
				if _, exists := m[key]; exists {
					return nil, fmt.Errorf("Duplicate JSON key")
				}
				v, err := value()
				if err != nil {
					return nil, err
				}
				m[key] = v
			}
			if _, err := d.Token(); err != nil {
				return nil, fmt.Errorf("Invalid JSON")
			}
			return m, nil
		case '[':
			a := []any{}
			for d.More() {
				v, err := value()
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
			if _, err := d.Token(); err != nil {
				return nil, fmt.Errorf("Invalid JSON")
			}
			return a, nil
		}
		return nil, fmt.Errorf("Invalid JSON")
	}
	v, err := value()
	if err != nil {
		return nil, err
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("Request must be a JSON object")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("Trailing JSON data")
	}
	return json.Marshal(v)
}
func headlessDecode(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return headlessFailure("invalid_request", "Invalid request fields or types")
	}
	return nil
}
func (s *headlessServer) read(w http.ResponseWriter, r *http.Request, route string) {
	if route == "virt" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		headlessJSON(w, 200, s.driver.Check(ctx))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch route {
	case "info":
		state := "ready"
		if s.closing {
			state = "shutting_down"
		}
		headlessJSON(w, 200, map[string]any{"protocol": "ndappx", "api_version": 1, "version": headlessBuildVersion(), "state": state, "platform": map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH}, "capabilities": map[string]any{"max_active_vms": 1, "native_glass": true, "gpu_acceleration": false}, "defaults": map[string]any{"image": s.config.DefaultImage, "memory_mib": s.defaults.Memory, "cpus": s.defaults.CPUs, "user": s.defaults.User, "home_mode": "persistent", "storage_path": s.defaults.Storage.HostPath}})
	case "operation":
		id := strings.TrimPrefix(r.URL.Path, "/v1/operations/")
		op := s.operations[id]
		if op == nil {
			headlessHTTPError(w, 404, "not_found", "Operation not found")
			return
		}
		if tracker := s.trackers[id]; tracker != nil {
			op.Progress = tracker.snapshot(time.Now())
			op.Phase = tracker.phase(op.State)
		}
		headlessJSON(w, 200, op)
	case "vm":
		id := strings.TrimPrefix(r.URL.Path, "/v1/vms/")
		vm := s.vms[id]
		if vm == nil {
			headlessHTTPError(w, 404, "not_found", "VM not found")
			return
		}
		headlessJSON(w, 200, vm)
	}
}
func (s *headlessServer) accept(w http.ResponseWriter, kind string, resource *string) *headlessOperation {
	op := &headlessOperation{ID: headlessID("op_"), Kind: kind, Resource: resource, State: "queued", Phase: "queued", Created: time.Now().UTC()}
	s.operations[op.ID] = op
	w.Header().Set("Location", "/v1/operations/"+op.ID)
	headlessJSON(w, 202, headlessAcceptance{op.ID, kind, "queued", resource})
	return op
}
func (s *headlessServer) finish(op *headlessOperation, result any, err error, code string) {
	now := time.Now().UTC()
	op.Finished = &now
	op.Phase = "complete"
	op.State = "succeeded"
	op.Result = result
	if err != nil {
		op.Error = asHeadlessError(err, code)
		op.Result = nil
		op.State = "failed"
		if op.Error.Code == "operation_cancelled" {
			op.State = "cancelled"
		}
	}
}
func (s *headlessServer) mutate(w http.ResponseWriter, r *http.Request, route string, body []byte) {
	fail := func(err error) {
		e := asHeadlessError(err, "internal_error")
		headlessJSON(w, headlessErrorStatus(e), map[string]any{"error": e})
	}
	if route == "shutdown" {
		req := struct {
			Timeout int `json:"timeout_seconds"`
		}{30}
		if err := headlessDecode(body, &req); err != nil {
			fail(err)
			return
		}
		if req.Timeout < 1 || req.Timeout > 300 {
			fail(headlessFailure("invalid_request", "Invalid timeout_seconds"))
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(req.Timeout)*time.Second)
		defer cancel()
		if err := s.shutdown(ctx); err != nil {
			e := asHeadlessError(err, "shutdown_failed")
			if errors.Is(err, context.DeadlineExceeded) {
				e = headlessFailure("shutdown_timeout", "Backend cleanup did not complete; retry shutdown")
			}
			fail(e)
			return
		}
		headlessJSON(w, 200, map[string]string{"state": "stopped"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		headlessHTTPError(w, 503, "shutting_down", "Backend is shutting down")
		return
	}
	switch route {
	case "pull":
		req := headlessPullRequest{Platform: "linux/" + runtime.GOARCH, Policy: "if_missing", Timeout: 1800}
		if err := headlessDecode(body, &req); err != nil {
			fail(err)
			return
		}
		if req.Reference == "" || strings.ContainsAny(req.Reference, " \t\n\r?#") || strings.Contains(req.Reference, "://") || (req.Policy != "if_missing" && req.Policy != "refresh") || req.Timeout < 1 || req.Timeout > 86400 {
			fail(headlessFailure("invalid_request", "Invalid image pull request"))
			return
		}
		if _, _, _, err := oci.ParseImageRef(req.Reference); err != nil {
			fail(headlessFailure("invalid_request", "Invalid OCI image reference"))
			return
		}
		if req.Platform != "linux/"+runtime.GOARCH {
			fail(headlessFailure("unsupported_configuration", "Only the native guest architecture is supported"))
			return
		}
		op := s.accept(w, "image_pull", nil)
		tracker := newHeadlessProgressTracker()
		s.trackers[op.ID] = tracker
		s.jobs.Add(1)
		go func() {
			defer s.jobs.Done()
			ctx, cancel := context.WithTimeout(s.ctx, time.Duration(req.Timeout)*time.Second)
			defer cancel()
			s.mu.Lock()
			op.State = "running"
			s.mu.Unlock()
			result, err := s.driver.Pull(ctx, req, func(t headlessTransfer) { s.mu.Lock(); tracker.update(t, time.Now()); s.mu.Unlock() })
			s.mu.Lock()
			defer s.mu.Unlock()
			tracker.finish(err, time.Now())
			if err == nil {
				s.images[result.ID] = result
			}
			s.finish(op, result, err, "image_pull_failed")
		}()
	case "start":
		req := headlessDefaults(s.config, s.defaults.Storage.HostPath)
		if err := headlessDecode(body, &req); err != nil {
			fail(err)
			return
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)
		for _, field := range []struct{ object, key string }{{"storage", "host_path"}} {
			if value, ok := raw[field.object]; ok {
				var nested map[string]json.RawMessage
				_ = json.Unmarshal(value, &nested)
				if _, ok := nested[field.key]; !ok {
					fail(headlessFailure("invalid_request", field.object+"."+field.key+" is required"))
					return
				}
			}
		}
		if value, ok := raw["cvmfs"]; ok {
			var nested map[string]json.RawMessage
			_ = json.Unmarshal(value, &nested)
			if _, ok := nested["cache_limit_bytes"]; ok && req.CVMFS.CacheLimit <= 0 {
				fail(headlessFailure("invalid_request", "cvmfs.cache_limit_bytes must be positive"))
				return
			}
		}
		if err := req.validate(s.config); err != nil {
			fail(err)
			return
		}
		if resolver, ok := s.driver.(interface {
			resolveVMConfig(*headlessVMRequest) error
		}); ok {
			if err := resolver.resolveVMConfig(&req); err != nil {
				fail(err)
				return
			}
		}
		if _, ok := s.images[req.ImageID]; !ok {
			fail(headlessFailure("image_not_prepared", "Pull the image and kernel before boot"))
			return
		}
		for _, v := range s.vms {
			if v.ownsResources {
				fail(headlessFailure("resource_in_use", "A VM still owns runtime resources"))
				return
			}
		}
		id := headlessID("vm_")
		op := s.accept(w, "vm_start", &id)
		ctx, cancel := context.WithTimeout(s.ctx, time.Duration(req.BootTimeout)*time.Second)
		vm := &headlessVM{ID: id, Name: req.Name, ImageID: req.ImageID, State: "starting", ownsResources: true, Config: req, Active: &op.ID, cancel: cancel, done: make(chan struct{})}
		s.vms[id] = vm
		s.jobs.Add(1)
		go func() {
			defer s.jobs.Done()
			defer close(vm.done)
			defer cancel()
			s.mu.Lock()
			op.State = "running"
			op.Phase = "booting"
			s.mu.Unlock()
			err := s.driver.Start(ctx, id, req)
			released := false
			if err != nil {
				cleanup, finishCleanup := context.WithTimeout(context.Background(), 15*time.Second)
				released = s.driver.Stop(cleanup, id) == nil
				finishCleanup()
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if vm.State == "starting" {
				vm.State = "running"
				if err != nil {
					vm.ownsResources = !released
					vm.State = "failed"
					vm.LastError = asHeadlessError(err, "boot_failed")
				}
				vm.Active = nil
			}
			s.finish(op, map[string]string{"vm_id": id, "state": "running"}, err, "boot_failed")
		}()
	case "glass", "stop":
		parts := strings.Split(r.URL.Path, "/")
		vm := s.vms[parts[3]]
		if vm == nil {
			fail(headlessFailure("not_found", "VM not found"))
			return
		}
		if route == "glass" {
			req := headlessGlassRequest{"Neurodesk", vm.Config.Display.Width, vm.Config.Display.Height, 120}
			if err := headlessDecode(body, &req); err != nil {
				fail(err)
				return
			}
			if len([]rune(req.Title)) < 1 || len([]rune(req.Title)) > 128 || strings.ContainsRune(req.Title, 0) || !headlessSize(req.Width, req.Height) || req.Timeout < 1 || req.Timeout > 600 {
				fail(headlessFailure("invalid_request", "Invalid Glass request"))
				return
			}
			if vm.State != "running" || vm.Active != nil || (vm.Glass != nil && (vm.Glass.State == "open" || vm.Glass.State == "starting")) {
				fail(headlessFailure("invalid_state", "VM is not ready for a new Glass window"))
				return
			}
			s.spawnGlass(w, vm, req)
			return
		}
		req := struct {
			Timeout int `json:"timeout_seconds"`
		}{30}
		if err := headlessDecode(body, &req); err != nil {
			fail(err)
			return
		}
		if req.Timeout < 1 || req.Timeout > 300 {
			fail(headlessFailure("invalid_request", "Invalid timeout_seconds"))
			return
		}
		if vm.State == "stopping" {
			fail(headlessFailure("invalid_state", "VM stop already in progress"))
			return
		}
		op := s.accept(w, "vm_stop", &vm.ID)
		vm.State = "stopping"
		vm.Active = &op.ID
		if vm.cancel != nil {
			vm.cancel()
		}
		done := vm.done
		s.jobs.Add(1)
		go func() {
			defer s.jobs.Done()
			ctx, cancel := context.WithTimeout(s.ctx, time.Duration(req.Timeout)*time.Second)
			defer cancel()
			s.mu.Lock()
			op.State = "running"
			op.Phase = "stopping"
			s.mu.Unlock()
			var err error
			if done != nil {
				select {
				case <-done:
				case <-ctx.Done():
					err = ctx.Err()
				}
			}
			if err == nil {
				err = s.driver.Stop(ctx, vm.ID)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			vm.Active = nil
			vm.State = "stopped"
			vm.ownsResources = err != nil
			if err != nil {
				vm.State = "failed"
				vm.LastError = asHeadlessError(err, "shutdown_failed")
				if errors.Is(err, context.DeadlineExceeded) {
					vm.LastError = headlessFailure("shutdown_timeout", "VM shutdown incomplete; retry stop")
				}
				err = vm.LastError
			}
			s.finish(op, map[string]string{"vm_id": vm.ID, "state": "stopped"}, err, "shutdown_failed")
		}()
	}
}
func (s *headlessServer) spawnGlass(w http.ResponseWriter, vm *headlessVM, req headlessGlassRequest) {
	op := s.accept(w, "glass_spawn", &vm.ID)
	vm.Active = &op.ID
	vm.Glass = &headlessGlass{headlessID("glass_"), "starting", req.Title, req.Width, req.Height}
	ctx, cancel := context.WithCancel(s.ctx)
	vm.cancel = cancel
	done := make(chan struct{})
	vm.done = done
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		defer close(done)
		defer cancel()
		var timedOut atomic.Bool
		timer := time.AfterFunc(time.Duration(req.Timeout)*time.Second, func() { timedOut.Store(true); cancel() })
		defer timer.Stop()
		s.mu.Lock()
		op.State = "running"
		op.Phase = "desktop_starting"
		s.mu.Unlock()
		err := s.driver.Glass(ctx, vm.ID, req, func(width, height int) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if ctx.Err() != nil {
				return
			}
			vm.Glass.Width = width
			vm.Glass.Height = height
			if op.State == "running" {
				timer.Stop()
				vm.Glass.State = "open"
				vm.Active = nil
				result := *vm.Glass
				s.finish(op, result, nil, "")
			}
		})
		s.mu.Lock()
		defer s.mu.Unlock()
		if op.State == "running" {
			if err == nil {
				err = headlessFailure("display_failed", "Glass closed before a complete frame was presented")
			}
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			if timedOut.Load() {
				err = headlessFailure("timeout", "Desktop startup timed out")
			}
			s.finish(op, nil, err, "display_failed")
			vm.Glass.State = "failed"
			vm.LastError = op.Error
		} else {
			vm.Glass.State = "closed"
			if err != nil {
				vm.Glass.State = "failed"
				vm.LastError = asHeadlessError(err, "display_failed")
			}
		}
		if vm.Active != nil && *vm.Active == op.ID {
			vm.Active = nil
		}
	}()
}
func (s *headlessServer) shutdown(ctx context.Context) error {
	select {
	case s.shutdownGate <- struct{}{}:
		defer func() { <-s.shutdownGate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	s.closing = true
	s.cancel()
	for _, vm := range s.vms {
		if vm.cancel != nil {
			vm.cancel()
		}
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.jobs.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := s.driver.Shutdown(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	for _, vm := range s.vms {
		vm.State = "stopped"
		vm.ownsResources = false
		vm.Active = nil
		if vm.Glass != nil {
			vm.Glass.State = "closed"
		}
	}
	s.mu.Unlock()
	return nil
}
