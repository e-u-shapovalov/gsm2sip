# mi6bridge — in-call audio through raw ALSA

**English** · [Русский](README.ru.md)

A small static Go binary (arm64, no dependencies) that moves call audio between
the gateway app and the phone's voice DSP directly through ALSA, bypassing
`AudioRecord` / `AudioTrack`. Needed on the Xiaomi Mi 6 (MSM8998); runs as root.

## Why

On this ROM the stock Android path never reaches the voice DSP:

| What the app tries | What happens |
|---|---|
| `AudioRecord` with `VOICE_DOWNLINK` | the policy routes it to `afe-proxy-record` — silence |
| `AudioRecord` with `VOICE_CALL` | opens, but uplink and downlink are mixed |
| `AudioTrack` to inject audio into the call | the audio policy has no `incall_music_uplink` mixPort, so it plays on the speaker instead of to the other party |

The kernel has the right paths; Android just does not open them. So the helper
opens the PCM devices itself:

- **GSM → SIP:** capture `/dev/snd/pcmC0D27c` (MultiMedia9). The
  `MultiMedia9 Mixer VOC_REC_DL` route carries only what came from the
  network — the GSM party's voice, without the phone's microphone.
- **SIP → GSM:** playback to `/dev/snd/pcmC0D1p` (MultiMedia2). The
  `Incall_Music Audio Mixer MultiMedia2` route mixes it into the voice uplink
  inside the DSP, so the other party hears the SIP side.

Both directions are digital: neither the speaker nor the microphone is in the
path.

## How it works

```
app ──stdin  (S16_LE mono, 8 kHz)──► queue ≤200 ms ──► PCM 1  (Incall_Music → uplink)
app ◄─stdout (S16_LE mono, 8 kHz)─── FIR 48→8 kHz ◄──── PCM 27 (VOC_REC_DL ← downlink)
                             stderr ──► log (OPEN / STATS / STOP)
```

- **Captured at 48 kHz and decimated with its own FIR** (Blackman window,
  cutoff ~3.6 kHz for 8 kHz output). Asking the DSP for 8 kHz directly leaves
  the conversion to it, and wideband VoLTE speech comes out with hiss on the
  sibilants.
- **Playback queue ≤ 200 ms.** A burst of packets from the network does not
  become permanent delay: the excess is dropped (`dropped`).
- **The PCM sets the pace, not the network.** With the queue empty, silence goes
  into the uplink (`starved`) and the DSP keeps running.
- Runs while stdin is open and stdout is alive; stops on SIGTERM.
- The helper **does not touch the mixer routes**. The app sets them before
  starting it (`setupCmd` of the `msm8998()` profile) and clears them after
  (`restoreCmd`): `Voc VSID`, `VOC_REC_DL`, `Incall_Music`, and the handset mic
  mute on `DEC7/DEC8`.

## Who runs it

- The Magisk module carries the binary; `service.sh` copies it to
  `/data/local/tmp/mi6bridge` (root:root, 755) at boot.
- The app (`rtp/AlsaBridge.kt`) sets the routes on every call and starts it as
  root:
  ```
  exec /data/local/tmp/mi6bridge -in 27 -out 1 -rate 8000
  ```
  Its stderr goes to logcat prefixed with `helper:`.
- After the call the app closes stdin, makes sure the process is gone
  (`pkill -x mi6bridge`) and restores the routes.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-card` | `0` | ALSA card |
| `-in` | `27` | capture PCM (MultiMedia9) |
| `-out` | `1` | playback PCM (MultiMedia2) |
| `-rate` | `8000` | rate on stdin/stdout: 8000 or 16000 |
| `-cap-rate` | `48000` | capture rate, a multiple of `-rate` |
| `-period-ms` | `20` | period length |
| `-max-queue-ms` | `200` | how much SIP audio to keep ahead of playback |

On another phone the PCM numbers are almost certainly different — check
`cat /proc/asound/pcm` and verify the routes on a live call.

## Log

Every 10 s a line like:

```
STATS captured=80000 played=80800 starved=247 queue=800 dropped=15200 xrun_in=0 xrun_out=0 peakRMS=8566
```

| Field | Meaning |
|---|---|
| `captured` / `played` | samples written to stdout / played into the uplink (≈ 8000 per second) |
| `starved` | periods with an empty queue, played as silence (network audio was late) |
| `dropped` | samples discarded because the queue overflowed (a burst from the network) |
| `xrun_in` / `xrun_out` | ALSA buffer overruns / underruns |
| `peakRMS` | peak capture level in the interval: ~0 is silence, thousands is speech |

## Build

```sh
cd tools/mi6bridge
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o ../../magisk/mi6bridge .
```

`build.sh` does the same whenever Go is installed. The tests check the
decimation: 1 kHz passes unchanged, 6 kHz (above half the output rate) is
rejected.
