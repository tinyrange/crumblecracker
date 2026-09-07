package shmem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const (
	PageSize          = uint64(4096)
	MaxRegionSize     = uint64(1 << 30)
	MaxDomainSize     = uint64(4 << 30)
	MaxRegistrySize   = uint64(16 << 30)
	MaxRegions        = 15
	ControlWindowSize = uint64(4096)
)

type Config struct {
	Domain   string
	PhysAddr uint64
}

type Registry struct {
	mu      sync.Mutex
	domains map[string]*domain
	total   uint64
}

type domain struct {
	name        string
	attachments int
	total       uint64
	regions     map[uint32]*region
}

type region struct {
	size  uint64
	mem   []byte
	close func() error
}

type Attachment struct {
	registry *Registry
	domain   *domain
	config   Config

	mu       sync.Mutex
	claimed  bool
	released bool
}

var ErrRegionSizeConflict = errors.New("shared memory region size conflicts with existing allocation")

func NewRegistry() *Registry {
	return &Registry{domains: make(map[string]*domain)}
}

func ValidateConfig(config Config) error {
	config.Domain = strings.TrimSpace(config.Domain)
	if config.Domain == "" {
		return fmt.Errorf("shared memory domain is required")
	}
	if len(config.Domain) > 128 {
		return fmt.Errorf("shared memory domain exceeds 128 bytes")
	}
	if config.PhysAddr == 0 || config.PhysAddr%PageSize != 0 {
		return fmt.Errorf("shared memory control address %#x must be page-aligned and non-zero", config.PhysAddr)
	}
	if config.PhysAddr > ^uint64(0)-(ControlWindowSize-1) {
		return fmt.Errorf("shared memory control address %#x overflows", config.PhysAddr)
	}
	return nil
}

func (r *Registry) Attach(config Config) (*Attachment, error) {
	if r == nil {
		return nil, fmt.Errorf("shared memory registry is not configured")
	}
	config.Domain = strings.TrimSpace(config.Domain)
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.domains[config.Domain]
	if d == nil {
		d = &domain{name: config.Domain, regions: make(map[uint32]*region)}
		r.domains[config.Domain] = d
	}
	d.attachments++
	return &Attachment{registry: r, domain: d, config: config}, nil
}

func (a *Attachment) Config() Config {
	if a == nil {
		return Config{}
	}
	return a.config
}

func (a *Attachment) Claim() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.claimed = true
	a.mu.Unlock()
}

func (a *Attachment) Claimed() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.claimed
}

func (a *Attachment) Region(id uint32, size uint64) ([]byte, error) {
	if a == nil || a.domain == nil || a.registry == nil {
		return nil, fmt.Errorf("shared memory attachment is unavailable")
	}
	if id == 0 {
		return nil, fmt.Errorf("shared memory region ID zero is reserved")
	}
	if size == 0 || size%PageSize != 0 || size > MaxRegionSize {
		return nil, fmt.Errorf("shared memory region size %d is invalid", size)
	}
	r := a.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	a.mu.Lock()
	released := a.released
	a.mu.Unlock()
	if released {
		return nil, fmt.Errorf("shared memory attachment is released")
	}
	if existing := a.domain.regions[id]; existing != nil {
		if existing.size != size {
			return nil, ErrRegionSizeConflict
		}
		return existing.mem, nil
	}
	if len(a.domain.regions) >= MaxRegions {
		return nil, fmt.Errorf("shared memory domain has reached its region limit")
	}
	if a.domain.total > MaxDomainSize-size {
		return nil, fmt.Errorf("shared memory domain exceeds its size limit")
	}
	if r.total > MaxRegistrySize-size {
		return nil, fmt.Errorf("shared memory registry exceeds its size limit")
	}
	mem, closeBacking, err := allocateBacking(size)
	if err != nil {
		return nil, err
	}
	a.domain.regions[id] = &region{size: size, mem: mem, close: closeBacking}
	a.domain.total += size
	r.total += size
	return mem, nil
}

func (a *Attachment) Release() error {
	if a == nil || a.registry == nil || a.domain == nil {
		return nil
	}
	a.mu.Lock()
	if a.released {
		a.mu.Unlock()
		return nil
	}
	a.released = true
	a.mu.Unlock()

	r := a.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	a.domain.attachments--
	if a.domain.attachments > 0 {
		return nil
	}
	delete(r.domains, a.domain.name)
	r.total -= a.domain.total
	var errs []error
	for _, region := range a.domain.regions {
		if region.close != nil {
			errs = append(errs, region.close())
		}
	}
	a.domain.regions = nil
	return errors.Join(errs...)
}

type attachmentContextKey struct{}

func WithAttachment(ctx context.Context, attachment *Attachment) context.Context {
	return context.WithValue(ctx, attachmentContextKey{}, attachment)
}

func FromContext(ctx context.Context) *Attachment {
	if ctx == nil {
		return nil
	}
	attachment, _ := ctx.Value(attachmentContextKey{}).(*Attachment)
	return attachment
}
