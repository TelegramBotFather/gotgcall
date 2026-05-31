# gotgcall

Pure-Go library for streaming audio and video into Telegram group calls. A drop-in alternative to [ntgcalls](https://github.com/pytgcalls/ntgcalls) — no libwebrtc, no cgo, no native build chain. Just `go build`.

WebRTC runs on [pion v4](https://github.com/pion/webrtc). ffmpeg is invoked as a runtime binary for transcoding; nothing is linked in.

## Status

Work in progress. Built for my own bots; the API is intentionally close to ntgcalls so existing code translates with minimal change.

## Install

```sh
go get gotgcall
```

ffmpeg must be on `PATH` at runtime (or set `gotgcall.WithFFmpegPath("/path/to/ffmpeg")`).

## Sources

Three constructors, all targeting Opus-in-OGG (audio) or VP8-in-IVF (video):

```go
gotgcall.FromFile("song.mp3")                        // local file
gotgcall.FromURL("https://stream.example.com/...")   // HTTP / HLS / RTMP
gotgcall.FromShell("ffmpeg -i thing.mp3 ...", gotgcall.TrackAudio)
```

Defaults to **audio only**. Pass `EncodeOptions{Tracks: gotgcall.TrackAudio | gotgcall.TrackVideo}` to also extract video.

`FromShell` accepts a partial command — missing essentials (`-analyzeduration 0`, `-probesize 64k` before `-i`; `-c:a libopus`, `-f ogg`, opus pacing, `pipe:1` after) are filled in automatically. Raw-PCM output codecs are rejected up front; the frame readers can't parse them.

## Quick start

```go
client, _ := gotgcall.New()
defer client.Close()

client.OnStreamEnd(func(chat int64, t gotgcall.StreamType, d gotgcall.Device, err error) {
    log.Printf("stream end: %v", err)
})

// 1. Local-side JSON.
localParams, _ := client.CreateCall(chatID)

// 2. Drive Telegram via your MTProto layer (gogram, etc.).
//    Pass localParams to phone.JoinGroupCall; read the response.
remoteParams := joinViaYourMTProto(localParams)

// 3. Finish the WebRTC handshake.
client.Connect(chatID, remoteParams)

// 4. Stream.
client.SetStreamSources(chatID, gotgcall.FromFile("song.mp3"))

// 5. Pause / resume / mute / change source any time.
client.Pause(chatID)
client.Resume(chatID)
client.SetStreamSources(chatID, gotgcall.FromURL("https://stream.example.com/radio.m3u8"))

// 6. Stop tears down the call.
client.Stop(chatID)
```

The library is **blob-only** — it never imports gogram or any MTProto stack. You drive `phone.JoinGroupCall` / `phone.LeaveGroupCall` yourself; the library just produces and consumes JSON. See [`examples/bot/`](examples/bot) for the full wiring against gogram.

## RTMP mode

For "go live" (host) broadcasts, swap WebRTC for RTMP push. Obtain the URL via `phone.GetGroupCallStreamRtmpUrl`, then:

```go
client.StartRTMP(chatID, rtmpURL)
client.SetStreamSources(chatID, gotgcall.FromFile("movie.mp4"))
// Pause/Resume/Stop work identically.
```

Pause is kill-and-restart-with-`-ss` (Telegram's RTMP ingest times out silent streams; SIGSTOP can't be used).

## Concurrency

One `*Client` per process multiplexes any number of group calls. Methods are safe for concurrent use; per-chat operations are serialised internally. Two concurrent `CreateCall`s for the same chat won't allocate twice — the per-chat creation mutex gates them.

## Options

```go
gotgcall.New(
    gotgcall.WithFFmpegPath("/opt/ffmpeg/bin/ffmpeg"),
    gotgcall.WithLogger(slog.Default()),
    gotgcall.WithSharedUDPMux(),    // single UDP socket for all calls (high-concurrency)
    gotgcall.WithDTLSCertPool(16),  // pre-generate N certs to absorb burst joins
    gotgcall.WithDispatchBuffer(512),
)
```

## Callbacks

```go
client.OnStreamEnd(func(chat int64, t StreamType, d Device, err error) { ... })
client.OnConnectionChange(func(chat int64, info NetworkInfo) { ... })
```

All callbacks fire from a single dispatcher goroutine so they can safely re-enter the API (e.g. call `client.Stop` from inside `OnStreamEnd`).

Server-side media-state changes (admin mute, video disabled) come in through your own gogram `UpdateGroupCallParticipants` handler — react there by calling `client.Pause` / `client.Resume` directly. The library deliberately stays out of MTProto.

## Why pure Go

ntgcalls works fine but pulls in libwebrtc + glibc + a C++ build chain. Cross-compiling music bots becomes a maintenance burden. `gotgcall` builds with `CGO_ENABLED=0` to a single static binary on every supported platform. The trade-off is ffmpeg as a runtime dependency, which most bot deployments already have.

## License

MIT — see [LICENSE](LICENSE).
