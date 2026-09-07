//go:build windows && amd64

package whp

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
)

type bootPIC struct {
	mu       sync.Mutex
	master   bootPICChip
	slave    bootPICChip
	elcr     [2]byte
	lineHigh [16]bool
}

type bootPICChip struct {
	vectorBase uint8
	mask       uint8
	icwStep    uint8
	ocw3       byte
	irr        byte
	isr        byte
	lastIRR    byte
}

type bootPICState struct {
	Master   bootPICChipState `json:"master"`
	Slave    bootPICChipState `json:"slave"`
	ELCR     [2]byte          `json:"elcr"`
	LineHigh [16]bool         `json:"line_high"`
}

type bootPICChipState struct {
	VectorBase uint8 `json:"vector_base"`
	Mask       uint8 `json:"mask"`
	ICWStep    uint8 `json:"icw_step"`
	OCW3       byte  `json:"ocw3"`
	IRR        byte  `json:"irr"`
	ISR        byte  `json:"isr"`
	LastIRR    byte  `json:"last_irr"`
}

func (p *bootPIC) SnapshotState() bootPICState {
	if p == nil {
		return bootPICState{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return bootPICState{
		Master:   p.master.snapshotState(),
		Slave:    p.slave.snapshotState(),
		ELCR:     p.elcr,
		LineHigh: p.lineHigh,
	}
}

func (p *bootPIC) RestoreState(state bootPICState) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.master.restoreState(state.Master)
	p.slave.restoreState(state.Slave)
	p.elcr = state.ELCR
	p.lineHigh = state.LineHigh
}

func (c bootPICChip) snapshotState() bootPICChipState {
	return bootPICChipState{
		VectorBase: c.vectorBase,
		Mask:       c.mask,
		ICWStep:    c.icwStep,
		OCW3:       c.ocw3,
		IRR:        c.irr,
		ISR:        c.isr,
		LastIRR:    c.lastIRR,
	}
}

func (c *bootPICChip) restoreState(state bootPICChipState) {
	c.vectorBase = state.VectorBase
	c.mask = state.Mask
	c.icwStep = state.ICWStep
	c.ocw3 = state.OCW3
	c.irr = state.IRR
	c.isr = state.ISR
	c.lastIRR = state.LastIRR
}

func (p *bootPIC) Read(port uint16, data []byte) bool {
	if len(data) != 1 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch port {
	case 0x20:
		if p.master.ocw3&0x1 != 0 {
			data[0] = p.master.isr
		} else {
			data[0] = p.master.irr
		}
	case 0x21:
		data[0] = p.master.mask
	case 0xa0:
		if p.slave.ocw3&0x1 != 0 {
			data[0] = p.slave.isr
		} else {
			data[0] = p.slave.irr
		}
	case 0xa1:
		data[0] = p.slave.mask
	case 0x4d0:
		data[0] = p.elcr[0]
	case 0x4d1:
		data[0] = p.elcr[1]
	default:
		return false
	}
	return true
}

func (p *bootPIC) Write(port uint16, data []byte) bool {
	if len(data) != 1 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	value := data[0]
	switch port {
	case 0x20:
		p.master.writeCommand(value)
	case 0x21:
		p.master.writeData(value)
	case 0xa0:
		p.slave.writeCommand(value)
	case 0xa1:
		p.slave.writeData(value)
	case 0x4d0:
		p.elcr[0] = value
	case 0x4d1:
		p.elcr[1] = value
	default:
		return false
	}
	return true
}

func (p *bootPIC) SetIRQ(line uint8, level bool) {
	if line >= 16 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lineHigh[line] = level
	p.setLineLocked(line, level)
}

func (p *bootPIC) setLineLocked(line uint8, level bool) {
	chip := &p.master
	irq := line
	if line >= 8 {
		if p.master.mask&(1<<2) != 0 {
			// Keep the slave line state; it will be visible once cascade unmasks.
			chip = &p.slave
			irq = line - 8
		} else {
			chip = &p.slave
			irq = line - 8
		}
	}
	mask := byte(1 << irq)
	levelTriggered := p.elcr[line/8]&mask != 0
	if levelTriggered {
		if level {
			chip.irr |= mask
			chip.lastIRR |= mask
		} else {
			chip.irr &^= mask
			chip.lastIRR &^= mask
		}
		return
	}
	if level {
		if chip.lastIRR&mask == 0 {
			chip.irr |= mask
		}
		chip.lastIRR |= mask
	} else {
		chip.lastIRR &^= mask
	}
}

func (p *bootPIC) AcknowledgePending() (uint8, uint8, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.slave.pendingIRQ() >= 0 {
		p.master.irr |= 1 << 2
	}
	masterIRQ := p.master.pendingIRQ()
	if masterIRQ < 0 {
		return 0, 0, false
	}
	if masterIRQ == 2 {
		slaveIRQ := p.slave.pendingIRQ()
		if slaveIRQ >= 0 {
			p.ackLocked(&p.master, 2, 2)
			line := uint8(8 + slaveIRQ)
			return p.ackLocked(&p.slave, uint8(slaveIRQ), line), line, true
		}
	}
	line := uint8(masterIRQ)
	return p.ackLocked(&p.master, line, line), line, true
}

func (p *bootPIC) EndOfInterrupt(vector uint8) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case vector >= p.slave.vectorBase && vector < p.slave.vectorBase+8:
		irq := vector - p.slave.vectorBase
		mask := byte(1 << irq)
		if p.slave.isr&mask == 0 {
			return false
		}
		p.slave.isr &^= mask
		p.master.isr &^= 1 << 2
		p.setLineLocked(8+irq, p.lineHigh[8+irq])
		return true
	case vector >= p.master.vectorBase && vector < p.master.vectorBase+8:
		irq := vector - p.master.vectorBase
		mask := byte(1 << irq)
		if p.master.isr&mask == 0 {
			return false
		}
		p.master.isr &^= mask
		p.setLineLocked(irq, p.lineHigh[irq])
		return true
	default:
		return false
	}
}

func (p *bootPIC) ackLocked(chip *bootPICChip, irq uint8, line uint8) uint8 {
	mask := byte(1 << irq)
	chip.isr |= mask
	if p.elcr[line/8]&mask == 0 {
		chip.irr &^= mask
	}
	return chip.vectorBase + irq
}

func (p *bootPIC) Resample() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for line, high := range p.lineHigh {
		p.setLineLocked(uint8(line), high)
	}
}

func (p *bootPIC) Clear(line uint8) {
	if line >= 16 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lineHigh[line] = false
	chip := &p.master
	irq := line
	if line >= 8 {
		chip = &p.slave
		irq = line - 8
	}
	chip.irr &^= 1 << irq
}

func (p *bootPIC) LevelTriggered(line uint8) bool {
	if line >= 16 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.elcr[line/8]&(1<<(line%8)) != 0
}

func (p *bootPIC) SetLevelTriggered(line uint8, level bool) {
	if line >= 16 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	mask := byte(1 << (line % 8))
	if level {
		p.elcr[line/8] |= mask
	} else {
		p.elcr[line/8] &^= mask
	}
	p.setLineLocked(line, p.lineHigh[line])
}

func (p *bootPIC) summaryForLines(lines []uint8) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(lines) == 0 {
		return "pic=[]"
	}
	var b strings.Builder
	b.WriteString("pic=[")
	for i, line := range lines {
		if i > 0 {
			b.WriteByte(',')
		}
		if line >= 16 {
			fmt.Fprintf(&b, "%d:<out-of-range>", line)
			continue
		}
		chip := p.master
		irq := line
		if line >= 8 {
			chip = p.slave
			irq = line - 8
		}
		mask := byte(1 << irq)
		fmt.Fprintf(&b, "%d:mask=%t,irr=%t,isr=%t,level=%t,high=%t",
			line,
			chip.mask&mask != 0,
			chip.irr&mask != 0,
			chip.isr&mask != 0,
			p.elcr[line/8]&mask != 0,
			p.lineHigh[line],
		)
	}
	b.WriteByte(']')
	return b.String()
}

func (c *bootPICChip) pendingIRQ() int {
	pending := c.irr &^ c.mask
	if pending == 0 {
		return -1
	}
	for irq := 0; irq < 8; irq++ {
		if pending&(1<<irq) != 0 {
			return irq
		}
	}
	return -1
}

func (c *bootPICChip) writeCommand(value byte) {
	if value&0x10 != 0 {
		c.icwStep = 2
		c.mask = 0xff
		c.irr = 0
		c.isr = 0
		c.lastIRR = 0
		return
	}
	if value&0x20 != 0 {
		c.isr = 0
		return
	}
	if value&0x08 != 0 {
		c.ocw3 = value
	}
}

func (c *bootPICChip) writeData(value byte) {
	switch c.icwStep {
	case 2:
		c.vectorBase = value & 0xf8
		c.icwStep = 3
	case 3:
		c.icwStep = 4
	case 4:
		c.icwStep = 0
	default:
		c.mask = value
	}
}

type bootPIT struct {
	mu         sync.Mutex
	onIRQ0     func()
	ticker     *time.Ticker
	tickerStop chan struct{}
	tickerWG   sync.WaitGroup
	stop       chan struct{}
	closed     bool
	channels   [3]bootPITChannel
	selected   uint8
	port61     byte
}

func newBootPIT(onIRQ0 func()) *bootPIT {
	p := &bootPIT{onIRQ0: onIRQ0, stop: make(chan struct{})}
	for i := range p.channels {
		p.channels[i].reload = 0xffff
		p.channels[i].start = time.Now()
	}
	return p
}

type bootPITChannel struct {
	reload   uint16
	lowByte  byte
	haveLow  bool
	readHigh bool
	start    time.Time
	gate     bool
}

type bootPITState struct {
	Channels [3]bootPITChannelState `json:"channels"`
	Selected uint8                  `json:"selected"`
	Port61   byte                   `json:"port61"`
}

type bootPITChannelState struct {
	Reload       uint16 `json:"reload"`
	LowByte      byte   `json:"low_byte"`
	HaveLow      bool   `json:"have_low"`
	ReadHigh     bool   `json:"read_high"`
	ElapsedNanos int64  `json:"elapsed_nanos"`
	Gate         bool   `json:"gate"`
}

func (p *bootPIT) SnapshotState() bootPITState {
	if p == nil {
		return bootPITState{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var state bootPITState
	state.Selected = p.selected
	state.Port61 = p.port61
	for i, ch := range p.channels {
		state.Channels[i] = bootPITChannelState{
			Reload:       ch.reload,
			LowByte:      ch.lowByte,
			HaveLow:      ch.haveLow,
			ReadHigh:     ch.readHigh,
			ElapsedNanos: now.Sub(ch.start).Nanoseconds(),
			Gate:         ch.gate,
		}
	}
	return state
}

func (p *bootPIT) RestoreState(state bootPITState) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopTickerLocked()
	p.selected = state.Selected
	p.port61 = state.Port61
	now := time.Now()
	for i, ch := range state.Channels {
		p.channels[i] = bootPITChannel{
			reload:   ch.Reload,
			lowByte:  ch.LowByte,
			haveLow:  ch.HaveLow,
			readHigh: ch.ReadHigh,
			start:    now.Add(-time.Duration(ch.ElapsedNanos)),
			gate:     ch.Gate,
		}
		if p.channels[i].reload == 0 {
			p.channels[i].reload = 0xffff
		}
	}
	p.armChannel0Locked()
}

func (p *bootPIT) Close() {
	p.mu.Lock()
	p.closed = true
	p.stopTickerLocked()
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.mu.Unlock()
	p.tickerWG.Wait()
}

func (p *bootPIT) Read(port uint16, data []byte) bool {
	if len(data) != 1 {
		return false
	}
	switch port {
	case 0x40:
		data[0] = p.readCounter(0)
	case 0x41:
		data[0] = p.readCounter(1)
	case 0x42:
		data[0] = p.readCounter(2)
	case 0x43:
		data[0] = 0xff
	case 0x61:
		data[0] = p.readPort61()
	default:
		return false
	}
	return true
}

func (p *bootPIT) Write(port uint16, data []byte) bool {
	if len(data) != 1 {
		return false
	}
	switch port {
	case 0x40:
		p.writeCounter(0, data[0])
	case 0x41:
		p.writeCounter(1, data[0])
	case 0x42:
		p.writeCounter(2, data[0])
	case 0x43:
		p.writeCommand(data[0])
	case 0x61:
		p.writePort61(data[0])
	default:
		return false
	}
	return true
}

func (p *bootPIT) writeCommand(value byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := (value >> 6) & 0x3
	if ch < 3 {
		p.selected = ch
		p.channels[ch].haveLow = false
		p.channels[ch].readHigh = false
	}
}

func (p *bootPIT) writeCounter(idx uint8, value byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx >= 3 {
		return
	}
	ch := &p.channels[idx]
	if !ch.haveLow {
		ch.lowByte = value
		ch.haveLow = true
		return
	}
	ch.reload = uint16(ch.lowByte) | uint16(value)<<8
	if ch.reload == 0 {
		ch.reload = 0xffff
	}
	ch.haveLow = false
	ch.start = time.Now()
	if idx == 0 {
		p.armChannel0Locked()
	}
}

func (p *bootPIT) readCounter(idx uint8) byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx >= 3 {
		return 0
	}
	ch := &p.channels[idx]
	reload := ch.reload
	if reload == 0 {
		reload = 0xffff
	}
	ticks := uint64(time.Since(ch.start).Nanoseconds()) * 1193182 / 1_000_000_000
	value := uint16((uint64(reload) - ticks%uint64(reload)) & 0xffff)
	if !ch.readHigh {
		ch.readHigh = true
		return byte(value)
	}
	ch.readHigh = false
	return byte(value >> 8)
}

func (p *bootPIT) readPort61() byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.port61
	ch := &p.channels[2]
	if ch.gate && time.Since(ch.start) >= time.Duration(ch.reload)*time.Second/1193182 {
		out |= 1 << 5
	} else {
		out &^= 1 << 5
	}
	return out
}

func (p *bootPIT) writePort61(value byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	oldGate := p.channels[2].gate
	p.port61 = value
	p.channels[2].gate = value&0x1 != 0
	if p.channels[2].gate && !oldGate {
		p.channels[2].start = time.Now()
	}
}

func (p *bootPIT) armChannel0Locked() {
	if p.closed {
		return
	}
	p.stopTickerLocked()
	reload := p.channels[0].reload
	if reload == 0 {
		reload = 0xffff
	}
	period := time.Duration(reload) * time.Second / 1193182
	if period < 10*time.Millisecond {
		period = 10 * time.Millisecond
	}
	p.ticker = time.NewTicker(period)
	ticker := p.ticker
	tickerStop := make(chan struct{})
	p.tickerStop = tickerStop
	p.tickerWG.Add(1)
	go func() {
		defer p.tickerWG.Done()
		for {
			select {
			case <-ticker.C:
				p.onIRQ0()
			case <-tickerStop:
				return
			case <-p.stop:
				return
			}
		}
	}()
}

func (p *bootPIT) stopTickerLocked() {
	if p.ticker != nil {
		p.ticker.Stop()
		p.ticker = nil
	}
	if p.tickerStop != nil {
		close(p.tickerStop)
		p.tickerStop = nil
	}
}

type bootIOAPIC struct {
	mu       sync.Mutex
	index    uint32
	redir    [ioapicRedirEntries]uint64
	version  uint32
	inFlight [256]bool
	lineHigh [ioapicRedirEntries]bool
}

type bootIOAPICState struct {
	Index    uint32                     `json:"index"`
	Redir    [ioapicRedirEntries]uint64 `json:"redir"`
	Version  uint32                     `json:"version"`
	InFlight [256]bool                  `json:"in_flight"`
	LineHigh [ioapicRedirEntries]bool   `json:"line_high"`
}

func (a *bootIOAPIC) SnapshotState() bootIOAPICState {
	if a == nil {
		return bootIOAPICState{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return bootIOAPICState{
		Index:    a.index,
		Redir:    a.redir,
		Version:  a.version,
		InFlight: a.inFlight,
		LineHigh: a.lineHigh,
	}
}

func (a *bootIOAPIC) RestoreState(state bootIOAPICState) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.index = state.Index
	a.redir = state.Redir
	a.version = state.Version
	if a.version == 0 {
		a.version = 0x11 | ((ioapicRedirEntries - 1) << 16)
	}
	a.inFlight = state.InFlight
	a.lineHigh = state.LineHigh
}

type bootIOAPICRoute struct {
	line            uint8
	vector          uint8
	level           bool
	interruptType   interruptType
	destinationMode interruptDestinationMode
	destination     uint32
}

func (a *bootIOAPIC) init() {
	a.version = 0x11 | ((ioapicRedirEntries - 1) << 16)
	for i := range a.redir {
		a.redir[i] = 1 << 16
	}
}

func (a *bootIOAPIC) Read(addr uint64, data []byte) bool {
	if len(data) != 4 || addr < ioapicBaseAddress || addr+uint64(len(data)) > ioapicBaseAddress+ioapicMMIOSize {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var value uint32
	switch addr - ioapicBaseAddress {
	case 0x00:
		value = a.index
	case 0x10:
		value = a.readRegister(a.index)
	default:
		value = 0
	}
	binary.LittleEndian.PutUint32(data, value)
	return true
}

func (a *bootIOAPIC) Write(addr uint64, data []byte) (bool, bootIOAPICRoute, bool) {
	if len(data) != 4 || addr < ioapicBaseAddress || addr+uint64(len(data)) > ioapicBaseAddress+ioapicMMIOSize {
		return false, bootIOAPICRoute{}, false
	}
	value := binary.LittleEndian.Uint32(data)
	a.mu.Lock()
	defer a.mu.Unlock()
	var route bootIOAPICRoute
	var pending bool
	switch addr - ioapicBaseAddress {
	case 0x00:
		a.index = value & 0xff
	case 0x10:
		route, pending = a.writeRegister(a.index, value)
	default:
	}
	return true, route, pending
}

func (a *bootIOAPIC) enabled(line uint8) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if int(line) >= len(a.redir) {
		return false
	}
	return a.redir[line]&(1<<16) == 0
}

func (a *bootIOAPIC) vector(line uint8) uint8 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if int(line) >= len(a.redir) {
		return 0
	}
	return uint8(a.redir[line])
}

func (a *bootIOAPIC) levelTriggered(line uint8) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if int(line) >= len(a.redir) {
		return false
	}
	return a.redir[line]&(1<<15) != 0
}

func (a *bootIOAPIC) beginInterrupt(vector uint8) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.inFlight[vector] {
		return false
	}
	a.inFlight[vector] = true
	return true
}

func (a *bootIOAPIC) endInterrupt(vector uint8) {
	a.mu.Lock()
	a.inFlight[vector] = false
	a.mu.Unlock()
}

func (a *bootIOAPIC) assert(line uint8, high bool) (bootIOAPICRoute, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if int(line) >= len(a.redir) {
		return bootIOAPICRoute{}, false
	}
	if high {
		edge := !a.lineHigh[line]
		a.lineHigh[line] = true
		return a.evaluateLocked(line, edge)
	}
	a.lineHigh[line] = false
	if a.redir[line]&(1<<15) != 0 {
		a.inFlight[uint8(a.redir[line])] = false
	}
	return bootIOAPICRoute{}, false
}

func (a *bootIOAPIC) handleEOI(vector uint32) (bootIOAPICRoute, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inFlight[uint8(vector)] = false
	for line := range a.redir {
		if uint8(a.redir[line]) != uint8(vector) {
			continue
		}
		if route, ok := a.evaluateLocked(uint8(line), false); ok {
			return route, true
		}
	}
	return bootIOAPICRoute{}, false
}

func (a *bootIOAPIC) deviceHighRoute(line uint8) (bootIOAPICRoute, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if int(line) >= len(a.redir) || !a.lineHigh[line] {
		return bootIOAPICRoute{}, false
	}
	return a.routeForLineLocked(line)
}

func (a *bootIOAPIC) routeForLine(line uint8) (bootIOAPICRoute, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.routeForLineLocked(line)
}

func (a *bootIOAPIC) routeForLineLocked(line uint8) (bootIOAPICRoute, bool) {
	if int(line) >= len(a.redir) {
		return bootIOAPICRoute{}, false
	}
	entry := a.redir[line]
	if entry&(1<<16) != 0 {
		return bootIOAPICRoute{}, false
	}
	vector := uint8(entry)
	if vector < 0x10 {
		return bootIOAPICRoute{}, false
	}
	level := entry&(1<<15) != 0
	return bootIOAPICRoute{
		line:            line,
		vector:          vector,
		level:           level,
		interruptType:   ioapicInterruptType(entry),
		destinationMode: ioapicDestinationMode(entry),
		destination:     uint32(entry >> 56),
	}, true
}

func (a *bootIOAPIC) cancel(route bootIOAPICRoute) {
	if !route.level {
		return
	}
	a.endInterrupt(route.vector)
}

func (a *bootIOAPIC) summaryForLines(lines []uint8) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(lines) == 0 {
		return "ioapic=[]"
	}
	var b strings.Builder
	b.WriteString("ioapic=[")
	for i, line := range lines {
		if i > 0 {
			b.WriteByte(',')
		}
		if int(line) >= len(a.redir) {
			fmt.Fprintf(&b, "%d:<out-of-range>", line)
			continue
		}
		entry := a.redir[line]
		trigger := "edge"
		if entry&(1<<15) != 0 {
			trigger = "level"
		}
		fmt.Fprintf(&b, "%d:vec=%#x,mask=%t,trig=%s,level=%t,irr=%t", line, uint8(entry), entry&(1<<16) != 0, trigger, a.lineHigh[line], a.inFlight[uint8(entry)])
	}
	b.WriteByte(']')
	return b.String()
}

func (a *bootIOAPIC) readRegister(index uint32) uint32 {
	switch {
	case index == 0:
		return 0
	case index == 1:
		return a.version
	case index >= 0x10 && index < 0x10+uint32(len(a.redir))*2:
		entry := a.redir[(index-0x10)/2]
		if index&1 == 0 {
			return uint32(entry)
		}
		return uint32(entry >> 32)
	default:
		return 0
	}
}

func (a *bootIOAPIC) writeRegister(index uint32, value uint32) (bootIOAPICRoute, bool) {
	if index < 0x10 || index >= 0x10+uint32(len(a.redir))*2 {
		return bootIOAPICRoute{}, false
	}
	entryIndex := (index - 0x10) / 2
	entry := &a.redir[entryIndex]
	wasMasked := *entry&(1<<16) != 0
	if index&1 == 0 {
		*entry = (*entry & 0xffffffff00000000) | uint64(value)
	} else {
		*entry = (*entry & 0x00000000ffffffff) | uint64(value)<<32
	}
	if wasMasked && *entry&(1<<16) == 0 && a.lineHigh[entryIndex] {
		return a.evaluateLocked(uint8(entryIndex), true)
	}
	return bootIOAPICRoute{}, false
}

func (a *bootIOAPIC) evaluateLocked(line uint8, edge bool) (bootIOAPICRoute, bool) {
	if int(line) >= len(a.redir) {
		return bootIOAPICRoute{}, false
	}
	entry := a.redir[line]
	if entry&(1<<16) != 0 {
		return bootIOAPICRoute{}, false
	}
	vector := uint8(entry)
	level := entry&(1<<15) != 0
	if entry&(1<<13) != 0 && !level {
		edge = !edge
	}
	switch {
	case level && (!a.lineHigh[line] || a.inFlight[vector]):
		return bootIOAPICRoute{}, false
	case !level && !edge:
		return bootIOAPICRoute{}, false
	}
	return bootIOAPICRoute{
		line:            line,
		vector:          vector,
		level:           level,
		interruptType:   ioapicInterruptType(entry),
		destinationMode: ioapicDestinationMode(entry),
		destination:     uint32(entry >> 56),
	}, true
}

func ioapicInterruptType(entry uint64) interruptType {
	if (entry>>8)&0x7 == 1 {
		return interruptTypeLowestPriority
	}
	return interruptTypeFixed
}

func ioapicDestinationMode(entry uint64) interruptDestinationMode {
	if entry&(1<<11) != 0 {
		return interruptDestinationLogical
	}
	return interruptDestinationPhysical
}
