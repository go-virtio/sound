// go-virtio/sound — controlq command issuance for the PCM lifecycle.
//
// All five PCM control commands (PCMInfo, PCMSetParams, PCMPrepare,
// PCMStart, PCMStop, PCMRelease) build a request body via the helpers
// in messages.go, send it through controlRoundTrip in sound.go, and
// surface a typed status / payload to the caller.

package sound

// PCMInfo issues a R_PCM_INFO query (Virtio 1.2 §5.14.6.6.1) for every
// stream the device advertises. Returns one PCMInfoEntry per stream
// (length == VirtioSound.Device.Streams).
//
// On success the device's status code is SOK; any other code is
// surfaced as ErrDeviceStatus (the raw code is included in the
// sentinel's message for debugging).
func (v *VirtioSound) PCMInfo() ([]PCMInfoEntry, error) {
	count := v.Device.Streams
	if count == 0 {
		// Device advertises no PCM streams — nothing to query. We do
		// not error here; the caller's iteration becomes a no-op.
		return nil, nil
	}
	req := buildQueryInfoReq(RPCMInfo, 0, count, PCMInfoEntrySize)
	extraLen := count * PCMInfoEntrySize
	status, payload, err := v.controlRoundTrip(req, extraLen)
	if err != nil {
		return nil, err
	}
	if status != SOK {
		return nil, ErrDeviceStatus
	}
	out := make([]PCMInfoEntry, count)
	for i := uint32(0); i < count; i++ {
		entry, perr := parsePCMInfoEntry(payload[i*PCMInfoEntrySize : (i+1)*PCMInfoEntrySize])
		if perr != nil {
			return nil, perr
		}
		out[i] = entry
		// v0.2.0: cache so PCMSetParams / PCMSetParamsTyped can
		// validate the requested (rate, format, channels) tuple.
		v.cacheStreamInfo(i, entry)
	}
	return out, nil
}

// PCMSetParams issues a R_PCM_SET_PARAMS command (Virtio 1.2
// §5.14.6.6.3.2) configuring buffer + period sizes, channel count,
// sample format and rate for a single stream.
//
// MUST be called once before PCMPrepare for a given stream.
//
// v0.2.0: if the driver has previously cached PCMInfo (via PCMInfo()),
// PCMSetParams validates the (rate, format, channels) tuple against the
// stream's advertised bitmap and returns ErrUnsupportedRate /
// ErrUnsupportedFormat / ErrUnsupportedChannelCount when the request
// is outside the device's range. Pre-PCMInfo callers see no
// validation — backward-compat with v0.1.0 callers that never call
// PCMInfo before PCMSetParams.
func (v *VirtioSound) PCMSetParams(streamID uint32, p PCMParams) error {
	if err := v.checkStreamID(streamID); err != nil {
		return err
	}
	if err := v.validateParams(streamID, p); err != nil {
		return err
	}
	req := buildPCMSetParamsReq(streamID, p)
	status, _, err := v.controlRoundTrip(req, 0)
	if err != nil {
		return err
	}
	if status != SOK {
		return ErrDeviceStatus
	}
	v.recordStreamParams(streamID, p)
	return nil
}

// PCMSetParamsTyped is the v0.2.0 helper that accepts typed PCMRate /
// PCMFormat single-bit constants (e.g. Rate11025, FormatU8) instead of
// the raw PCMFmt* / PCMRate* byte IDs the MVP-shape PCMParams uses.
// Internally it converts the typed bitmaps via ByteID() then delegates
// to PCMSetParams.
//
// Returns ErrInvalidRateValue / ErrInvalidFormatValue if the caller
// passes a multi-bit bitmap (the helper expects a single-bit constant);
// surface validation errors are the same as PCMSetParams.
func (v *VirtioSound) PCMSetParamsTyped(streamID uint32, p TypedPCMParams) error {
	if !isSingleBit(uint64(p.Rate)) || p.Rate == 0 {
		return ErrInvalidRateValue
	}
	if !isSingleBit(uint64(p.Format)) || p.Format == 0 {
		return ErrInvalidFormatValue
	}
	return v.PCMSetParams(streamID, PCMParams{
		BufferBytes: p.BufferBytes,
		PeriodBytes: p.PeriodBytes,
		Features:    p.Features,
		Channels:    p.Channels,
		Format:      p.Format.ByteID(),
		Rate:        p.Rate.ByteID(),
	})
}

// validateParams checks the (rate, format, channels) tuple in `p`
// against the cached PCMInfo bitmap for `streamID`. Returns nil if the
// driver has not cached PCMInfo (no info ⇒ no validation possible) or
// if every dimension is in range.
func (v *VirtioSound) validateParams(streamID uint32, p PCMParams) error {
	info, ok := v.cachedStreamInfo(streamID)
	if !ok {
		return nil
	}
	if info.Rates&(1<<p.Rate) == 0 {
		return ErrUnsupportedRate
	}
	if info.Formats&(1<<p.Format) == 0 {
		return ErrUnsupportedFormat
	}
	if p.Channels < info.ChannelsMin || p.Channels > info.ChannelsMax {
		return ErrUnsupportedChannelCount
	}
	return nil
}

// isSingleBit reports whether x has exactly one bit set. Used by the
// typed-params helper to enforce single-rate / single-format inputs.
func isSingleBit(x uint64) bool { return x != 0 && (x&(x-1)) == 0 }

// TypedPCMParams is the v0.2.0 typed-bitmap variant of PCMParams. The
// `Rate` and `Format` fields are PCMRate / PCMFormat single-bit
// constants (Rate11025, FormatU8, ...) instead of the raw byte IDs the
// MVP-shape PCMParams uses.
type TypedPCMParams struct {
	// BufferBytes / PeriodBytes / Features / Channels — same semantics
	// as PCMParams.
	BufferBytes uint32
	PeriodBytes uint32
	Features    uint32
	Channels    uint8

	// Format is a PCMFormat single-bit constant (Format* in messages.go).
	Format PCMFormat

	// Rate is a PCMRate single-bit constant (Rate* in messages.go).
	Rate PCMRate
}

// PCMPrepare issues a R_PCM_PREPARE command (Virtio 1.2 §5.14.6.6.3.3)
// moving the stream from the SET_PARAMS state to the PREPARED state.
// Must follow a successful PCMSetParams.
func (v *VirtioSound) PCMPrepare(streamID uint32) error {
	return v.simplePCMCmd(streamID, RPCMPrepare)
}

// PCMStart issues a R_PCM_START command (Virtio 1.2 §5.14.6.6.3.4)
// transitioning the stream to the RUNNING state. After PCMStart the
// device begins consuming PCM frames on the data queue.
func (v *VirtioSound) PCMStart(streamID uint32) error {
	return v.simplePCMCmd(streamID, RPCMStart)
}

// PCMStop issues a R_PCM_STOP command (Virtio 1.2 §5.14.6.6.3.5)
// transitioning the stream from RUNNING back to PREPARED. In-flight
// data-queue requests complete with an underflow status code.
func (v *VirtioSound) PCMStop(streamID uint32) error {
	return v.simplePCMCmd(streamID, RPCMStop)
}

// PCMRelease issues a R_PCM_RELEASE command (Virtio 1.2 §5.14.6.6.3.6)
// transitioning the stream back to the unconfigured state. After
// PCMRelease the stream MUST be re-configured via PCMSetParams before
// it can be started again.
func (v *VirtioSound) PCMRelease(streamID uint32) error {
	return v.simplePCMCmd(streamID, RPCMRelease)
}

// simplePCMCmd factors the shared three-line body of PCMPrepare /
// PCMStart / PCMStop / PCMRelease: build a virtio_snd_pcm_hdr, round-
// trip it, surface non-OK status as ErrDeviceStatus.
func (v *VirtioSound) simplePCMCmd(streamID, code uint32) error {
	if err := v.checkStreamID(streamID); err != nil {
		return err
	}
	req := buildPCMHdrReq(code, streamID)
	status, _, err := v.controlRoundTrip(req, 0)
	if err != nil {
		return err
	}
	if status != SOK {
		return ErrDeviceStatus
	}
	return nil
}

// checkStreamID returns ErrStreamIDOutOfRange if streamID is greater
// than or equal to the device-advertised stream count. The check is
// optional but cheap and catches caller bugs without involving the
// device.
func (v *VirtioSound) checkStreamID(streamID uint32) error {
	if streamID >= v.Device.Streams {
		return ErrStreamIDOutOfRange
	}
	return nil
}

