// go-virtio/sound — controlq message serialization (Virtio 1.2 §5.14.6).
//
// Every controlq command is a chain of three descriptors:
//
//	[ device-readable request  ]  — the command code + per-command params
//	[ device-writable response ]  — virtio_snd_hdr (status code + 0 pad)
//	[ optional payload         ]  — present on R_*_INFO queries (the array
//	                                of per-entity info records)
//
// This file owns the binary layouts of the request / response / payload
// structs and the encode helpers (`buildXxxReq`, `parseHdr`, etc.). All
// fields are little-endian per Virtio 1.0 §1.4.

package sound

import (
	"encoding/binary"
)

// le is the little-endian byte order used for every wire field
// (Virtio 1.0 §1.4 — all multi-byte fields on virtio are LE).
var le = binary.LittleEndian

// Control command codes (Virtio 1.2 §5.14.6). The driver only issues a
// subset (PCM lifecycle + info queries). The full enum is reproduced
// for completeness so future callers can extend without re-deriving
// constants.
const (
	// Jack control (not driven by this MVP).
	RJackInfo  uint32 = 1
	RJackRemap uint32 = 2

	// PCM control — issued by PCMInfo / PCMSetParams / PCMPrepare /
	// PCMRelease / PCMStart / PCMStop.
	RPCMInfo      uint32 = 0x0100
	RPCMSetParams uint32 = 0x0101
	RPCMPrepare   uint32 = 0x0102
	RPCMRelease   uint32 = 0x0103
	RPCMStart     uint32 = 0x0104
	RPCMStop      uint32 = 0x0105

	// Channel-map control (not driven by this MVP).
	RChmapInfo uint32 = 0x0200

	// Control-element control (VIRTIO_SND_F_CTLS, §5.14.6.5). v0.2.0.
	RCtlInfo       uint32 = 0x0300
	RCtlEnumItems  uint32 = 0x0301
	RCtlRead       uint32 = 0x0302
	RCtlWrite      uint32 = 0x0303
	RCtlTLVRead    uint32 = 0x0304
	RCtlTLVWrite   uint32 = 0x0305
	RCtlTLVCommand uint32 = 0x0306

	// Device-to-driver events (Virtio 1.2 §5.14.6.7).
	REvtJackConnected    uint32 = 0x1000
	REvtJackDisconnected uint32 = 0x1001
	REvtPCMPeriodElapsed uint32 = 0x1100
	REvtPCMXrun          uint32 = 0x1101
	REvtCtlNotify        uint32 = 0x1200 // v0.2.0 (F_CTLS)

	// Response status codes (Virtio 1.2 §5.14.6.6, `virtio_snd_hdr.code`).
	SOK      uint32 = 0x8000
	SBadMsg  uint32 = 0x8001
	SNotSupp uint32 = 0x8002
	SIOErr   uint32 = 0x8003
)

// FeatureCTLS (VIRTIO_SND_F_CTLS, bit 0) signals the device exposes the
// control-element interface (§5.14.6.5). v0.2.0 negotiates this when
// the host offers it.
const FeatureCTLS uint64 = 1 << 0

// FeatureMultiPorts is a logical driver-side flag (no spec feature bit —
// the spec lets any device expose more than one PCM stream through the
// existing `streams` device-config counter, but the MVP only drove
// stream 0). v0.2.0 always drives every advertised stream independently;
// FeatureMultiPorts is exposed so callers can branch on whether the
// driver build is the v0.2.0 multi-port driver. The bit is the spec's
// reserved high half that no real device uses.
const FeatureMultiPorts uint64 = 1 << 63

// PCM format / rate / direction enums (Virtio 1.2 §5.14.6.6.3.1). The
// MVP only advertised S16 + 44100 + S16_LE; v0.2.0 exposes the full
// enums (per Virtio 1.2) plus typed bitmap helpers (PCMRate / PCMFormat
// below).
const (
	// Direction (used in PCM_INFO response).
	PCMDirOutput uint8 = 0
	PCMDirInput  uint8 = 1

	// Sample formats. The on-wire byte (used by virtio_snd_pcm_set_params'
	// `format` field) is the format's bit index in the `formats` bitmap:
	// e.g. PCMFmtS16=5 means the device advertises support by setting
	// bit 5 of `formats`.
	PCMFmtImaAdpcm uint8 = 0
	PCMFmtMuLaw    uint8 = 1
	PCMFmtALaw     uint8 = 2
	PCMFmtS8       uint8 = 3
	PCMFmtU8       uint8 = 4
	PCMFmtS16      uint8 = 5
	PCMFmtU16      uint8 = 6
	PCMFmtS18Pad3  uint8 = 7
	PCMFmtU18Pad3  uint8 = 8
	PCMFmtS20Pad3  uint8 = 9
	PCMFmtU20Pad3  uint8 = 10
	PCMFmtS24Pad3  uint8 = 11
	PCMFmtU24Pad3  uint8 = 12
	PCMFmtS20      uint8 = 13
	PCMFmtU20      uint8 = 14
	PCMFmtS24      uint8 = 15
	PCMFmtU24      uint8 = 16
	PCMFmtS32      uint8 = 17
	PCMFmtU32      uint8 = 18
	PCMFmtFloat    uint8 = 19
	PCMFmtFloat64  uint8 = 20

	// Sample rates. The on-wire byte (used by virtio_snd_pcm_set_params'
	// `rate` field) is the rate's bit index in the `rates` bitmap.
	PCMRate5512   uint8 = 0
	PCMRate8000   uint8 = 1
	PCMRate11025  uint8 = 2
	PCMRate16000  uint8 = 3
	PCMRate22050  uint8 = 4
	PCMRate32000  uint8 = 5
	PCMRate44100  uint8 = 6
	PCMRate48000  uint8 = 7
	PCMRate64000  uint8 = 8
	PCMRate88200  uint8 = 9
	PCMRate96000  uint8 = 10
	PCMRate176400 uint8 = 11
	PCMRate192000 uint8 = 12
	PCMRate384000 uint8 = 13
)

// PCMRate is the bitmap-typed view of a single PCM sample rate. Bit N
// set ⇒ the corresponding PCMRate* byte ID is supported by the device
// (Virtio 1.2 §5.14.6.6.1.4). The value is the bitmask, NOT the byte ID,
// so the same type can carry either a single rate or a multi-rate set
// (e.g. `Rate11025 | Rate44100`).
//
// To get the byte ID (used by virtio_snd_pcm_set_params), call ByteID().
// To get the rate in hertz, call Hz().
type PCMRate uint64

// PCMRate single-rate constants. The value is `1 << <byte id>` so a
// device's `rates` bitmap can be ORed/ANDed against these directly.
const (
	RateUnknown PCMRate = 0
	Rate5512    PCMRate = 1 << PCMRate5512
	Rate8000    PCMRate = 1 << PCMRate8000
	Rate11025   PCMRate = 1 << PCMRate11025
	Rate16000   PCMRate = 1 << PCMRate16000
	Rate22050   PCMRate = 1 << PCMRate22050
	Rate32000   PCMRate = 1 << PCMRate32000
	Rate44100   PCMRate = 1 << PCMRate44100
	Rate48000   PCMRate = 1 << PCMRate48000
	Rate64000   PCMRate = 1 << PCMRate64000
	Rate88200   PCMRate = 1 << PCMRate88200
	Rate96000   PCMRate = 1 << PCMRate96000
	Rate176400  PCMRate = 1 << PCMRate176400
	Rate192000  PCMRate = 1 << PCMRate192000
	Rate384000  PCMRate = 1 << PCMRate384000
)

// rateHzTable maps PCMRate* byte IDs (low end of the bitmap) to the
// corresponding rate in hertz. Defined in the order matching the spec's
// enum; indexing the table by byte ID gives the hertz value.
var rateHzTable = [14]uint32{
	5512, 8000, 11025, 16000, 22050, 32000, 44100,
	48000, 64000, 88200, 96000, 176400, 192000, 384000,
}

// Hz returns the rate in Hz when the receiver is a SINGLE-bit value
// (e.g. Rate11025 → 11025). For a multi-rate bitmap the returned value
// is the Hz of the lowest set bit; 0 for RateUnknown / no-bit-set.
func (r PCMRate) Hz() uint32 {
	if r == 0 {
		return 0
	}
	for i, hz := range rateHzTable {
		if r&(1<<i) != 0 {
			return hz
		}
	}
	return 0
}

// ByteID returns the PCMRate* byte ID corresponding to the lowest set
// bit, suitable for embedding into virtio_snd_pcm_set_params' `rate`
// field. Returns 0xFF if the bitmap is empty or no bit corresponds to a
// defined rate.
func (r PCMRate) ByteID() uint8 {
	if r == 0 {
		return 0xFF
	}
	for i := range rateHzTable {
		if r&(1<<i) != 0 {
			return uint8(i)
		}
	}
	return 0xFF
}

// IsSet reports whether `r` (a single-rate constant) is set in the
// receiver bitmap. Convenience for `bitmap & r != 0`.
func (r PCMRate) IsSet(single PCMRate) bool { return r&single != 0 }

// RateFromByteID returns the single-rate PCMRate constant for a given
// PCMRate* byte ID (i.e. `1 << id`), or RateUnknown if `id` is outside
// the spec's defined range.
func RateFromByteID(id uint8) PCMRate {
	if int(id) >= len(rateHzTable) {
		return RateUnknown
	}
	return PCMRate(1) << id
}

// SupportedRates parses the receiver's Rates bitmap (raw uint64 as the
// device returned it) into a slice of single-rate PCMRate values, in
// ascending order. Useful for iterating "what rates does this stream
// accept?".
func (p PCMInfoEntry) SupportedRates() []PCMRate {
	out := []PCMRate{}
	for i := range rateHzTable {
		if p.Rates&(1<<i) != 0 {
			out = append(out, PCMRate(1)<<i)
		}
	}
	return out
}

// PCMFormat is the bitmap-typed analogue of PCMRate for sample formats.
// Bit N set ⇒ the corresponding PCMFmt* byte ID is supported by the
// device. Methods mirror PCMRate's API.
type PCMFormat uint64

// PCMFormat single-format constants — `1 << <byte id>`.
const (
	FormatUnknown PCMFormat = 0
	FormatImaAdpcm PCMFormat = 1 << PCMFmtImaAdpcm
	FormatMuLaw    PCMFormat = 1 << PCMFmtMuLaw
	FormatALaw     PCMFormat = 1 << PCMFmtALaw
	FormatS8       PCMFormat = 1 << PCMFmtS8
	FormatU8       PCMFormat = 1 << PCMFmtU8       // DOOM (PC speaker / WAV)
	FormatS16LE    PCMFormat = 1 << PCMFmtS16
	FormatU16LE    PCMFormat = 1 << PCMFmtU16
	FormatS18Pad3  PCMFormat = 1 << PCMFmtS18Pad3
	FormatU18Pad3  PCMFormat = 1 << PCMFmtU18Pad3
	FormatS20Pad3  PCMFormat = 1 << PCMFmtS20Pad3
	FormatU20Pad3  PCMFormat = 1 << PCMFmtU20Pad3
	FormatS24Pad3  PCMFormat = 1 << PCMFmtS24Pad3
	FormatU24Pad3  PCMFormat = 1 << PCMFmtU24Pad3
	FormatS20      PCMFormat = 1 << PCMFmtS20
	FormatU20      PCMFormat = 1 << PCMFmtU20
	FormatS24LE    PCMFormat = 1 << PCMFmtS24
	FormatU24LE    PCMFormat = 1 << PCMFmtU24
	FormatS32LE    PCMFormat = 1 << PCMFmtS32
	FormatU32LE    PCMFormat = 1 << PCMFmtU32
	FormatFloat    PCMFormat = 1 << PCMFmtFloat
	FormatFloat64  PCMFormat = 1 << PCMFmtFloat64
)

// formatNames maps PCMFmt* byte IDs to short labels. Used by String().
var formatNames = [21]string{
	"IMA-ADPCM", "mu-law", "A-law",
	"S8", "U8", "S16_LE", "U16_LE",
	"S18_3LE", "U18_3LE", "S20_3LE", "U20_3LE", "S24_3LE", "U24_3LE",
	"S20_LE", "U20_LE", "S24_LE", "U24_LE", "S32_LE", "U32_LE",
	"FLOAT_LE", "FLOAT64_LE",
}

// FormatsKnownGood is the set of PCM formats v0.2.0 drives end-to-end
// (sample bytes are written unchanged but the format is on the
// driver-tested list). Other formats are "format-passthrough-only" —
// the driver will forward sample bytes verbatim but does not assert
// they round-trip through every backend.
const FormatsKnownGood PCMFormat = FormatS8 | FormatU8 | FormatS16LE | FormatS24LE | FormatS32LE

// ByteID returns the PCMFmt* byte ID corresponding to the lowest set
// bit, suitable for embedding into virtio_snd_pcm_set_params' `format`
// field. Returns 0xFF if the bitmap is empty or no bit corresponds to a
// defined format.
func (f PCMFormat) ByteID() uint8 {
	if f == 0 {
		return 0xFF
	}
	for i := range formatNames {
		if f&(1<<i) != 0 {
			return uint8(i)
		}
	}
	return 0xFF
}

// String returns a short human-readable label for the lowest set bit
// (e.g. FormatS16LE → "S16_LE"). For an empty bitmap returns "UNKNOWN".
func (f PCMFormat) String() string {
	if f == 0 {
		return "UNKNOWN"
	}
	for i, name := range formatNames {
		if f&(1<<i) != 0 {
			return name
		}
	}
	return "UNKNOWN"
}

// IsSet reports whether `f` (a single-format constant) is set in the
// receiver bitmap. Convenience for `bitmap & f != 0`.
func (f PCMFormat) IsSet(single PCMFormat) bool { return f&single != 0 }

// FormatFromByteID returns the single-format PCMFormat constant for a
// given PCMFmt* byte ID, or FormatUnknown if `id` is outside the spec's
// defined range.
func FormatFromByteID(id uint8) PCMFormat {
	if int(id) >= len(formatNames) {
		return FormatUnknown
	}
	return PCMFormat(1) << id
}

// SupportedFormats parses the receiver's Formats bitmap into a slice of
// single-format PCMFormat values, in ascending byte-ID order.
func (p PCMInfoEntry) SupportedFormats() []PCMFormat {
	out := []PCMFormat{}
	for i := range formatNames {
		if p.Formats&(1<<i) != 0 {
			out = append(out, PCMFormat(1)<<i)
		}
	}
	return out
}

// Wire-struct sizes (Virtio 1.2 §5.14.6). Used to bounds-check parses
// + size the DMA buffers the driver allocates.
const (
	// HdrSize is sizeof(struct virtio_snd_hdr) — 4 bytes (le32 code).
	HdrSize uint32 = 4

	// QueryInfoReqSize is sizeof(struct virtio_snd_query_info) — used
	// by R_*_INFO queries.
	//
	//	le32 code; le32 start_id; le32 count; le32 size;
	QueryInfoReqSize uint32 = 16

	// PCMHdrReqSize is sizeof(struct virtio_snd_pcm_hdr) — used by the
	// PCM_PREPARE / PCM_START / PCM_STOP / PCM_RELEASE commands. The
	// first 4 bytes are the request code, the next 4 the stream id.
	//
	//	le32 code; le32 stream_id;
	PCMHdrReqSize uint32 = 8

	// PCMSetParamsReqSize is sizeof(struct virtio_snd_pcm_set_params).
	//
	//	struct virtio_snd_pcm_hdr hdr;   // 8
	//	le32  buffer_bytes;              // 12
	//	le32  period_bytes;              // 16
	//	le32  features;                  // 20
	//	u8    channels;                  // 21
	//	u8    format;                    // 22
	//	u8    rate;                      // 23
	//	u8    padding;                   // 24
	PCMSetParamsReqSize uint32 = 24

	// PCMInfoEntrySize is sizeof(struct virtio_snd_pcm_info) — the
	// per-stream info record returned by R_PCM_INFO.
	//
	//	struct virtio_snd_info {       // 16 bytes
	//	    le32 hda_fn_nid;
	//	    u8   _padding[12];
	//	};
	//	struct virtio_snd_pcm_info {   // 48 bytes total
	//	    struct virtio_snd_info hdr;     // +0  ..+16
	//	    le32 features;                  // +16 ..+20
	//	    le64 formats;                   // +20 ..+28
	//	    le64 rates;                     // +28 ..+36
	//	    u8   direction;                 // +36
	//	    u8   channels_min;              // +37
	//	    u8   channels_max;              // +38
	//	    u8   _padding[9];               // +39 ..+48
	//	};
	PCMInfoEntrySize uint32 = 48

	// PCMXferHdrSize is sizeof(struct virtio_snd_pcm_xfer) — the 4-byte
	// header (`le32 stream_id`) every PCM data-queue request prepends
	// to its raw audio buffer.
	PCMXferHdrSize uint32 = 4

	// PCMStatusSize is sizeof(struct virtio_snd_pcm_status) — the
	// device-writable trailer every PCM data-queue request appends.
	//
	//	le32 status;
	//	le32 latency_bytes;
	PCMStatusSize uint32 = 8
)

// PCMParams is the typed view of the parameters PCMSetParams sends to
// the device (Virtio 1.2 §5.14.6.6.3.2). The encode helper below packs
// these into the on-the-wire `virtio_snd_pcm_set_params` layout.
type PCMParams struct {
	// BufferBytes is the full ring buffer size in bytes the device
	// should reserve for this stream. Caller chooses the trade-off
	// (latency vs. underrun resilience).
	BufferBytes uint32

	// PeriodBytes is the chunk size the device will fire a period-
	// elapsed event after (when eventq monitoring is wired up). MUST
	// divide BufferBytes evenly; this driver does not check.
	PeriodBytes uint32

	// Features is the bitmap of per-stream features the driver
	// requests. Zero for the MVP (no shared-memory + no message
	// polling extension).
	Features uint32

	// Channels is the channel count (1 = mono, 2 = stereo, …). The
	// caller is responsible for matching the device's advertised
	// `channels_min`/`channels_max` range.
	Channels uint8

	// Format is one of the PCMFmt* constants. The MVP uses PCMFmtS16
	// (16-bit signed little-endian), but the helper accepts whatever
	// the caller passes.
	Format uint8

	// Rate is one of the PCMRate* constants.
	Rate uint8
}

// PCMInfoEntry is the decoded view of one virtio_snd_pcm_info record
// (Virtio 1.2 §5.14.6.6.1). Returned by PCMInfo as a one-entry-per-
// stream slice.
type PCMInfoEntry struct {
	// HDAFnGroup is the High-Definition-Audio function-group node ID
	// the device assigns this stream (from struct virtio_snd_info).
	HDAFnGroup uint32

	// Features is the per-stream feature bitmap the device advertises.
	Features uint32

	// Formats is the bitmap of accepted PCM formats (bit N set ⇒ the
	// stream accepts PCMFmt(N)).
	Formats uint64

	// Rates is the bitmap of accepted PCM rates (bit N set ⇒ the
	// stream accepts PCMRate(N)).
	Rates uint64

	// Direction is PCMDirOutput (0, playback) or PCMDirInput (1,
	// capture).
	Direction uint8

	// ChannelsMin / ChannelsMax are the accepted channel-count range
	// (inclusive).
	ChannelsMin uint8
	ChannelsMax uint8
}

// buildQueryInfoReq encodes a R_*_INFO request body (16 bytes) into a
// fresh byte slice. Used for PCM_INFO (and would be used for
// JACK_INFO / CHMAP_INFO if those were driven).
func buildQueryInfoReq(code, startID, count, entrySize uint32) []byte {
	b := make([]byte, QueryInfoReqSize)
	le.PutUint32(b[0:4], code)
	le.PutUint32(b[4:8], startID)
	le.PutUint32(b[8:12], count)
	le.PutUint32(b[12:16], entrySize)
	return b
}

// buildPCMHdrReq encodes a struct virtio_snd_pcm_hdr (8 bytes). Used
// by PCM_PREPARE / PCM_START / PCM_STOP / PCM_RELEASE.
func buildPCMHdrReq(code, streamID uint32) []byte {
	b := make([]byte, PCMHdrReqSize)
	le.PutUint32(b[0:4], code)
	le.PutUint32(b[4:8], streamID)
	return b
}

// buildPCMSetParamsReq encodes a struct virtio_snd_pcm_set_params
// (24 bytes).
func buildPCMSetParamsReq(streamID uint32, p PCMParams) []byte {
	b := make([]byte, PCMSetParamsReqSize)
	le.PutUint32(b[0:4], RPCMSetParams)
	le.PutUint32(b[4:8], streamID)
	le.PutUint32(b[8:12], p.BufferBytes)
	le.PutUint32(b[12:16], p.PeriodBytes)
	le.PutUint32(b[16:20], p.Features)
	b[20] = p.Channels
	b[21] = p.Format
	b[22] = p.Rate
	b[23] = 0 // padding
	return b
}

// buildPCMXferHdr encodes the 4-byte stream-id header every PCM data-
// queue buffer prepends.
func buildPCMXferHdr(streamID uint32) []byte {
	b := make([]byte, PCMXferHdrSize)
	le.PutUint32(b[0:4], streamID)
	return b
}

// parseHdr decodes the 4-byte struct virtio_snd_hdr response that
// terminates every controlq exchange. Returns the status code or
// ErrShortResponse if the buffer is undersized.
func parseHdr(b []byte) (uint32, error) {
	if uint32(len(b)) < HdrSize {
		return 0, ErrShortResponse
	}
	return le.Uint32(b[0:4]), nil
}

// parsePCMInfoEntry decodes one virtio_snd_pcm_info record
// (PCMInfoEntrySize = 48 bytes). Returns ErrShortResponse if the
// buffer is undersized. Layout matches the comment block above
// PCMInfoEntrySize.
func parsePCMInfoEntry(b []byte) (PCMInfoEntry, error) {
	var out PCMInfoEntry
	if uint32(len(b)) < PCMInfoEntrySize {
		return out, ErrShortResponse
	}
	out.HDAFnGroup = le.Uint32(b[0:4])
	// b[4:16] is padding from struct virtio_snd_info.
	out.Features = le.Uint32(b[16:20])
	out.Formats = le.Uint64(b[20:28])
	out.Rates = le.Uint64(b[28:36])
	out.Direction = b[36]
	out.ChannelsMin = b[37]
	out.ChannelsMax = b[38]
	// b[39:48] is trailing padding.
	return out, nil
}
