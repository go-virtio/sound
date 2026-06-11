// Transport-error injection harness for the OpenVirtioSound path and
// the controlq / data-queue paths. Mirrors the pattern used by
// go-virtio/console's injectTransport.

package sound

import (
	"errors"
	"testing"

	"github.com/go-virtio/common"
)

var errInjected = errors.New("injected transport failure")

type failPoint struct {
	method string
	nth    int // fail on this 1-based call count to method; 0 = never
}

// injectTransport wraps a fakeSoundDevice and fails the nth call to a
// chosen method once `enable` is set.
type injectTransport struct {
	*fakeSoundDevice
	fp     failPoint
	counts map[string]int
	enable bool

	// zeroPhys forces AllocatePages to return phys==0 (counted only
	// while enabled).
	zeroPhys bool

	// shortAllocBytes truncates the returned mem to that many bytes
	// (counted only while enabled).
	shortAllocBytes int
}

func newInject(d *fakeSoundDevice, enable bool) *injectTransport {
	return &injectTransport{fakeSoundDevice: d, counts: map[string]int{}, enable: enable}
}

func (t *injectTransport) fail(m string) bool {
	if !t.enable || t.fp.method != m {
		return false
	}
	t.counts[m]++
	return t.counts[m] == t.fp.nth
}

func (t *injectTransport) ReadConfig16(o uint8) (uint16, error) {
	if t.fail("ReadConfig16") {
		return 0, errInjected
	}
	return t.fakeSoundDevice.ReadConfig16(o)
}

func (t *injectTransport) Read8(b uint8, o uint64) (uint8, error) {
	if t.fail("Read8") {
		return 0, errInjected
	}
	return t.fakeSoundDevice.Read8(b, o)
}
func (t *injectTransport) Read16(b uint8, o uint64) (uint16, error) {
	if t.fail("Read16") {
		return 0, errInjected
	}
	return t.fakeSoundDevice.Read16(b, o)
}
func (t *injectTransport) Read32(b uint8, o uint64) (uint32, error) {
	if t.fail("Read32") {
		return 0, errInjected
	}
	return t.fakeSoundDevice.Read32(b, o)
}
func (t *injectTransport) Write8(b uint8, o uint64, v uint8) error {
	if t.fail("Write8") {
		return errInjected
	}
	return t.fakeSoundDevice.Write8(b, o, v)
}
func (t *injectTransport) Write16(b uint8, o uint64, v uint16) error {
	if t.fail("Write16") {
		return errInjected
	}
	return t.fakeSoundDevice.Write16(b, o, v)
}
func (t *injectTransport) Write32(b uint8, o uint64, v uint32) error {
	if t.fail("Write32") {
		return errInjected
	}
	// Doorbell-targeted failures: notify offset is 0x1000 + queueIdx*4.
	for _, target := range []struct {
		key string
		off uint64
	}{
		{"Write32@0x1000", 0x1000}, // controlq doorbell
		{"Write32@0x1004", 0x1004}, // eventq doorbell
		{"Write32@0x1008", 0x1008}, // txq doorbell
		{"Write32@0x100c", 0x100c}, // rxq doorbell
	} {
		if t.enable && t.fp.method == target.key && o == target.off {
			t.counts[target.key]++
			if t.counts[target.key] == t.fp.nth {
				return errInjected
			}
		}
	}
	return t.fakeSoundDevice.Write32(b, o, v)
}
func (t *injectTransport) Write64(b uint8, o uint64, v uint64) error {
	if t.fail("Write64") {
		return errInjected
	}
	return t.fakeSoundDevice.Write64(b, o, v)
}
func (t *injectTransport) AllocatePages(c int) (uint64, []byte, error) {
	if t.fail("AllocatePages") {
		return 0, nil, errInjected
	}
	phys, mem, err := t.fakeSoundDevice.AllocatePages(c)
	if err != nil {
		return phys, mem, err
	}
	if t.enable && t.zeroPhys {
		return 0, mem, nil
	}
	if t.enable && t.shortAllocBytes > 0 && t.shortAllocBytes < len(mem) {
		mem = mem[:t.shortAllocBytes]
	}
	return phys, mem, err
}

// TestOpenVirtioSound_TransportErrors drives every `if err != nil`
// return inside OpenVirtioSound + setupQueue by failing the
// corresponding transport call.
func TestOpenVirtioSound_TransportErrors(t *testing.T) {
	cases := []struct {
		name string
		fp   failPoint
	}{
		{"DIDRead", failPoint{"ReadConfig16", 1}},
		{"InitModernConfig", failPoint{"ReadConfig16", 2}}, // PCI status read
		{"ResetStatus", failPoint{"Write8", 1}},
		{"PostResetStatusRead", failPoint{"Read8", 1}},
		{"AckStatus", failPoint{"Write8", 2}},
		{"DriverStatus", failPoint{"Write8", 3}},
		{"DeviceFeatures", failPoint{"Write32", 1}}, // WriteDeviceFeatureSelect(0)
		{"DriverFeatures", failPoint{"Write32", 3}}, // WriteDriverFeatureSelect(0)
		{"FeaturesOKStatus", failPoint{"Write8", 4}},
		{"PostFeaturesStatusRead", failPoint{"Read8", 2}},
		// controlq (queue setup #1).
		{"CtrlSelectQueue", failPoint{"Write16", 1}},
		{"CtrlQueueSize", failPoint{"Read16", 1}},
		{"CtrlSetQueueSize", failPoint{"Write16", 2}},
		{"CtrlQueueNotifyOff", failPoint{"Read16", 2}},
		{"CtrlAllocVirtqueue", failPoint{"AllocatePages", 1}},
		{"CtrlSetQueueDesc", failPoint{"Write64", 1}},
		{"CtrlSetQueueDriver", failPoint{"Write64", 2}},
		{"CtrlSetQueueDevice", failPoint{"Write64", 3}},
		{"CtrlSetQueueEnable", failPoint{"Write16", 3}},
		// DRIVER_OK status write.
		{"DriverOKStatus", failPoint{"Write8", 5}},
		// Device-config read (32-bit; the first one after queue setup).
		{"ReadDeviceConfigJacks", failPoint{"Read32", 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newFakeSoundDevice(common.FeatureVersion1)
			it := newInject(d, true)
			it.fp = tc.fp
			if _, err := OpenVirtioSound(it); err == nil {
				t.Fatalf("%s: expected error injected at %+v", tc.name, tc.fp)
			}
		})
	}
}

// TestOpenVirtioSound_FillEventBufferTooSmall covers fillEventRing's
// ErrBufferTooSmall branch: the event-buffer AllocatePages returns a
// truncated page.
func TestOpenVirtioSound_FillEventBufferTooSmall(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	w := &shortAllocAfter{injectTransport: it, shortAfter: 4, shortBytes: 8}
	if _, err := OpenVirtioSound(w); !errors.Is(err, ErrBufferTooSmall) {
		t.Errorf("got %v, want ErrBufferTooSmall", err)
	}
}

// TestOpenVirtioSound_FillEventZeroPhys covers fillEventRing's
// ErrAllocReturnedZero branch.
func TestOpenVirtioSound_FillEventZeroPhys(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	w := &zeroPhysAfter{injectTransport: it, zeroAfter: 4}
	if _, err := OpenVirtioSound(w); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

// TestOpenVirtioSound_FillEventQueueFull covers fillEventRing's
// AddBuffer error branch.
func TestOpenVirtioSound_FillEventQueueFull(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	for i := range v.eventq.Buffers {
		v.eventq.Buffers[i].InUse = true
	}
	if err := v.fillEventRing(); !errors.Is(err, common.ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

// TestOpenVirtioSound_EventNotify covers the post-fillEventRing notify
// failure branch.
func TestOpenVirtioSound_EventNotify(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, true)
	// EventQ doorbell is at 0x1004 (notify_off=1, multiplier=4 → 0x1000+1*4).
	it.fp = failPoint{"Write32@0x1004", 1}
	if _, err := OpenVirtioSound(it); err == nil {
		t.Error("expected event-notify error")
	}
}

// --- controlRoundTrip + Write / Read error branches -------------------

func TestControlRoundTrip_AllocFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.allocFail = true
	if _, err := v.PCMInfo(); err == nil {
		t.Error("expected alloc error")
	}
}

func TestControlRoundTrip_ZeroPhys(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	it.zeroPhys = true
	if _, err := v.PCMInfo(); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

func TestControlRoundTrip_BufferTooSmall(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	it.shortAllocBytes = 4
	if _, err := v.PCMInfo(); !errors.Is(err, ErrBufferTooSmall) {
		t.Errorf("got %v, want ErrBufferTooSmall", err)
	}
}

func TestControlRoundTrip_QueueFull(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	for i := range v.ctrlq.Buffers {
		v.ctrlq.Buffers[i].InUse = true
	}
	if _, err := v.PCMInfo(); !errors.Is(err, common.ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

func TestControlRoundTrip_NotifyFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	// Controlq doorbell at 0x1000 (notify_off=0, multiplier=4).
	it.fp = failPoint{"Write32@0x1000", 1}
	if _, err := v.PCMInfo(); err == nil {
		t.Error("expected notify error")
	}
}

func TestWrite_AllocFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.allocFail = true
	if _, err := v.Write(0, []byte{1, 2, 3}); err == nil {
		t.Error("expected alloc error")
	}
}

func TestWrite_ZeroPhys(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	it.zeroPhys = true
	if _, err := v.Write(0, []byte{1, 2, 3}); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

func TestWrite_BufferTooSmall(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	it.shortAllocBytes = 4
	if _, err := v.Write(0, []byte{1, 2, 3}); !errors.Is(err, ErrBufferTooSmall) {
		t.Errorf("got %v, want ErrBufferTooSmall", err)
	}
}

func TestWrite_QueueFull(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	for i := range v.txq.Buffers {
		v.txq.Buffers[i].InUse = true
	}
	if _, err := v.Write(0, []byte{1, 2, 3}); !errors.Is(err, common.ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

func TestWrite_NotifyFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	// TxQ doorbell at 0x1008 (notify_off=2, multiplier=4).
	it.fp = failPoint{"Write32@0x1008", 1}
	if _, err := v.Write(0, []byte{1, 2, 3}); err == nil {
		t.Error("expected notify error")
	}
}

func TestRead_AllocFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.allocFail = true
	if _, err := v.Read(1, make([]byte, 4)); err == nil {
		t.Error("expected alloc error")
	}
}

func TestRead_NotifyFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	// RxQ doorbell at 0x100c (notify_off=3, multiplier=4).
	it.fp = failPoint{"Write32@0x100c", 1}
	if _, err := v.Read(1, make([]byte, 4)); err == nil {
		t.Error("expected notify error")
	}
}

func TestRead_QueueFull(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	for i := range v.rxq.Buffers {
		v.rxq.Buffers[i].InUse = true
	}
	if _, err := v.Read(1, make([]byte, 4)); !errors.Is(err, common.ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

// --- helpers truncating / zeroing allocations after a delay ----------

type shortAllocAfter struct {
	*injectTransport
	shortAfter int
	shortBytes int
	count      int
}

func (s *shortAllocAfter) AllocatePages(c int) (uint64, []byte, error) {
	phys, mem, err := s.injectTransport.fakeSoundDevice.AllocatePages(c)
	if err != nil {
		return phys, mem, err
	}
	s.count++
	if s.count > s.shortAfter && s.shortBytes < len(mem) {
		mem = mem[:s.shortBytes]
	}
	return phys, mem, err
}

type zeroPhysAfter struct {
	*injectTransport
	zeroAfter int
	count     int
}

func (z *zeroPhysAfter) AllocatePages(c int) (uint64, []byte, error) {
	phys, mem, err := z.injectTransport.fakeSoundDevice.AllocatePages(c)
	if err != nil {
		return phys, mem, err
	}
	z.count++
	if z.count > z.zeroAfter {
		return 0, mem, nil
	}
	return phys, mem, err
}
