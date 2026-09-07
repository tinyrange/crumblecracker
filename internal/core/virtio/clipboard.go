package virtio

import "sync"

// Clipboard connects a graphical frontend to a guest desktop clipboard bridge.
// Frontend updates are coalesced for the bridge; guest updates are versioned so
// an RFB writer can deliver them without polling.
type Clipboard struct {
	mu                 sync.Mutex
	frontendText       string
	frontendGeneration uint64
	guestText          string
	guestGeneration    uint64
	guestChanged       chan struct{}
	toGuest            chan string
}

func NewClipboard() *Clipboard {
	return &Clipboard{
		guestChanged: make(chan struct{}),
		toGuest:      make(chan string, 1),
	}
}

func (c *Clipboard) SetFromFrontend(text string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.frontendText = text
	c.frontendGeneration++
	c.mu.Unlock()
	select {
	case c.toGuest <- text:
		return
	default:
	}
	select {
	case <-c.toGuest:
	default:
	}
	select {
	case c.toGuest <- text:
	default:
	}
}

func (c *Clipboard) SetFromGuest(text string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if text == c.guestText {
		c.mu.Unlock()
		return
	}
	c.guestText = text
	c.guestGeneration++
	close(c.guestChanged)
	c.guestChanged = make(chan struct{})
	c.mu.Unlock()
}

func (c *Clipboard) GuestSnapshot() (string, uint64) {
	if c == nil {
		return "", 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.guestText, c.guestGeneration
}

func (c *Clipboard) FrontendText() string {
	text, _ := c.FrontendSnapshot()
	return text
}

func (c *Clipboard) FrontendSnapshot() (string, uint64) {
	if c == nil {
		return "", 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frontendText, c.frontendGeneration
}

func (c *Clipboard) GuestChanged() <-chan struct{} {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.guestChanged
}

func (c *Clipboard) ToGuest() <-chan string {
	if c == nil {
		return nil
	}
	return c.toGuest
}
