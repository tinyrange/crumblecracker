package timing

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}

type Recorder struct {
	mu      sync.Mutex
	records map[string]time.Duration
	counts  map[string]int
	order   []string
}

type Snapshot struct {
	Name     string
	Duration time.Duration
	Count    int
}

func NewRecorder() *Recorder {
	return &Recorder{
		records: map[string]time.Duration{},
		counts:  map[string]int{},
	}
}

func WithRecorder(ctx context.Context, recorder *Recorder) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, recorder)
}

func FromContext(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(contextKey{}).(*Recorder)
	return recorder
}

func Record(ctx context.Context, name string, duration time.Duration) {
	if recorder := FromContext(ctx); recorder != nil {
		recorder.Record(name, duration)
	}
}

func Since(ctx context.Context, name string, start time.Time) {
	Record(ctx, name, time.Since(start))
}

func (r *Recorder) Record(name string, duration time.Duration) {
	name = strings.TrimSpace(name)
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.records[name]; !ok {
		r.order = append(r.order, name)
	}
	r.records[name] += duration
	r.counts[name]++
}

func (r *Recorder) RecordCount(name string, count int) {
	name = strings.TrimSpace(name)
	if r == nil || name == "" || count <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.records[name]; !ok {
		r.order = append(r.order, name)
		r.records[name] = 0
	}
	r.counts[name] += count
}

func (r *Recorder) Snapshots() []Snapshot {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Snapshot, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, Snapshot{
			Name:     name,
			Duration: r.records[name],
			Count:    r.counts[name],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}
