// v0.2.0 driver tests — rate/format negotiation, F_CTLS controls,
// F_MULTI_PORTS multi-stream tracking, eventq async write +
// ReadEvents().
//
// SPDX-License-Identifier: BSD-3-Clause

package sound

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/go-virtio/common"
)

// --- rate negotiation -------------------------------------------------

func TestPCMRate_Hz(t *testing.T) {
	cases := []struct {
		rate PCMRate
		want uint32
	}{
		{RateUnknown, 0},
		{Rate5512, 5512},
		{Rate11025, 11025}, // DOOM
		{Rate44100, 44100},
		{Rate48000, 48000},
		{Rate96000, 96000},
		{Rate384000, 384000},
		{Rate11025 | Rate48000, 11025}, // multi-bit ⇒ lowest set
	}
	for _, tc := range cases {
		if got := tc.rate.Hz(); got != tc.want {
			t.Errorf("Hz(0x%x): got %d, want %d", tc.rate, got, tc.want)
		}
	}
}

func TestPCMRate_HzNoBitsKnown(t *testing.T) {
	// A rate with a bit set above the spec's defined range falls
	// through to 0.
	r := PCMRate(1 << 60)
	if got := r.Hz(); got != 0 {
		t.Errorf("Hz(out-of-range): got %d, want 0", got)
	}
}

func TestPCMRate_ByteID(t *testing.T) {
	if got := Rate11025.ByteID(); got != PCMRate11025 {
		t.Errorf("Rate11025.ByteID: got %d, want %d", got, PCMRate11025)
	}
	if got := Rate44100.ByteID(); got != PCMRate44100 {
		t.Errorf("Rate44100.ByteID: got %d, want %d", got, PCMRate44100)
	}
	if got := RateUnknown.ByteID(); got != 0xFF {
		t.Errorf("RateUnknown.ByteID: got %d, want 0xFF", got)
	}
	if got := PCMRate(1 << 60).ByteID(); got != 0xFF {
		t.Errorf("out-of-range.ByteID: got %d, want 0xFF", got)
	}
}

func TestPCMRate_IsSet(t *testing.T) {
	bitmap := Rate11025 | Rate44100 | Rate48000
	if !bitmap.IsSet(Rate11025) {
		t.Error("bitmap missing Rate11025")
	}
	if bitmap.IsSet(Rate8000) {
		t.Error("bitmap unexpectedly contains Rate8000")
	}
}

func TestRateFromByteID(t *testing.T) {
	if got := RateFromByteID(PCMRate11025); got != Rate11025 {
		t.Errorf("RateFromByteID(11025): got 0x%x, want 0x%x", got, Rate11025)
	}
	if got := RateFromByteID(99); got != RateUnknown {
		t.Errorf("RateFromByteID(99): got 0x%x, want RateUnknown", got)
	}
}

func TestPCMInfoEntry_SupportedRates(t *testing.T) {
	info := PCMInfoEntry{Rates: uint64(Rate11025 | Rate44100 | Rate48000)}
	rates := info.SupportedRates()
	if len(rates) != 3 {
		t.Fatalf("SupportedRates: got %d, want 3", len(rates))
	}
	if rates[0] != Rate11025 || rates[1] != Rate44100 || rates[2] != Rate48000 {
		t.Errorf("SupportedRates: got %v", rates)
	}
}

// --- format negotiation -----------------------------------------------

func TestPCMFormat_ByteID(t *testing.T) {
	if got := FormatU8.ByteID(); got != PCMFmtU8 {
		t.Errorf("FormatU8.ByteID: got %d, want %d", got, PCMFmtU8)
	}
	if got := FormatS16LE.ByteID(); got != PCMFmtS16 {
		t.Errorf("FormatS16LE.ByteID: got %d, want %d", got, PCMFmtS16)
	}
	if got := FormatUnknown.ByteID(); got != 0xFF {
		t.Errorf("FormatUnknown.ByteID: got %d, want 0xFF", got)
	}
	if got := PCMFormat(1 << 60).ByteID(); got != 0xFF {
		t.Errorf("out-of-range.ByteID: got %d, want 0xFF", got)
	}
}

func TestPCMFormat_String(t *testing.T) {
	cases := []struct {
		f    PCMFormat
		want string
	}{
		{FormatUnknown, "UNKNOWN"},
		{FormatU8, "U8"},
		{FormatS16LE, "S16_LE"},
		{FormatFloat, "FLOAT_LE"},
		{PCMFormat(1 << 60), "UNKNOWN"},
	}
	for _, tc := range cases {
		if got := tc.f.String(); got != tc.want {
			t.Errorf("String(0x%x): got %q, want %q", tc.f, got, tc.want)
		}
	}
}

func TestPCMFormat_IsSet(t *testing.T) {
	bitmap := FormatU8 | FormatS16LE
	if !bitmap.IsSet(FormatU8) {
		t.Error("bitmap missing FormatU8")
	}
	if bitmap.IsSet(FormatFloat) {
		t.Error("bitmap unexpectedly contains FormatFloat")
	}
}

func TestFormatFromByteID(t *testing.T) {
	if got := FormatFromByteID(PCMFmtU8); got != FormatU8 {
		t.Errorf("FormatFromByteID(U8): got 0x%x, want 0x%x", got, FormatU8)
	}
	if got := FormatFromByteID(99); got != FormatUnknown {
		t.Errorf("FormatFromByteID(99): got 0x%x, want FormatUnknown", got)
	}
}

func TestPCMInfoEntry_SupportedFormats(t *testing.T) {
	info := PCMInfoEntry{Formats: uint64(FormatU8 | FormatS16LE | FormatS24LE)}
	fmts := info.SupportedFormats()
	if len(fmts) != 3 {
		t.Fatalf("SupportedFormats: got %d, want 3", len(fmts))
	}
	if fmts[0] != FormatU8 || fmts[1] != FormatS16LE || fmts[2] != FormatS24LE {
		t.Errorf("SupportedFormats: got %v", fmts)
	}
}

func TestFormatsKnownGood(t *testing.T) {
	// Spec-explicit: the v0.2.0 known-good set covers DOOM (U8 + S8 +
	// S16_LE) plus modern hi-fi (S24_LE / S32_LE). Format conversion
	// for everything else is application-layer.
	for _, f := range []PCMFormat{FormatS8, FormatU8, FormatS16LE, FormatS24LE, FormatS32LE} {
		if !FormatsKnownGood.IsSet(f) {
			t.Errorf("FormatsKnownGood missing %s", f)
		}
	}
	if FormatsKnownGood.IsSet(FormatFloat) {
		t.Error("FormatsKnownGood should NOT include FormatFloat (no conversion guaranteed)")
	}
}

// --- PCMSetParams DOOM-style: 11025 Hz, U8, mono ----------------------

func TestPCMSetParams_DOOMRequest(t *testing.T) {
	// Advertise stream 0 with DOOM's needs: U8 + 11025 Hz + mono.
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.pcmInfoOverrides = map[uint32]PCMInfoEntry{
		0: {
			Formats:     uint64(FormatU8 | FormatS16LE),
			Rates:       uint64(Rate11025 | Rate44100),
			Direction:   PCMDirOutput,
			ChannelsMin: 1, ChannelsMax: 2,
		},
	}
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	infos, err := v.PCMInfo()
	if err != nil {
		t.Fatalf("PCMInfo: %v", err)
	}
	if !infos[0].SupportedRates()[0].IsSet(Rate11025) {
		t.Errorf("stream 0 first-rate: %v", infos[0].SupportedRates())
	}
	// DOOM params via the typed helper.
	p := TypedPCMParams{
		BufferBytes: 4096, PeriodBytes: 1024,
		Channels: 1, Format: FormatU8, Rate: Rate11025,
	}
	if err := v.PCMSetParamsTyped(0, p); err != nil {
		t.Fatalf("PCMSetParamsTyped: %v", err)
	}
	// Verify the wire request the device received.
	if got := d.lastSetParamsReq[20]; got != 1 {
		t.Errorf("channels byte: got %d, want 1", got)
	}
	if got := d.lastSetParamsReq[21]; got != PCMFmtU8 {
		t.Errorf("format byte: got %d, want %d (U8)", got, PCMFmtU8)
	}
	if got := d.lastSetParamsReq[22]; got != PCMRate11025 {
		t.Errorf("rate byte: got %d, want %d (11025)", got, PCMRate11025)
	}
	// And the StreamParams cache reflects the most-recent params.
	gotP, ok := v.StreamParams(0)
	if !ok {
		t.Error("StreamParams: not cached after SetParams")
	}
	if gotP.Rate != PCMRate11025 || gotP.Format != PCMFmtU8 {
		t.Errorf("StreamParams: got %+v", gotP)
	}
}

func TestPCMSetParams_ValidatesUnsupportedRate(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.pcmInfoOverrides = map[uint32]PCMInfoEntry{
		0: {
			Formats:     uint64(FormatS16LE),
			Rates:       uint64(Rate44100),
			Direction:   PCMDirOutput,
			ChannelsMin: 1, ChannelsMax: 2,
		},
	}
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.PCMInfo(); err != nil {
		t.Fatalf("PCMInfo: %v", err)
	}
	err = v.PCMSetParams(0, PCMParams{
		BufferBytes: 4096, PeriodBytes: 1024,
		Channels: 2, Format: PCMFmtS16, Rate: PCMRate48000,
	})
	if !errors.Is(err, ErrUnsupportedRate) {
		t.Errorf("got %v, want ErrUnsupportedRate", err)
	}
}

func TestPCMSetParams_ValidatesUnsupportedFormat(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.pcmInfoOverrides = map[uint32]PCMInfoEntry{
		0: {
			Formats:     uint64(FormatS16LE),
			Rates:       uint64(Rate44100),
			Direction:   PCMDirOutput,
			ChannelsMin: 1, ChannelsMax: 2,
		},
	}
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.PCMInfo(); err != nil {
		t.Fatalf("PCMInfo: %v", err)
	}
	err = v.PCMSetParams(0, PCMParams{
		Channels: 2, Format: PCMFmtFloat, Rate: PCMRate44100,
	})
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("got %v, want ErrUnsupportedFormat", err)
	}
}

func TestPCMSetParams_ValidatesUnsupportedChannels(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.pcmInfoOverrides = map[uint32]PCMInfoEntry{
		0: {
			Formats:     uint64(FormatS16LE),
			Rates:       uint64(Rate44100),
			ChannelsMin: 1, ChannelsMax: 2,
		},
	}
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.PCMInfo(); err != nil {
		t.Fatalf("PCMInfo: %v", err)
	}
	err = v.PCMSetParams(0, PCMParams{
		Channels: 8, Format: PCMFmtS16, Rate: PCMRate44100,
	})
	if !errors.Is(err, ErrUnsupportedChannelCount) {
		t.Errorf("got %v, want ErrUnsupportedChannelCount", err)
	}
}

func TestPCMSetParams_NoValidationWithoutInfo(t *testing.T) {
	// v0.1.0 callers that skip PCMInfo see no validation — the wire
	// request goes out unchanged.
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	// No PCMInfo call: validation skipped, request goes through.
	if err := v.PCMSetParams(0, PCMParams{
		Channels: 2, Format: PCMFmtS16, Rate: PCMRate44100,
	}); err != nil {
		t.Errorf("PCMSetParams: %v", err)
	}
}

func TestPCMSetParamsTyped_RejectsMultiBitRate(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	err = v.PCMSetParamsTyped(0, TypedPCMParams{
		Channels: 2, Format: FormatS16LE, Rate: Rate44100 | Rate48000,
	})
	if !errors.Is(err, ErrInvalidRateValue) {
		t.Errorf("got %v, want ErrInvalidRateValue", err)
	}
}

func TestPCMSetParamsTyped_RejectsZeroRate(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	err = v.PCMSetParamsTyped(0, TypedPCMParams{
		Channels: 2, Format: FormatS16LE, Rate: RateUnknown,
	})
	if !errors.Is(err, ErrInvalidRateValue) {
		t.Errorf("got %v, want ErrInvalidRateValue", err)
	}
}

func TestPCMSetParamsTyped_RejectsMultiBitFormat(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	err = v.PCMSetParamsTyped(0, TypedPCMParams{
		Channels: 2, Format: FormatS16LE | FormatS24LE, Rate: Rate44100,
	})
	if !errors.Is(err, ErrInvalidFormatValue) {
		t.Errorf("got %v, want ErrInvalidFormatValue", err)
	}
}

func TestPCMSetParamsTyped_RejectsZeroFormat(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	err = v.PCMSetParamsTyped(0, TypedPCMParams{
		Channels: 2, Format: FormatUnknown, Rate: Rate44100,
	})
	if !errors.Is(err, ErrInvalidFormatValue) {
		t.Errorf("got %v, want ErrInvalidFormatValue", err)
	}
}

func TestStreamParams_Empty(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, ok := v.StreamParams(0); ok {
		t.Error("StreamParams should be empty before SetParams")
	}
	if _, ok := v.StreamParams(99); ok {
		t.Error("StreamParams out-of-range should return false")
	}
}

func TestPCMSetParams_PeriodBytesRoundTrip(t *testing.T) {
	// The MVP plumbed period_bytes through but didn't expose tests
	// for it. v0.2.0 documents period_bytes as a latency hint and
	// asserts the wire encoding round-trips correctly.
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	p := PCMParams{
		BufferBytes: 8192, PeriodBytes: 256, // tight latency
		Channels: 2, Format: PCMFmtS16, Rate: PCMRate48000,
	}
	if err := v.PCMSetParams(1, p); err != nil {
		t.Fatalf("PCMSetParams: %v", err)
	}
	if got := le.Uint32(d.lastSetParamsReq[8:12]); got != 8192 {
		t.Errorf("buffer_bytes wire: got %d, want 8192", got)
	}
	if got := le.Uint32(d.lastSetParamsReq[12:16]); got != 256 {
		t.Errorf("period_bytes wire: got %d, want 256", got)
	}
}

// --- F_MULTI_PORTS: two streams in flight independently ---------------

func TestMultiPort_TwoStreamsIndependent(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	// fakeSoundDevice already advertises streams=2.
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	// Configure both streams with different rates so we can confirm
	// each is tracked independently.
	if err := v.PCMSetParams(0, PCMParams{Channels: 2, Format: PCMFmtS16, Rate: PCMRate44100}); err != nil {
		t.Fatalf("SetParams stream 0: %v", err)
	}
	if err := v.PCMSetParams(1, PCMParams{Channels: 1, Format: PCMFmtS16, Rate: PCMRate48000}); err != nil {
		t.Fatalf("SetParams stream 1: %v", err)
	}
	p0, _ := v.StreamParams(0)
	p1, _ := v.StreamParams(1)
	if p0.Rate != PCMRate44100 || p1.Rate != PCMRate48000 {
		t.Errorf("StreamParams isolation: stream0=%+v stream1=%+v", p0, p1)
	}
	// Lifecycle commands on stream 0 must not interfere with stream 1.
	if err := v.PCMPrepare(0); err != nil {
		t.Fatalf("Prepare 0: %v", err)
	}
	if err := v.PCMStart(0); err != nil {
		t.Fatalf("Start 0: %v", err)
	}
	// Stream 1 still in SET_PARAMS state — but the fake's stateless
	// model accepts any command, so we just check both succeed.
	if err := v.PCMPrepare(1); err != nil {
		t.Fatalf("Prepare 1: %v", err)
	}
	// Simultaneous writes: stream 0 = playback, stream 1 = capture.
	if _, err := v.Write(0, []byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("Write 0: %v", err)
	}
	d.rxPayload = []byte("hello")
	buf := make([]byte, 8)
	if n, err := v.Read(1, buf); err != nil || n != 5 {
		t.Errorf("Read 1: got (%d, %v), want (5, nil)", n, err)
	}
	if err := v.PCMStop(0); err != nil {
		t.Fatalf("Stop 0: %v", err)
	}
}

// --- F_CTLS controls --------------------------------------------------

func newCTLSDevice(t *testing.T) (*fakeSoundDevice, *VirtioSound) {
	t.Helper()
	d := newFakeSoundDevice(common.FeatureVersion1 | FeatureCTLS)
	d.setControls([]Control{
		{
			Role: 0, Type: ControlTypeInteger, Access: ControlAccessRead | ControlAccessWrite,
			NumChannels: 2, Min: 0, Max: 100, Step: 1,
			Name: "PCM 0 Playback Volume",
		},
		{
			Role: 0, Type: ControlTypeBoolean, Access: ControlAccessRead | ControlAccessWrite,
			NumChannels: 1, Min: 0, Max: 1, Step: 1,
			Name: "PCM 0 Playback Switch",
		},
		{
			Role: 0, Type: ControlTypeInteger, Access: ControlAccessRead | ControlAccessWrite,
			NumChannels: 2, Min: 0, Max: 100, Step: 1,
			Name: "Master Playback Volume",
		},
	}, map[uint32][]int32{
		0: {50, 50},
		1: {1},
		2: {75, 75},
	})
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if !v.ControlsFeatureNegotiated() {
		t.Fatal("F_CTLS not negotiated")
	}
	return d, v
}

func TestControls_Enumerate(t *testing.T) {
	_, v := newCTLSDevice(t)
	ctrls, err := v.Controls()
	if err != nil {
		t.Fatalf("Controls: %v", err)
	}
	if len(ctrls) != 3 {
		t.Fatalf("Controls: got %d, want 3", len(ctrls))
	}
	if ctrls[0].Name != "PCM 0 Playback Volume" {
		t.Errorf("ctrls[0].Name: got %q", ctrls[0].Name)
	}
	if ctrls[0].Type != ControlTypeInteger {
		t.Errorf("ctrls[0].Type: got %d", ctrls[0].Type)
	}
	if ctrls[0].NumChannels != 2 {
		t.Errorf("ctrls[0].NumChannels: got %d", ctrls[0].NumChannels)
	}
	if ctrls[0].ID != 0 {
		t.Errorf("ctrls[0].ID: got %d", ctrls[0].ID)
	}
	if !strings.Contains(ctrls[1].Name, "Switch") {
		t.Errorf("ctrls[1].Name: got %q", ctrls[1].Name)
	}
}

func TestControlRead_Write(t *testing.T) {
	_, v := newCTLSDevice(t)
	vals, err := v.ControlRead(0)
	if err != nil {
		t.Fatalf("ControlRead: %v", err)
	}
	if len(vals) < 2 || vals[0] != 50 || vals[1] != 50 {
		t.Errorf("ControlRead: got %v, want [50,50,...]", vals[:min(2, len(vals))])
	}
	if err := v.ControlWrite(0, []int32{80, 80}); err != nil {
		t.Fatalf("ControlWrite: %v", err)
	}
	vals2, err := v.ControlRead(0)
	if err != nil {
		t.Fatalf("ControlRead 2: %v", err)
	}
	if vals2[0] != 80 || vals2[1] != 80 {
		t.Errorf("ControlRead after write: got %v", vals2[:2])
	}
}

func TestControlWrite_InvalidID(t *testing.T) {
	_, v := newCTLSDevice(t)
	if err := v.ControlWrite(99, []int32{50}); !errors.Is(err, ErrInvalidControlID) {
		t.Errorf("got %v, want ErrInvalidControlID", err)
	}
	if _, err := v.ControlRead(99); !errors.Is(err, ErrInvalidControlID) {
		t.Errorf("ControlRead 99: got %v, want ErrInvalidControlID", err)
	}
}

func TestControlWrite_EmptyValues(t *testing.T) {
	_, v := newCTLSDevice(t)
	if err := v.ControlWrite(0, nil); !errors.Is(err, ErrInvalidControlValues) {
		t.Errorf("got %v, want ErrInvalidControlValues", err)
	}
}

func TestSetVolume_StreamSpecific(t *testing.T) {
	d, v := newCTLSDevice(t)
	if err := v.SetVolume(0, 100); err != nil {
		t.Fatalf("SetVolume: %v", err)
	}
	// PCM 0 Playback Volume is at id=0; expect [100, 100] since
	// streamID 0 + 100% scales to Max.
	if d.controlValues[0][0] != 100 || d.controlValues[0][1] != 100 {
		t.Errorf("control 0: got %v, want [100,100]", d.controlValues[0])
	}
	// Master (id=2) should be untouched (stream-specific wins).
	if d.controlValues[2][0] != 75 {
		t.Errorf("Master untouched: got %v", d.controlValues[2])
	}
}

func TestSetVolume_FallsBackToMaster(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1 | FeatureCTLS)
	d.setControls([]Control{
		{Type: ControlTypeInteger, NumChannels: 2, Min: 0, Max: 100, Step: 1,
			Name: "Master Playback Volume"},
	}, map[uint32][]int32{0: {0, 0}})
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if err := v.SetVolume(0, 50); err != nil {
		t.Fatalf("SetVolume: %v", err)
	}
	if d.controlValues[0][0] != 50 {
		t.Errorf("Master volume: got %v, want [50,50]", d.controlValues[0])
	}
}

func TestSetVolume_PercentTooBig(t *testing.T) {
	_, v := newCTLSDevice(t)
	if err := v.SetVolume(0, 200); !errors.Is(err, ErrInvalidVolumePercent) {
		t.Errorf("got %v, want ErrInvalidVolumePercent", err)
	}
}

func TestSetVolume_NoMatchingControl(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1 | FeatureCTLS)
	d.setControls([]Control{
		{Type: ControlTypeBoolean, NumChannels: 1, Name: "Some Switch"},
	}, map[uint32][]int32{0: {0}})
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if err := v.SetVolume(0, 50); !errors.Is(err, ErrInvalidControlID) {
		t.Errorf("got %v, want ErrInvalidControlID", err)
	}
}

func TestSetMute_StreamSpecific(t *testing.T) {
	d, v := newCTLSDevice(t)
	if err := v.SetMute(0, true); err != nil {
		t.Fatalf("SetMute: %v", err)
	}
	// switch=0 means muted (ALSA convention).
	if d.controlValues[1][0] != 0 {
		t.Errorf("mute control: got %v, want [0]", d.controlValues[1])
	}
	if err := v.SetMute(0, false); err != nil {
		t.Fatalf("SetMute unmute: %v", err)
	}
	if d.controlValues[1][0] != 1 {
		t.Errorf("unmute control: got %v, want [1]", d.controlValues[1])
	}
}

func TestSetMute_NoMatchingControl(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1 | FeatureCTLS)
	d.setControls([]Control{
		{Type: ControlTypeInteger, NumChannels: 1, Name: "Volume"},
	}, map[uint32][]int32{0: {0}})
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if err := v.SetMute(0, true); !errors.Is(err, ErrInvalidControlID) {
		t.Errorf("got %v, want ErrInvalidControlID", err)
	}
}

func TestControls_EmptyTable(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1 | FeatureCTLS)
	// setControls with empty table sets devcfg[12:16]=0; the driver
	// should return (nil, nil).
	d.setControls(nil, nil)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	ctrls, err := v.Controls()
	if err != nil || ctrls != nil {
		t.Errorf("Controls empty: got (%v, %v)", ctrls, err)
	}
}

func TestControls_DeviceError(t *testing.T) {
	d, v := newCTLSDevice(t)
	d.ctrlStatus = SIOErr
	if _, err := v.Controls(); !errors.Is(err, ErrDeviceStatus) {
		t.Errorf("got %v, want ErrDeviceStatus", err)
	}
	if _, err := v.ControlRead(0); !errors.Is(err, ErrDeviceStatus) {
		t.Errorf("ControlRead: got %v, want ErrDeviceStatus", err)
	}
	if err := v.ControlWrite(0, []int32{1, 1}); !errors.Is(err, ErrDeviceStatus) {
		t.Errorf("ControlWrite: got %v, want ErrDeviceStatus", err)
	}
}

func TestParseControlInfoEntry_Short(t *testing.T) {
	if _, err := parseControlInfoEntry(make([]byte, 4)); !errors.Is(err, ErrShortResponse) {
		t.Errorf("got %v, want ErrShortResponse", err)
	}
}

func TestStreamIDTag(t *testing.T) {
	cases := []struct {
		in   uint32
		want string
	}{
		{0, "0 "},
		{1, "1 "},
		{42, "42 "},
		{1234567, "1234567 "},
	}
	for _, tc := range cases {
		if got := streamIDTag(tc.in); got != tc.want {
			t.Errorf("streamIDTag(%d): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestContainsSubstring(t *testing.T) {
	if !containsSubstring("hello world", "") {
		t.Error("empty substring should match")
	}
	if containsSubstring("hi", "hello") {
		t.Error("sub longer than s should not match")
	}
	if !containsSubstring("Playback Volume", "Volume") {
		t.Error("substring at end should match")
	}
	if containsSubstring("Playback Volume", "Mute") {
		t.Error("missing substring should not match")
	}
}

// --- eventq async path ------------------------------------------------

func TestWriteAsync_RoundTrip(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.txDeferComplete = true
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	cookie, err := v.WriteAsync(0, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatalf("WriteAsync: %v", err)
	}
	if cookie == 0 {
		t.Error("WriteAsync returned zero cookie")
	}
	if v.PendingAsyncCount() != 1 {
		t.Errorf("PendingAsyncCount: got %d, want 1", v.PendingAsyncCount())
	}
	// No completions yet.
	if got := v.ReadEvents(); len(got) != 0 {
		t.Errorf("ReadEvents before deliver: got %v", got)
	}
	// Trigger the deferred completion.
	d.deliverTxComplete()
	evs := v.ReadEvents()
	if len(evs) != 1 {
		t.Fatalf("ReadEvents: got %d events, want 1", len(evs))
	}
	if evs[0].Kind != EventKindWriteComplete {
		t.Errorf("Kind: got %d, want EventKindWriteComplete", evs[0].Kind)
	}
	if evs[0].Cookie != cookie {
		t.Errorf("Cookie: got %d, want %d", evs[0].Cookie, cookie)
	}
	if evs[0].StatusCode != SOK {
		t.Errorf("StatusCode: got 0x%x, want SOK", evs[0].StatusCode)
	}
	if v.PendingAsyncCount() != 0 {
		t.Errorf("PendingAsyncCount after drain: got %d", v.PendingAsyncCount())
	}
}

func TestWriteAsync_ZeroLen(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	cookie, err := v.WriteAsync(0, nil)
	if err != nil {
		t.Errorf("WriteAsync(nil): %v", err)
	}
	if cookie != 0 {
		t.Errorf("WriteAsync(nil): cookie=%d, want 0", cookie)
	}
}

func TestWriteAsync_BadStreamID(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	if _, err := v.WriteAsync(99, []byte{1}); !errors.Is(err, ErrStreamIDOutOfRange) {
		t.Errorf("got %v, want ErrStreamIDOutOfRange", err)
	}
}

func TestWriteAsync_AllocFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.allocFail = true
	if _, err := v.WriteAsync(0, []byte{1, 2, 3}); err == nil {
		t.Error("expected alloc error")
	}
}

func TestWriteAsync_MultipleCookies(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	d.txDeferComplete = true
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	c1, err := v.WriteAsync(0, []byte{1, 2})
	if err != nil {
		t.Fatalf("WriteAsync c1: %v", err)
	}
	c2, err := v.WriteAsync(0, []byte{3, 4})
	if err != nil {
		t.Fatalf("WriteAsync c2: %v", err)
	}
	if c1 == c2 {
		t.Error("cookies should be distinct")
	}
	if v.PendingAsyncCount() != 2 {
		t.Errorf("PendingAsyncCount: got %d, want 2", v.PendingAsyncCount())
	}
	d.deliverTxComplete()
	d.deliverTxComplete()
	evs := v.ReadEvents()
	if len(evs) != 2 {
		t.Fatalf("ReadEvents: got %d events, want 2", len(evs))
	}
	cookies := map[uint64]bool{evs[0].Cookie: true, evs[1].Cookie: true}
	if !cookies[c1] || !cookies[c2] {
		t.Errorf("cookies seen: %v, want {%d,%d}", cookies, c1, c2)
	}
}

func TestReadEvents_PeriodElapsed(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.pushEvent(REvtPCMPeriodElapsed, 0)
	evs := v.ReadEvents()
	if len(evs) != 1 || evs[0].Kind != EventKindPeriodElapsed {
		t.Fatalf("ReadEvents: got %+v", evs)
	}
	if evs[0].StreamID != 0 {
		t.Errorf("StreamID: got %d, want 0", evs[0].StreamID)
	}
}

func TestReadEvents_Xrun(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.pushEvent(REvtPCMXrun, 1)
	evs := v.ReadEvents()
	if len(evs) != 1 || evs[0].Kind != EventKindXrun {
		t.Fatalf("ReadEvents: got %+v", evs)
	}
	if evs[0].StreamID != 1 {
		t.Errorf("StreamID: got %d, want 1", evs[0].StreamID)
	}
}

func TestReadEvents_JackHotplug(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	d.pushEvent(REvtJackConnected, 5)
	d.pushEvent(REvtJackDisconnected, 5)
	evs := v.ReadEvents()
	if len(evs) != 2 {
		t.Fatalf("ReadEvents: got %d events", len(evs))
	}
	if evs[0].Kind != EventKindJackConnected || evs[1].Kind != EventKindJackDisconnected {
		t.Errorf("Event kinds: %+v", evs)
	}
}

func TestReadEvents_CtlNotify(t *testing.T) {
	d, v := newCTLSDevice(t)
	d.pushEvent(REvtCtlNotify, 42)
	evs := v.ReadEvents()
	if len(evs) != 1 || evs[0].Kind != EventKindControlChanged || evs[0].ControlID != 42 {
		t.Fatalf("ReadEvents: got %+v", evs)
	}
}

func TestReadEvents_Empty(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	_ = d
	evs := v.ReadEvents()
	if len(evs) != 0 {
		t.Errorf("ReadEvents empty: got %v", evs)
	}
}

// --- internals --------------------------------------------------------

func TestIsSingleBit(t *testing.T) {
	if isSingleBit(0) {
		t.Error("0 should not be single-bit")
	}
	if !isSingleBit(1) {
		t.Error("1 should be single-bit")
	}
	if !isSingleBit(1 << 7) {
		t.Error("1<<7 should be single-bit")
	}
	if isSingleBit(3) {
		t.Error("3 (0b11) should not be single-bit")
	}
}

func TestEnsureStreams(t *testing.T) {
	v := &VirtioSound{Device: DeviceConfig{Streams: 4}}
	v.ensureStreams()
	if len(v.streams) != 4 {
		t.Errorf("ensureStreams: got %d, want 4", len(v.streams))
	}
	// Idempotent.
	v.ensureStreams()
	if len(v.streams) != 4 {
		t.Errorf("ensureStreams 2nd: got %d, want 4", len(v.streams))
	}
	// Zero-streams case.
	v2 := &VirtioSound{Device: DeviceConfig{Streams: 0}}
	v2.ensureStreams()
	if v2.streams != nil {
		t.Error("ensureStreams(0) should leave nil")
	}
}

func TestCacheStreamInfo_OutOfRange(t *testing.T) {
	v := &VirtioSound{Device: DeviceConfig{Streams: 1}}
	v.cacheStreamInfo(99, PCMInfoEntry{}) // should be a no-op
	if v.streams != nil && len(v.streams) >= 99 {
		t.Error("cacheStreamInfo OOR should not grow streams")
	}
	if _, ok := v.cachedStreamInfo(99); ok {
		t.Error("cachedStreamInfo(99): got ok, want false")
	}
}

func TestRecordStreamParams_OutOfRange(t *testing.T) {
	v := &VirtioSound{Device: DeviceConfig{Streams: 1}}
	v.recordStreamParams(99, PCMParams{}) // no-op
	if _, ok := v.StreamParams(99); ok {
		t.Error("StreamParams 99: got ok, want false")
	}
}

func TestRepostEventBuffer_OutOfRange(t *testing.T) {
	v := &VirtioSound{eventq: &common.Virtqueue{Buffers: make([]common.VirtqueueBuffer, 0)}}
	// Out-of-range head: no-op, no panic.
	if err := v.repostEventBuffer(99); err != nil {
		t.Errorf("repostEventBuffer OOR: %v", err)
	}
}

func TestCompleteAsyncCookie_NoMatch(t *testing.T) {
	v := &VirtioSound{}
	if _, ok := v.completeAsyncCookie(0); ok {
		t.Error("completeAsyncCookie nil-map: got ok")
	}
	v.asyncCookies = map[uint64]asyncInflight{1: {head: 5}}
	if _, ok := v.completeAsyncCookie(99); ok {
		t.Error("completeAsyncCookie no-match: got ok")
	}
}

// min is a tiny helper for go versions where it's not built-in.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- header-size sanity: documented constants match the spec ---------

func TestCtlInfoEntrySize(t *testing.T) {
	if CtlInfoEntrySize != 88 {
		t.Errorf("CtlInfoEntrySize: got %d, want 88 per spec", CtlInfoEntrySize)
	}
}

func TestParseHdrShort(t *testing.T) {
	// Already covered in sound_test, but mirror the shape here too so
	// the v2 file's coverage of parsing helpers is self-sustained.
	if _, err := parseHdr(nil); !errors.Is(err, ErrShortResponse) {
		t.Errorf("got %v, want ErrShortResponse", err)
	}
}

func TestWriteAsync_NotifyFail(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	it.fp = failPoint{"Write32@0x1008", 1}
	if _, err := v.WriteAsync(0, []byte{1, 2}); err == nil {
		t.Error("expected notify error")
	}
}

func TestWriteAsync_QueueFull(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	for i := range v.txq.Buffers {
		v.txq.Buffers[i].InUse = true
	}
	if _, err := v.WriteAsync(0, []byte{1, 2}); !errors.Is(err, common.ErrQueueFull) {
		t.Errorf("got %v, want ErrQueueFull", err)
	}
}

func TestWriteAsync_ZeroPhys(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	it.zeroPhys = true
	if _, err := v.WriteAsync(0, []byte{1, 2}); !errors.Is(err, common.ErrAllocReturnedZero) {
		t.Errorf("got %v, want ErrAllocReturnedZero", err)
	}
}

func TestWriteAsync_BufferTooSmall(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	it := newInject(d, false)
	v, err := OpenVirtioSound(it)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	it.enable = true
	it.shortAllocBytes = 4
	if _, err := v.WriteAsync(0, []byte{1, 2}); !errors.Is(err, ErrBufferTooSmall) {
		t.Errorf("got %v, want ErrBufferTooSmall", err)
	}
}

// Quick exercise so that AcceptedFeatures stays in sync with the docs.
func TestAcceptedFeaturesIncludesCTLS(t *testing.T) {
	if AcceptedFeatures&FeatureCTLS == 0 {
		t.Error("AcceptedFeatures must include FeatureCTLS for v0.2.0")
	}
	if AcceptedFeatures&common.FeatureVersion1 == 0 {
		t.Error("AcceptedFeatures must include FeatureVersion1")
	}
}

// Multi-port: the streams-state registry handles streams > 2 properly.
func TestMultiPort_FourStreams(t *testing.T) {
	d := newFakeSoundDevice(common.FeatureVersion1)
	binary.LittleEndian.PutUint32(d.devcfg[4:8], 4) // streams=4
	v, err := OpenVirtioSound(d)
	if err != nil {
		t.Fatalf("OpenVirtioSound: %v", err)
	}
	for s := uint32(0); s < 4; s++ {
		if err := v.PCMSetParams(s, PCMParams{Channels: 2, Format: PCMFmtS16, Rate: PCMRate48000}); err != nil {
			t.Fatalf("SetParams stream %d: %v", s, err)
		}
		if _, ok := v.StreamParams(s); !ok {
			t.Errorf("StreamParams %d: not cached", s)
		}
	}
}
