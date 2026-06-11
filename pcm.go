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
	}
	return out, nil
}

// PCMSetParams issues a R_PCM_SET_PARAMS command (Virtio 1.2
// §5.14.6.6.3.2) configuring buffer + period sizes, channel count,
// sample format and rate for a single stream.
//
// MUST be called once before PCMPrepare for a given stream.
func (v *VirtioSound) PCMSetParams(streamID uint32, p PCMParams) error {
	if err := v.checkStreamID(streamID); err != nil {
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
	return nil
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

