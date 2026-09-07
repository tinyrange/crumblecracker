package desktopapp

import (
	"context"
	"github.com/tinyrange/crumblecracker/internal/display"
	appruntime "github.com/tinyrange/crumblecracker/internal/runtime"
	"time"
)

type embeddedBackend struct{ api *appruntime.Runtime }

func startEmbeddedBackend(cacheDir, name string, displayReady chan<- display.Session, shareGroup func() (context, pixelFormat uintptr)) (*embeddedBackend, error) {
	opts := appruntime.Options{CacheDir: cacheDir, OpenGLShareGroup: shareGroup, OnDisplay: func(id string, session display.Session) {
		if id == name {
			select {
			case displayReady <- session:
			default:
			}
		}
	}}
	if c := appConfig.CVMFSHostMount; c != nil {
		opts.CVMFSMounts = []appruntime.CVMFSHostMount{{Mount: c.Mount, Mirror: c.Mirror, Mirrors: append([]string(nil), c.Mirrors...), Repo: c.Repo, Path: c.Path, CacheLimitBytes: c.CacheLimitBytes}}
	}
	api, err := appruntime.New(opts)
	if err != nil {
		return nil, err
	}
	return &embeddedBackend{api: api}, nil
}
func (b *embeddedBackend) stop() error {
	if b == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return b.api.ShutdownContext(ctx)
}
