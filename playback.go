// Copyright (c) 2026 the go-virtio/sound authors.
// SPDX-License-Identifier: BSD-3-Clause
//
// go-virtio/sound — PCM playback path (txq).
//
// Write builds a per-call DMA buffer holding:
//
//	[ struct virtio_snd_pcm_xfer  ] ro  ← le32 stream_id (header)
//	[ raw PCM frame bytes         ] ro  ← caller-supplied L16 / S16_LE
//	[ struct virtio_snd_pcm_status ] wo ← device-writable trailer
//
// chains all three on the tx virtqueue, kicks the doorbell, and
// busy-polls for completion (Virtio 1.2 §5.14.6.8). The chain-then-poll
// model is intentionally synchronous in the MVP: it keeps the data
// path identical in shape to console / net.
//
// Completion correlation. The virtio-snd TX queue is fundamentally
// asynchronous: the device parks a posted chain in the per-stream
// queue and the host audio backend consumes it whenever a period
// elapses (~93 ms at 11025 Hz / 2048-byte period -- closer to several
// hundred ms when the backend is the `wav` audiodev under QEMU TCG).
// A busy-poll Write that bails with ErrXferTimeout therefore leaves
// the device still holding our chain; the completion shows up later,
// possibly while a SUBSEQUENT Write is mid-busy-poll. Two consequences
// the implementation has to defend against:
//
//  1. Slot reuse. The timed-out chain's descriptor slots are STILL
//     in flight (the device may DMA-write the status trailer at any
//     moment). Reclaiming them would let the next Write reuse the
//     same slots + reissue them with fresh content, and the device's
//     late status write would clobber the new caller's bytes. So
//     timeouts park their bookkeeping in `pendingTxq` and leave the
//     slots InUse=true.
//
//  2. Mis-attributed completions. The used-ring entry's `id` is the
//     head descriptor index, which is reused as soon as a chain
//     completes -- a polled completion with `id=0` could belong to
//     either OUR chain (just published) or a prior in-flight chain
//     whose head was also 0. The fix matches the popped id against
//     OUR head AND drains any pending chains that come back first;
//     bytes are read out of the originating chain's mem slice.

package sound

import (
	"github.com/go-virtio/common"
)

// pendingTxq is the per-VirtioSound list of chains a sync Write()
// abandoned on ErrXferTimeout. Each entry captures the descriptor
// head + the originating mem slice + the status-trailer offset so a
// future Write can drain the late completion (when it eventually
// arrives in the used ring), free the slot, and ignore the bytes the
// device wrote into a buffer the caller has already moved on from.
type pendingTxq struct {
	head      uint16
	mem       []byte
	statusOff uint32
}

// Write enqueues `frames` as a single PCM transfer for `streamID` on
// the tx virtqueue, notifies the device, and busy-polls for completion.
// Returns the number of audio-payload bytes the device accepted
// (excluding the 4-byte xfer header and the 8-byte status trailer).
//
// The caller MUST have transitioned the stream to RUNNING via
// PCMSetParams → PCMPrepare → PCMStart beforehand; otherwise the device
// will complete the descriptor with a non-OK status code which Write
// surfaces as ErrDeviceStatus.
//
// `frames` MUST be raw S16_LE samples (per the README's caller-
// responsibility note). The driver performs no format conversion.
//
// Write(nil) / Write(streamID, []byte{}) is a no-op returning (0, nil).
//
// Completion semantics. On busy-poll timeout Write returns
// ErrXferTimeout but DOES NOT reclaim the chain -- the slot stays
// reserved + the bookkeeping moves to v.pendingTxq, so a subsequent
// Write call can drain the late completion (matched by head index)
// instead of mis-attributing it to its own chain. Callers that retry
// after a timeout can therefore expect the next Write to succeed
// once the device flushes the backlog.
func (v *VirtioSound) Write(streamID uint32, frames []byte) (int, error) {
	if len(frames) == 0 {
		return 0, nil
	}
	if err := v.checkStreamID(streamID); err != nil {
		return 0, err
	}

	// Drain any prior-Write completions that arrived after we bailed
	// with ErrXferTimeout. Freeing those slots BEFORE we go searching
	// for a fresh chain's slots keeps AddChain happy when the ring
	// fills up with in-flight timeouts under steady playback.
	v.drainPendingTxq()

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
	// Lay the header + payload + status trailer out contiguously.
	hdr := buildPCMXferHdr(streamID)
	copy(mem[:len(hdr)], hdr)
	copy(mem[len(hdr):len(hdr)+len(frames)], frames)
	// mem[len(hdr)+len(frames):totalLen] is the device-writable status
	// trailer — zero-initialised by the PageAllocator contract.

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
	statusOff := PCMXferHdrSize + uint32(len(frames))
	if err := v.Cfg.NotifyQueue(TxQueueIdx, v.txq.NotifyOff); err != nil {
		_ = v.txq.ReclaimChain(head)
		return 0, err
	}
	for spin := 0; spin < XferPollIterations; spin++ {
		gotIdx, _, ok := v.txq.PollUsed()
		if !ok {
			continue
		}
		if gotIdx != head {
			// Completion belongs to a prior chain (most likely one
			// that timed out -- see pendingTxq). Free its slot +
			// drop the bytes; the originating Write has long since
			// returned to its caller, so the trailer code is moot.
			v.reclaimPending(gotIdx)
			continue
		}
		_ = v.txq.ReclaimChain(gotIdx)
		// Status trailer: le32 status; le32 latency_bytes.
		statusBytes := mem[statusOff : statusOff+PCMStatusSize]
		statusCode := le.Uint32(statusBytes[0:4])
		if statusCode != SOK {
			return 0, ErrDeviceStatus
		}
		return len(frames), nil
	}
	// Timed out: park the chain so a later Write can mop up its
	// completion. DO NOT ReclaimChain -- the device may still DMA
	// into our mem buffer + the slot must remain InUse so AddChain
	// doesn't hand it to a new caller.
	v.pendingTxq = append(v.pendingTxq, pendingTxq{
		head:      head,
		mem:       mem,
		statusOff: statusOff,
	})
	return 0, ErrXferTimeout
}

// drainPendingTxq polls the txq used ring and frees any pending
// (timed-out) chains whose completions have caught up. The drained
// chain's status trailer is ignored -- the originating Write already
// returned ErrXferTimeout, so the caller no longer cares about the
// per-frame outcome. Stale completions whose head index doesn't match
// any tracked pending are simply discarded (the slot is freed via
// ReclaimChain), keeping the descriptor table from leaking under a
// pathological "timed out then never polled" workload.
func (v *VirtioSound) drainPendingTxq() {
	if len(v.pendingTxq) == 0 {
		return
	}
	for {
		gotIdx, _, ok := v.txq.PollUsed()
		if !ok {
			return
		}
		v.reclaimPending(gotIdx)
		if len(v.pendingTxq) == 0 {
			return
		}
	}
}

// reclaimPending finds the pending chain whose head matches `idx`,
// reclaims its descriptor slots, and drops it from the pending list.
// When no entry matches (the completion is for a chain we never parked)
// the descriptor chain rooted at `idx` is still reclaimed so the slot
// returns to the free pool. Idempotent: a second call for the same
// idx is a no-op.
func (v *VirtioSound) reclaimPending(idx uint16) {
	for i, p := range v.pendingTxq {
		if p.head == idx {
			_ = v.txq.ReclaimChain(idx)
			v.pendingTxq = append(v.pendingTxq[:i], v.pendingTxq[i+1:]...)
			return
		}
	}
	_ = v.txq.ReclaimChain(idx)
}

// pagesFor rounds a byte length up to whole 4 KiB pages — the unit the
// PageAllocator hands out. A typical PCM period is well under a page,
// but larger frame buffers (e.g. a 4 KiB period @ 48 kHz stereo S16_LE
// = ~21 ms) cross the page boundary and need >1 page.
func pagesFor(totalLen uint32) int {
	if totalLen == 0 {
		return 1
	}
	return int((uint64(totalLen) + uint64(common.PageSize) - 1) / uint64(common.PageSize))
}
