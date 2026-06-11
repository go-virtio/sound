// go-virtio/sound — driver core: feature negotiation + init sequence +
// shared controlq round-trip helper.
//
// This driver targets the single-jack PCM baseline (Virtio 1.2 §5.14):
// it negotiates only VIRTIO_F_VERSION_1 (so VIRTIO_SND_F_CTLS is NOT
// acknowledged) and brings the four mandatory virtqueues up — controlq
// (0), eventq (1), tx (2), rx (3). The MVP drives the PCM lifecycle
// over controlq and the raw PCM frame transfer over tx / rx; the eventq
// is reserved for future period-elapsed / xrun notification.
//
// References:
//
//   - Virtio 1.2 §5.14    "Sound Device".
//   - Virtio 1.2 §5.14.4  "Device configuration layout".
//   - Virtio 1.2 §5.14.6  "Device Operation" — controlq + dataq flow.
package sound

import (
	"github.com/go-virtio/common"
)

// PCIDeviceIDModernSound is the modern-transport (Virtio 1.0+) PCI
// device ID for virtio-sound (DeviceTypeSound=25, modern DID =
// 0x1040 + 25 = 0x1059). There is no legacy variant — virtio-sound
// postdates the 0.9 transport.
const PCIDeviceIDModernSound uint16 = 0x1059

// Virtqueue indices (Virtio 1.2 §5.14.2). The four queues are mandatory
// for any conformant device.
const (
	ControlQueueIdx uint16 = 0
	EventQueueIdx   uint16 = 1
	TxQueueIdx      uint16 = 2
	RxQueueIdx      uint16 = 3
)

// ControlQueueSize / EventQueueSize / TxQueueSize / RxQueueSize are the
// desired ring sizes. Each is clamped to the device's advertised
// maximum during setup and rounded DOWN to a power of two (some QEMU
// versions report non-power-of-two QueueSize on legacy queues).
const (
	ControlQueueSize uint16 = 16
	EventQueueSize   uint16 = 16
	TxQueueSize      uint16 = 32
	RxQueueSize      uint16 = 32
)

// CtrlPollIterations / XferPollIterations bound the busy-poll budget
// used by the controlq round-trip helper and the PCM xfer paths.
// Sub-millisecond round-trip is the norm on every backend; this is a
// generous upper bound for the busy-poll model the driver uses.
const (
	CtrlPollIterations = 200000
	XferPollIterations = 200000
)

// AcceptedFeatures is the feature mask the driver negotiates ON.
// v0.2.0 accepts VIRTIO_F_VERSION_1 (modern transport, mandatory) plus
// VIRTIO_SND_F_CTLS (bit 0) when the host offers it — when CTLS is
// negotiated, Controls() / ControlRead() / ControlWrite() /
// SetVolume() / SetMute() become usable.
const AcceptedFeatures uint64 = common.FeatureVersion1 | FeatureCTLS

// AcceptFeatures returns the negotiated feature mask: the intersection
// of what the device offers and what we accept. The caller writes this
// back via DriverFeature.
//
// We require VIRTIO_F_VERSION_1 — if the device doesn't offer it the
// device is legacy-only and we return ErrNotModernDevice.
func AcceptFeatures(deviceFeatures uint64) (uint64, error) {
	if deviceFeatures&common.FeatureVersion1 == 0 {
		return 0, ErrNotModernDevice
	}
	return deviceFeatures & AcceptedFeatures, nil
}

// VirtioSound wraps one initialised virtio-sound device. The caller
// holds this for the lifetime of the device; the underlying virtqueue
// pages live as long as the supplied PageAllocator's lifetime contract.
type VirtioSound struct {
	// Cfg is the modern-transport handle (BARs + offsets + the
	// BARMemoryAccessor used for every register access).
	Cfg *common.ModernConfig

	// NegotiatedFeatures records what the driver-feature handshake
	// settled on. Exposed for diagnostic prints.
	NegotiatedFeatures uint64

	// Device is the parsed device-config region (jacks / streams /
	// chmaps counters). Read once after FEATURES_OK.
	Device DeviceConfig

	// transport is the underlying Transport — held so the data path
	// can route DMA-buffer allocations through the PageAllocator side.
	transport common.Transport

	// ctrlq / eventq / txq / rxq are the four virtqueues set up by
	// OpenVirtioSound.
	ctrlq  *common.Virtqueue
	eventq *common.Virtqueue
	txq    *common.Virtqueue
	rxq    *common.Virtqueue

	// streams is the per-stream state registry (v0.2.0). Populated on
	// PCMInfo() / PCMSetParams() and consulted by PCMSetParams /
	// PCMSetParamsTyped to validate the requested (rate, format,
	// channels) tuple against the device-advertised bitmaps. Lazily
	// allocated so that v0.1.0 callers that never touch PCMInfo see no
	// extra allocations.
	streams []streamState

	// asyncCookies tracks WriteAsync chains awaiting completion on the
	// txq. Indexed by `cookie` (a monotonically increasing uint64); the
	// value is the descriptor head index the caller should reclaim when
	// the completion event is consumed via ReadEvents().
	asyncCookies map[uint64]asyncInflight
	nextCookie   uint64

	// pendingEvents queues completion / xrun / period-elapsed events
	// pulled out of the eventq + the txq's used-ring on each
	// ReadEvents() call. v0.2.0 surfaces these to the caller; v0.1.0
	// drained-on-busy-poll Write() ignored them.
	pendingEvents []Event
}

// streamState is the per-stream cache populated when the driver issues
// PCMInfo() — it lets PCMSetParams validate the requested (rate,
// format, channels) tuple against what the device actually advertises.
// `infoCached` distinguishes "PCMInfo hasn't been called for this
// stream" from "PCMInfo returned a zero record" (an empty bitmap is a
// legitimate device response that should reject every SetParams call).
type streamState struct {
	infoCached  bool
	info        PCMInfoEntry
	lastParams  PCMParams
	paramsValid bool
}

// asyncInflight tracks one in-flight WriteAsync chain. `head` is the
// virtqueue descriptor head index to reclaim once the device publishes
// a used-ring entry for it. `statusOff` is the offset within `mem`
// where the device writes the virtio_snd_pcm_status trailer.
type asyncInflight struct {
	streamID  uint32
	head      uint16
	mem       []byte
	statusOff uint32
	dataLen   uint32
}

// Event is the v0.2.0 driver-side representation of a completion event
// pulled off the txq used-ring or the eventq. Distinct from a raw
// VIRTIO_SND_EVT_* device event — the driver surfaces a normalised
// shape covering "this WriteAsync cookie completed", "the device fired
// a period-elapsed notification", "the stream went into xrun", and
// (when F_CTLS is up) "a control element changed".
type Event struct {
	// Kind is one of the EventKind* constants — what happened.
	Kind EventKind

	// StreamID is the stream the event pertains to, when meaningful
	// (Kind in {EventKindWriteComplete, EventKindPeriodElapsed,
	// EventKindXrun}). Zero for control-element events.
	StreamID uint32

	// Cookie is the WriteAsync cookie that finished, when
	// Kind==EventKindWriteComplete. Zero otherwise.
	Cookie uint64

	// BytesAccepted is the device's view of how many payload bytes it
	// consumed for the completed chain (excludes the 4-byte xfer header
	// and the 8-byte status trailer). Zero when not meaningful.
	BytesAccepted uint32

	// StatusCode is the raw VIRTIO_SND_S_* status code the device wrote
	// into the chain's status trailer. SOK on a clean completion.
	StatusCode uint32

	// ControlID is the control-element ID for EventKindControlChanged
	// events (F_CTLS). Zero otherwise.
	ControlID uint32
}

// EventKind classifies an Event.
type EventKind uint8

// EventKind* constants. Stable on the wire (the driver may serialise
// events for cross-process delivery, e.g. via a vsock bridge).
const (
	// EventKindUnknown is the zero value — an Event with this kind is
	// always a programmer error.
	EventKindUnknown EventKind = 0
	// EventKindWriteComplete is emitted when a WriteAsync chain hits
	// the used-ring. `Cookie` is the matching WriteAsync return value.
	EventKindWriteComplete EventKind = 1
	// EventKindPeriodElapsed is the device's VIRTIO_SND_EVT_PCM_PERIOD_ELAPSED.
	EventKindPeriodElapsed EventKind = 2
	// EventKindXrun is the device's VIRTIO_SND_EVT_PCM_XRUN.
	EventKindXrun EventKind = 3
	// EventKindJackConnected is VIRTIO_SND_EVT_JACK_CONNECTED.
	EventKindJackConnected EventKind = 4
	// EventKindJackDisconnected is VIRTIO_SND_EVT_JACK_DISCONNECTED.
	EventKindJackDisconnected EventKind = 5
	// EventKindControlChanged is VIRTIO_SND_EVT_CTL_NOTIFY (F_CTLS).
	EventKindControlChanged EventKind = 6
)

// OpenVirtioSound drives the full bring-up of one virtio-sound device:
//
//  1. Verify the PCI device ID is 0x1059 (modern sound).
//  2. InitModernConfig walks PCI caps + populates the BAR locators.
//  3. Reset → ACK → DRIVER status progression.
//  4. Read DeviceFeature, require VERSION_1, mask, write DriverFeature.
//  5. Set FEATURES_OK, verify it stuck.
//  6. Allocate + publish controlq, eventq, txq, rxq (queues 0..3).
//  7. DRIVER_OK status.
//  8. Read the device-config region (jacks / streams / chmaps).
//  9. Pre-post eventq buffers + notify the device.
//
// On success the device is in DRIVER_OK state, the eventq is pre-posted
// with one-page buffers, the controlq / txq / rxq are empty + ready,
// and the device-config counters are cached on the returned struct.
func OpenVirtioSound(t common.Transport) (*VirtioSound, error) {
	// Sanity-check this really is a modern virtio-sound device.
	did, err := t.ReadConfig16(common.PCICfgDeviceID)
	if err != nil {
		return nil, err
	}
	if did != PCIDeviceIDModernSound {
		return nil, ErrInitWrongDeviceID
	}

	cfg, err := common.InitModernConfig(t)
	if err != nil {
		return nil, err
	}

	// Step 1: full reset (write 0 to DeviceStatus).
	if err := cfg.SetDeviceStatus(0); err != nil {
		return nil, err
	}
	// Spec §3.1.1: after reset DeviceStatus reads back as 0. We don't
	// sleep — the read itself is a firmware-liveness check; its value
	// is discarded.
	if _, err := cfg.DeviceStatus(); err != nil {
		return nil, err
	}

	// Step 2: ACKNOWLEDGE.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge); err != nil {
		return nil, err
	}
	// Step 3: DRIVER.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver); err != nil {
		return nil, err
	}

	// Step 4: read DeviceFeature, mask to our accepted set, write
	// DriverFeature.
	deviceFeats, err := cfg.DeviceFeatures64()
	if err != nil {
		return nil, err
	}
	if deviceFeats&common.FeatureVersion1 == 0 {
		return nil, ErrNotModernDevice
	}
	negotiated := deviceFeats & AcceptedFeatures
	if err := cfg.SetDriverFeatures64(negotiated); err != nil {
		return nil, err
	}

	// Step 5: FEATURES_OK + verify the device accepted our subset.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK); err != nil {
		return nil, err
	}
	status, err := cfg.DeviceStatus()
	if err != nil {
		return nil, err
	}
	if status&common.StatusFeaturesOK == 0 {
		return nil, ErrFeaturesNotOK
	}

	// Step 6: queue setup (controlq, eventq, txq, rxq, in spec order).
	ctrlq, err := setupQueue(cfg, t, ControlQueueIdx, ControlQueueSize)
	if err != nil {
		return nil, err
	}
	eventq, err := setupQueue(cfg, t, EventQueueIdx, EventQueueSize)
	if err != nil {
		return nil, err
	}
	txq, err := setupQueue(cfg, t, TxQueueIdx, TxQueueSize)
	if err != nil {
		return nil, err
	}
	rxq, err := setupQueue(cfg, t, RxQueueIdx, RxQueueSize)
	if err != nil {
		return nil, err
	}

	// Step 7: DRIVER_OK.
	if err := cfg.SetDeviceStatus(common.StatusAcknowledge | common.StatusDriver | common.StatusFeaturesOK | common.StatusDriverOK); err != nil {
		return nil, err
	}

	// Step 8: read the device-config region.
	devcfg, err := readDeviceConfig(cfg)
	if err != nil {
		return nil, err
	}

	v := &VirtioSound{
		Cfg:                cfg,
		NegotiatedFeatures: negotiated,
		Device:             devcfg,
		transport:          t,
		ctrlq:              ctrlq,
		eventq:             eventq,
		txq:                txq,
		rxq:                rxq,
	}

	// Step 9: pre-post eventq buffers so the device has somewhere to
	// land asynchronous events (period-elapsed / xrun). The MVP does
	// not actively consume them but the descriptors must be present
	// for the device to function on QEMU.
	if err := v.fillEventRing(); err != nil {
		return nil, err
	}
	if err := cfg.NotifyQueue(EventQueueIdx, eventq.NotifyOff); err != nil {
		return nil, err
	}

	return v, nil
}

// setupQueue performs the per-queue init: select, read max-size, write
// our size (= min(desired, max), rounded down to a power of two),
// allocate the Virtqueue, publish its descriptor/avail/used physical
// addresses, enable.
func setupQueue(cfg *common.ModernConfig, t common.Transport, queueIdx uint16, desiredSize uint16) (*common.Virtqueue, error) {
	if err := cfg.SelectQueue(queueIdx); err != nil {
		return nil, err
	}
	maxSize, err := cfg.QueueSize()
	if err != nil {
		return nil, err
	}
	if maxSize == 0 {
		// Device doesn't have this queue; spec says the driver must
		// not use it. (maxSize >= 1 from here on, so the size computed
		// below is always a non-zero power of two — no further
		// zero-check needed.)
		return nil, ErrQueueNotAvailable
	}
	size := desiredSize
	if size > maxSize {
		size = maxSize
	}
	for size&(size-1) != 0 {
		size &= size - 1
	}
	if err := cfg.SetQueueSize(size); err != nil {
		return nil, err
	}
	notifyOff, err := cfg.QueueNotifyOff()
	if err != nil {
		return nil, err
	}
	q, err := common.NewVirtqueue(t, size, queueIdx, notifyOff)
	if err != nil {
		return nil, err
	}
	descAddr := q.BasePhys + uint64(q.Layout.DescTableOffset)
	availAddr := q.BasePhys + uint64(q.Layout.AvailRingOffset)
	usedAddr := q.BasePhys + uint64(q.Layout.UsedRingOffset)
	if err := cfg.SetQueueDesc(descAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDriver(availAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueDevice(usedAddr); err != nil {
		return nil, err
	}
	if err := cfg.SetQueueEnable(1); err != nil {
		return nil, err
	}
	return q, nil
}

// ControlQueue / EventQueue / TxQueue / RxQueue expose the four
// *common.Virtqueue handles. Read-only accessors so callers can inspect
// ring state for diagnostic dumps; the fields themselves stay
// unexported.
func (v *VirtioSound) ControlQueue() *common.Virtqueue { return v.ctrlq }

// EventQueue returns the event virtqueue handle.
func (v *VirtioSound) EventQueue() *common.Virtqueue { return v.eventq }

// TxQueue returns the PCM playback virtqueue handle.
func (v *VirtioSound) TxQueue() *common.Virtqueue { return v.txq }

// RxQueue returns the PCM capture virtqueue handle.
func (v *VirtioSound) RxQueue() *common.Virtqueue { return v.rxq }

// fillEventRing posts one device-writable one-page buffer per eventq
// slot so the device has somewhere to land asynchronous events
// (period-elapsed / xrun / jack hot-plug). The MVP does not actively
// consume them but the descriptors must be present.
func (v *VirtioSound) fillEventRing() error {
	for i := uint16(0); i < v.eventq.Layout.Size; i++ {
		phys, mem, err := v.transport.AllocatePages(1)
		if err != nil {
			return err
		}
		if phys == 0 {
			return common.ErrAllocReturnedZero
		}
		bufLen := uint32(common.PageSize)
		if uint64(bufLen) > uint64(len(mem)) {
			return ErrBufferTooSmall
		}
		addr := uintptr(phys) // identity-mapped on supported hosts
		if _, err := v.eventq.AddBuffer(addr, phys, bufLen, true); err != nil {
			return err
		}
	}
	return nil
}

// controlRoundTrip issues a controlq command consisting of a
// device-readable request body, an optional device-writable extra
// payload (for R_*_INFO queries), and the mandatory device-writable
// struct virtio_snd_hdr response. Returns the response status code
// and the parsed payload (or nil if extraLen==0).
//
// Wire pattern (Virtio 1.2 §5.14.6):
//
//	[ desc 0  ro  request body  ]
//	[ desc 1  wo  hdr (4 bytes) ]
//	[ desc 2  wo  payload       ]  ← only when extraLen > 0
//
// The buffers live in a single freshly-allocated page; the request
// body is copied in, the response slots are zero-initialised by the
// PageAllocator contract.
func (v *VirtioSound) controlRoundTrip(req []byte, extraLen uint32) (status uint32, payload []byte, err error) {
	totalLen := uint32(len(req)) + HdrSize + extraLen
	phys, mem, err := v.transport.AllocatePages(1)
	if err != nil {
		return 0, nil, err
	}
	if phys == 0 {
		return 0, nil, common.ErrAllocReturnedZero
	}
	if uint64(totalLen) > uint64(len(mem)) {
		return 0, nil, ErrBufferTooSmall
	}
	copy(mem[:len(req)], req)

	reqPhys := phys
	hdrPhys := phys + uint64(len(req))
	payloadPhys := hdrPhys + uint64(HdrSize)

	bufs := []common.ChainBuffer{
		{Addr: uintptr(reqPhys), Phys: reqPhys, Len: uint32(len(req)), Writable: false},
		{Addr: uintptr(hdrPhys), Phys: hdrPhys, Len: HdrSize, Writable: true},
	}
	if extraLen > 0 {
		bufs = append(bufs, common.ChainBuffer{
			Addr: uintptr(payloadPhys), Phys: payloadPhys, Len: extraLen, Writable: true,
		})
	}
	head, err := v.ctrlq.AddChain(bufs)
	if err != nil {
		return 0, nil, err
	}
	if err := v.Cfg.NotifyQueue(ControlQueueIdx, v.ctrlq.NotifyOff); err != nil {
		// The chain is published; the device may still complete it
		// later. We have to free the slots here because the caller is
		// abandoning the round-trip.
		_ = v.ctrlq.ReclaimChain(head)
		return 0, nil, err
	}
	for spin := 0; spin < CtrlPollIterations; spin++ {
		gotIdx, _, ok := v.ctrlq.PollUsed()
		if !ok {
			continue
		}
		_ = v.ctrlq.ReclaimChain(gotIdx)
		hdrSlice := mem[len(req) : uint32(len(req))+HdrSize]
		s, perr := parseHdr(hdrSlice)
		if perr != nil {
			return 0, nil, perr
		}
		var pl []byte
		if extraLen > 0 {
			plOff := uint32(len(req)) + HdrSize
			pl = make([]byte, extraLen)
			copy(pl, mem[plOff:plOff+extraLen])
		}
		return s, pl, nil
	}
	_ = v.ctrlq.ReclaimChain(head)
	return 0, nil, ErrControlTimeout
}

// Sentinel errors for the virtio-sound path. All exported so callers
// can branch + format them.
var (
	ErrNotModernDevice         = commonSoundError("go-virtio/sound: device doesn't offer VIRTIO_F_VERSION_1 (legacy-only)")
	ErrFeaturesNotOK           = commonSoundError("go-virtio/sound: FEATURES_OK status bit didn't stick after DriverFeature write")
	ErrInitWrongDeviceID       = commonSoundError("go-virtio/sound: PCI device ID is not 0x1059 (modern sound device)")
	ErrQueueNotAvailable       = commonSoundError("go-virtio/sound: device reports QueueSize=0 for a required queue")
	ErrBufferTooSmall          = commonSoundError("go-virtio/sound: PageAllocator returned a chunk smaller than one page")
	ErrControlTimeout          = commonSoundError("go-virtio/sound: controlq poll timeout (device did not respond)")
	ErrXferTimeout             = commonSoundError("go-virtio/sound: PCM xfer poll timeout (device did not return descriptor)")
	ErrShortResponse           = commonSoundError("go-virtio/sound: device response shorter than expected struct size")
	ErrDeviceStatus            = commonSoundError("go-virtio/sound: device reported non-OK status for controlq command")
	ErrStreamIDOutOfRange      = commonSoundError("go-virtio/sound: stream id is outside the device-advertised range")
	ErrUnsupportedRate         = commonSoundError("go-virtio/sound: requested rate is not in the stream's advertised bitmap")
	ErrUnsupportedFormat       = commonSoundError("go-virtio/sound: requested format is not in the stream's advertised bitmap")
	ErrUnsupportedChannelCount = commonSoundError("go-virtio/sound: requested channel count is outside the stream's advertised range")
	ErrInvalidRateValue        = commonSoundError("go-virtio/sound: typed rate must be a single-bit PCMRate constant")
	ErrInvalidFormatValue      = commonSoundError("go-virtio/sound: typed format must be a single-bit PCMFormat constant")
	ErrInvalidControlID        = commonSoundError("go-virtio/sound: control id is outside the device-advertised range")
	ErrInvalidControlValues    = commonSoundError("go-virtio/sound: control values slice length does not match control's NumChannels")
	ErrControlsNotNegotiated   = commonSoundError("go-virtio/sound: VIRTIO_SND_F_CTLS was not negotiated with the device")
	ErrCookieNotFound          = commonSoundError("go-virtio/sound: WriteAsync cookie not tracked (already drained?)")
	ErrInvalidVolumePercent    = commonSoundError("go-virtio/sound: volume percent must be in [0,100]")
)

// ensureStreams lazily allocates the per-stream state registry once
// PCMInfo / PCMSetParams need it. Idempotent.
func (v *VirtioSound) ensureStreams() {
	if v.streams == nil && v.Device.Streams > 0 {
		v.streams = make([]streamState, v.Device.Streams)
	}
}

// cacheStreamInfo records a PCMInfo entry for `streamID` so subsequent
// PCMSetParams calls can validate the (rate, format, channels) tuple
// against it. Called by PCMInfo() after the device's response parses
// cleanly.
func (v *VirtioSound) cacheStreamInfo(streamID uint32, info PCMInfoEntry) {
	v.ensureStreams()
	if int(streamID) >= len(v.streams) {
		return
	}
	v.streams[streamID].infoCached = true
	v.streams[streamID].info = info
}

// cachedStreamInfo returns the cached PCMInfo entry for `streamID`,
// along with a "is cached?" flag. Returns (zero, false) when PCMInfo()
// has never been called.
func (v *VirtioSound) cachedStreamInfo(streamID uint32) (PCMInfoEntry, bool) {
	if int(streamID) >= len(v.streams) {
		return PCMInfoEntry{}, false
	}
	if !v.streams[streamID].infoCached {
		return PCMInfoEntry{}, false
	}
	return v.streams[streamID].info, true
}

// recordStreamParams remembers the params the driver last sent for
// `streamID`. Used by SetVolume to scale the requested percent against
// the stream's channel count, and for diagnostic dumps.
func (v *VirtioSound) recordStreamParams(streamID uint32, p PCMParams) {
	v.ensureStreams()
	if int(streamID) >= len(v.streams) {
		return
	}
	v.streams[streamID].lastParams = p
	v.streams[streamID].paramsValid = true
}

// StreamParams returns the last params the driver successfully sent for
// `streamID` along with a flag indicating whether PCMSetParams has run
// at least once. Useful for callers building latency / underrun
// diagnostics without re-issuing controlq round-trips.
func (v *VirtioSound) StreamParams(streamID uint32) (PCMParams, bool) {
	if int(streamID) >= len(v.streams) {
		return PCMParams{}, false
	}
	if !v.streams[streamID].paramsValid {
		return PCMParams{}, false
	}
	return v.streams[streamID].lastParams, true
}

// commonSoundError is the package's tiny sentinel-error type — same
// pattern as go-virtio/common.commonError and
// go-virtio/console.commonConsoleError.
type commonSoundError string

// Error implements the `error` interface.
func (e commonSoundError) Error() string { return string(e) }
