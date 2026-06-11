// go-virtio/sound — PCM capture path (rxq).
//
// Read builds a per-call DMA buffer holding:
//
//	[ struct virtio_snd_pcm_xfer  ] ro  ← le32 stream_id (header)
//	[ raw PCM frame bytes         ] wo  ← device fills with capture data
//	[ struct virtio_snd_pcm_status ] wo ← device-writable trailer
//
// chains all three on the rx virtqueue, kicks the doorbell, and busy-
// polls for completion (Virtio 1.2 §5.14.6.8). On a successful round-
// trip the captured bytes are copied into the caller's slice.

package sound

import (
	"github.com/go-virtio/common"
)

// Read enqueues a single PCM capture request for `streamID` on the rx
// virtqueue (asking the device to fill the supplied `buf` slice),
// notifies the device, and busy-polls for completion. On success the
// captured bytes have been copied into `buf` and the byte count
// returned is the number of bytes the device wrote.
//
// The caller MUST have transitioned the capture stream to RUNNING via
// PCMSetParams → PCMPrepare → PCMStart beforehand; otherwise the
// device will complete the descriptor with a non-OK status code which
// Read surfaces as ErrDeviceStatus.
//
// Read(streamID, nil) / Read(streamID, []byte{}) is a no-op returning
// (0, nil).
func (v *VirtioSound) Read(streamID uint32, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if err := v.checkStreamID(streamID); err != nil {
		return 0, err
	}
	totalLen := PCMXferHdrSize + uint32(len(buf)) + PCMStatusSize
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
	// mem[PCMXferHdrSize : PCMXferHdrSize+len(buf)] is the device-
	// writable capture region; zero-initialised by the PageAllocator
	// contract.

	hdrPhys := phys
	dataPhys := phys + uint64(PCMXferHdrSize)
	statusPhys := dataPhys + uint64(len(buf))

	bufs := []common.ChainBuffer{
		{Addr: uintptr(hdrPhys), Phys: hdrPhys, Len: PCMXferHdrSize, Writable: false},
		{Addr: uintptr(dataPhys), Phys: dataPhys, Len: uint32(len(buf)), Writable: true},
		{Addr: uintptr(statusPhys), Phys: statusPhys, Len: PCMStatusSize, Writable: true},
	}
	head, err := v.rxq.AddChain(bufs)
	if err != nil {
		return 0, err
	}
	if err := v.Cfg.NotifyQueue(RxQueueIdx, v.rxq.NotifyOff); err != nil {
		_ = v.rxq.ReclaimChain(head)
		return 0, err
	}
	for spin := 0; spin < XferPollIterations; spin++ {
		gotIdx, length, ok := v.rxq.PollUsed()
		if !ok {
			continue
		}
		_ = v.rxq.ReclaimChain(gotIdx)
		// length is the device-written byte count across the chain's
		// device-writable descriptors (Virtio 1.1 §2.6.8). For an rx
		// xfer that's the capture buffer + the status trailer — the
		// 4-byte xfer header is device-readable so it isn't counted.
		// Subtract the fixed status-trailer size to get the captured-
		// data byte count.
		payloadBytes := int(len(buf))
		if int(length) >= int(PCMStatusSize) {
			devWritten := int(length) - int(PCMStatusSize)
			if devWritten >= 0 && devWritten < payloadBytes {
				payloadBytes = devWritten
			}
		}
		// Status trailer: le32 status; le32 latency_bytes.
		statusOff := PCMXferHdrSize + uint32(len(buf))
		statusBytes := mem[statusOff : statusOff+PCMStatusSize]
		statusCode := le.Uint32(statusBytes[0:4])
		if statusCode != SOK {
			return 0, ErrDeviceStatus
		}
		copy(buf[:payloadBytes], mem[PCMXferHdrSize:PCMXferHdrSize+uint32(payloadBytes)])
		return payloadBytes, nil
	}
	_ = v.rxq.ReclaimChain(head)
	return 0, ErrXferTimeout
}
