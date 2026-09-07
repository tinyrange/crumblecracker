package guestagent

import "sync"

type PendingRequests[T any] struct {
	mu       sync.Mutex
	requests map[string]T
}

func NewPendingRequests[T any]() *PendingRequests[T] {
	return &PendingRequests[T]{requests: map[string]T{}}
}

func (p *PendingRequests[T]) Put(id string, req T) {
	if p == nil || id == "" {
		return
	}
	p.mu.Lock()
	p.requests[id] = req
	p.mu.Unlock()
}

func (p *PendingRequests[T]) Take(id string) (T, bool) {
	var zero T
	if p == nil || id == "" {
		return zero, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	req, ok := p.requests[id]
	if ok {
		delete(p.requests, id)
	}
	return req, ok
}
