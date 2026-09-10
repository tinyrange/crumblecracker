package desktopapp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/tinyrange/gowin/window"
)

func runHeadless(cacheOverride string) error {
	// The caller is the product's original main goroutine. In particular, Cocoa
	// creation and polling must stay here while HTTP handlers run independently.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	settings, _, err := loadAppSettings()
	if err != nil {
		return err
	}
	cache, err := resolveAppCacheDir(cacheOverride, settings.systemInstall())
	if err != nil {
		return err
	}
	storage, err := headlessStoragePath(appConfig.DefaultStorage)
	if err != nil {
		return err
	}
	driver, err := newHeadlessRuntimeDriver(cache, appConfig)
	if err != nil {
		return err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(secret[:])
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	control := newHeadlessServer(appConfig, storage, listener.Addr().String(), token, driver)
	server := &http.Server{Handler: control, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 310 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer server.Close()
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"event": "ready", "protocol": "ndappx", "api_version": 1, "base_url": "http://" + listener.Addr().String(), "token": token, "pid": os.Getpid()}); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	eof := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); close(eof) }()
	fatal := make(chan error, 1)
	var shutdownOnce sync.Once
	shutdown := func(cause error) {
		shutdownOnce.Do(func() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				err := control.shutdown(ctx)
				if err != nil {
					release, cancelRelease := context.WithTimeout(context.Background(), 5*time.Second)
					err = errors.Join(err, driver.releaseOwned(release))
					cancelRelease()
				}
				if err != nil || cause != nil {
					fatal <- fmt.Errorf("headless backend stopped with an error: %w", errors.Join(cause, err))
					return
				}
				control.stopOnce.Do(func() { close(control.stopped) })
			}()
		})
	}
	// Watch ownership independently of the UI thread, which runs the native
	// window loop while Glass is attached. Cleanup cancels that loop as well.
	watch, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	go func() {
		select {
		case <-watch.Done():
		case <-signals:
			shutdown(nil)
		case <-eof:
			shutdown(nil)
		case err := <-serveErr:
			if !errors.Is(err, http.ErrServerClosed) {
				shutdown(err)
			}
		}
	}()
	go control.monitorVMs()
	for {
		select {
		case request := <-driver.native:
			err := openHeadlessDisplay(request)
			fmt.Fprintln(os.Stderr, "NeurodeskAppX: native Glass window detached")
			request.done <- err
		case <-control.stopped:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return server.Shutdown(ctx)
		case err := <-fatal:
			return err
		}
	}
}
func openHeadlessDisplay(request headlessNativeRequest) error {
	if err := request.ctx.Err(); err != nil {
		return err
	}
	session, ok := request.api.DisplaySession(request.id)
	if !ok {
		return headlessFailure("display_unavailable", "VM has no native display session")
	}
	win, err := window.New(request.request.Title, request.request.Width, request.request.Height, true)
	if err != nil {
		return headlessFailure("display_unavailable", "Cannot open a native window in this graphical session")
	}
	viewer := &displayViewer{window: win, session: session, guestCursor: newGuestCursorHost(), keysDown: make(map[window.Key]bool), updateConsumedKeys: make(map[window.Key]bool), parentContext: request.ctx, settings: startupOptions{DisplayWidth: request.request.Width, DisplayHeight: request.request.Height}, startup: initialStartupProgress(), onPresentation: request.ready}
	if err := viewer.init(); err != nil {
		if viewer.gl != nil {
			viewer.close()
		} else {
			viewer.guestCursor.Close()
			win.Close()
		}
		return err
	}
	defer viewer.close()
	// Release guest input before detaching a window so reopening cannot leave a
	// modifier/button logically held down by the old native session.
	defer func() {
		for key, down := range viewer.keysDown {
			if down {
				if code, ok := linuxKeycode(key); ok {
					_ = session.Key(code, false)
				}
			}
		}
		if viewer.sentButtons != 0 {
			x, y := win.Cursor()
			_ = viewer.sendPointer(x, y, 0)
		}
	}()
	viewer.presentation.markGuestReady()
	return viewer.loop(request.ctx)
}
func (s *headlessServer) monitorVMs() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
		s.mu.Lock()
		ids := []string{}
		for id, v := range s.vms {
			if v.State == "running" {
				ids = append(ids, id)
			}
		}
		s.mu.Unlock()
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(s.ctx, time.Second)
			state, err := s.driver.Status(ctx, id)
			cancel()
			if err != nil || state == "running" {
				continue
			}
			s.mu.Lock()
			vm := s.vms[id]
			if vm.State == "running" {
				vm.State = "failed"
				vm.LastError = headlessFailure("guest_exited", "Guest exited unexpectedly")
				if vm.cancel != nil {
					vm.cancel()
				}
			}
			s.mu.Unlock()
		}
	}
}
