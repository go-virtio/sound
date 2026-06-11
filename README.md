# go-virtio/sound

Pure-Go virtio-sound driver targeting the `go-virtio/common` transport
interfaces. Implements the modern-transport (Virtio 1.2+) init sequence
and the minimum PCM playback + capture data paths for the standard
PCI-bound virtio-sound device (VID 0x1AF4, DID 0x1059, device type 25).

This package targets the single-jack baseline (Virtio 1.2 §5.14): it
negotiates only `VIRTIO_F_VERSION_1`, so `VIRTIO_SND_F_CTLS` and the
control-jack reconfiguration extensions are NOT acknowledged. The
device exposes four virtqueues — `controlq` (queue 0, control
commands), `eventq` (queue 1, device events, not driven by this MVP),
`tx` (queue 2, PCM playback frames), and `rx` (queue 3, PCM capture
frames). The driver issues the minimal PCM stream lifecycle commands
(`PCM_INFO`, `PCM_SET_PARAMS`, `PCM_PREPARE`, `PCM_START`, `PCM_STOP`,
`PCM_RELEASE`) and routes raw signed 16-bit little-endian (`S16_LE`)
frames over the PCM data queues. Format conversion is the caller's
responsibility.

Every transport-level operation is routed through `go-virtio/common`'s
`Transport` interface, so any implementation of that interface (UEFI's
`EFI_PCI_IO_PROTOCOL`, bare-metal MMIO, virtio-mmio adapter) drives
the same driver code.

## Quick start

```go
import (
    virtiosound "github.com/go-virtio/sound"
)

// transport is any value that implements go-virtio/common.Transport.
vs, err := virtiosound.OpenVirtioSound(transport)
if err != nil {
    return err
}

// Inspect the device's PCM stream inventory (control-q PCM_INFO).
infos, err := vs.PCMInfo()
if err != nil {
    return err
}

// Configure stream 0 (the output jack on a single-jack device).
params := virtiosound.PCMParams{
    BufferBytes: 4096,
    PeriodBytes: 1024,
    Features:    0,
    Channels:    2,
    Format:      virtiosound.PCMFmtS16,
    Rate:        virtiosound.PCMRate44100,
}
if err := vs.PCMSetParams(0, params); err != nil {
    return err
}
if err := vs.PCMPrepare(0); err != nil {
    return err
}
if err := vs.PCMStart(0); err != nil {
    return err
}

// Push 16-bit signed little-endian frames. The caller is responsible for
// any format conversion; this driver writes raw bytes.
if _, err := vs.Write(0, pcmFrames); err != nil {
    return err
}

if err := vs.PCMStop(0); err != nil {
    return err
}
if err := vs.PCMRelease(0); err != nil {
    return err
}
_ = infos
```

`OpenVirtioSound` leaves the device in DRIVER_OK state with the eventq
pre-posted with one-page buffers; the PCM streams are stopped until
`PCMStart` is issued.

## Scope

In scope:

  - Single output (playback) PCM stream over the `tx` virtqueue.
  - Single input (capture) PCM stream over the `rx` virtqueue.
  - Minimal PCM lifecycle: `PCM_INFO`, `PCM_SET_PARAMS`, `PCM_PREPARE`,
    `PCM_START`, `PCM_STOP`, `PCM_RELEASE` over the `controlq`.
  - Pure-Go, no cgo, transport-agnostic.

Out of scope (deliberately, for the cloud-boot DOOM sprint):

  - `VIRTIO_SND_F_CTLS` (control-jack reconfiguration).
  - Multi-stream switching beyond the single output + input pair.
  - 24-bit / 32-bit / float PCM formats (caller passes S16_LE).
  - Audio format conversion (sample-rate, channel layout, dithering).
  - Control jack reconfiguration over the `eventq`.

## Sibling packages

  - [`github.com/go-virtio/common`](https://github.com/go-virtio/common)
    — transport-agnostic infrastructure (PCI cap walker, modern config
    layout, split-virtqueue impl, transport interfaces).
  - [`github.com/go-virtio/net`](https://github.com/go-virtio/net) —
    pure-Go virtio-net driver.
  - [`github.com/go-virtio/console`](https://github.com/go-virtio/console)
    — pure-Go virtio-console driver.
  - [`github.com/go-virtio/input`](https://github.com/go-virtio/input)
    — pure-Go virtio-input driver (sibling for the DOOM input path).
  - [`github.com/go-virtio/rng`](https://github.com/go-virtio/rng) —
    pure-Go virtio-rng driver.

## License

BSD-3-Clause. See [LICENSE](LICENSE).
