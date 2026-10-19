# Xiaomi Mi 6 (MSM8998 / WCD9335) — verified setup

Device profile `msm8998()` in `DeviceProfile.kt`, selected automatically when
`Build.BOARD` contains `msm8998`. Other MSM8998 phones (e.g. Mi Mix 2) share
the kernel audio driver and will likely need only the PCM numbers checked.

## What was verified

October 2026, Xiaomi Mi 6 (`sagit`), LineageOS 22 (Android 15), Magisk,
module installed as priv-app, gateway holding the DIALER role.

- VoLTE calls in both directions (SIP → GSM and GSM → SIP), several minutes
  each, G.711 A-law.
- The call stays on LTE (`call_type=LTE`); mobile data keeps working during
  the call, no CSFB to 3G.
- Clean audio both ways after the A-law encoder fix.
- Room pickup: with the handset mics muted, an echo test with the far-end
  phone's own mic switched off is completely silent; the injected SIP audio
  still reaches the GSM side.
- Survives reboot: mixer state is re-applied on every call.

## Why the stock Android audio path does not work here

| Android path | What happens on this ROM |
|---|---|
| `AudioSource.VOICE_DOWNLINK` | policy routes it to `afe-proxy-record` → silence |
| `AudioSource.VOICE_CALL` | opens `incall-rec-*`, but uplink and downlink mixed |
| AudioTrack for injection | audio policy has no `incall_music_uplink` mixPort → plays on the speaker, not into the call |
| `AudioManager.setMicrophoneMute` | HAL sets `Voice Tx Mute` — a CVS stream mute that also kills the injected audio |

So the profile bypasses AudioRecord/AudioTrack entirely and drives the voice
DSP through raw ALSA with a small root helper.

## What the profile does

Root helper `mi6bridge` (`tools/mi6bridge`, static Go binary, installed to
`/data/local/tmp/` by `service.sh`), stdin/stdout PCM pipe to the app:

- **GSM → SIP:** capture `/dev/snd/pcmC0D27c` (MultiMedia9) with
  `MultiMedia9 Mixer VOC_REC_DL = 1`, `VOC_REC_UL = 0`. Captured at 48 kHz mono,
  decimated to 8 kHz with a FIR filter.
- **SIP → GSM:** playback to `/dev/snd/pcmC0D1p` (MultiMedia2) with
  `Incall_Music Audio Mixer MultiMedia2 = 1` — injected into the voice uplink
  inside the DSP. Queue ≤ 200 ms, the helper pads with silence when empty.
- **`Voc VSID = 297816064`** (0x11C05000, VoiceMMode1) set explicitly before
  each call: after boot the selector is `4294967295` (ALL_SESSION) and
  recording returns nothing. Routes are toggled 0 → 1 so a changed VSID applies.
- **Handset mics muted:** `DEC7 Volume = 0` and `DEC8 Volume = 0` (decimator
  digital gain, 84 = 0 dB, 0 = −84 dB). Restored to 84 after the call.
  While muted, `tinymix` reads the value back as **256** — that is expected
  (`snd_soc_get_volsw_sx` mask), the register holds −84 dB.
- Call runs in handset mode, voice volume minimal, no noise gate.

Why DEC volume and not something else: it is a plain gain register, not a
DAPM widget, so the `SLIMBUS_0_TX` backend stays up; the injection enters the
vocoder stream (CVS) past the codec, so muting the decimators does not touch
it; and the handset mixer paths never rewrite DEC volume, so the HAL does not
undo it mid-call.

## Values that must match the device

| Value | Where it comes from | How to check |
|---|---|---|
| Capture PCM 27, playback PCM 1 | `/proc/asound/pcm` (MultiMedia9 / MultiMedia2) | `cat /proc/asound/pcm` |
| `Voc VSID` 297816064 | the voice session the modem reports | `logcat | grep QcRilAudio` during a call → `vsid=…` |
| `MultiMedia9 Mixer VOC_REC_DL`, `Incall_Music Audio Mixer MultiMedia2` | kernel routing (`msm-pcm-routing-v2`) | `tinymix | grep -E 'MultiMedia9 Mixer|Incall_Music'` |
| Handset mics on DEC7/DEC8 | `mixer_paths_tasha.xml`, path `handset-mic` | during a call: `tinymix 'SLIM TX7 MUX'` → `DEC7`, `tinymix 'SLIM TX8 MUX'` → `DEC8` |

On another SoC: check these first on a live call with `tinycap`/`tinymix`
before tuning the profile from the app's logs.

## Gotchas

**Audio**

1. **Never touch the voice backend mixers** (`QUAT_MI2S_RX_Voice Mixer
   VoiceMMode1` and similar). After switching one off the voice session stops
   starting (`no backend DAIs enabled for VoiceMMode1` →
   `voice_start_usecase() failed`) and stays broken across calls until reboot.
   Every later "silent capture" result is false. Before trusting an
   experiment, check `RUNNING` in `/proc/asound/card0/pcm2p/sub0/status`.
2. The bundled Go `tinymix` used to write the first value into **every**
   element of a multi-value control: `'Voice Tx Mute' 0 4294967295 20` became
   `{0,0,0}` (`Invalid session_id`). Fixed in this branch: N values are written
   element by element in one `ELEM_WRITE`; a single value still fills all.
3. **AOSP tinymix cannot write `Voice Tx Mute` / `Voice Tx Device Mute`.**
   `mixer_ctl_set_value` reads the control first, these controls have no
   getter (`(read error)` in the list) → `-EPERM`. Use the bundled tinymix.
4. `Voice Tx Device Mute [1, 297816064, 0]` (CVP device mute) also mutes the
   mics and keeps the injection, but the DSP only accepts it while the voice
   session is running (`VOC_RUN`); written before the call it is lost, and it
   is reset at call end. DEC volume works from the pre-call setup.
5. DEC7/DEC8 cover the handset only. Speakerphone uses `speaker-mic` → ADC5 →
   **DEC5**, a wired headset DEC0.
6. No noise gate on the digital downlink: it is already quiet between words,
   gating chops word tails and makes line noise pump.
7. Capture at 48 kHz and decimate yourself; asking the DSP for 8 kHz gives
   audibly worse resampling.
8. **A-law encoder bug (fixed in this branch):** for samples 16…255 the segment
   table was indexed with `x >> 4`, so quiet samples were coded as loud ones
   (150 → 2240). Every zero crossing of speech became a spike: the GSM → SIP
   direction sounded like a radio at the edge of range (SNR −21…+16 dB on test
   tones vs 23…38 dB for G.711). The decoder was correct, so the other
   direction was clean. Test codecs on all 65536 input values.
9. **DTMF from the GSM caller over VoLTE** never shows up as audio: the RFC
   4733 events stay in IMS, and there is no in-band tone in the downlink and
   no event to Android telephony. The modem does report every key press as
   `QMI_VOICE_DTMF_IND` (events 8/9, start/stop) to any QMI VOICE client
   registered for DTMF. `mi6bridge -dtmf` registers one and the app sends the
   digits to the PBX as RFC 4733 (since 1.0.2). The voice service requires the
   radio/net_raw groups (`/vendor/etc/sec_config`). DTMF towards the GSM side
   uses `playDtmfTone`.
10. The logcat ring buffer is small (256 KB) and a restart loses it: run
    `logcat -G 16M` before a test call and save the log before restarting.

**Android 15**

11. `dumpsys package` prints `appId=`, not `userId=` — `service.sh` failed to
    find the UID, root was never granted and the first unattended su request
    became a permanent deny. Fixed: match either.
12. `appops get --uid` shows the UID mode and the package mode on separate
    lines. `Uid mode: RECORD_AUDIO: foreground` + `RECORD_AUDIO: allow` passes
    a naive "contains allow" check, while AudioFlinger goes by the UID mode
    and keeps capture silent. AppOps are now checked by every mode being
    `allow`.
13. PermissionController lives in an APEX since Android 11: the module's
    `.replace` overlay does not hide it, and `pm disable` is a path to a
    bootloop (exactly one `MANAGE_PERMISSIONS` handler is required). It is left
    alone; the gateway re-asserts AppOps by UID mode instead of killing it.
14. `ro.control_privapp_permissions=enforce`: any privileged permission in the
    manifest missing from `privapp-permissions-gateway.xml` stops the phone
    from booting. Change both together.

## Testing tips

- **Echo test without a second person:** route a number on the PBX to
  `Answer()` + `Echo()` (with `Monitor()` to keep a recording), call it
  through the gateway. You hear yourself across the whole path and back.
- **Hear only the gateway's room pickup:** during the echo test mute your own
  phone's mic. Anything you still hear comes from the gateway phone's
  microphone; with DEC7/DEC8 muted it is dead silent.
- **Network or audio content?** `sip show channelstats` on Asterisk during the
  call: packets flowing both ways with no loss while one side hears nothing
  means the problem is the audio content (a silent source), not the network.
  The gateway's own `RTP: tx/rx … capRMS/playRMS` log lines show the same
  from the phone side.
