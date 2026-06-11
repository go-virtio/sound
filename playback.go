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

package sound

import (
	"github.com/go-virtio/common"
)

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
func (v *VirtioSound) Write(streamID uint32, frames []byte) (int, error) {
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
	if err := v.Cfg.NotifyQueue(TxQueueIdx, v.txq.NotifyOff); err != nil {
		_ = v.txq.ReclaimChain(head)
		return 0, err
	}
	for spin := 0; spin < XferPollIterations; spin++ {
		gotIdx, _, ok := v.txq.PollUsed()
		if !ok {
			continue
		}
		_ = v.txq.ReclaimChain(gotIdx)
		// Status trailer: le32 status; le32 latency_bytes.
		statusOff := PCMXferHdrSize + uint32(len(frames))
		statusBytes := mem[statusOff : statusOff+PCMStatusSize]
		statusCode := le.Uint32(statusBytes[0:4])
		if statusCode != SOK {
			return 0, ErrDeviceStatus
		}
		return len(frames), nil
	}
	_ = v.txq.ReclaimChain(head)
	return 0, ErrXferTimeout
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
