// End-to-end tests for the OpenVirtioSound driver path + the controlq
// and PCM data-queue paths. Uses a fakeSoundDevice transport that:
//
//   - Publishes a valid virtio-sound PCI config-space cap chain
//     (CommonCfg + NotifyCfg + DeviceCfg).
//   - Tracks COMMON_CFG register state: the device-status progression,
//     feature-select index, and the four queues' address publication.
//   - Simulates the device side of TX / control completion: on a
//     doorbell write to a data queue it walks the most-recently-
//     published descriptor chain, writes the status trailer, and
//     publishes the head in the used ring.
//   - For control commands it parses the request body, optionally
//     populates the payload buffer (PCM_INFO), and writes SOK into the
//     virtio_snd_hdr response.

package sound

import (
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"github.com/go-virtio/common"
)

// fakeSoundDevice is a minimal in-memory virtio-sound device for
// driver tests.
type fakeSoundDevice struct {
	mu sync.Mutex

	// PCI config-space contents.
	cfg []byte

	// COMMON_CFG state.
	deviceFeatureSelect uint32
	deviceFeatures      uint64 // what the device offers
	driverFeatures      uint64 // what the driver acked
	deviceStatus        uint8
	currentQueue        uint16
	// Per-queue state. Key: queue idx.
	qsize      map[uint16]uint16
	qenable    map[uint16]uint16
	qdesc      map[uint16]uint64
	qdriver    map[uint16]uint64
	qdevice    map[uint16]uint64
	qnotifyOff map[uint16]uint16

	// Device-config region (16 bytes in v0.2.0: jacks / streams /
	// chmaps / controls).
	devcfg [16]byte

	// BAR memory store (other reads/writes).
	bar map[uint64]uint64 // key = (bar<<48 | offset)

	// FEATURES_OK gate override.
	clearFeaturesOK bool

	// completes: per data-queue, when true a doorbell publishes a
	// used-ring entry for the most-recently-added chain.
	ctrlCompletes bool
	txCompletes   bool
	rxCompletes   bool
	// txDeferComplete: when true, doorbell does NOT publish a used-
	// ring entry; the test triggers it manually via deliverTxComplete().
	txDeferComplete bool

	// ctrlStatus is the status code the device writes into the
	// virtio_snd_hdr response. Default SOK.
	ctrlStatus uint32

	// xferStatus is the status code the device writes into the
	// virtio_snd_pcm_status trailer for tx/rx completions. Default SOK.
	xferStatus uint32

	// pcmInfoSeed is the seed byte the device writes into the PCM_INFO
	// response payload (the test just checks it round-trips).
	pcmInfoSeed byte

	// heldPages pins references to allocated pages so the GC does not
	// reclaim them — the driver retains addresses via uintptr which
	// the GC doesn't trace.
	heldPages [][]byte
	allocFail bool

	// rxPayload is the bytes the device writes into the rx data
	// buffer on a capture completion.
	rxPayload []byte

	// pcmInfoOverrides[streamID] overrides the default PCM_INFO entry
	// the device returns. Used by the rate / format / multi-port tests
	// to advertise specific format+rate bitmaps.
	pcmInfoOverrides map[uint32]PCMInfoEntry

	// controls is the device's control-element table (F_CTLS). When
	// non-empty, R_CTL_INFO returns these; R_CTL_READ returns
	// controlValues[id]; R_CTL_WRITE updates them.
	controls      []Control
	controlValues map[uint32][]int32

	// txDeferred is the queue of head-indices the fake will publish on
	// the next deliverTxComplete() call when txDeferComplete is set.
	txDeferred []uint16

	// lastSetParamsReq is a copy of the last R_PCM_SET_PARAMS request
	// body the fake observed — used by tests to assert the rate +
	// format bytes hit the wire as expected.
	lastSetParamsReq []byte
}

func newFakeSoundDevice(deviceFeats uint64) *fakeSoundDevice {
	d := &fakeSoundDevice{
		deviceFeatures: deviceFeats,
		qsize:          map[uint16]uint16{0: 32, 1: 32, 2: 32, 3: 32},
		qenable:        map[uint16]uint16{},
		qdesc:          map[uint16]uint64{},
		qdriver:        map[uint16]uint64{},
		qdevice:        map[uint16]uint64{},
		qnotifyOff:     map[uint16]uint16{0: 0, 1: 1, 2: 2, 3: 3},
		bar:            map[uint64]uint64{},
		ctrlCompletes:  true,
		txCompletes:    true,
		rxCompletes:    true,
		ctrlStatus:     SOK,
		xferStatus:     SOK,
		pcmInfoSeed:    0xAA,
	}
	// Advertise: jacks=1, streams=2 (one playback, one capture),
	// chmaps=1, controls=0 (filled in per-test via setControls).
	binary.LittleEndian.PutUint32(d.devcfg[0:4], 1)
	binary.LittleEndian.PutUint32(d.devcfg[4:8], 2)
	binary.LittleEndian.PutUint32(d.devcfg[8:12], 1)
	binary.LittleEndian.PutUint32(d.devcfg[12:16], 0)
	d.cfg = buildVirtioSoundCfgSpace()
	return d
}

// setControls populates the F_CTLS control table and bumps the
// device-config region's controls counter.
func (d *fakeSoundDevice) setControls(ctrls []Control, values map[uint32][]int32) {
	d.controls = ctrls
	d.controlValues = values
	binary.LittleEndian.PutUint32(d.devcfg[12:16], uint32(len(ctrls)))
}

func barKey(bar uint8, off uint64) uint64 { return uint64(bar)<<48 | off }

// PCIConfigReader.
func (d *fakeSoundDevice) ReadConfig8(off uint8) (uint8, error) {
	if int(off) >= len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return d.cfg[off], nil
}
func (d *fakeSoundDevice) ReadConfig16(off uint8) (uint16, error) {
	if int(off)+2 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint16(d.cfg[off : off+2]), nil
}
func (d *fakeSoundDevice) ReadConfig32(off uint8) (uint32, error) {
	if int(off)+4 > len(d.cfg) {
		return 0, errors.New("read past cfg")
	}
	return le.Uint32(d.cfg[off : off+4]), nil
}

// PageAllocator.
func (d *fakeSoundDevice) AllocatePages(count int) (uint64, []byte, error) {
	if d.allocFail {
		return 0, nil, errors.New("alloc fail")
	}
	mem := make([]byte, count*int(common.PageSize))
	addr := uintptr(0)
	if len(mem) > 0 {
		d.heldPages = append(d.heldPages, mem)
		addr = uintptrFromSlice(mem)
	}
	return uint64(addr), mem, nil
}

// commonCfgBAR / commonCfgOffset are unexported but the cap-chain
// construction needs to agree on them.
func (d *fakeSoundDevice) commonCfgBAR() uint8     { return 0 }
func (d *fakeSoundDevice) commonCfgOffset() uint64 { return 0 }
func (d *fakeSoundDevice) deviceCfgOffset() uint64 { return 0x2000 }

// BARMemoryAccessor.
func (d *fakeSoundDevice) Read8(bar uint8, off uint64) (uint8, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceStatus:
			return d.deviceStatus, nil
		case common.CfgConfigGeneration:
			return 0, nil
		}
		// DeviceCfg reads (8-bit) — only used if the driver reads
		// device-cfg byte-by-byte; we route by absolute offset.
		dco := d.deviceCfgOffset()
		if off >= dco && off < dco+uint64(len(d.devcfg)) {
			return d.devcfg[off-dco], nil
		}
	}
	return uint8(d.bar[barKey(bar, off)] & 0xFF), nil
}

func (d *fakeSoundDevice) Read16(bar uint8, off uint64) (uint16, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgNumQueues:
			return 4, nil
		case common.CfgQueueSelect:
			return d.currentQueue, nil
		case common.CfgQueueSize:
			return d.qsize[d.currentQueue], nil
		case common.CfgQueueEnable:
			return d.qenable[d.currentQueue], nil
		case common.CfgQueueNotifyOff:
			return d.qnotifyOff[d.currentQueue], nil
		}
	}
	return uint16(d.bar[barKey(bar, off)] & 0xFFFF), nil
}

func (d *fakeSoundDevice) Read32(bar uint8, off uint64) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			return d.deviceFeatureSelect, nil
		case common.CfgDeviceFeature:
			if d.deviceFeatureSelect == 0 {
				return uint32(d.deviceFeatures & 0xFFFFFFFF), nil
			}
			return uint32(d.deviceFeatures >> 32), nil
		}
		dco := d.deviceCfgOffset()
		if off >= dco && off+4 <= dco+uint64(len(d.devcfg)) {
			return le.Uint32(d.devcfg[off-dco:]), nil
		}
	}
	return uint32(d.bar[barKey(bar, off)] & 0xFFFFFFFF), nil
}

func (d *fakeSoundDevice) Read64(bar uint8, off uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			return d.qdesc[d.currentQueue], nil
		case common.CfgQueueDriver:
			return d.qdriver[d.currentQueue], nil
		case common.CfgQueueDevice:
			return d.qdevice[d.currentQueue], nil
		}
	}
	return d.bar[barKey(bar, off)], nil
}

func (d *fakeSoundDevice) Write8(bar uint8, off uint64, v uint8) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() && off-d.commonCfgOffset() == common.CfgDeviceStatus {
		if v&common.StatusFeaturesOK != 0 {
			if d.clearFeaturesOK || d.driverFeatures&common.FeatureVersion1 == 0 {
				v &^= common.StatusFeaturesOK
			}
		}
		d.deviceStatus = v
		return nil
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeSoundDevice) Write16(bar uint8, off uint64, v uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueSelect:
			d.currentQueue = v
			return nil
		case common.CfgQueueSize:
			d.qsize[d.currentQueue] = v
			return nil
		case common.CfgQueueEnable:
			d.qenable[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = uint64(v)
	return nil
}

func (d *fakeSoundDevice) Write32(bar uint8, off uint64, v uint32) error {
	d.mu.Lock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgDeviceFeatureSelect:
			d.deviceFeatureSelect = v
			d.mu.Unlock()
			return nil
		case common.CfgDriverFeatureSelect:
			d.bar[barKey(bar, off)] = uint64(v)
			d.mu.Unlock()
			return nil
		case common.CfgDriverFeature:
			sel := d.bar[barKey(bar, common.CfgDriverFeatureSelect)]
			if sel == 0 {
				d.driverFeatures = (d.driverFeatures &^ 0xFFFFFFFF) | uint64(v)
			} else {
				d.driverFeatures = (d.driverFeatures & 0xFFFFFFFF) | (uint64(v) << 32)
			}
			d.mu.Unlock()
			return nil
		}
	}
	// Notify-cfg doorbell range is 0x1000..0x1100 with multiplier=4.
	if off >= 0x1000 && off < 0x2000 {
		d.bar[barKey(bar, off)] = uint64(v)
		// queue index = (off - 0x1000) / 4
		qIdx := uint16((off - 0x1000) / 4)
		d.mu.Unlock()
		d.handleNotify(qIdx)
		return nil
	}
	d.bar[barKey(bar, off)] = uint64(v)
	d.mu.Unlock()
	return nil
}

func (d *fakeSoundDevice) Write64(bar uint8, off uint64, v uint64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if bar == d.commonCfgBAR() {
		switch off - d.commonCfgOffset() {
		case common.CfgQueueDesc:
			d.qdesc[d.currentQueue] = v
			return nil
		case common.CfgQueueDriver:
			d.qdriver[d.currentQueue] = v
			return nil
		case common.CfgQueueDevice:
			d.qdevice[d.currentQueue] = v
			return nil
		}
	}
	d.bar[barKey(bar, off)] = v
	return nil
}

// handleNotify simulates the device-side reaction to a doorbell on
// queue qIdx. The eventq doorbell is a no-op (the MVP never reads from
// it). The controlq / txq / rxq doorbells walk the most-recently-
// published chain, write the appropriate device-writable bytes (status
// hdr / xfer trailer / capture payload), and publish a used-ring entry.
func (d *fakeSoundDevice) handleNotify(qIdx uint16) {
	switch qIdx {
	case ControlQueueIdx:
		if !d.ctrlCompletes {
			return
		}
		d.completeChain(qIdx, d.processControl)
	case TxQueueIdx:
		if !d.txCompletes {
			return
		}
		if d.txDeferComplete {
			// Capture the head index for the test to release later via
			// deliverTxComplete().
			head := d.peekLastHead(qIdx)
			d.mu.Lock()
			d.txDeferred = append(d.txDeferred, head)
			d.mu.Unlock()
			return
		}
		d.completeChain(qIdx, d.processTx)
	case RxQueueIdx:
		if !d.rxCompletes {
			return
		}
		d.completeChain(qIdx, d.processRx)
	}
}

// completeChain walks the most-recently-published available-ring entry
// for queue qIdx, calls `process` with the descriptor slice + the head
// index + the chain length (total bytes written by device), then
// publishes a used-ring entry reporting that length.
func (d *fakeSoundDevice) completeChain(qIdx uint16, process func(desc []byte, head uint16) uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	availAddr := d.qdriver[qIdx]
	usedAddr := d.qdevice[qIdx]
	descAddr := d.qdesc[qIdx]
	if availAddr == 0 || usedAddr == 0 || descAddr == 0 {
		return
	}
	size := d.qsize[qIdx]
	availSlice := readBufferBytes(uintptr(availAddr), 4+2*int(size))
	if availSlice == nil {
		return
	}
	availIdx := le.Uint16(availSlice[2:4])
	if availIdx == 0 {
		return
	}
	lastSlot := (availIdx - 1) % size
	head := le.Uint16(availSlice[4+lastSlot*2 : 4+lastSlot*2+2])
	descSlice := readBufferBytes(uintptr(descAddr), 16*int(size))
	if descSlice == nil {
		return
	}
	length := process(descSlice, head)
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(size))
	if usedSlice == nil {
		return
	}
	usedIdx := le.Uint16(usedSlice[2:4])
	slot := usedIdx % size
	uo := 4 + int(slot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(head))
	le.PutUint32(usedSlice[uo+4:uo+8], length)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
}

// peekLastHead returns the head-index of the most-recently-published
// chain on queue `qIdx`, without dequeueing anything. Used by
// deferred-tx tests.
func (d *fakeSoundDevice) peekLastHead(qIdx uint16) uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	availAddr := d.qdriver[qIdx]
	if availAddr == 0 {
		return 0
	}
	size := d.qsize[qIdx]
	availSlice := readBufferBytes(uintptr(availAddr), 4+2*int(size))
	if availSlice == nil {
		return 0
	}
	availIdx := le.Uint16(availSlice[2:4])
	if availIdx == 0 {
		return 0
	}
	lastSlot := (availIdx - 1) % size
	return le.Uint16(availSlice[4+lastSlot*2 : 4+lastSlot*2+2])
}

// deliverTxComplete releases one pending deferred-tx completion, walking
// the buffered head and publishing a used-ring entry.
func (d *fakeSoundDevice) deliverTxComplete() {
	d.mu.Lock()
	if len(d.txDeferred) == 0 {
		d.mu.Unlock()
		return
	}
	head := d.txDeferred[0]
	d.txDeferred = d.txDeferred[1:]
	d.mu.Unlock()
	d.completeKnownChain(TxQueueIdx, head, d.processTx)
}

// completeKnownChain is completeChain with a caller-supplied `head`
// (rather than dequeueing from the avail-ring). Used by
// deliverTxComplete.
func (d *fakeSoundDevice) completeKnownChain(qIdx, head uint16, process func(desc []byte, head uint16) uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	usedAddr := d.qdevice[qIdx]
	descAddr := d.qdesc[qIdx]
	if usedAddr == 0 || descAddr == 0 {
		return
	}
	size := d.qsize[qIdx]
	descSlice := readBufferBytes(uintptr(descAddr), 16*int(size))
	if descSlice == nil {
		return
	}
	length := process(descSlice, head)
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(size))
	if usedSlice == nil {
		return
	}
	usedIdx := le.Uint16(usedSlice[2:4])
	slot := usedIdx % size
	uo := 4 + int(slot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(head))
	le.PutUint32(usedSlice[uo+4:uo+8], length)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
}

// pushEvent writes one virtio_snd_event into the next-available eventq
// buffer and publishes a used-ring entry for it. Used by the eventq
// tests to inject period-elapsed / xrun / ctl-notify events.
func (d *fakeSoundDevice) pushEvent(code, data uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	availAddr := d.qdriver[EventQueueIdx]
	usedAddr := d.qdevice[EventQueueIdx]
	descAddr := d.qdesc[EventQueueIdx]
	if availAddr == 0 || usedAddr == 0 || descAddr == 0 {
		return
	}
	size := d.qsize[EventQueueIdx]
	availSlice := readBufferBytes(uintptr(availAddr), 4+2*int(size))
	usedSlice := readBufferBytes(uintptr(usedAddr), 4+8*int(size))
	descSlice := readBufferBytes(uintptr(descAddr), 16*int(size))
	if availSlice == nil || usedSlice == nil || descSlice == nil {
		return
	}
	availIdx := le.Uint16(availSlice[2:4])
	if availIdx == 0 {
		return
	}
	usedIdx := le.Uint16(usedSlice[2:4])
	// Use the next un-consumed avail slot, i.e. usedIdx itself.
	slot := usedIdx % size
	head := le.Uint16(availSlice[4+slot*2 : 4+slot*2+2])
	// Write the 8-byte event into the head descriptor's data buffer.
	o := int(head) * 16
	addr := le.Uint64(descSlice[o : o+8])
	length := le.Uint32(descSlice[o+8 : o+12])
	if length < 8 {
		return
	}
	mem := readBufferBytes(uintptr(addr), 8)
	if mem == nil {
		return
	}
	le.PutUint32(mem[0:4], code)
	le.PutUint32(mem[4:8], data)
	// Publish the used-ring entry.
	uo := 4 + int(slot)*8
	le.PutUint32(usedSlice[uo:uo+4], uint32(head))
	le.PutUint32(usedSlice[uo+4:uo+8], 8)
	le.PutUint16(usedSlice[2:4], usedIdx+1)
}

// processControl walks a controlq chain: descriptor 0 is the request
// body, descriptor 1 is the device-writable response header (4 bytes),
// optional descriptor 2 is the response payload. The fake parses the
// command code, writes ctrlStatus into the header, and seeds the
// payload for PCM_INFO.
func (d *fakeSoundDevice) processControl(desc []byte, head uint16) uint32 {
	// Walk up to 3 descriptors via VIRTQ_DESC_F_NEXT.
	addrs := make([]uint64, 0, 3)
	lengths := make([]uint32, 0, 3)
	idx := head
	for i := 0; i < 3; i++ {
		o := int(idx) * 16
		if o+16 > len(desc) {
			break
		}
		addr := le.Uint64(desc[o : o+8])
		length := le.Uint32(desc[o+8 : o+12])
		flags := le.Uint16(desc[o+12 : o+14])
		next := le.Uint16(desc[o+14 : o+16])
		addrs = append(addrs, addr)
		lengths = append(lengths, length)
		if flags&0x1 == 0 { // not VIRTQ_DESC_F_NEXT
			break
		}
		idx = next
	}
	if len(addrs) < 2 {
		return 0
	}
	// Parse the request body's first 4 bytes (the command code).
	reqBytes := readBufferBytes(uintptr(addrs[0]), int(lengths[0]))
	if len(reqBytes) < 4 {
		return 0
	}
	code := le.Uint32(reqBytes[0:4])
	// Write the response header.
	hdrBytes := readBufferBytes(uintptr(addrs[1]), int(lengths[1]))
	if len(hdrBytes) >= 4 {
		le.PutUint32(hdrBytes[0:4], d.ctrlStatus)
	}
	// PCM_INFO: seed the payload so the test can verify a round-trip.
	if code == RPCMInfo && len(addrs) >= 3 {
		plBytes := readBufferBytes(uintptr(addrs[2]), int(lengths[2]))
		// Per stream: 48-byte virtio_snd_pcm_info.
		// Stream 0 = output, stream 1 = input.
		count := uint32(len(plBytes)) / PCMInfoEntrySize
		for s := uint32(0); s < count; s++ {
			off := int(s * PCMInfoEntrySize)
			info, ok := d.pcmInfoOverrides[s]
			if !ok {
				// Defaults: stream 0 = output, stream 1 = input.
				info = PCMInfoEntry{
					HDAFnGroup:  0x1000 + s,
					Features:    0,
					Formats:     1 << PCMFmtS16,
					Rates:       1 << PCMRate44100,
					Direction:   uint8(s),
					ChannelsMin: 1,
					ChannelsMax: 2,
				}
			}
			le.PutUint32(plBytes[off:off+4], info.HDAFnGroup)
			le.PutUint32(plBytes[off+16:off+20], info.Features)
			le.PutUint64(plBytes[off+20:off+28], info.Formats)
			le.PutUint64(plBytes[off+28:off+36], info.Rates)
			plBytes[off+36] = info.Direction
			plBytes[off+37] = info.ChannelsMin
			plBytes[off+38] = info.ChannelsMax
		}
	}
	// PCM_SET_PARAMS: record the request for the test to inspect.
	if code == RPCMSetParams {
		d.lastSetParamsReq = append([]byte(nil), reqBytes...)
	}
	// CTL_INFO: emit one virtio_snd_ctl_info entry per registered
	// control.
	if code == RCtlInfo && len(addrs) >= 3 {
		plBytes := readBufferBytes(uintptr(addrs[2]), int(lengths[2]))
		count := uint32(len(plBytes)) / CtlInfoEntrySize
		for i := uint32(0); i < count && int(i) < len(d.controls); i++ {
			off := int(i * CtlInfoEntrySize)
			c := d.controls[i]
			copy(plBytes[off:off+16], c.HDA[:])
			le.PutUint32(plBytes[off+16:off+20], c.Role)
			le.PutUint32(plBytes[off+20:off+24], uint32(c.Type))
			le.PutUint32(plBytes[off+24:off+28], uint32(c.Access))
			le.PutUint32(plBytes[off+28:off+32], c.NumChannels)
			le.PutUint32(plBytes[off+32:off+36], c.Min)
			le.PutUint32(plBytes[off+36:off+40], c.Max)
			le.PutUint32(plBytes[off+40:off+44], c.Step)
			// Name: zero-fill then copy the prefix.
			for k := 0; k < ControlNameMaxLen; k++ {
				plBytes[off+44+k] = 0
			}
			n := len(c.Name)
			if n > ControlNameMaxLen {
				n = ControlNameMaxLen
			}
			copy(plBytes[off+44:off+44+n], []byte(c.Name)[:n])
		}
	}
	// CTL_READ: return controlValues[id] packed as int32-LE.
	if code == RCtlRead && len(addrs) >= 3 {
		id := le.Uint32(reqBytes[4:8])
		plBytes := readBufferBytes(uintptr(addrs[2]), int(lengths[2]))
		vals := d.controlValues[id]
		for i, v := range vals {
			off := i * 4
			if off+4 > len(plBytes) {
				break
			}
			le.PutUint32(plBytes[off:off+4], uint32(v))
		}
	}
	// CTL_WRITE: update controlValues[id] from the request body.
	if code == RCtlWrite && len(reqBytes) >= 8 {
		id := le.Uint32(reqBytes[4:8])
		nvals := (len(reqBytes) - 8) / 4
		vals := make([]int32, nvals)
		for i := 0; i < nvals; i++ {
			off := 8 + i*4
			vals[i] = int32(le.Uint32(reqBytes[off : off+4]))
		}
		if d.controlValues == nil {
			d.controlValues = map[uint32][]int32{}
		}
		d.controlValues[id] = vals
	}
	// Return the device-written byte count across the chain (header +
	// optional payload).
	total := uint32(0)
	for _, l := range lengths[1:] {
		total += l
	}
	return total
}

// processTx walks a tx chain: descriptor 0 is the 4-byte xfer header
// (ro), descriptor 1 is the audio data (ro), descriptor 2 is the
// 8-byte device-writable status trailer (wo). The fake writes
// xferStatus into the trailer.
func (d *fakeSoundDevice) processTx(desc []byte, head uint16) uint32 {
	addrs, lengths := walkChain(desc, head, 3)
	if len(addrs) < 3 {
		return 0
	}
	statusBytes := readBufferBytes(uintptr(addrs[2]), int(lengths[2]))
	if len(statusBytes) >= 8 {
		le.PutUint32(statusBytes[0:4], d.xferStatus)
		le.PutUint32(statusBytes[4:8], 0)
	}
	// Bytes written by device = the status trailer.
	return lengths[2]
}

// processRx walks an rx chain: descriptor 0 is the 4-byte xfer header
// (ro), descriptor 1 is the device-writable capture buffer (wo),
// descriptor 2 is the device-writable status trailer (wo). The fake
// writes rxPayload into the capture buffer and xferStatus into the
// trailer.
func (d *fakeSoundDevice) processRx(desc []byte, head uint16) uint32 {
	addrs, lengths := walkChain(desc, head, 3)
	if len(addrs) < 3 {
		return 0
	}
	cap := readBufferBytes(uintptr(addrs[1]), int(lengths[1]))
	n := 0
	if cap != nil {
		n = copy(cap, d.rxPayload)
	}
	statusBytes := readBufferBytes(uintptr(addrs[2]), int(lengths[2]))
	if len(statusBytes) >= 8 {
		le.PutUint32(statusBytes[0:4], d.xferStatus)
		le.PutUint32(statusBytes[4:8], 0)
	}
	// Device-written byte count = the bytes it filled in the capture
	// buffer + the status trailer (Virtio 1.1 §2.6.8: used-ring `len`
	// is the *device-writable* total).
	return uint32(n) + lengths[2]
}

// walkChain returns the addr+length pair for up to `max` descriptors
// in the chain starting at `head`, following VIRTQ_DESC_F_NEXT.
func walkChain(desc []byte, head uint16, max int) ([]uint64, []uint32) {
	addrs := make([]uint64, 0, max)
	lengths := make([]uint32, 0, max)
	idx := head
	for i := 0; i < max; i++ {
		o := int(idx) * 16
		if o+16 > len(desc) {
			break
		}
		addr := le.Uint64(desc[o : o+8])
		length := le.Uint32(desc[o+8 : o+12])
		flags := le.Uint16(desc[o+12 : o+14])
		next := le.Uint16(desc[o+14 : o+16])
		addrs = append(addrs, addr)
		lengths = append(lengths, length)
		if flags&0x1 == 0 {
			break
		}
		idx = next
	}
	return addrs, lengths
}

// buildVirtioSoundCfgSpace builds a 256-byte PCI config-space buffer
// with a virtio-sound cap chain:
//
//	0x00 VID=0x1AF4 DID=0x1059
//	0x06 Status[CapList]=1
//	0x34 CapPtr=0x40
//	0x40 CommonCfg cap (16 bytes) BAR=0 offset=0 length=0x38
//	0x50 NotifyCfg ext cap (20 bytes) BAR=0 offset=0x1000 length=0x100
//	     [+16..+20] = 4 (notify_off_multiplier)
//	0x64 DeviceCfg cap (16 bytes) BAR=0 offset=0x2000 length=0x10
//	     next = end
func buildVirtioSoundCfgSpace() []byte {
	cfg := make([]byte, 256)
	le.PutUint16(cfg[0:], common.PCIVendorID)
	le.PutUint16(cfg[2:], PCIDeviceIDModernSound)
	le.PutUint16(cfg[6:], common.PCIStatusCapabilityList)
	cfg[0x34] = 0x40

	// CommonCfg cap at 0x40.
	cfg[0x40] = common.PCICapIDVendorSpecific
	cfg[0x41] = 0x50 // next
	cfg[0x42] = 16   // cap_len
	cfg[0x43] = common.PCICapCommonCfg
	cfg[0x44] = 0                  // bar
	cfg[0x45] = 0                  // id
	le.PutUint32(cfg[0x48:], 0)    // offset
	le.PutUint32(cfg[0x4C:], 0x38) // length

	// NotifyCfg ext cap at 0x50, 20 bytes.
	cfg[0x50] = common.PCICapIDVendorSpecific
	cfg[0x51] = 0x64 // next
	cfg[0x52] = 20   // cap_len (extended)
	cfg[0x53] = common.PCICapNotifyCfg
	cfg[0x54] = 0
	cfg[0x55] = 0
	le.PutUint32(cfg[0x58:], 0x1000) // offset
	le.PutUint32(cfg[0x5C:], 0x100)  // length
	le.PutUint32(cfg[0x60:], 4)      // notify_off_multiplier

	// DeviceCfg cap at 0x64, 16 bytes, next = end.
	cfg[0x64] = common.PCICapIDVendorSpecific
	cfg[0x65] = 0x00
	cfg[0x66] = 16
	cfg[0x67] = common.PCICapDeviceCfg
	cfg[0x68] = 0
	cfg[0x69] = 0
	le.PutUint32(cfg[0x6C:], 0x2000) // offset
	le.PutUint32(cfg[0x70:], 0x10)   // length

	return cfg
}

// --- happy-path + semantic tests --------------------------------------

func TestOpenVirtioSound_Success(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if v.NegotiatedFeatures != common.FeatureVersion1 {
		t.Errorf("Negotiated: got 0x%x, want 0x%x", v.NegotiatedFeatures, common.FeatureVersion1)
	}
	if v.Device.Jacks != 1 || v.Device.Streams != 2 || v.Device.Chmaps != 1 {
		t.Errorf("DeviceConfig: %+v", v.Device)
	}
	if v.ControlQueue() == nil || v.EventQueue() == nil || v.TxQueue() == nil || v.RxQueue() == nil {
		t.Error("a virtqueue accessor returned nil")
	}
}

func TestOpenVirtioSound_IgnoresExtraDeviceBits(t *testing.T) {
	// v0.2.0 negotiates FeatureCTLS (bit 0) when the device offers it;
	// bit 40 is reserved and must be masked out.
	d := newFakeSoundDevice(common.FeatureVersion1 | (1 << 40) | FeatureCTLS)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	want := common.FeatureVersion1 | FeatureCTLS
	if v.NegotiatedFeatures != want {
		t.Errorf("Negotiated: got 0x%x, want 0x%x", v.NegotiatedFeatures, want)
	}
}

func TestOpenVirtioSound_NoCTLS(t *testing.T) {
	// When the host doesn't offer F_CTLS, v0.2.0 must still bring the
	// device up — F_CTLS is opportunistic, not required.
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if v.ControlsFeatureNegotiated() {
		t.Error("ControlsFeatureNegotiated should be false without F_CTLS")
	}
	if _, err := v.Controls(); !errors.Is(err, ErrControlsNotNegotiated) {
		t.Errorf("got %v, want ErrControlsNotNegotiated", err)
	}
	if _, err := v.ControlRead(0); !errors.Is(err, ErrControlsNotNegotiated) {
		t.Errorf("ControlRead: got %v, want ErrControlsNotNegotiated", err)
	}
	if err := v.ControlWrite(0, []int32{1}); !errors.Is(err, ErrControlsNotNegotiated) {
		t.Errorf("ControlWrite: got %v, want ErrControlsNotNegotiated", err)
	}
	if err := v.SetVolume(0, 50); !errors.Is(err, ErrControlsNotNegotiated) {
		t.Errorf("SetVolume: got %v, want ErrControlsNotNegotiated", err)
	}
	if err := v.SetMute(0, true); !errors.Is(err, ErrControlsNotNegotiated) {
		t.Errorf("SetMute: got %v, want ErrControlsNotNegotiated", err)
	}
}

func TestAcceptFeatures(t *testing.T) {
	if got, err := AcceptFeatures(common.FeatureVersion1 | (1 << 7)); err != nil || got != common.FeatureVersion1 {
		t.Errorf("AcceptFeatures(modern): got 0x%x, %v", got, err)
	}
	if _, err := AcceptFeatures(1 << 7); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("AcceptFeatures(legacy): got %v, want ErrNotModernDevice", err)
	}
}

func TestOpenVirtioSound_WrongDeviceID(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	le.PutUint16(d.cfg[2:], common.PCIDeviceIDModernNet)
	if _, err := OpenVirtioSound(d); !errors.Is(err, ErrInitWrongDeviceID) {
		t.Errorf("got %v, want ErrInitWrongDeviceID", err)
	}
}

func TestOpenVirtioSound_LegacyDevice(t *testing.T) {
	d := newFakeSoundDevice(1 << 7)
	if _, err := OpenVirtioSound(d); !errors.Is(err, ErrNotModernDevice) {
		t.Errorf("got %v, want ErrNotModernDevice", err)
	}
}

func TestOpenVirtioSound_FeaturesNotOK(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.clearFeaturesOK = true
	if _, err := OpenVirtioSound(d); !errors.Is(err, ErrFeaturesNotOK) {
		t.Errorf("got %v, want ErrFeaturesNotOK", err)
	}
}

func TestOpenVirtioSound_QueueZeroSize(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.qsize[0] = 0
	if _, err := OpenVirtioSound(d); !errors.Is(err, ErrQueueNotAvailable) {
		t.Errorf("got %v, want ErrQueueNotAvailable", err)
	}
}

func TestOpenVirtioSound_QueueSizeClampAndRound(t *testing.T) {
	// maxSize=6 → clamp 16 → 6, round 6 → 4.
	d := newFakeSoundDevice(common.FeatureVersion1)
	for i := uint16(0); i < 4; i++ {
		d.qsize[i] = 6
	}
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if got := v.ControlQueue().Layout.Size; got != 4 {
		t.Errorf("ctrlq size: got %d, want 4", got)
	}
}

func TestOpenVirtioSound_AllocFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.allocFail = true
	if _, err := OpenVirtioSound(d); err == nil {
		t.Error("expected alloc error")
	}
}

func TestSentinelError(t *testing.T) {
	if got := ErrControlTimeout.Error(); got != string(ErrControlTimeout) {
		t.Errorf("Error(): got %q", got)
	}
}

func TestReadBufferBytes_NilGuard(t *testing.T) {
	if readBufferBytes(0, 8) != nil {
		t.Error("addr==0 should return nil")
	}
	if readBufferBytes(1234, 0) != nil {
		t.Error("length<=0 should return nil")
	}
}

// --- controlq PCM lifecycle -------------------------------------------

func TestPCMInfo_RoundTrip(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	infos, err := v.PCMInfo()
	if err != nil {
		t.Fatalf("PCMInfo: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("PCMInfo: got %d entries, want 2", len(infos))
	}
	if infos[0].Direction != PCMDirOutput {
		t.Errorf("stream 0 direction: got %d, want %d", infos[0].Direction, PCMDirOutput)
	}
	if infos[1].Direction != PCMDirInput {
		t.Errorf("stream 1 direction: got %d, want %d", infos[1].Direction, PCMDirInput)
	}
	if infos[0].Formats&(1<<PCMFmtS16) == 0 {
		t.Errorf("stream 0 formats missing S16: 0x%x", infos[0].Formats)
	}
	if infos[0].ChannelsMax != 2 {
		t.Errorf("stream 0 ChannelsMax: got %d, want 2", infos[0].ChannelsMax)
	}
}

func TestPCMInfo_ZeroStreams(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	binary.LittleEndian.PutUint32(d.devcfg[4:8], 0) // streams = 0
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	infos, err := v.PCMInfo()
	if err != nil || infos != nil {
		t.Errorf("PCMInfo zero-streams: got (%v, %v), want (nil, nil)", infos, err)
	}
}

func TestPCMInfo_DeviceError(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.ctrlStatus = SBadMsg
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.PCMInfo(); !errors.Is(err, ErrDeviceStatus) {
		t.Errorf("got %v, want ErrDeviceStatus", err)
	}
}

func TestPCMInfo_ControlTimeout(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.ctrlCompletes = false
	if _, err := v.PCMInfo(); !errors.Is(err, ErrControlTimeout) {
		t.Errorf("got %v, want ErrControlTimeout", err)
	}
}

func TestPCMSetParams_RoundTrip(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	p := PCMParams{
		BufferBytes: 4096, PeriodBytes: 1024, Features: 0,
		Channels: 2, Format: PCMFmtS16, Rate: PCMRate44100,
	}
	if err := v.PCMSetParams(0, p); err != nil {
		t.Errorf("PCMSetParams: %v", err)
	}
}

func TestPCMSetParams_DeviceError(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.ctrlStatus = SNotSupp
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if err := v.PCMSetParams(0, PCMParams{}); !errors.Is(err, ErrDeviceStatus) {
		t.Errorf("got %v, want ErrDeviceStatus", err)
	}
}

func TestPCMSetParams_BadStreamID(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if err := v.PCMSetParams(99, PCMParams{}); !errors.Is(err, ErrStreamIDOutOfRange) {
		t.Errorf("got %v, want ErrStreamIDOutOfRange", err)
	}
}

func TestPCMSimpleCommands(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*VirtioSound) error
	}{
		{"Prepare", func(v *VirtioSound) error { return v.PCMPrepare(0) }},
		{"Start", func(v *VirtioSound) error { return v.PCMStart(0) }},
		{"Stop", func(v *VirtioSound) error { return v.PCMStop(0) }},
		{"Release", func(v *VirtioSound) error { return v.PCMRelease(0) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeSoundDevice(common.FeatureVersion1)
			v, err := OpenVirtioSound(d)
			if err != nil {
				t.Fatalf("OpenVirtioSound: %v", err)
			}
			if err := tc.fn(v); err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
		})
	}
}

func TestPCMSimpleCommands_DeviceError(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.ctrlStatus = SIOErr
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if err := v.PCMStart(0); !errors.Is(err, ErrDeviceStatus) {
		t.Errorf("got %v, want ErrDeviceStatus", err)
	}
}

func TestPCMSimpleCommands_BadStreamID(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if err := v.PCMPrepare(99); !errors.Is(err, ErrStreamIDOutOfRange) {
		t.Errorf("got %v, want ErrStreamIDOutOfRange", err)
	}
}

// --- Write path (PCM playback / txq) ----------------------------------

func TestWrite_RoundTrip(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	pcm := make([]byte, 512)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	n, err := v.Write(0, pcm)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(pcm) {
		t.Errorf("Write n: got %d, want %d", n, len(pcm))
	}
}

func TestWrite_ZeroLen(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	n, err := v.Write(0, nil)
	if err != nil || n != 0 {
		t.Errorf("Write(nil): got (%d, %v)", n, err)
	}
}

func TestWrite_BadStreamID(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.Write(99, []byte{1, 2, 3}); !errors.Is(err, ErrStreamIDOutOfRange) {
		t.Errorf("got %v, want ErrStreamIDOutOfRange", err)
	}
}

func TestWrite_DeviceError(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.xferStatus = SIOErr
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.Write(0, []byte{1, 2, 3}); !errors.Is(err, ErrDeviceStatus) {
		t.Errorf("got %v, want ErrDeviceStatus", err)
	}
}

func TestWrite_Timeout(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.txCompletes = false
	if _, err := v.Write(0, []byte{1, 2, 3}); !errors.Is(err, ErrXferTimeout) {
		t.Errorf("got %v, want ErrXferTimeout", err)
	}
}

// --- Read path (PCM capture / rxq) ------------------------------------

func TestRead_RoundTrip(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.rxPayload = []byte("capturedaudio")
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	buf := make([]byte, 64)
	n, err := v.Read(1, buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != len(d.rxPayload) {
		t.Errorf("Read n: got %d, want %d", n, len(d.rxPayload))
	}
	if string(buf[:n]) != string(d.rxPayload) {
		t.Errorf("Read bytes: got %q, want %q", buf[:n], d.rxPayload)
	}
}

func TestRead_ZeroLen(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	n, err := v.Read(1, nil)
	if err != nil || n != 0 {
		t.Errorf("Read(nil): got (%d, %v)", n, err)
	}
}

func TestRead_BadStreamID(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.Read(99, make([]byte, 4)); !errors.Is(err, ErrStreamIDOutOfRange) {
		t.Errorf("got %v, want ErrStreamIDOutOfRange", err)
	}
}

func TestRead_DeviceError(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.xferStatus = SIOErr
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.Read(1, make([]byte, 4)); !errors.Is(err, ErrDeviceStatus) {
		t.Errorf("got %v, want ErrDeviceStatus", err)
	}
}

func TestRead_Timeout(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.rxCompletes = false
	if _, err := v.Read(1, make([]byte, 4)); !errors.Is(err, ErrXferTimeout) {
		t.Errorf("got %v, want ErrXferTimeout", err)
	}
}

// --- messages.go encode/decode -----------------------------------------

func TestBuildQueryInfoReq(t *testing.T) {
	b := buildQueryInfoReq(RPCMInfo, 0, 2, PCMInfoEntrySize)
	if len(b) != int(QueryInfoReqSize) {
		t.Errorf("size: got %d", len(b))
	}
	if le.Uint32(b[0:4]) != RPCMInfo {
		t.Errorf("code mismatch")
	}
	if le.Uint32(b[12:16]) != PCMInfoEntrySize {
		t.Errorf("entry size mismatch")
	}
}

func TestBuildPCMHdrReq(t *testing.T) {
	b := buildPCMHdrReq(RPCMStart, 7)
	if le.Uint32(b[0:4]) != RPCMStart || le.Uint32(b[4:8]) != 7 {
		t.Errorf("round-trip: got code=%d, streamID=%d", le.Uint32(b[0:4]), le.Uint32(b[4:8]))
	}
}

func TestBuildPCMSetParamsReq(t *testing.T) {
	p := PCMParams{BufferBytes: 0x1234, PeriodBytes: 0x100, Features: 0x5,
		Channels: 2, Format: PCMFmtS16, Rate: PCMRate48000}
	b := buildPCMSetParamsReq(3, p)
	if le.Uint32(b[0:4]) != RPCMSetParams || le.Uint32(b[4:8]) != 3 {
		t.Errorf("header mismatch")
	}
	if le.Uint32(b[8:12]) != 0x1234 {
		t.Errorf("buffer_bytes mismatch")
	}
	if b[20] != 2 || b[21] != PCMFmtS16 || b[22] != PCMRate48000 {
		t.Errorf("tail mismatch")
	}
}

func TestBuildPCMXferHdr(t *testing.T) {
	b := buildPCMXferHdr(42)
	if le.Uint32(b[0:4]) != 42 {
		t.Errorf("stream_id mismatch")
	}
}

func TestParseHdr_Short(t *testing.T) {
	if _, err := parseHdr([]byte{1, 2}); !errors.Is(err, ErrShortResponse) {
		t.Errorf("got %v, want ErrShortResponse", err)
	}
}

func TestParseHdr_Round(t *testing.T) {
	b := make([]byte, 4)
	le.PutUint32(b, SOK)
	if s, err := parseHdr(b); err != nil || s != SOK {
		t.Errorf("got (%d, %v)", s, err)
	}
}

func TestParsePCMInfoEntry_Short(t *testing.T) {
	if _, err := parsePCMInfoEntry(make([]byte, 4)); !errors.Is(err, ErrShortResponse) {
		t.Errorf("got %v, want ErrShortResponse", err)
	}
}

func TestPagesFor(t *testing.T) {
	if got := pagesFor(0); got != 1 {
		t.Errorf("pagesFor(0): got %d, want 1", got)
	}
	if got := pagesFor(uint32(common.PageSize)); got != 1 {
		t.Errorf("pagesFor(PageSize): got %d, want 1", got)
	}
	if got := pagesFor(uint32(common.PageSize) + 1); got != 2 {
		t.Errorf("pagesFor(PageSize+1): got %d, want 2", got)
	}
}
