// go-virtio/sound — async playback (txq) + eventq draining (v0.2.0).
//
// The MVP's Write() chained one xfer, kicked the doorbell, and busy-
// polled for completion — fine when the caller's frame budget is
// generous (DOOM's 35 Hz = 28 ms / frame, vs sub-ms PCM round-trip)
// but useless for low-latency audio apps that want to keep multiple
// buffers in flight.
//
// WriteAsync builds the same xfer chain, kicks the doorbell, and
// returns immediately with a cookie. The caller drains completions via
// ReadEvents(); each EventKindWriteComplete event carries the cookie
// the caller can correlate to the originating WriteAsync.
//
// ReadEvents() ALSO consumes the eventq used-ring: VIRTIO_SND_EVT_PCM_*
// notifications (period-elapsed / xrun) and, when F_CTLS is up,
// VIRTIO_SND_EVT_CTL_NOTIFY events are surfaced into the same Event
// stream.
//
// SPDX-License-Identifier: BSD-3-Clause

package sound

import (
	"github.com/go-virtio/common"
)

// WriteAsync queues PCM frames for async completion on the txq. The
// device walks the chain at its own pace; the returned cookie identifies
// the chain for correlation in ReadEvents().
//
// Unlike Write(), WriteAsync does NOT busy-poll for completion — it
// publishes the chain, kicks the doorbell, registers the cookie, and
// returns. The caller MUST eventually call ReadEvents() to learn which
// cookies completed (and to free the txq descriptor slots — completions
// stack up in the inflight registry until drained).
func (v *VirtioSound) WriteAsync(streamID uint32, frames []byte) (uint64, error) {
	if len(frames) == 0 {
		return 0, nil
	}
	if err := v.checkStreamID(streamID); err != nil {
		return 0, err
	}
	totalLen := PCMXferHdrSize + uint32(len(frames)) + PCMStatusSize
	phys, mem, err := v.transport.AllocatePages(pagesFor(totalLen))
	if err != nil {
		return 0, err
	}
	if phys == 0 {
		return 0, common.ErrAllocReturnedZero
	}
	if uint64(totalLen) > uint64(len(mem)) {
		return 0, ErrBufferTooSmall
	}
	hdr := buildPCMXferHdr(streamID)
	copy(mem[:len(hdr)], hdr)
	copy(mem[len(hdr):len(hdr)+len(frames)], frames)

	hdrPhys := phys
	dataPhys := phys + uint64(PCMXferHdrSize)
	statusPhys := dataPhys + uint64(len(frames))

	bufs := []common.ChainBuffer{
		{Addr: uintptr(hdrPhys), Phys: hdrPhys, Len: PCMXferHdrSize, Writable: false},
		{Addr: uintptr(dataPhys), Phys: dataPhys, Len: uint32(len(frames)), Writable: false},
		{Addr: uintptr(statusPhys), Phys: statusPhys, Len: PCMStatusSize, Writable: true},
	}
	head, err := v.txq.AddChain(bufs)
	if err != nil {
		return 0, err
	}
	if err := v.Cfg.NotifyQueue(TxQueueIdx, v.txq.NotifyOff); err != nil {
		_ = v.txq.ReclaimChain(head)
		return 0, err
	}
	v.ensureAsyncRegistry()
	v.nextCookie++
	cookie := v.nextCookie
	v.asyncCookies[cookie] = asyncInflight{
		streamID:  streamID,
		head:      head,
		mem:       mem,
		statusOff: PCMXferHdrSize + uint32(len(frames)),
		dataLen:   uint32(len(frames)),
	}
	return cookie, nil
}

// ReadEvents drains the txq used-ring for any completed WriteAsync
// chains AND the eventq used-ring for any pending device notifications,
// returning the union as a slice of normalised Events. Empty slice
// when there is nothing pending.
//
// Each successfully drained Event MUST correspond to a chain in the
// inflight registry: the matching cookie is removed and the descriptor
// slot is reclaimed. Pre-existing v0.1.0 callers that only ever use
// the synchronous Write() can keep ignoring ReadEvents (Write drains
// its own completion).
func (v *VirtioSound) ReadEvents() []Event {
	out := v.pendingEvents
	v.pendingEvents = nil
	// Drain the txq used-ring for any WriteAsync completions.
	for {
		gotIdx, _, ok := v.txq.PollUsed()
		if !ok {
			break
		}
		ev, matched := v.completeAsyncCookie(gotIdx)
		_ = v.txq.ReclaimChain(gotIdx)
		if matched {
			out = append(out, ev)
		}
	}
	// Drain the eventq used-ring for any device-side notifications.
	for {
		gotIdx, length, ok := v.eventq.PollUsed()
		if !ok {
			break
		}
		ev, matched := v.parseEventqEntry(gotIdx, length)
		_ = v.eventq.ReclaimChain(gotIdx)
		if matched {
			out = append(out, ev)
		}
	}
	return out
}

// ensureAsyncRegistry lazily allocates the inflight-cookie map. Idem-
// potent — WriteAsync calls it on every invocation.
func (v *VirtioSound) ensureAsyncRegistry() {
	if v.asyncCookies == nil {
		v.asyncCookies = make(map[uint64]asyncInflight)
	}
}

// completeAsyncCookie matches a txq used-ring entry (`head` index) to
// a tracked WriteAsync cookie. Returns (event, true) when matched,
// (zero, false) when the used-ring entry has no tracked cookie (e.g.
// it's the completion of a synchronous Write() that's already been
// reclaimed elsewhere — should not happen in practice but the
// fallthrough keeps the drain loop robust).
func (v *VirtioSound) completeAsyncCookie(head uint16) (Event, bool) {
	if v.asyncCookies == nil {
		return Event{}, false
	}
	for cookie, inf := range v.asyncCookies {
		if inf.head == head {
			statusCode := uint32(SOK)
			if uint32(len(inf.mem)) >= inf.statusOff+PCMStatusSize {
				statusCode = le.Uint32(inf.mem[inf.statusOff : inf.statusOff+4])
			}
			delete(v.asyncCookies, cookie)
			return Event{
				Kind:          EventKindWriteComplete,
				StreamID:      inf.streamID,
				Cookie:        cookie,
				BytesAccepted: inf.dataLen,
				StatusCode:    statusCode,
			}, true
		}
	}
	return Event{}, false
}

// parseEventqEntry decodes one eventq used-ring entry into an Event.
// The eventq's per-buffer layout is `struct virtio_snd_event`:
//
//	struct virtio_snd_event {
//	    struct virtio_snd_hdr hdr;   // le32 code (the REvt* constants)
//	    le32 data;                   // event-specific payload
//	};
//
// The driver pre-posted one-page buffers in fillEventRing; the device
// writes the 8-byte event into the head of the buffer and reports a
// length of 8 in the used-ring entry.
func (v *VirtioSound) parseEventqEntry(head uint16, length uint32) (Event, bool) {
	if int(head) >= len(v.eventq.Buffers) {
		return Event{}, false
	}
	buf := v.eventq.Buffers[head]
	if buf.Addr == 0 || length < 8 {
		// Refill the event ring with this buffer so the device has
		// somewhere to land the next event.
		_ = v.repostEventBuffer(head)
		return Event{}, false
	}
	mem := readBufferBytes(uintptr(buf.Addr), 8)
	code := le.Uint32(mem[0:4])
	data := le.Uint32(mem[4:8])
	_ = v.repostEventBuffer(head)
	switch code {
	case REvtPCMPeriodElapsed:
		return Event{Kind: EventKindPeriodElapsed, StreamID: data}, true
	case REvtPCMXrun:
		return Event{Kind: EventKindXrun, StreamID: data}, true
	case REvtJackConnected:
		return Event{Kind: EventKindJackConnected, StreamID: data}, true
	case REvtJackDisconnected:
		return Event{Kind: EventKindJackDisconnected, StreamID: data}, true
	case REvtCtlNotify:
		return Event{Kind: EventKindControlChanged, ControlID: data}, true
	}
	return Event{}, false
}

// repostEventBuffer re-publishes the buffer at descriptor index `head`
// so the device has somewhere to land the next event. Best-effort; if
// re-posting fails the event ring drains over time (the slot is still
// freed by ReclaimChain), but the driver hands back what it has.
func (v *VirtioSound) repostEventBuffer(head uint16) error {
	if int(head) >= len(v.eventq.Buffers) {
		return nil
	}
	buf := v.eventq.Buffers[head]
	if buf.Addr == 0 {
		return nil
	}
	bufLen := uint32(common.PageSize)
	if _, err := v.eventq.AddBuffer(buf.Addr, buf.Phys, bufLen, true); err != nil {
		return err
	}
	return v.Cfg.NotifyQueue(EventQueueIdx, v.eventq.NotifyOff)
}

// PendingAsyncCount returns the number of WriteAsync cookies that have
// not yet been drained by ReadEvents(). Useful for back-pressure (cap
// the in-flight depth so the txq's descriptor pool doesn't deadlock).
func (v *VirtioSound) PendingAsyncCount() int {
	return len(v.asyncCookies)
}
