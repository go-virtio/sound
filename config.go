// go-virtio/sound — device-config layout (Virtio 1.2 §5.14.4).
//
// The virtio-sound device-config region is a fixed 12-byte structure
// holding three little-endian 32-bit counters that advertise the
// device's inventory:
//
//	struct virtio_snd_config {
//	    le32 jacks;
//	    le32 streams;
//	    le32 chmaps;
//	};
//
// Per Virtio 1.2 §5.14.4 the counters describe how many jack-info /
// pcm-info / chmap-info records the device will return when the driver
// issues the corresponding R_*_INFO query on the controlq.
//
// This package only consumes `streams` (passed to PCM_INFO to size the
// response array). The other two fields are read for diagnostic
// completeness and so future callers can inspect them via VirtioSound's
// Cfg field without re-reading device-config.

package sound

import (
	"github.com/go-virtio/common"
)

// Device-config region byte offsets (Virtio 1.2 §5.14.4). Used by
// readDeviceConfig to fetch the three counters in one place.
const (
	CfgJacks   uint32 = 0
	CfgStreams uint32 = 4
	CfgChmaps  uint32 = 8

	// CfgTotalSize is the on-the-wire size of struct virtio_snd_config.
	// Exposed so tests can build a fake device-config region of the
	// exact right size.
	CfgTotalSize uint32 = 12
)

// DeviceConfig is the parsed device-config region. All three fields are
// little-endian 32-bit counts in the on-the-wire encoding (§5.14.4).
type DeviceConfig struct {
	// Jacks is the number of jack endpoints the device advertises
	// (one per audio connector). This driver does not iterate the
	// jack table; the count is exposed for callers that want it.
	Jacks uint32

	// Streams is the number of PCM streams (the sum of capture +
	// playback). PCM_INFO queries fetch one virtio_snd_pcm_info entry
	// per stream; streamID values are in [0, Streams).
	Streams uint32

	// Chmaps is the number of channel-map entries the device
	// advertises. Not consumed by this MVP.
	Chmaps uint32
}

// readDeviceConfig reads the three device-config counters from the
// modern transport's DeviceCfg region. Returns the parsed struct.
func readDeviceConfig(cfg *common.ModernConfig) (DeviceConfig, error) {
	var out DeviceConfig
	jacks, err := cfg.DeviceCfgRead32(CfgJacks)
	if err != nil {
		return out, err
	}
	streams, err := cfg.DeviceCfgRead32(CfgStreams)
	if err != nil {
		return out, err
	}
	chmaps, err := cfg.DeviceCfgRead32(CfgChmaps)
	if err != nil {
		return out, err
	}
	out.Jacks = jacks
	out.Streams = streams
	out.Chmaps = chmaps
	return out, nil
}
