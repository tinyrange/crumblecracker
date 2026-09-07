package guestagent

import "sync"

type ActiveExec interface {
	WriteStdin([]byte) error
	CloseStdin() error
	Signal(string) error
	Resize(cols, rows int) error
}

type ActiveExecSet struct {
	mu                sync.Mutex
	active            map[string]ActiveExec
	pendingStdinClose map[string]bool
	acknowledged      map[string]map[string]struct{}
}

func NewActiveExecSet() *ActiveExecSet {
	return &ActiveExecSet{
		active:            map[string]ActiveExec{},
		pendingStdinClose: map[string]bool{},
		acknowledged:      map[string]map[string]struct{}{},
	}
}

func (s *ActiveExecSet) Add(id string, exec ActiveExec) bool {
	if s == nil || id == "" || exec == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[id] = exec
	closePending := s.pendingStdinClose[id]
	delete(s.pendingStdinClose, id)
	return closePending
}

func (s *ActiveExecSet) Delete(id string) {
	if s == nil || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, id)
	delete(s.pendingStdinClose, id)
	delete(s.acknowledged, id)
}

func (s *ActiveExecSet) ControlAcknowledged(id, controlID string) bool {
	if s == nil || id == "" || controlID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.acknowledged[id][controlID]
	return ok
}

func (s *ActiveExecSet) AcknowledgeControl(id, controlID string) {
	if s == nil || id == "" || controlID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acknowledged[id] == nil {
		s.acknowledged[id] = make(map[string]struct{})
	}
	s.acknowledged[id][controlID] = struct{}{}
}

func (s *ActiveExecSet) Get(id string) ActiveExec {
	if s == nil || id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[id]
}

func (s *ActiveExecSet) WriteStdin(id string, data []byte) (bool, error) {
	exec := s.Get(id)
	if exec == nil {
		return false, nil
	}
	return true, exec.WriteStdin(data)
}

func (s *ActiveExecSet) CloseStdin(id string, rememberPending bool) (bool, error) {
	if s == nil || id == "" {
		return false, nil
	}
	s.mu.Lock()
	exec := s.active[id]
	if exec == nil && rememberPending {
		s.pendingStdinClose[id] = true
	}
	s.mu.Unlock()
	if exec == nil {
		return false, nil
	}
	return true, exec.CloseStdin()
}

func (s *ActiveExecSet) Signal(id, name string) (bool, error) {
	exec := s.Get(id)
	if exec == nil {
		return false, nil
	}
	return true, exec.Signal(name)
}

func (s *ActiveExecSet) Resize(id string, cols, rows int) (bool, error) {
	exec := s.Get(id)
	if exec == nil {
		return false, nil
	}
	return true, exec.Resize(cols, rows)
}

type ActiveControlRequest struct {
	Kind                      string
	ID                        string
	Stdin                     []byte
	Signal                    string
	Cols                      int
	Rows                      int
	RememberPendingStdinClose bool
}

type ActiveControlResult struct {
	Handled bool
	Found   bool
	Exec    ActiveExec
	Err     error
}

func HandleActiveControl(active *ActiveExecSet, req ActiveControlRequest) ActiveControlResult {
	switch req.Kind {
	case "stdin":
		found, err := active.WriteStdin(req.ID, req.Stdin)
		return ActiveControlResult{Handled: true, Found: found, Err: err}
	case "stdin_close":
		exec := active.Get(req.ID)
		found, err := active.CloseStdin(req.ID, req.RememberPendingStdinClose)
		return ActiveControlResult{Handled: true, Found: found, Exec: exec, Err: err}
	case "signal":
		found, err := active.Signal(req.ID, req.Signal)
		return ActiveControlResult{Handled: true, Found: found, Err: err}
	case "resize":
		found, err := active.Resize(req.ID, req.Cols, req.Rows)
		return ActiveControlResult{Handled: true, Found: found, Err: err}
	default:
		return ActiveControlResult{}
	}
}
