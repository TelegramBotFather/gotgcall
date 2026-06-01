# gotgcall — Pure-Go Telegram Group Call & Voice Chat Streaming Library

[![Go Reference](https://pkg.go.dev/badge/github.com/annihilatorrrr/gotgcall.svg)](https://pkg.go.dev/github.com/annihilatorrrr/gotgcall)
[![Go Report Card](https://goreportcard.com/badge/github.com/annihilatorrrr/gotgcall)](https://goreportcard.com/report/github.com/annihilatorrrr/gotgcall)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![CGO-free](https://img.shields.io/badge/cgo-disabled-brightgreen)](#why-pure-go)

**gotgcall** is a pure-Go library for streaming audio and video into **Telegram group calls** (voice chats and video chats). It is a **drop-in alternative to [ntgcalls](https://github.com/pytgcalls/ntgcalls) / [pytgcalls](https://github.com/pytgcalls/pytgcalls)** built for Go music bots, livestream bots, and broadcast tooling.

Use it to:

- Build a **Telegram music bot in Go** that joins a voice chat and plays MP3/FLAC/M4A/Opus/HLS or any audio.
- Stream a **live video broadcast** (mp4/mkv/webm/RTMP/RTSP) into a Telegram group call.
- Push a "go live" RTMP broadcast to a channel via `phone.GetGroupCallStreamRtmpUrl`.
- Wrap any ffmpeg pipeline as a streaming source — atempo, scaling, hardware encoders, atomic source switches.

**No libwebrtc, no cgo, no native build chain.** `CGO_ENABLED=0 go build` produces a single static binary on every supported platform. WebRTC runs on [pion v4](https://github.com/pion/webrtc); `ffmpeg` is invoked as a runtime binary for transcoding only — nothing is linked in.

> **Keywords:** Telegram group call · Telegram voice chat · pure Go WebRTC · ntgcalls Go alternative · pytgcalls Go · pion WebRTC Telegram · Telegram music bot Go · gogram voice chat · Telegram video chat streaming · RTMP push Telegram livestream

## Status

Work in progress. Built for my own bots; the API is intentionally close to ntgcalls so existing code translates with minimal change.

## Contents

- [Install](#install)
- [Architecture at a glance](#architecture-at-a-glance)
- [Quick start](#quick-start)
- [Sources](#sources)
  - [`FromFile` / `FromURL`](#fromfile--fromurl)
  - [`FromShell` — single custom ffmpeg leg](#fromshell--single-custom-ffmpeg-leg)
  - [`FromShells` — dual ffmpeg legs](#fromshells--dual-ffmpeg-legs)
  - [`EncodeOptions`](#encodeoptions)
- [Client options](#client-options)
- [Lifecycle](#lifecycle)
  - [WebRTC mode](#webrtc-mode)
  - [RTMP mode](#rtmp-mode)
- [Pause / Resume / Mute](#pause--resume--mute)
- [Callbacks](#callbacks)
- [Server-side media-state changes](#server-side-media-state-changes-admin-mute-video-off)
- [Errors](#errors)
- [Concurrency model](#concurrency-model)
- [Networking](#networking)
- [Performance notes](#performance-notes)
- [A/V sync](#av-sync)
- [Pitfalls](#pitfalls)
- [Performance vs ntgcalls](#performance-vs-ntgcalls)
- [Why pure Go](#why-pure-go)
- [License](#license)

## Install

```sh
go get github.com/annihilatorrrr/gotgcall
```

`ffmpeg` must be on `PATH` at runtime (or set `gotgcall.WithFFmpegPath("/path/to/ffmpeg")`). `New()` fails fast if the binary isn't found, so the error surfaces at startup rather than on the first stream.

Requires Go 1.26+ (uses `errors.AsType[T]` and a few stdlib features added in 1.26).

## Architecture at a glance

```
                       ┌──────────────────────────────┐
                       │           Client             │  one process-wide handle
                       │  (gotgcall.go)               │  multiplexes any number of calls
                       └──────────────────────────────┘
                            │           │
                            ▼           ▼
                   ┌──────────────┐  ┌──────────────┐
                   │  GroupCall   │  │   RTMPCall   │  per-chat call instance
                   │  (WebRTC)    │  │  (FFmpeg→RTMP│
                   └──────────────┘  └──────────────┘
                            │              │
                            │              └── single ffmpeg push to Telegram's RTMP URL
                            ▼
                   ┌──────────────┐   ┌──────────────┐
                   │   Streamer   │──▶│ pion Track   │──▶ Telegram SFU
                   │ (paces opus/ │   │ Local Static │
                   │  ivf frames) │   │   Sample     │
                   └──────────────┘   └──────────────┘
                            ▲
                            │ media.Sample (Opus / VP8)
                            │
                   ┌──────────────┐   ┌──────────────┐
                   │ FrameReader  │◀──│ ShellReader  │◀── ffmpeg subprocess
                   │ (OGG / IVF)  │   │ (stdout pipe)│
                   └──────────────┘   └──────────────┘
```

**Blob-only signaling.** The library never imports `gogram` or any MTProto layer. `CreateCall(chatID)` returns a JSON string; the caller passes it to `phone.JoinGroupCall` via their own MTProto stack, then hands the response back via `Connect(chatID, respJSON)`. This keeps the library MTProto-version-independent.

**One PeerConnection per call.** Send-only audio (Opus PT=111) and video (VP8 PT=100). All calls share one `wrtc.Factory` (and optionally one UDP socket; see `WithSharedUDPMux`).

**ffmpeg outputs ENCODED Opus (OGG) and VP8 (IVF), not raw PCM/YUV.** Pion's `TrackLocalStaticSample.WriteSample` expects already-encoded frames, so we let ffmpeg do the encoding and skip a Go-side Opus encoder (which would force cgo). This also saves ~48× pipe bandwidth versus PCM.

## Quick start

```go
client, err := gotgcall.New()
if err != nil { log.Fatal(err) }
defer client.Close()

client.OnStreamEnd(func(chat int64, t gotgcall.StreamType, d gotgcall.Device, err error) {
    log.Printf("stream end: %v", err)
})
client.OnConnectionChange(func(chat int64, info gotgcall.NetworkInfo) {
    log.Printf("conn state: %s", info.State)
})
client.OnMediaStateChange(func(chat int64, state gotgcall.MediaState) {
    // Mirror to phone.EditGroupCallParticipant so Telegram knows the
    // bot just toggled video/mute/pause. Required for /play → /vplay
    // to actually show video to other participants.
})

// 1. Local-side JSON.
localParams, _ := client.CreateCall(chatID)

// 2. Drive Telegram via your MTProto layer (gogram, etc.).
//    Pass localParams to phone.JoinGroupCall; read the response.
remoteParams := joinViaYourMTProto(localParams)

// 3. Finish the WebRTC handshake.
client.Connect(chatID, remoteParams)

// 4. Stream.
client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{}))

// 5. Pause / resume / mute / change source any time.
client.Pause(chatID)
client.Resume(chatID)
client.SetStreamSources(chatID, gotgcall.FromURL("https://stream.example.com/radio.m3u8", gotgcall.EncodeOptions{}))

// 6. Stop tears down the call.
client.Stop(chatID)
```

See [`examples/bot/`](examples/bot) for a runnable skeleton against gogram (own `go.mod` so the example doesn't taint the library's dependency tree).

## Sources

All sources target **Opus-in-OGG** (audio) and/or **VP8-in-IVF** (video) on ffmpeg's stdout. The library will not accept raw PCM/YUV — the frame readers can't parse them.

### `FromFile` / `FromURL`

```go
gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{})
gotgcall.FromURL("https://stream.example.com/...", gotgcall.EncodeOptions{})
```

Anything ffmpeg can decode is fair game — mp3, m4a, flac, ogg, opus, wav, webm, mp4, mkv, mov, m3u8 (HLS), live RTMP/RTSP, etc.

Defaults to **audio only**, regardless of what the container holds. Opt in to video extraction:

```go
client.SetStreamSources(chatID, gotgcall.FromFile("movie.mp4", gotgcall.EncodeOptions{
    Tracks: gotgcall.TrackAudio | gotgcall.TrackVideo,
    // Or just TrackVideo — TrackVideo implies TrackAudio (a video file is a
    // video file with audio).
}))
```

Fast-start probing (`-analyzeduration 0 -probesize 64k`) is on by default for every source — cuts ~1-2 s off ffmpeg's startup latency vs the stock defaults (5 s + 5 MB). HLS sources additionally get `-user_agent`, `-protocol_whitelist file,http,https,tcp,tls`, `-rw_timeout 10s`, `-http_persistent 1`; HTTP/HTTPS sources get `-reconnect 1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 5 -timeout 10s` so transient network blips don't kill the stream.

Both `FromFile` and `FromURL` return seekable sources. `Pause` records the elapsed offset and `Resume` re-spawns ffmpeg with `-ss <offset>` injected before the input.

### `FromShell` — single custom ffmpeg leg

```go
gotgcall.FromShell("ffmpeg -i thing.mp3", gotgcall.TrackAudio)
```

`FromShell` parses the cmdline as a shell-like argv (handles double-quoted args, plus `\"` and `\\` escape sequences for filenames containing literal `"` or `\` — e.g. a Telegram audio titled `(From "Foo")` that would otherwise slice the path mid-string when the embedded quote toggled the quote state) and spawns it **directly via `exec`**, NOT via `/bin/sh`. Shell metacharacters in filenames can't inject commands; use `%q` for filenames!

Missing essentials are filled in automatically:

- Input-side (always on): fast-start probing + `-err_detect ignore_err` before `-i`.
- Output-side (audio): `-c:a libopus`, `-application audio`, `-frame_duration 20`, `-page_duration 20000`, `-mapping_family 0`, `-ar 48000`, `-ac 2`, `-f ogg`, `pipe:1`.
- Output-side (video): `-c:v libvpx`, `-deadline realtime`, `-f ivf`, `pipe:1`.

So the minimum command works:

```go
gotgcall.FromShell(`ffmpeg -i "song.mp3"`, gotgcall.TrackAudio)
```

…and is equivalent to the fully-spelled-out form:

```go
gotgcall.FromShell(`ffmpeg -analyzeduration 0 -probesize 64k -err_detect ignore_err `+
    `-i "song.mp3" -vn -c:a libopus -b:a 64k -application audio `+
    `-frame_duration 20 -page_duration 20000 -mapping_family 0 `+
    `-ar 48000 -ac 2 -f ogg pipe:1`, gotgcall.TrackAudio)
```

Video-only example:

```go
gotgcall.FromShell(`ffmpeg -i "movie.mp4" -an -c:v libvpx -deadline realtime `+
    `-b:v 800k -vf scale=1280:720 -r 30 -f ivf pipe:1`, gotgcall.TrackVideo)
```

A single `FromShell` call produces a single output (audio OR video). Raw PCM/YUV output codecs (`-c:a pcm_*`, `-f rawvideo`, etc.) are rejected up front with a useful error.

### `FromShells` — dual ffmpeg legs

For ntgcalls-style "microphone + camera" patterns where you want full control over both legs:

```go
gotgcall.FromShells(
    `ffmpeg -i "x.mp4"`,                                            // audio leg
    `ffmpeg -i "x.mp4" -vf scale=1280:720 -b:v 1500k`,              // video leg
)
```

Each cmd goes through the same auto-flag injection as `FromShell`. Either string may be empty to skip that track.

For the convenience path use `FromFile`/`FromURL` with `Tracks: TrackVideo` and let the library construct both ffmpeg commands for you.

### `EncodeOptions`

```go
type EncodeOptions struct {
    VideoBitrateKbps int   // default 800
    VideoWidth       int   // default 1280
    VideoHeight      int   // default 720
    VideoFPS         int   // default 30
    AudioBitrateKbps int   // default 64
    AudioChannels    int   // default 2
    Tracks           Track // default TrackAudio; TrackVideo implies +TrackAudio
}
```

Set on the constructor (`FromFile`/`FromURL`); rides with the Source. `FromShell` / `FromShells` ignore `EncodeOptions` because you control ffmpeg directly.

## Client options

```go
gotgcall.New(
    gotgcall.WithFFmpegPath("/opt/ffmpeg/bin/ffmpeg"),  // override binary lookup
    gotgcall.WithLogger(slog.Default()),                // structured logger
    gotgcall.WithDebugLogs(),                           // shortcut: text handler @ Debug level to stderr
    gotgcall.WithFFmpegStderrLog(),                     // tee ffmpeg stderr → debug log
    gotgcall.WithSharedUDPMux(),                        // one UDP socket for all calls
    gotgcall.WithDTLSCertPool(16),                      // pre-generate N DTLS certs
    gotgcall.WithDispatchBuffer(512),                   // event-dispatcher queue size
    gotgcall.WithICEServers([]gotgcall.ICEServer{       // STUN + TURN
        {URLs: []string{"stun:stun.l.google.com:19302"}},
        {URLs: []string{"turn:turn.example.com:3478"},
         Username: "u", Credential: "p"},
    }),
    gotgcall.WithNetworkTypes(                          // enable IPv6/TCP for restrictive nets
        gotgcall.NetworkTypeUDP4,
        gotgcall.NetworkTypeUDP6,
        gotgcall.NetworkTypeTCP4,
    ),
    gotgcall.WithICETimeouts(60*time.Second, 120*time.Second, 5*time.Second),
)
```

| Option | Default | Notes |
| --- | --- | --- |
| `WithFFmpegPath` | `"ffmpeg"` | `New()` fails fast with `exec.LookPath` if the binary is missing. |
| `WithLogger` | discard | Plumbed into the WebRTC factory, the media package (ffmpeg stderr/exit), the dispatcher, **and pion's internal ICE/DTLS/SCTP logs** via the slog bridge in `wrtc/pion_logger.go`. |
| `WithDebugLogs` | off | Convenience shortcut that installs a `slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})`. Use when reporting bugs. |
| `WithFFmpegStderrLog` | off | Tees ffmpeg stderr line-by-line into the logger at Debug level. Without this, stderr is only surfaced in the final error message (last 512 bytes) when ffmpeg crashes — useless for "stream runs but I hear nothing" symptoms. |
| `WithSharedUDPMux` | off | Opens **one** `udp4:0` socket and routes ICE for every call through it. See [UDP mux scaling](#udp-mux--scaling) below. |
| `WithDTLSCertPool` | 8 | Background goroutine keeps N pre-generated ECDSA-P256 certs ready so `CreateCall` doesn't stall on keygen during bursts. 0 = disabled. |
| `WithDispatchBuffer` | 256 | Size of the single callback-dispatcher channel. Larger absorbs bursts of state changes before the consumer drains. |
| `WithICEServers` | 3× Google STUN | Replaces the default list. Add TURN entries for users behind symmetric NAT / restrictive firewalls. |
| `WithNetworkTypes` | UDP4 only | Override the ICE candidate network-type whitelist. Enable IPv6 / TCP for restrictive environments where UDP4 is blocked. |
| `WithICETimeouts` | 60 s / 120 s / 2 s | `(disconnect, failed, keepalive)`. Bumped 2× from gortc's 30/60/2 baseline in v0.6.4 because Telegram's edge wobble on rejoin frequently takes 60-90 s to settle on a working candidate pair. Pass `0` for any value to keep the default; ultra-responsive UIs can shorten back to 30/60. |

### Enabling debug logs

If you've heard "I set WithLogger but I see nothing" — **before** the slog bridge below was added, pion's internal logs (ICE state, DTLS handshake, SCTP) went straight to stderr via the `log` package, completely bypassing `WithLogger`. That is now fixed: the bridge wraps every pion logger into your slog handler, tagged with `pion=<scope>` (e.g. `pion=ice`, `pion=dtls`).

The fastest way to see everything:

```go
client, err := gotgcall.New(
    gotgcall.WithDebugLogs(),
    gotgcall.WithFFmpegStderrLog(),
)
```

With both on, you get: gotgcall internals (Debug), pion ICE/DTLS/SCTP/interceptor (Debug+), ffmpeg stderr lines (Debug). Filter by attribute key if it's too much:

```go
slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
    ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
        // example: drop pion=interceptor lines
        if a.Key == "pion" && a.Value.String() == "interceptor" {
            return slog.Attr{}
        }
        return a
    },
})))
```

### UDP mux & scaling

The old README said "use `WithSharedUDPMux` at 100+ calls". That was a conservative guess — the real picture:

**Default (one socket per call):**
- 1 UDP socket = 1 file descriptor + 1 ephemeral port per call.
- Linux defaults: `ulimit -n 1024` (raise to 65535), ephemeral port range `32768–60999` (~28000 usable).
- Practical ceiling **without** any tuning: ~900 calls (bounded by FDs, leaving room for other FDs).
- After `ulimit -n 65535` and `net.ipv4.ip_local_port_range="1024 65000"`: **tens of thousands** of calls on a beefy server.
- Benefit: kernel-level UDP receive-queue per call, parallelism scales with CPU cores naturally.

**`WithSharedUDPMux` (one socket total):**
- 1 UDP socket, 1 FD, 1 port for the entire process — FD/port limits stop mattering.
- All traffic funnels through one socket → kernel UDP buffer might become contended at extreme rates.
- Per-socket UDP throughput on modern Linux: easily 1–10 Gbps. At ~50 kbps per voice call, that's **20 000–200 000 concurrent voice calls** through one socket before throughput becomes the bottleneck.
- Best for huge call counts where FD/port pressure is the limiting factor, or where firewall rules need to pin a single port.

**Rule of thumb:**
- < 1000 calls: per-call sockets is fine, simpler, and gives you natural per-call kernel-queue isolation.
- 1000–10000 calls: either works; `WithSharedUDPMux` simplifies sysctl tuning.
- 10000+ calls: `WithSharedUDPMux` is the easier path; tune the kernel UDP receive buffer (`net.core.rmem_max`, `net.core.rmem_default`).

**Note:** `client.Stop(chatID)` closes only that call's `PeerConnection` (and the per-call socket if not using the shared mux). The shared mux survives every `Stop` and is only closed when you call `client.Close()` on the parent client. So you can spin calls up and down freely without leaking or thrashing the shared socket.

## Lifecycle

### WebRTC mode

The default. Use for normal group voice/video.

```go
localParams, err := client.CreateCall(chatID)
// → send localParams to phone.JoinGroupCall; read remoteParams from response.
err = client.Connect(chatID, remoteParams)
err = client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3", gotgcall.EncodeOptions{}))
// …
err = client.Stop(chatID)
```

- `CreateCall` returns `ErrConnectionExists` if a call for that chat already exists. Per-chat creation mutex serialises concurrent calls so it never allocates twice.
- `Connect` is idempotent only in the sense that re-calling it re-SetRemoteDescription's; if you call `Connect` before `CreateCall` you get `ErrConnectionNotFound`.
- `Stop` removes the call from the internal map and clears the per-chat mutex; after `Stop` you can re-use the same `chatID` cleanly.
- `client.AudioSSRC(chatID)` returns the audio SSRC for `phone.LeaveGroupCall`'s `Source` field. RTMP calls return `ErrWrongMode`.

### RTMP mode

For "go live" / host-style broadcasts. Obtain the URL via `phone.GetGroupCallStreamRtmpUrl`:

```go
err := client.StartRTMP(chatID, rtmpURL)
err  = client.SetStreamSources(chatID, gotgcall.FromFile("movie.mp4", gotgcall.EncodeOptions{}))
// Pause/Resume/Stop work identically. Mute/Unmute are best-effort (RTMP push has
// no per-track control); the lib tracks state but doesn't drop frames.
```

`StartRTMP` is serialised with `CreateCall` via the same per-chat mutex. RTMP transcodes to H.264+AAC and pushes FLV.

Pause in RTMP mode is **kill-and-restart-with-`-ss`** (Telegram's RTMP ingest times out silent streams, so `SIGSTOP` can't be used). WebRTC mode uses a channel-based gate that keeps ffmpeg alive — the OS pipe absorbs ~1s of frames during pause.

## Pause / Resume / Mute

```go
ok, err := client.Pause(chatID)   // false if already paused
ok, err  = client.Resume(chatID)
ok, err  = client.Mute(chatID)    // mute audio track; video keeps going
ok, err  = client.Unmute(chatID)
```

WebRTC mode:
- **Pause** gate-blocks the streamer's pull loop. ffmpeg keeps running; its stdout pipe buffers the next ~1s of frames. Resume wakes the loop and the pacing baseline jumps forward over the paused window so we don't burst the buffered frames on resume.
- **Mute** is a flag on the streamer — samples are read at the natural cadence but `WriteSample` is skipped.

RTMP mode:
- **Pause** records `elapsed_ms`, kills ffmpeg, frees the connection. **Resume** spawns a fresh ffmpeg with `-ss <elapsed>`.

`SetStreamSources` can be called any time. While paused the new source is recorded but not started; Resume starts it at offset 0 (a new source resets the resume offset — it's a different track).

## Callbacks

```go
client.OnStreamEnd(func(chat int64, t StreamType, d Device, err error) {
    // Fires on natural EOF, ffmpeg crash, Stop. err == nil for clean EOF/Stop.
})

client.OnConnectionChange(func(chat int64, info NetworkInfo) {
    // info.State: Connecting | Connected | Failed | Closed | Timeout
})

client.OnMediaStateChange(func(chat int64, state MediaState) {
    // Fires on every Muted / Paused / VideoStopped transition. Wire to
    // your MTProto layer's phone.EditGroupCallParticipant so Telegram
    // mirrors the change for other participants. Critical for the
    // /play → /vplay swap: without flipping VideoStopped=false on the
    // server side, Telegram's SFU silently drops the late video even
    // though our RTP is correct.
})
```

All callbacks fire on a single dispatcher goroutine, so you can safely re-enter the API from inside (e.g. call `client.Stop(chat)` from inside `OnStreamEnd`). If your callback panics it is recovered and logged; the dispatcher keeps running.

If the dispatch queue fills up (slow consumer), the dispatcher drops the **oldest** queued event and logs a warning. Tune with `WithDispatchBuffer`.

## Server-side media-state changes (admin mute, video off)

The library is blob-only and never sees MTProto updates. When Telegram tells you the bot was admin-muted (via your `UpdateGroupCallParticipants` handler), react directly:

```go
tg.AddRawHandler(&telegram.UpdateGroupCallParticipants{}, func(u telegram.Update, _ *telegram.Client) error {
    upd := u.(*telegram.UpdateGroupCallParticipants)
    for _, p := range upd.Participants {
        // compare p.Peer to your own user id, then:
        if p.Muted {
            client.Pause(chatID)
        } else if p.CanSelfUnmute {
            client.Resume(chatID)
        }
    }
    return nil
})
```

There is no `OnUpgrade` / `NotifyUpgrade` API by design — out of scope for a blob-only library.

## Errors

All errors are sentinels — branch with `errors.Is`:

| Error | Returned when |
| --- | --- |
| `ErrConnectionExists` | `CreateCall`/`StartRTMP` for a chatID that already has a live call. |
| `ErrConnectionNotFound` | Any method called with an unknown chatID, or after `Stop`. |
| `ErrConnectionTimeout` | Declared for future use (currently surfaced via `OnConnectionChange(Failed)` after pion's 120 s ICE-failed timeout). |
| `ErrConnectionFailed` | Same — declared for branching; current ICE-failed manifests as `OnConnectionChange(Failed)`. |
| `ErrInvalidParams` | Malformed remote JSON in `Connect` (missing ufrag/pwd/fingerprints), or `FromShell` with empty/invalid command. |
| `ErrFFmpegSpawn` | `exec.Cmd.Start` failed (binary missing / permission denied / OS resource exhaustion). |
| `ErrFFmpegCrashed` | ffmpeg exited non-zero; wrapped error carries `exit=<code>` and the last 512 bytes of stderr for diagnosis. Surfaced both via `OnStreamEnd` and on the `ShellReader.Read` EOF path (the Reader briefly waits — bounded 200 ms — for the reap goroutine to capture the real exit before returning, so a fast-failing child no longer collapses to a bare `io.EOF` swallowed by the OGG/IVF parser). |
| `ErrFile` | Source contained no playable audio or video stream (OGG / IVF parse failed). |
| `ErrClosed` | Any method called after `Client.Close()`. |
| `ErrNotConnected` | Declared for branching; not currently emitted. |
| `ErrInternal` | Wrapping for pion API errors that shouldn't happen (e.g. `CreateOffer` failure). |
| `ErrWrongMode` | WebRTC-only method called on an RTMP call (or vice versa). |

## Concurrency model

- One `*Client` per process multiplexes any number of group calls.
- All public methods are safe for concurrent use.
- Per-chat operations are serialised internally via a `sync.RWMutex` on each call instance.
- Concurrent `CreateCall` / `StartRTMP` for the same chat are gated by a per-chat creation mutex; the first wins, others get `ErrConnectionExists` without allocating a pion `PeerConnection`.
- The createMu map entry is freed in `Stop` (you can re-use the chatID cleanly).
- Callbacks fire on a single dispatcher goroutine — no inter-callback parallelism, but no risk of deadlocking the producer either.

## Networking

**Transport.** Pion v4 is the only WebRTC stack. We disable IPv6 and TCP ICE network types because Telegram's edge mixers favor IPv4/UDP — restricting candidate types trims the ICE checklist (faster connect) and avoids spurious failed pairings.

**Interface filter.** Virtual / VPN interfaces are skipped by name match: `vethernet`, `vmware`, `virtualbox`, `vbox`, `hyper-v`, `loopback`, `teredo`, `isatap`, `tap-`, `docker`, `wsl`, `tailscale`, `zerotier`, `openvpn`. Gathering candidates on these would slow ICE and produce unreachable pairs.

**STUN.** Three Google STUN servers are configured by default so pion can gather server-reflexive candidates behind NAT (`stun.l.google.com:19302` and `stun1/2`). Without these, only host candidates are emitted and any non-LAN connection fails ICE.

**TURN.** Not configured by default. Telegram's SFU exposes reflexive candidates in the JOIN response and our outbound NAT path is normally enough.

**ICE timeouts.** Disconnect grace = 60 s, failed declaration = 120 s, keepalive = 2 s. Bumped 2× from gortc's 30/60/2 baseline in v0.6.4 — in practice Telegram's edge wobble on rejoin takes 60-90 s to settle on a working candidate pair, and the shorter window made pion declare Failed before the SFU finished steering. Override via `WithICETimeouts` for ultra-responsive UIs (shorter) or extra-unstable networks (longer). Pion surfaces failure via `OnConnectionStateChange(Failed)`.

**UDP mux.** Default behavior: each call binds its own UDP socket. Enable `WithSharedUDPMux()` to route every call through one shared `udp4:0` socket. Useful at 100+ concurrent calls where you don't want N ephemeral ports open.

**RTP header extensions.** The full Telegram-required set is registered: `ssrc-audio-level` (RFC 6464), `abs-send-time`, `transport-cc`, `sdes-mid`, `video-orientation`. The library auto-stamps `ssrc-audio-level` (`-20 dBov`, voice-activity bit set) and `abs-send-time` on every outbound audio packet via a pion interceptor — Telegram's SFU silently drops streams that don't carry audio-level (it treats them as silence and stops forwarding to listeners).

**Outbound RTP marker bit.** Pion's packetizer sets `marker=true` on every single-payload Opus packet, but per RFC 7587 the marker should only be set on the first packet after silence. An always-set marker forces jitter-buffer resync at the SFU and degrades audio. We clear it via a small interceptor on outbound audio.

**Pion log noise filter.** Telegram's mixer forwards every other participant's RTP to us; our PeerConnection has only send-only tracks, so pion logs `Simulcast probing failed` for each unknown incoming SSRC. We filter these out at the `Error` level so they don't bury real errors. Other levels pass through.

**HLS / HTTP.** ffmpeg-side, not pion-side. See [`FromFile` / `FromURL`](#fromfile--fromurl) for the auto-injected reconnect / timeout flags.

## Performance notes

- **Cert pool.** ECDSA-P256 keygen is ~10ms per call. The cert pool keeps `N` ready so burst joins don't queue behind keygen latency. Defaults to 8; raise for very bursty workloads.
- **Single dispatcher.** All callbacks serialise on one goroutine. Tune `WithDispatchBuffer` if you see drop warnings.
- **Single timer per streamer.** The pacing loop reuses one `time.Timer` for the whole stream rather than allocating a `NewTimer` per sample (Go 1.23+ Reset semantics make this safe without manual drain).
- **OS-pipe-managed stdout.** ShellReader uses `os.Pipe` rather than `cmd.StdoutPipe` so `cmd.Wait` doesn't close the read end out from under us. Without this, the last chunk of audio buffered in the kernel pipe would be discarded the moment ffmpeg exits.
- **Per-page OGG flush.** `-page_duration 20000` on `libopus` forces ffmpeg to emit one OGG page per Opus frame. The default (1s) would batch ~50 frames per page and the frame-per-page reader would consume the song at ~50× real-time.
- **Fast-probe flags.** `-analyzeduration 0 -probesize 64k` on local files cuts ~1-2s of startup latency (default is 5s + 5MB).

## A/V sync

- **RTP timestamps + RTCP Sender Reports** are what synchronise audio and video at the receiver. Pion's default interceptors send SR automatically; the receiver maps RTP timestamps to NTP via the SR.
- **Both streamers are started in the same `startLocked` call** (`instances/group_call.go`), so their wall-clock baselines are within microseconds of each other.
- **Each streamer paces by `sample.Duration`** (audio = 20 ms per Opus frame, video = `1/fps` per VP8 frame) using a single hoisted timer. Real-time accuracy is sub-millisecond; no drift accumulates over time.
- **Don't apply different time-distortion filters to the audio and video legs of one source** — e.g., `atempo=1.25` on audio without `setpts=PTS/1.25` on video. The two will desync linearly at 25 % per minute.
- For RTMP mode (single ffmpeg push) sync is ffmpeg's responsibility — typically not a concern for properly-muxed source files.

## Pitfalls

- **Requesting video on an audio-only source fails the call.** The library opens two ffmpeg subprocesses for `Tracks: TrackVideo`. If the source has no video stream, ffmpeg's `-map 0:v?` makes the video leg exit cleanly (no stream), the OGG/IVF parser sees EOF, and the video track is silently skipped — but if audio also fails, you get `ErrFile`. Don't request video tracks unless you know the container has them.
- **Don't switch ffmpeg output back to PCM.** It will "work" but defeats the design — you'd be re-encoding in Go (which would require cgo, the very thing this library exists to avoid).
- **Raw PCM/YUV is rejected at construction time.** `FromShell` validates the codec/container args and returns `ErrInvalidParams` with a useful hint pointing at `libopus`/`libvpx`.
- **`SetStreamSources` does not wait for ICE.** It starts ffmpeg immediately. ICE/DTLS run in the background. Watch `OnConnectionChange(Failed)` if you need to react to connection failure during streaming.
- **Pause in RTMP mode is destructive to the connection.** ffmpeg is killed; Telegram drops the RTMP ingest. Resume re-establishes from `elapsed_ms`. Listeners will see a brief silence.

## Performance vs ntgcalls

Both ship into the same Telegram SFU and use the same Opus + VP8 codecs at the same bitrates, so wire bandwidth is identical. The differences are operational, not protocol-level.

| Dimension | ntgcalls (libwebrtc, C++) | gotgcall (pion + ffmpeg subprocess, pure Go) |
| --- | --- | --- |
| **Per-call CPU (steady state, audio-only)** | ~1–2 % of one core (in-process Opus encoder, no IPC) | ~2–4 % of one core (ffmpeg encodes Opus, pipe IPC to Go, pion packetises) |
| **Per-call memory baseline** | ~15–25 MB (libwebrtc allocators + jitter buffers) | ~5–10 MB Go heap + ~10–20 MB per ffmpeg subprocess (drops to ~5 MB on libopus-only audio) |
| **Cold-start to first packet** | ~50–150 ms (compiled-in encoder ready immediately) | ~80–300 ms (ffmpeg spawn + first OGG page; the `-analyzeduration 0 -probesize 64k` fast-probe flags shave ~1–2 s vs ffmpeg defaults) |
| **Cross-compile / deploy** | Requires libwebrtc + glibc + a C++ toolchain on the target ABI; cgo enabled | `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` — single static binary, scp it, run. ffmpeg as a runtime dep (typically already installed). |
| **Binary size** | ~20–30 MB (libwebrtc + cgo glue) | ~12–18 MB Go binary (no ffmpeg bundled) |
| **Subprocess footprint** | None (everything in-process) | 1 ffmpeg per call for audio, 2 if video is on (one per leg). Easy to inspect/kill with standard Unix tools; isolates encoder crashes from the bot process. |
| **Pause/resume latency** | Mute internal pipeline, sub-ms | Pause: gate the streamer (sub-ms, ffmpeg keeps running). Resume: wake the gate (sub-ms). RTMP mode: kill+restart with `-ss`, ~100–300 ms gap. |
| **Concurrent calls per process** | Bounded by libwebrtc thread pool sizing (~hundreds without tuning) | Bounded by ffmpeg subprocess count + FDs. With `WithSharedUDPMux` and raised FD limits: **tens of thousands**. See [UDP mux & scaling](#udp-mux--scaling). |
| **Hot-reload of encoder logic** | Recompile + redeploy the whole bot | Swap an ffmpeg flag string at runtime (`FromShell`) — no rebuild |

**Trade-offs at a glance:**

- ntgcalls wins on raw CPU/memory per call (in-process encoder, no IPC overhead).
- gotgcall wins on operability (static binaries, no C++ chain, ffmpeg-flag flexibility, OS-level subprocess isolation).
- For a typical music bot (10–500 concurrent voice calls), the per-call overhead difference is invisible on any reasonable server.
- For 10 000+ concurrent calls on one box, ntgcalls' lower per-call memory footprint matters; gotgcall offsets some of this by sharing one UDP socket via `WithSharedUDPMux`.

The actual numbers above are order-of-magnitude estimates; benchmark on your workload before committing to either.

## Why pure Go

ntgcalls works fine but pulls in libwebrtc + glibc + a C++ build chain. Cross-compiling music bots becomes a maintenance burden. `gotgcall` builds with `CGO_ENABLED=0` to a single static binary on every supported platform. The trade-off is ffmpeg as a runtime dependency, which most bot deployments already have anyway.

## FAQ

### Is this a port of ntgcalls / pytgcalls to Go?

No — it's an independent implementation with a deliberately ntgcalls-shaped API so existing bot code translates almost line-for-line. ntgcalls wraps libwebrtc (C++); `gotgcall` uses [pion](https://github.com/pion/webrtc), the pure-Go WebRTC stack.

### Does it work with gogram, MTProto-Go, or other MTProto libraries?

Yes — any of them. The library is blob-only: it produces and consumes JSON strings; you handle the MTProto layer (`phone.JoinGroupCall` / `phone.LeaveGroupCall`) in your bot using whichever MTProto Go library you prefer. The `examples/bot/` directory has a runnable skeleton against [gogram](https://github.com/amarnathcjd/gogram).

### Can I use this for a Telegram music bot?

That's the primary use case. See [`examples/bot/`](examples/bot) and the [`FromShell` recipes](#fromshell--single-custom-ffmpeg-leg) for piping yt-dlp / atempo / loudness-normalised ffmpeg pipelines.

### Does it support video chats / livestreams / RTMP push?

Yes — three modes:
1. **WebRTC**: send-only audio + video into a normal voice/video chat.
2. **RTMP push**: "go live" broadcasts to a channel via Telegram's RTMP ingest URL. See [RTMP mode](#rtmp-mode).
3. **Custom ffmpeg**: `FromShell` / `FromShells` lets you point at any decodable container or live source — HLS, RTSP, MJPEG, screen capture, etc.

### Does it support TGCalls / MTProto E2E voice calls?

No — only group calls and channel RTMP livestreams. 1-on-1 MTProto voice/video calls (TGCalls) require a different signalling path that this library does not currently target.

### What Go version is required?

Go 1.26 or newer (uses `errors.AsType[T]` and a few stdlib refinements added in 1.26).

### Does it run on Windows?

Yes. Pure-Go means no Make/gcc/clang. Pause/Resume in WebRTC mode uses a channel gate (works on every OS); RTMP mode uses kill+restart-with-`-ss` (also OS-agnostic — `SIGSTOP` would be killed by Telegram's RTMP ingest timeout anyway).

### How many concurrent calls can one process handle?

The library has no hardcoded limit. The practical ceiling is ffmpeg subprocess count + ICE socket count. Use `WithSharedUDPMux()` to collapse all calls onto one UDP socket once you're above ~100 concurrent calls.

### Where do I report bugs?

Open an issue with logs from `WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))` — debug-level logging covers streamer state, ffmpeg exit, ICE transitions.

## See also

- [pion/webrtc](https://github.com/pion/webrtc) — pure-Go WebRTC stack (the WebRTC layer underneath).
- [amarnathcjd/gogram](https://github.com/amarnathcjd/gogram) — pure-Go MTProto client used in the example.
- [pytgcalls/ntgcalls](https://github.com/pytgcalls/ntgcalls) — the C++ library this is an alternative to.

## License

MIT — see [LICENSE](LICENSE).
