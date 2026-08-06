// Copyright (c) 2026 the go-virtio/sound authors.
// SPDX-License-Identifier: BSD-3-Clause

// Regression tests for the PCM playback (txq) chain layout + the
// timeout-recovery completion-correlation path. Pins the wire shape
// the device sees (Virtio 1.2 §5.14.6.8 PCM xfer chain: header RO,
// PCM RO, status WO) so a future refactor that re-shapes the chain
// trips a test rather than silently slipping past CI + only failing
// on real virtio-snd-pci against QEMU.

package sound

import (
	"errors"
	"testing"

	"github.com/go-virtio/common"
)

// TestWrite_ChainLayout pins the descriptor chain Write() publishes
// onto the txq: exactly three descriptors, first two are
// device-readable (no VIRTQ_DESC_F_WRITE), third is device-writable.
// The header descriptor MUST be 4 bytes (sizeof(virtio_snd_pcm_xfer)),
// the data descriptor MUST cover every payload byte (period_bytes),
// and the status descriptor MUST be 8 bytes
// (sizeof(virtio_snd_pcm_status)). Any drift in the chain shape would
// have QEMU's virtio_snd_handle_tx_xfer() reject the xfer with
// VIRTIO_SND_S_BAD_MSG (the iov header parse is the first thing it
// does); this test catches such drift in unit-tests rather than on
// the real bare-metal smoke run.
func TestWrite_ChainLayout(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.txDeferComplete = true
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	// Write a known-length frame so the data descriptor length is a
	// distinct value (catches an "ok, but length wrong" regression
	// that a trivial 1-byte payload could hide).
	const payload = 2048
	frames := make([]byte, payload)
	for i := range frames {
		frames[i] = byte(i)
	}
	// Async-shape Write so the chain stays in the queue + we can
	// inspect descriptor bytes. Use WriteAsync — same chain layout
	// as Write per its dockstring; sync-Write times out under the
	// deferred fake which we don't want here.
	cookie, err := v.WriteAsync(0, frames)
	if err != nil {
		t.Fatalf("WriteAsync: %v", err)
	}
	if cookie == 0 {
		t.Fatal("WriteAsync returned zero cookie")
	}
	head := d.peekLastHead(TxQueueIdx)

	// Walk the chain straight out of the txq's descriptor table.
	d0 := v.txq.DescBytes(head)
	if d0 == nil {
		t.Fatalf("desc[%d] not populated", head)
	}
	hdrLen := le.Uint32(d0[8:12])
	hdrFlags := le.Uint16(d0[12:14])
	hdrNext := le.Uint16(d0[14:16])
	if hdrLen != PCMXferHdrSize {
		t.Errorf("header desc len=%d, want %d", hdrLen, PCMXferHdrSize)
	}
	if hdrFlags&common.VirtqDescFWrite != 0 {
		t.Errorf("header desc has VIRTQ_DESC_F_WRITE (must be RO for TX)")
	}
	if hdrFlags&common.VirtqDescFNext == 0 {
		t.Errorf("header desc lacks VIRTQ_DESC_F_NEXT (chain truncated)")
	}

	d1 := v.txq.DescBytes(hdrNext)
	if d1 == nil {
		t.Fatalf("desc[%d] not populated", hdrNext)
	}
	dataLen := le.Uint32(d1[8:12])
	dataFlags := le.Uint16(d1[12:14])
	dataNext := le.Uint16(d1[14:16])
	if dataLen != payload {
		t.Errorf("data desc len=%d, want %d", dataLen, payload)
	}
	if dataFlags&common.VirtqDescFWrite != 0 {
		t.Errorf("data desc has VIRTQ_DESC_F_WRITE (must be RO for TX)")
	}
	if dataFlags&common.VirtqDescFNext == 0 {
		t.Errorf("data desc lacks VIRTQ_DESC_F_NEXT (chain truncated)")
	}

	d2 := v.txq.DescBytes(dataNext)
	if d2 == nil {
		t.Fatalf("desc[%d] not populated", dataNext)
	}
	statusLen := le.Uint32(d2[8:12])
	statusFlags := le.Uint16(d2[12:14])
	if statusLen != PCMStatusSize {
		t.Errorf("status desc len=%d, want %d", statusLen, PCMStatusSize)
	}
	if statusFlags&common.VirtqDescFWrite == 0 {
		t.Errorf("status desc lacks VIRTQ_DESC_F_WRITE (device can't return status)")
	}
	if statusFlags&common.VirtqDescFNext != 0 {
		t.Errorf("status desc has VIRTQ_DESC_F_NEXT (chain MUST end here)")
	}

	// The header bytes the device reads MUST be the LE32 stream id.
	hdrAddr := le.Uint64(d0[0:8])
	hdrBytes := readBufferBytes(uintptr(hdrAddr), 4)
	if got := le.Uint32(hdrBytes); got != 0 {
		t.Errorf("header stream_id = %d, want 0", got)
	}
}

// TestWrite_TimeoutRecovery proves the fix for the QEMU virtio-snd
// BAD_MSG that surfaced under sync busy-poll: the txq is fundamentally
// async, so a sync Write that times out before the device flushes its
// chain MUST NOT reclaim the slot (the device may still DMA-write into
// the buffer) AND a SUBSEQUENT Write MUST NOT mis-attribute the late
// completion to its own freshly-published chain.
//
// Sequence:
//
//  1. Write #1 with txCompletes=false → ErrXferTimeout (chain parked
//     in pendingTxq, slots stay reserved).
//  2. Re-enable completion, manually deliver Write #1's deferred
//     completion onto the used ring (the device "catching up").
//  3. Write #2 should DRAIN Write #1's pending completion at entry,
//     then publish its own chain on FRESH slots and return SOK from
//     its OWN status trailer (not Write #1's stale one).
//
// Before the fix, step 3 returned ErrDeviceStatus because PollUsed
// surfaced Write #1's completion (head index 0, the slot reused by
// Write #2), and the code read the status trailer out of Write #2's
// untouched mem slice → statusCode=0x0 → "non-OK".
func TestWrite_TimeoutRecovery(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	frames := []byte{1, 2, 3, 4}

	// Step 1: timeout the first Write.
	d.txCompletes = false
	if _, err := v.Write(0, frames); !errors.Is(err, ErrXferTimeout) {
		t.Fatalf("Write #1: got %v, want ErrXferTimeout", err)
	}
	if len(v.pendingTxq) != 1 {
		t.Fatalf("pendingTxq after timeout: got %d entries, want 1", len(v.pendingTxq))
	}
	staleHead := v.pendingTxq[0].head

	// Step 2: device catches up + publishes the deferred completion
	// for Write #1's chain (which is still in InUse state on the
	// driver side -- confirms the slot wasn't reclaimed).
	if !v.txq.Buffers[staleHead].InUse {
		t.Errorf("Write #1's head slot %d was reclaimed -- the device's late DMA would clobber a subsequent caller", staleHead)
	}
	d.completeKnownChain(TxQueueIdx, staleHead, d.processTx)

	// Step 3: re-enable normal completion + issue Write #2. The fix's
	// drainPendingTxq pulls Write #1's completion out of the way at
	// the top of Write, then AddChain hands out fresh slots; the
	// poll loop matches Write #2's head + reads SOK from Write #2's
	// own buffer.
	d.txCompletes = true
	n, err := v.Write(0, frames)
	if err != nil {
		t.Fatalf("Write #2: got %v, want nil", err)
	}
	if n != len(frames) {
		t.Errorf("Write #2 n=%d, want %d", n, len(frames))
	}
	if len(v.pendingTxq) != 0 {
		t.Errorf("pendingTxq after Write #2: got %d entries, want 0", len(v.pendingTxq))
	}
	// Write #1's stale head must be free again (drained by Write #2).
	if v.txq.Buffers[staleHead].InUse {
		t.Errorf("Write #1's stale slot %d still marked InUse after drain", staleHead)
	}
}

// TestWrite_TimeoutPendingSlotStaysInUse pins the half of the fix that
// keeps the slot reserved: after ErrXferTimeout, AddChain MUST NOT
// hand the timed-out chain's slot to a subsequent caller (the device
// may still DMA into that buffer). The driver verifies this by
// checking the txq's per-descriptor bookkeeping rather than re-issuing
// a real Write, so the test stays independent of the slot-allocator's
// search order.
func TestWrite_TimeoutPendingSlotStaysInUse(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.txCompletes = false
	if _, err := v.Write(0, []byte{1, 2, 3, 4}); !errors.Is(err, ErrXferTimeout) {
		t.Fatalf("Write: got %v, want ErrXferTimeout", err)
	}
	if len(v.pendingTxq) != 1 {
		t.Fatalf("pendingTxq: got %d, want 1", len(v.pendingTxq))
	}
	p := v.pendingTxq[0]
	// All three slots of the chain MUST remain InUse so AddChain on
	// the next Write skips them + grabs fresh ones.
	idx := p.head
	for i := 0; i < 3; i++ {
		if !v.txq.Buffers[idx].InUse {
			t.Errorf("slot %d (chain pos %d) was reclaimed -- device DMA would clobber a new caller", idx, i)
		}
		descBytes := v.txq.DescBytes(idx)
		if descBytes == nil {
			t.Fatalf("desc[%d] missing", idx)
		}
		flags := le.Uint16(descBytes[12:14])
		next := le.Uint16(descBytes[14:16])
		if i < 2 {
			if flags&common.VirtqDescFNext == 0 {
				t.Errorf("slot %d (chain pos %d) lost VIRTQ_DESC_F_NEXT -- chain truncated by ReclaimChain", idx, i)
			}
		}
		idx = next
	}
}

// TestWrite_StaleCompletionUnknownIdReclaimed covers the defensive
// branch in reclaimPending where a polled completion's head doesn't
// match any entry in pendingTxq -- e.g. an exotic backend or a
// out-of-order delivery. The slot still gets reclaimed (otherwise the
// descriptor table leaks slowly until AddChain hits ErrQueueFull) and
// the Write proceeds + succeeds.
func TestWrite_StaleCompletionUnknownIdReclaimed(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	// Hand-craft a used-ring entry for a head index that the driver
	// has no in-flight chain for. We use slot 5 (well past anything
	// Write touches in practice).
	v.txq.Buffers[5].InUse = true
	usedAddr := d.qdevice[TxQueueIdx]
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(d.qsize[TxQueueIdx]))
	if usedSlice == nil {
		t.Fatal("could not access used ring bytes")
	}
	le.PutUint32(usedSlice[4:8], 5)  // ring[0].id = 5
	le.PutUint32(usedSlice[8:12], 8) // ring[0].len = 8 (status only)
	le.PutUint16(usedSlice[2:4], 1)  // idx = 1

	// Now Write -- the poll loop sees the unknown completion, reclaims
	// the slot via the defensive branch, keeps polling, and eventually
	// gets its own completion when processTx fires.
	n, err := v.Write(0, []byte{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("Write: got %v, want nil", err)
	}
	if n != 4 {
		t.Errorf("n=%d, want 4", n)
	}
	if v.txq.Buffers[5].InUse {
		t.Errorf("unknown-id slot 5 still InUse after Write -- defensive reclaim didn't fire")
	}
}

// TestPendingTxq_DrainEmptyNoop verifies drainPendingTxq is a no-op
// when no chains are parked. Cheap line-coverage of the early-return.
func TestPendingTxq_DrainEmptyNoop(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	// Should not touch the used ring or pendingTxq.
	v.drainPendingTxq()
	if len(v.pendingTxq) != 0 {
		t.Errorf("pendingTxq grew under no-op drain: %d", len(v.pendingTxq))
	}
}

// TestPendingTxq_DrainNoCompletion covers the `if !ok` branch of
// drainPendingTxq: a chain is parked but the device hasn't published
// any used-ring entry yet. The drain returns without freeing anything.
func TestPendingTxq_DrainNoCompletion(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	// Force a timeout so pendingTxq has one entry.
	d.txCompletes = false
	if _, err := v.Write(0, []byte{1, 2, 3}); !errors.Is(err, ErrXferTimeout) {
		t.Fatalf("priming Write: got %v, want ErrXferTimeout", err)
	}
	if len(v.pendingTxq) != 1 {
		t.Fatalf("pendingTxq: got %d, want 1", len(v.pendingTxq))
	}
	// Drain finds the parked chain but PollUsed returns ok=false
	// (no completion published) so we bail without touching anything.
	v.drainPendingTxq()
	if len(v.pendingTxq) != 1 {
		t.Errorf("pendingTxq shrank to %d under empty used-ring drain", len(v.pendingTxq))
	}
}
