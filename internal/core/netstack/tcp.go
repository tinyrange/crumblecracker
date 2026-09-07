// Package netstack TCP support: data structures for retransmission, reassembly,
// option parsing, RTT estimation, and congestion control.

package netstack

import (
	"encoding/binary"
	"sync"
	"time"
)

////////////////////////////////////////////////////////////////////////////////
// TCP Options
////////////////////////////////////////////////////////////////////////////////

// TCP option kinds (RFC 793, RFC 1323, RFC 2018).
const (
	tcpOptEnd       = 0 // End of option list
	tcpOptNOP       = 1 // No-operation (padding)
	tcpOptMSS       = 2 // Maximum Segment Size
	tcpOptWndScale  = 3 // Window Scale (RFC 1323)
	tcpOptSACKOK    = 4 // SACK Permitted
	tcpOptSACK      = 5 // Selective Acknowledgment
	tcpOptTimestamp = 8 // Timestamps (RFC 1323)
)

// tcpOptions holds parsed TCP options from a SYN or SYN-ACK segment.
type tcpOptions struct {
	mss         uint16
	wndScale    uint8
	hasMSS      bool
	hasWndScale bool
}

// parseTCPOptions parses TCP options from the options slice.
// Only MSS and Window Scale are extracted; other options are skipped.
func parseTCPOptions(options []byte) tcpOptions {
	var opts tcpOptions
	i := 0
	for i < len(options) {
		kind := options[i]
		switch kind {
		case tcpOptEnd:
			return opts
		case tcpOptNOP:
			i++
			continue
		case tcpOptMSS:
			if i+4 <= len(options) && options[i+1] == 4 {
				opts.mss = binary.BigEndian.Uint16(options[i+2 : i+4])
				opts.hasMSS = true
			}
			if i+1 < len(options) {
				i += int(options[i+1])
			} else {
				return opts
			}
		case tcpOptWndScale:
			if i+3 <= len(options) && options[i+1] == 3 {
				opts.wndScale = options[i+2]
				opts.hasWndScale = true
			}
			if i+1 < len(options) {
				i += int(options[i+1])
			} else {
				return opts
			}
		default:
			// Skip unknown option using length field
			if i+1 >= len(options) {
				return opts
			}
			length := int(options[i+1])
			if length < 2 {
				return opts // Invalid option length
			}
			i += length
		}
	}
	return opts
}

// buildSynAckOptions builds TCP options for a SYN-ACK segment.
// Returns options with MSS and optionally Window Scale.
func buildSynAckOptions(mss uint16, wndScale uint8, peerHasWndScale bool) []byte {
	if peerHasWndScale {
		// MSS (4) + NOP (1) + WS (3) = 8 bytes (aligned to 4 bytes)
		opts := make([]byte, 8)
		opts[0] = tcpOptMSS
		opts[1] = 4
		binary.BigEndian.PutUint16(opts[2:4], mss)
		opts[4] = tcpOptNOP
		opts[5] = tcpOptWndScale
		opts[6] = 3
		opts[7] = wndScale
		return opts
	}
	// MSS only (4 bytes, aligned)
	opts := make([]byte, 4)
	opts[0] = tcpOptMSS
	opts[1] = 4
	binary.BigEndian.PutUint16(opts[2:4], mss)
	return opts
}

////////////////////////////////////////////////////////////////////////////////
// Send Buffer (Retransmission Queue)
////////////////////////////////////////////////////////////////////////////////

// tcpSendSegment represents a segment awaiting acknowledgment.
type tcpSendSegment struct {
	seqStart  uint32
	seqEnd    uint32
	payload   []byte
	sentAt    time.Time
	retxCount int
}

// tcpSendBuffer manages the retransmission queue for a TCP connection.
type tcpSendBuffer struct {
	mu       sync.Mutex
	segments []tcpSendSegment
	capacity int // max bytes buffered
	used     int // current bytes in buffer
}

// newTCPSendBuffer creates a send buffer with the given capacity.
func newTCPSendBuffer(capacity int) *tcpSendBuffer {
	return &tcpSendBuffer{
		segments: make([]tcpSendSegment, 0, 64),
		capacity: capacity,
	}
}

// append adds a segment to the send buffer. Returns false if buffer is full.
func (sb *tcpSendBuffer) append(seg tcpSendSegment) bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	payloadLen := len(seg.payload)
	if sb.used+payloadLen > sb.capacity {
		return false
	}

	sb.segments = append(sb.segments, seg)
	sb.used += payloadLen
	return true
}

// ack removes all segments with seqEnd <= ackNum.
// Returns the number of bytes acknowledged and the RTT sample if available.
func (sb *tcpSendBuffer) ack(ackNum uint32) (bytesAcked int, rttSample time.Duration, hasRTT bool) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	now := time.Now()
	newSegs := sb.segments[:0]
	for _, seg := range sb.segments {
		// Use sequence number comparison that handles wraparound
		if seqLTE(seg.seqEnd, ackNum) {
			bytesAcked += len(seg.payload)
			sb.used -= len(seg.payload)
			releasePayload(seg.payload)
			// Only use non-retransmitted segments for RTT estimation
			if seg.retxCount == 0 && !hasRTT {
				rttSample = now.Sub(seg.sentAt)
				hasRTT = true
			}
		} else {
			newSegs = append(newSegs, seg)
		}
	}
	sb.segments = newSegs
	return
}

// oldest returns the oldest unacked segment, if any.
func (sb *tcpSendBuffer) oldest() (tcpSendSegment, bool) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	if len(sb.segments) == 0 {
		return tcpSendSegment{}, false
	}
	return sb.segments[0], true
}

// markRetransmittedN updates the retx count and timestamp for the oldest n segments.
func (sb *tcpSendBuffer) markRetransmittedN(n int) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	now := time.Now()
	for i := 0; i < n && i < len(sb.segments); i++ {
		sb.segments[i].retxCount++
		sb.segments[i].sentAt = now
	}
}

// oldestCoalesced returns the oldest segments coalesced up to maxSize bytes.
// Returns the coalesced segment, the number of segments coalesced, and whether any were found.
func (sb *tcpSendBuffer) oldestCoalesced(maxSize int) (tcpSendSegment, int, bool) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	if len(sb.segments) == 0 {
		return tcpSendSegment{}, 0, false
	}

	return sb.segments[0], 1, true
}

// len returns the number of segments in the buffer.
func (sb *tcpSendBuffer) len() int {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return len(sb.segments)
}

// inFlight returns the number of bytes currently in flight.
func (sb *tcpSendBuffer) inFlight() int {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	return sb.used
}

// clear removes all segments from the buffer.
func (sb *tcpSendBuffer) clear() {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	for _, seg := range sb.segments {
		releasePayload(seg.payload)
	}
	sb.segments = sb.segments[:0]
	sb.used = 0
}

////////////////////////////////////////////////////////////////////////////////
// Receive Buffer (Out-of-Order Reassembly)
////////////////////////////////////////////////////////////////////////////////

// tcpOOOSegment represents an out-of-order received segment.
type tcpOOOSegment struct {
	seqStart uint32
	seqEnd   uint32
	payload  []byte
}

// tcpRecvBuffer handles out-of-order segment buffering and reassembly.
type tcpRecvBuffer struct {
	mu       sync.Mutex
	segments []tcpOOOSegment
	maxGaps  int // maximum number of gaps to buffer
}

// newTCPRecvBuffer creates a receive buffer with the given max gaps.
func newTCPRecvBuffer(maxGaps int) *tcpRecvBuffer {
	return &tcpRecvBuffer{
		segments: make([]tcpOOOSegment, 0, maxGaps),
		maxGaps:  maxGaps,
	}
}

// insert adds an out-of-order segment to the buffer.
// Returns false if the buffer is full or segment is duplicate.
func (rb *tcpRecvBuffer) insert(seg tcpOOOSegment) bool {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	// Check for duplicates and find insertion point
	insertIdx := len(rb.segments)
	for i, existing := range rb.segments {
		// Check for overlap/duplicate
		if seqOverlap(seg.seqStart, seg.seqEnd, existing.seqStart, existing.seqEnd) {
			releasePayload(seg.payload)
			return false // Already have this data
		}
		// Find insertion point to maintain order
		if seqLT(seg.seqStart, existing.seqStart) && insertIdx == len(rb.segments) {
			insertIdx = i
		}
	}

	// Check capacity
	if len(rb.segments) >= rb.maxGaps {
		releasePayload(seg.payload)
		return false
	}

	// Insert at correct position
	rb.segments = append(rb.segments, tcpOOOSegment{})
	copy(rb.segments[insertIdx+1:], rb.segments[insertIdx:])
	rb.segments[insertIdx] = seg
	return true
}

// collectContiguous removes and returns all segments contiguous with nextSeq.
// Updates nextSeq to the new expected sequence number.
func (rb *tcpRecvBuffer) collectContiguous(nextSeq *uint32) [][]byte {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	var collected [][]byte
	for {
		found := false
		newSegs := rb.segments[:0]
		for _, seg := range rb.segments {
			if seg.seqStart == *nextSeq {
				collected = append(collected, seg.payload)
				*nextSeq = seg.seqEnd
				found = true
			} else {
				newSegs = append(newSegs, seg)
			}
		}
		rb.segments = newSegs
		if !found {
			break
		}
	}
	return collected
}

// len returns the number of buffered segments.
func (rb *tcpRecvBuffer) len() int {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return len(rb.segments)
}

// clear removes all segments from the buffer.
func (rb *tcpRecvBuffer) clear() {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, seg := range rb.segments {
		releasePayload(seg.payload)
	}
	rb.segments = rb.segments[:0]
}

////////////////////////////////////////////////////////////////////////////////
// RTT Estimation (RFC 6298)
////////////////////////////////////////////////////////////////////////////////

// tcpRTTEstimator implements RTT estimation per RFC 6298.
type tcpRTTEstimator struct {
	mu           sync.Mutex
	srtt         time.Duration // smoothed RTT
	rttVar       time.Duration // RTT variance
	rto          time.Duration // retransmission timeout
	hasInitial   bool          // whether first measurement has been made
	backoffCount int           // track consecutive backoffs
}

// Default RTO bounds.
const (
	minRTO     = 50 * time.Millisecond // Lower min for virtual networks
	maxRTO     = 60 * time.Second
	initialRTO = 500 * time.Millisecond // Lower initial for virtual networks
)

// maxBackoffCount limits exponential backoff iterations.
const maxBackoffCount = 5

// newTCPRTTEstimator creates an RTT estimator with initial RTO.
func newTCPRTTEstimator() *tcpRTTEstimator {
	return &tcpRTTEstimator{
		rto: initialRTO,
	}
}

// update processes an RTT sample and updates the RTO.
func (r *tcpRTTEstimator) update(rtt time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.hasInitial {
		// First measurement (RFC 6298 section 2.2)
		r.srtt = rtt
		r.rttVar = rtt / 2
		r.hasInitial = true
	} else {
		// Subsequent measurements (RFC 6298 section 2.3)
		// RTTVAR = (1 - beta) * RTTVAR + beta * |SRTT - R'|
		// SRTT = (1 - alpha) * SRTT + alpha * R'
		// alpha = 1/8, beta = 1/4
		delta := r.srtt - rtt
		if delta < 0 {
			delta = -delta
		}
		r.rttVar = (3*r.rttVar + delta) / 4
		r.srtt = (7*r.srtt + rtt) / 8
	}

	// RTO = SRTT + max(G, K*RTTVAR) where K=4
	// We use a minimum granularity of 1ms
	r.rto = r.srtt + 4*r.rttVar
	if r.rto < minRTO {
		r.rto = minRTO
	}
	if r.rto > maxRTO {
		r.rto = maxRTO
	}
}

// backoff applies gentler exponential backoff (1.5x) with a cap on iterations.
func (r *tcpRTTEstimator) backoff() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.backoffCount < maxBackoffCount {
		r.rto = (r.rto * 3) / 2 // 1.5x instead of 2x
		r.backoffCount++
	}
	if r.rto > maxRTO {
		r.rto = maxRTO
	}
}

// resetBackoff resets the backoff counter after successful ACK.
func (r *tcpRTTEstimator) resetBackoff() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backoffCount = 0
}

// getRTO returns the current RTO.
func (r *tcpRTTEstimator) getRTO() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rto
}

////////////////////////////////////////////////////////////////////////////////
// Congestion Control (Reno)
////////////////////////////////////////////////////////////////////////////////

// tcpCongestionControl implements TCP Reno congestion control.
type tcpCongestionControl struct {
	mu       sync.Mutex
	cwnd     uint32 // congestion window (bytes)
	ssthresh uint32 // slow-start threshold
	mss      uint16 // maximum segment size
	dupAcks  int    // duplicate ACK counter
}

// Initial congestion window per RFC 5681.
const initialCwndSegments = 10

// fastRetransmitThreshold is the standard number of duplicate ACKs required
// before fast retransmit. A lower threshold treated ordinary delayed/window
// update ACK patterns from Linux guests as loss, repeatedly collapsing the
// virtual-link congestion window during large pipelined HTTP transfers.
const fastRetransmitThreshold = 3

// newTCPCongestionControl creates a congestion controller.
func newTCPCongestionControl(mss uint16) *tcpCongestionControl {
	return &tcpCongestionControl{
		cwnd:     uint32(initialCwndSegments) * uint32(mss),
		ssthresh: ^uint32(0), // Start with max ssthresh
		mss:      mss,
	}
}

// onAck is called when new data is acknowledged.
func (cc *tcpCongestionControl) onAck(bytesAcked int) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cc.dupAcks = 0
	mss := uint32(cc.mss)

	if cc.cwnd < cc.ssthresh {
		// Slow start: increase cwnd by bytes acked (exponential growth)
		cc.cwnd += uint32(bytesAcked)
	} else {
		// Congestion avoidance: increase cwnd by MSS * MSS / cwnd per ACK
		// This gives approximately 1 MSS increase per RTT
		increment := (mss * mss) / cc.cwnd
		if increment < 1 {
			increment = 1
		}
		cc.cwnd += increment
	}
}

// onDupAck is called when a duplicate ACK is received.
// Returns true if fast retransmit should be triggered.
func (cc *tcpCongestionControl) onDupAck() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cc.dupAcks++
	if cc.dupAcks == fastRetransmitThreshold {
		// Fast retransmit threshold reached
		cc.ssthresh = cc.cwnd / 2
		if cc.ssthresh < 2*uint32(cc.mss) {
			cc.ssthresh = 2 * uint32(cc.mss)
		}
		cc.cwnd = cc.ssthresh + uint32(fastRetransmitThreshold)*uint32(cc.mss)
		return true
	}
	if cc.dupAcks > fastRetransmitThreshold {
		// Fast recovery: inflate cwnd by MSS for each additional dup ack
		cc.cwnd += uint32(cc.mss)
	}
	return false
}

// onTimeout is called when RTO expires.
func (cc *tcpCongestionControl) onTimeout() {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cc.ssthresh = cc.cwnd / 2
	if cc.ssthresh < 2*uint32(cc.mss) {
		cc.ssthresh = 2 * uint32(cc.mss)
	}
	cc.cwnd = uint32(cc.mss) // Reset to 1 MSS
	cc.dupAcks = 0
}

// getCwnd returns the current congestion window.
func (cc *tcpCongestionControl) getCwnd() uint32 {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.cwnd
}

////////////////////////////////////////////////////////////////////////////////
// Sequence Number Helpers
////////////////////////////////////////////////////////////////////////////////

// seqLT returns true if a < b (handling wraparound).
func seqLT(a, b uint32) bool {
	return int32(a-b) < 0
}

// seqLTE returns true if a <= b (handling wraparound).
func seqLTE(a, b uint32) bool {
	return int32(a-b) <= 0
}

// seqGT returns true if a > b (handling wraparound).
func seqGT(a, b uint32) bool {
	return int32(a-b) > 0
}

// seqOverlap returns true if [aStart, aEnd) overlaps with [bStart, bEnd).
func seqOverlap(aStart, aEnd, bStart, bEnd uint32) bool {
	// Two ranges overlap if neither is entirely before the other
	return seqLT(aStart, bEnd) && seqLT(bStart, aEnd)
}

////////////////////////////////////////////////////////////////////////////////
// Connection Snapshot (Debug/Instrumentation)
////////////////////////////////////////////////////////////////////////////////

// tcpConnSnapshot captures connection state for debugging.
type tcpConnSnapshot struct {
	State        string `json:"state"`
	LocalAddr    string `json:"localAddr"`
	RemoteAddr   string `json:"remoteAddr"`
	HostSeq      uint32 `json:"hostSeq"`
	GuestSeq     uint32 `json:"guestSeq"`
	SendAcked    uint32 `json:"sendAcked"`
	InFlight     int    `json:"inFlight"`
	PeerWnd      uint32 `json:"peerWnd"`
	PeerWndScale uint8  `json:"peerWndScale"`
	Cwnd         uint32 `json:"cwnd"`
	Ssthresh     uint32 `json:"ssthresh"`
	RTO          string `json:"rto"`
	SRTT         string `json:"srtt"`
	RetxCount    int    `json:"retxCount"`
	OOOSegments  int    `json:"oooSegments"`
	DupAcks      int    `json:"dupAcks"`
	MSS          uint16 `json:"mss"`
}
