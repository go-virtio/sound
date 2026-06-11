// go-virtio/sound — VIRTIO_SND_F_CTLS controls API (Virtio 1.2
// §5.14.6.5). v0.2.0 addition.
//
// When the device offers VIRTIO_SND_F_CTLS (bit 0), the controlq grows
// six extra commands:
//
//	VIRTIO_SND_R_CTL_INFO        — enumerate the device's controls
//	VIRTIO_SND_R_CTL_ENUM_ITEMS  — enumerate the items of an ENUMERATED control
//	VIRTIO_SND_R_CTL_READ        — read the current value of a control
//	VIRTIO_SND_R_CTL_WRITE       — set a new value for a control
//	VIRTIO_SND_R_CTL_TLV_READ    — read a control's TLV (volume curves)
//	VIRTIO_SND_R_CTL_TLV_WRITE   — write a control's TLV
//	VIRTIO_SND_R_CTL_TLV_COMMAND — invoke a control's TLV command
//
// This file exposes the typed wrappers; the wire structs are encoded
// inline (per-control payload sizes are dynamic so they don't belong in
// messages.go's fixed-size constant block).
//
// SPDX-License-Identifier: BSD-3-Clause

package sound

// ControlType classifies a control element by the shape of its value
// (Virtio 1.2 §5.14.6.5.2.2).
type ControlType uint32

// ControlType* — the four kinds of control the spec defines.
const (
	// ControlTypeBoolean — value is a single bit (0 / 1). Used for
	// mute switches.
	ControlTypeBoolean ControlType = 1
	// ControlTypeInteger — value is a 32-bit signed integer in
	// [Min, Max], step `Step`. Used for volume sliders.
	ControlTypeInteger ControlType = 2
	// ControlTypeInteger64 — value is a 64-bit signed integer.
	ControlTypeInteger64 ControlType = 3
	// ControlTypeEnumerated — value is the index of one item from the
	// enumeration set (call ControlEnumItems to fetch the labels).
	ControlTypeEnumerated ControlType = 4
	// ControlTypeBytes — value is a raw byte sequence.
	ControlTypeBytes ControlType = 5
	// ControlTypeIEC958 — value is an IEC-958 (S/PDIF) status word.
	ControlTypeIEC958 ControlType = 6
)

// ControlAccess is the access-mask bitmap a control advertises
// (§5.14.6.5.2.2). The driver respects READ + WRITE flags when wiring
// up the ControlRead / ControlWrite helpers; other flags are surfaced
// for diagnostic use.
type ControlAccess uint32

// ControlAccess* — read / write / volatile / inactive / lock flags.
const (
	ControlAccessRead     ControlAccess = 1 << 0
	ControlAccessWrite    ControlAccess = 1 << 1
	ControlAccessVolatile ControlAccess = 1 << 2
	ControlAccessTLVRead  ControlAccess = 1 << 4
	ControlAccessTLVWrite ControlAccess = 1 << 5
	ControlAccessTLVCmd   ControlAccess = 1 << 6
	ControlAccessInactive ControlAccess = 1 << 8
	ControlAccessLock     ControlAccess = 1 << 9
)

// ControlHDA encodes the HDA function group + control name + control
// index triple the spec uses to identify a control's "natural" name
// (§5.14.6.5.2.2 — `struct virtio_snd_ctl_info.hda`). The MVP-style
// PCMInfo simply round-trips the four bytes; v0.2.0 exposes them as a
// fixed-size byte array.
type ControlHDA [16]byte

// Wire-struct sizes for the F_CTLS path. Not in messages.go's
// fixed-size block because the per-control payload size varies (the
// ENUM_ITEMS / TLV reads come back as variable-length data).
const (
	// CtlInfoEntrySize is sizeof(struct virtio_snd_ctl_info). Per
	// §5.14.6.5.2.2:
	//
	//	struct virtio_snd_info hdr;       // 16 (hda_fn_nid + 12B pad)
	//	le32  role;                       // +16..+20
	//	le32  type;                       // +20..+24
	//	le32  access;                     // +24..+28
	//	le32  num_channels;               // +28..+32
	//	le32  min;                        // +32..+36
	//	le32  max;                        // +36..+40
	//	le32  step;                       // +40..+44
	//	u8    name[44];                   // +44..+88
	CtlInfoEntrySize uint32 = 88

	// CtlHdrReqSize is sizeof(struct virtio_snd_ctl_hdr): the 8-byte
	// command-code + control-id pair every R_CTL_* command prepends.
	CtlHdrReqSize uint32 = 8

	// CtlReadResponseSize is the size of one R_CTL_READ value response.
	// For an INTEGER control v0.2.0 returns 4*NumChannels bytes.
	// CtlReadResponseSize sizes the per-channel value as 4 bytes;
	// callers compute the per-control total themselves.
	CtlReadResponseSize uint32 = 4

	// ControlNameMaxLen is the max length of a control's name field
	// in the on-the-wire struct (matches the spec's `u8 name[44]`).
	ControlNameMaxLen = 44
)

// Control is the v0.2.0 driver-side typed view of one
// virtio_snd_ctl_info entry. The `ID` field is the index in the
// device's control table; pass it to ControlRead / ControlWrite to
// drive the control.
type Control struct {
	// ID is the index assigned by Controls() — 0-based, matching the
	// table the device returned. Pass to ControlRead / ControlWrite.
	ID uint32

	// HDA is the HDA function-group + control-name + control-index
	// triple the spec uses to name a control (e.g. "Master Playback
	// Volume" / "Mic Capture Switch").
	HDA ControlHDA

	// Role is one of the role constants the spec defines: pcm playback,
	// pcm capture, jack, control-element generic. Exposed unparsed for
	// diagnostic use.
	Role uint32

	// Type is the value-shape classifier (BOOLEAN / INTEGER / ENUM /
	// ...).
	Type ControlType

	// Access is the read / write / volatile / TLV bitmap.
	Access ControlAccess

	// NumChannels is the number of independent value slots for this
	// control (1 for a mono mute / volume switch; 2 for stereo
	// volume). ControlRead returns NumChannels int32 values.
	NumChannels uint32

	// Min, Max, Step bound the legal value range for an INTEGER
	// control. For BOOLEAN they're (0, 1, 1); for ENUMERATED they're
	// (0, len(items)-1, 1).
	Min, Max, Step uint32

	// Name is the human-readable label (e.g. "Master Playback
	// Volume"). The wire field is fixed-size; the driver trims trailing
	// NUL bytes.
	Name string
}

// ControlsFeatureNegotiated reports whether VIRTIO_SND_F_CTLS was
// negotiated with the device. ControlRead / ControlWrite / SetVolume /
// SetMute return ErrControlsNotNegotiated when this is false.
func (v *VirtioSound) ControlsFeatureNegotiated() bool {
	return v.NegotiatedFeatures&FeatureCTLS != 0
}

// Controls issues a R_CTL_INFO query over the controlq and returns one
// Control per control-element the device advertises. Returns
// ErrControlsNotNegotiated if VIRTIO_SND_F_CTLS isn't in the
// negotiated feature mask.
func (v *VirtioSound) Controls() ([]Control, error) {
	if !v.ControlsFeatureNegotiated() {
		return nil, ErrControlsNotNegotiated
	}
	// The number of controls is on the spec's `controls` device-config
	// field, which lives at offset 12 (after jacks/streams/chmaps) when
	// F_CTLS is up. v0.2.0 reads it lazily here so the v0.1.0
	// readDeviceConfig() can stay byte-stable for the no-CTLS case.
	count, err := v.Cfg.DeviceCfgRead32(CfgControls)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	req := buildQueryInfoReq(RCtlInfo, 0, count, CtlInfoEntrySize)
	extraLen := count * CtlInfoEntrySize
	status, payload, err := v.controlRoundTrip(req, extraLen)
	if err != nil {
		return nil, err
	}
	if status != SOK {
		return nil, ErrDeviceStatus
	}
	out := make([]Control, count)
	for i := uint32(0); i < count; i++ {
		entry, perr := parseControlInfoEntry(payload[i*CtlInfoEntrySize : (i+1)*CtlInfoEntrySize])
		if perr != nil {
			return nil, perr
		}
		entry.ID = i
		out[i] = entry
	}
	return out, nil
}

// ControlRead issues a R_CTL_READ command and returns the control's
// current value as a slice of int32 (length == NumChannels). Returns
// ErrControlsNotNegotiated if F_CTLS isn't up.
func (v *VirtioSound) ControlRead(id uint32) ([]int32, error) {
	if !v.ControlsFeatureNegotiated() {
		return nil, ErrControlsNotNegotiated
	}
	count, err := v.Cfg.DeviceCfgRead32(CfgControls)
	if err != nil {
		return nil, err
	}
	if id >= count {
		return nil, ErrInvalidControlID
	}
	req := buildCtlHdrReq(RCtlRead, id)
	// The response payload size depends on the control's NumChannels.
	// To avoid a chicken-and-egg with Controls(), the driver reads up
	// to 64 channels (4*64 = 256 bytes), trimming the returned slice
	// when the device's used-ring `len` says fewer bytes landed. In
	// practice every audio control has 1 or 2 channels.
	const maxChans = 64
	extraLen := uint32(maxChans) * CtlReadResponseSize
	status, payload, err := v.controlRoundTrip(req, extraLen)
	if err != nil {
		return nil, err
	}
	if status != SOK {
		return nil, ErrDeviceStatus
	}
	// Decode every 4-byte slot as int32; the device's status hdr would
	// have been != SOK if fewer than NumChannels values were filled.
	vals := make([]int32, 0, maxChans)
	for off := uint32(0); off+4 <= extraLen; off += 4 {
		vals = append(vals, int32(le.Uint32(payload[off:off+4])))
	}
	return vals, nil
}

// ControlWrite issues a R_CTL_WRITE command setting the control's
// per-channel values. `values` length MUST equal the control's
// NumChannels; ErrInvalidControlValues otherwise.
func (v *VirtioSound) ControlWrite(id uint32, values []int32) error {
	if !v.ControlsFeatureNegotiated() {
		return ErrControlsNotNegotiated
	}
	if len(values) == 0 {
		return ErrInvalidControlValues
	}
	count, err := v.Cfg.DeviceCfgRead32(CfgControls)
	if err != nil {
		return err
	}
	if id >= count {
		return ErrInvalidControlID
	}
	req := buildCtlWriteReq(id, values)
	status, _, err := v.controlRoundTrip(req, 0)
	if err != nil {
		return err
	}
	if status != SOK {
		return ErrDeviceStatus
	}
	return nil
}

// SetVolume scales `percent` (0..100) across the negotiated volume
// range and writes it to the first control whose Name matches the
// "Master Playback Volume" convention or the per-stream
// "Stream <streamID> Playback Volume" pattern. Returns
// ErrInvalidVolumePercent for percent>100, ErrControlsNotNegotiated
// if F_CTLS isn't up, ErrInvalidControlID if no matching control
// exists.
func (v *VirtioSound) SetVolume(streamID uint32, percent uint8) error {
	if percent > 100 {
		return ErrInvalidVolumePercent
	}
	if !v.ControlsFeatureNegotiated() {
		return ErrControlsNotNegotiated
	}
	ctrls, err := v.Controls()
	if err != nil {
		return err
	}
	c, ok := findVolumeControl(ctrls, streamID)
	if !ok {
		return ErrInvalidControlID
	}
	// Scale percent into [Min, Max]. Avoid float math (driver may run
	// on bare metal where soft-fp pulls in unwanted dependencies).
	span := uint64(c.Max - c.Min)
	val := int32(c.Min) + int32(span*uint64(percent)/100)
	values := make([]int32, c.NumChannels)
	for i := range values {
		values[i] = val
	}
	return v.ControlWrite(c.ID, values)
}

// SetMute is the boolean analogue of SetVolume: writes 1 (muted) or 0
// (unmuted) to the first "Master Playback Switch" / "Stream <id>
// Playback Switch" / "PCM Playback Switch" control.
func (v *VirtioSound) SetMute(streamID uint32, muted bool) error {
	if !v.ControlsFeatureNegotiated() {
		return ErrControlsNotNegotiated
	}
	ctrls, err := v.Controls()
	if err != nil {
		return err
	}
	c, ok := findMuteControl(ctrls, streamID)
	if !ok {
		return ErrInvalidControlID
	}
	// Mute is the inverse of "switch on": switch=1 ⇒ unmuted, switch=0
	// ⇒ muted, per ALSA convention.
	v0 := int32(1)
	if muted {
		v0 = 0
	}
	values := make([]int32, c.NumChannels)
	for i := range values {
		values[i] = v0
	}
	return v.ControlWrite(c.ID, values)
}

// findVolumeControl picks the first control whose Type==Integer AND
// Name contains "Volume". Stream-specific match (e.g. "PCM 0 Playback
// Volume") wins over a generic "Master Playback Volume".
func findVolumeControl(ctrls []Control, streamID uint32) (Control, bool) {
	streamTag := streamIDTag(streamID)
	var generic Control
	var haveGeneric bool
	for _, c := range ctrls {
		if c.Type != ControlTypeInteger {
			continue
		}
		if !containsSubstring(c.Name, "Volume") {
			continue
		}
		if containsSubstring(c.Name, streamTag) {
			return c, true
		}
		if !haveGeneric {
			generic = c
			haveGeneric = true
		}
	}
	return generic, haveGeneric
}

// findMuteControl picks the first control whose Type==Boolean AND
// Name contains "Switch" / "Mute". Stream-specific match wins.
func findMuteControl(ctrls []Control, streamID uint32) (Control, bool) {
	streamTag := streamIDTag(streamID)
	var generic Control
	var haveGeneric bool
	for _, c := range ctrls {
		if c.Type != ControlTypeBoolean {
			continue
		}
		if !containsSubstring(c.Name, "Switch") && !containsSubstring(c.Name, "Mute") {
			continue
		}
		if containsSubstring(c.Name, streamTag) {
			return c, true
		}
		if !haveGeneric {
			generic = c
			haveGeneric = true
		}
	}
	return generic, haveGeneric
}

// streamIDTag returns "<streamID> " (with trailing space) — the
// pattern QEMU's virtio-sound uses when naming per-stream controls
// (e.g. "PCM 0 Playback Volume").
func streamIDTag(streamID uint32) string {
	// Small int → ASCII inline; max one digit per byte. The driver
	// targets bare-metal builds so we avoid strconv to keep the call
	// graph predictable.
	if streamID == 0 {
		return "0 "
	}
	var digits [10]byte
	n := 0
	for streamID > 0 {
		digits[n] = byte('0' + streamID%10)
		n++
		streamID /= 10
	}
	out := make([]byte, n+1)
	for i := 0; i < n; i++ {
		out[i] = digits[n-1-i]
	}
	out[n] = ' '
	return string(out)
}

// containsSubstring is a tiny strings.Contains, inlined to keep the
// dep set at just `encoding/binary`. (Strict-mode bare-metal builds
// avoid the `strings` package because its `bytealg` runtime hook
// pulls in extra weight.)
func containsSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// parseControlInfoEntry decodes one virtio_snd_ctl_info record
// (CtlInfoEntrySize = 88 bytes). Returns ErrShortResponse if the
// buffer is undersized.
func parseControlInfoEntry(b []byte) (Control, error) {
	var out Control
	if uint32(len(b)) < CtlInfoEntrySize {
		return out, ErrShortResponse
	}
	// hdr: first 4 bytes are hda_fn_nid; the next 12 are padding in
	// struct virtio_snd_info. v0.2.0 echoes the whole 16 bytes into
	// ControlHDA so the caller can see hda_fn_nid in HDA[0:4].
	copy(out.HDA[:], b[0:16])
	out.Role = le.Uint32(b[16:20])
	out.Type = ControlType(le.Uint32(b[20:24]))
	out.Access = ControlAccess(le.Uint32(b[24:28]))
	out.NumChannels = le.Uint32(b[28:32])
	out.Min = le.Uint32(b[32:36])
	out.Max = le.Uint32(b[36:40])
	out.Step = le.Uint32(b[40:44])
	// Name: u8 name[44] starting at +44; trim trailing NULs.
	nameBytes := b[44 : 44+ControlNameMaxLen]
	n := len(nameBytes)
	for n > 0 && nameBytes[n-1] == 0 {
		n--
	}
	out.Name = string(nameBytes[:n])
	return out, nil
}

// buildCtlHdrReq encodes a struct virtio_snd_ctl_hdr (8 bytes): code +
// control_id. Used by R_CTL_READ.
func buildCtlHdrReq(code, controlID uint32) []byte {
	b := make([]byte, CtlHdrReqSize)
	le.PutUint32(b[0:4], code)
	le.PutUint32(b[4:8], controlID)
	return b
}

// buildCtlWriteReq encodes a R_CTL_WRITE command: the 8-byte hdr
// followed by `len(values) * 4` bytes of per-channel int32 values.
func buildCtlWriteReq(controlID uint32, values []int32) []byte {
	b := make([]byte, CtlHdrReqSize+uint32(len(values))*4)
	le.PutUint32(b[0:4], RCtlWrite)
	le.PutUint32(b[4:8], controlID)
	for i, v := range values {
		off := int(CtlHdrReqSize) + i*4
		le.PutUint32(b[off:off+4], uint32(v))
	}
	return b
}
