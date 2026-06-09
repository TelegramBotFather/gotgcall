// Command bot is a runnable example wiring gotgcall against gogram MTProto.
//
// Usage:
//
//	# Local file (FromFile — lib builds the ffmpeg argv for you):
//	APP_ID=12345 APP_HASH=xxx SESSION=session.dat CHAT=-1001234567890 \
//	    go run . ./song.mp3
//
//	# URL / HLS / live radio (FromURL):
//	APP_ID=... APP_HASH=... SESSION=... CHAT=... \
//	    go run . https://stream.example.com/radio.m3u8
//
//	# Custom ffmpeg pipeline (FromShell — you control the argv, missing
//	# essentials like -c:a libopus / -f ogg / pipe:1 are auto-filled):
//	APP_ID=... APP_HASH=... SESSION=... CHAT=... \
//	    go run . 'shell:ffmpeg -i "song.mp3" -af "atempo=1.25"'
//
//	# Two custom ffmpeg legs, one for audio and one for video (FromShells):
//	APP_ID=... APP_HASH=... SESSION=... CHAT=... \
//	    go run . 'shells:ffmpeg -i movie.mp4|ffmpeg -i movie.mp4 -vf scale=1280:720'
//
//	# Same, with both legs spawning concurrently (independent inputs only —
//	# skip when both legs hit the same CDN URL):
//	APP_ID=... APP_HASH=... SESSION=... CHAT=... \
//	    go run . 'shellsp:ffmpeg -i song.mp3|ffmpeg -f v4l2 -i /dev/video0'
//
// Flow:
//
//  1. gogram fetches the full channel/chat to obtain the active group-call ref.
//  2. gotgcall.CreateCall produces local-side JSON.
//  3. gogram's PhoneJoinGroupCall sends that JSON to Telegram and returns
//     Updates containing UpdateGroupCallConnection — Telegram's response JSON.
//  4. gotgcall.Connect feeds that response back and finishes the WebRTC
//     handshake (ICE + DTLS happen async in pion).
//  5. gotgcall.SetStreamSources starts the streamer.
//  6. On SIGINT, we leave the call cleanly via PhoneLeaveGroupCall.
package main

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/annihilatorrrr/gotgcall"

	"github.com/amarnathcjd/gogram/telegram"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: bot <file-or-url>")
	}
	source := os.Args[1]

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// --- gogram (MTProto) -----------------------------------------------------
	tg, err := telegram.NewClient(telegram.ClientConfig{
		AppID:   int32(cfg.AppID),
		AppHash: cfg.AppHash,
		Session: cfg.Session,
	})
	if err != nil {
		log.Fatalf("create tg client: %v", err)
	}
	if err = tg.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer tg.Disconnect()

	// --- gotgcall (WebRTC) ----------------------------------------------------
	client, err := gotgcall.New(
		gotgcall.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelInfo,
		}))),
	)
	if err != nil {
		log.Fatalf("new gotgcall: %v", err)
	}
	defer client.Close()

	// Stream lifecycle events. Fires on natural EOF (err == nil) or ffmpeg
	// crash (err != nil). Manual Stop / SetSource don't fire — you already
	// know you initiated those. For video+audio sources the callback fires
	// twice: first Video, then Audio. In a music bot, this is where you
	// advance the queue (typically only on the Audio event).
	client.OnStreamEnd(func(chat int64, t gotgcall.StreamType, d gotgcall.Device, err error) {
		log.Printf("stream end chat=%d type=%s device=%s err=%v", chat, t, d, err)
	})

	// ICE/DTLS state. The Connected transition means media is flowing; Failed
	// means ICE couldn't establish (firewall, expired creds, etc.) — at that
	// point the safe move is Stop + re-join.
	client.OnConnectionChange(func(chat int64, info gotgcall.NetworkInfo) {
		log.Printf("conn chat=%d state=%s", chat, info.State)
	})

	// Fires on Mute/Unmute/Pause/Resume and on spontaneous transitions
	// (video leg dies mid-stream, ICE Failed/Closed while video was
	// active). SetStreamSources and Stop stay silent — your handler
	// for those already knows what it just did. Use this callback to
	// drive phone.EditGroupCallParticipant uniformly off MediaState.
	client.OnUpgrade(func(chat int64, state gotgcall.MediaState) {
		log.Printf("upgrade chat=%d muted=%v paused=%v video_stopped=%v presentation_paused=%v",
			chat, state.Muted, state.Paused, state.VideoStopped, state.PresentationPaused)
		// Example MTProto sync:
		//   tg.PhoneEditGroupCallParticipant(&telegram.PhoneEditGroupCallParticipantParams{
		//       Call:               inputCall,
		//       Participant:        &telegram.InputPeerSelf{},
		//       Muted:              state.Muted,
		//       VideoPaused:        state.Paused,
		//       VideoStopped:       state.VideoStopped,
		//       PresentationPaused: state.PresentationPaused,
		//   })
	})

	// React to admin mute / video-disable events server-side. gotgcall stays
	// out of MTProto, so we wire this in the bot.
	tg.AddRawHandler(&telegram.UpdateGroupCallParticipants{}, func(u telegram.Update, _ *telegram.Client) error {
		upd, ok := u.(*telegram.UpdateGroupCallParticipants)
		if !ok {
			return nil
		}
		me, err := tg.GetMe()
		if err != nil {
			return nil //nolint:nilerr
		}
		for _, p := range upd.Participants {
			user, ok := p.Peer.(*telegram.PeerUser)
			if !ok || user.UserID != me.ID {
				continue
			}
			switch {
			case p.Muted && !p.CanSelfUnmute:
				log.Println("admin muted us; pausing")
				_, _ = client.Pause(cfg.ChatID)
			case !p.Muted:
				log.Println("unmuted; resuming")
				_, _ = client.Resume(cfg.ChatID)
			}
		}
		return nil
	})
	// Steps in serial MUST you follow to start the VC: CreateCall -> Connect -> SetStreamSources
	// 1. Resolve the active group call for the chat.
	inputCall, err := resolveActiveCall(tg, cfg.ChatID)
	if err != nil {
		log.Fatalf("resolve call: %v", err)
	}
	// 2. Build local-side params.
	localParams, err := client.CreateCall(cfg.ChatID)
	if err != nil {
		log.Fatalf("create call: %v", err)
	}
	log.Printf("local params: %d bytes", len(localParams))
	// 3. Telegram MTProto join. The response contains the remote answer JSON.
	remoteParams, err := joinAndGetRemoteParams(tg, inputCall, localParams)
	if err != nil {
		log.Fatalf("join + extract: %v", err)
	}
	// 4. Finish WebRTC handshake. ICE/DTLS runs async after this returns.
	if err = client.Connect(cfg.ChatID, remoteParams); err != nil {
		log.Fatalf("client.Connect: %v", err)
	}
	// 5. Stream the source. FromFile/FromURL handle local files, HTTP, HLS,
	//    RTMP, RTSP — anything ffmpeg can decode.
	src := buildSource(source)
	if err = client.SetStreamSources(cfg.ChatID, src); err != nil {
		log.Fatalf("set source: %v", err)
	}
	// Other lifecycle ops available on `client` while streaming:
	//   client.SeekBy(chatID, +30_000)  // jump forward 30s
	//   client.SeekBy(chatID, -10_000)  // jump back 10s; underflow → OnStreamEnd
	//   client.Mute(chatID) / client.Unmute(chatID)
	//   client.SetStreamSources(chatID, ...)  // switch source mid-call
	log.Println("streaming; press Ctrl+C to stop")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	// 6. Tear down. Telegram infers the leaving participant from the joined
	//    MTProto session — Source = 0 works fine for self-leave.
	_ = client.Stop(cfg.ChatID)
	if err = leaveGroupCall(tg, inputCall); err != nil {
		log.Printf("leave: %v", err)
	}
	log.Println("done")
}

// --- config helpers ----------------------------------------------------------

type config struct {
	AppHash string
	Session string
	ChatID  int64
	AppID   int
}

func loadConfig() (config, error) {
	var c config
	c.AppID, _ = strconv.Atoi(os.Getenv("APP_ID"))
	c.AppHash = os.Getenv("APP_HASH")
	c.Session = envOr("SESSION", "session.dat")
	chatRaw := os.Getenv("CHAT")
	if c.AppID == 0 || c.AppHash == "" || chatRaw == "" {
		return c, errors.New("APP_ID, APP_HASH, CHAT env vars are required")
	}
	id, err := strconv.ParseInt(chatRaw, 10, 64)
	if err != nil {
		return c, fmt.Errorf("CHAT must be an int64 chat id: %w", err)
	}
	c.ChatID = id
	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// --- source helper -----------------------------------------------------------

// buildSource picks the right gotgcall constructor based on the argument shape:
//
//   - "shell:<cmd>"             → FromShell (one custom ffmpeg leg, audio)
//   - "shellv:<cmd>"            → FromShell (one custom ffmpeg leg, video)
//   - "shells:<audio>|<video>"  → FromShells (two custom legs; either side may be empty)
//   - "shellsp:<audio>|<video>" → FromShells + WithParallelSpawn (both legs concurrently)
//   - "http(s)://", "rtmp://", "rtsp://"
//     → FromURL (lib builds ffmpeg argv with HLS/HTTP
//     flags — reconnect, timeout, user-agent)
//   - anything else            → FromFile (lib builds ffmpeg argv with fast-probe flags)
//
// FromFile / FromURL are the convenience path: pass EncodeOptions, the library
// constructs the ffmpeg command for you. FromShell / FromShells are the escape
// hatch when you need an atempo filter, custom -map, hardware encoders, etc.
func buildSource(arg string) gotgcall.Source {
	opt := gotgcall.EncodeOptions{}
	switch {
	case strings.HasPrefix(arg, "shellsp:"):
		// "shellsp:<audio>|<video>" — same as shells: but spawns both legs
		// concurrently. Only safe when the legs read independent inputs
		// (separate files, cam/mic devices); same-URL CDN inputs should
		// stick to "shells:" to avoid per-IP concurrency throttles.
		rest := strings.TrimPrefix(arg, "shellsp:")
		audio, video, _ := strings.Cut(rest, "|")
		return gotgcall.FromShells(audio, video).WithParallelSpawn()
	case strings.HasPrefix(arg, "shells:"):
		// "shells:<audio>|<video>" — either side may be empty to skip that track.
		rest := strings.TrimPrefix(arg, "shells:")
		audio, video, _ := strings.Cut(rest, "|")
		return gotgcall.FromShells(audio, video)
	case strings.HasPrefix(arg, "shell:"):
		// Audio-only custom ffmpeg pipeline. Autofilled with libopus / ogg / pipe:1.
		return gotgcall.FromShell(strings.TrimPrefix(arg, "shell:"), gotgcall.TrackAudio)
	case strings.HasPrefix(arg, "shellv:"):
		// Video-only custom ffmpeg pipeline. Autofilled with libvpx / ivf / pipe:1.
		return gotgcall.FromShell(strings.TrimPrefix(arg, "shellv:"), gotgcall.TrackVideo)
	case strings.HasPrefix(arg, "http://"), strings.HasPrefix(arg, "https://"),
		strings.HasPrefix(arg, "rtmp://"), strings.HasPrefix(arg, "rtsp://"):
		return gotgcall.FromURL(arg, opt)
	default:
		return gotgcall.FromFile(arg, opt)
	}
}

// --- gogram glue -------------------------------------------------------------

// resolveActiveCall fetches the chat's full info and returns the active group-
// call reference. Works for both supergroups/channels and basic groups.
func resolveActiveCall(tg *telegram.Client, chatID int64) (telegram.InputGroupCall, error) {
	// Negative IDs prefixed with -100 are channels/supergroups; -1xxx is a
	// migrated basic chat. gogram normalizes via ResolvePeer.
	peer, err := tg.ResolvePeer(chatID)
	if err != nil {
		return nil, fmt.Errorf("resolve peer: %w", err)
	}

	switch p := peer.(type) {
	case *telegram.InputPeerChannel:
		full, err := tg.ChannelsGetFullChannel(&telegram.InputChannelObj{
			ChannelID:  p.ChannelID,
			AccessHash: p.AccessHash,
		})
		if err != nil {
			return nil, fmt.Errorf("channel full: %w", err)
		}
		cf, ok := full.FullChat.(*telegram.ChannelFull)
		if !ok || cf.Call == nil {
			return nil, errors.New("no active group call in this channel — start a voice chat first")
		}
		return cf.Call, nil
	case *telegram.InputPeerChat:
		full, err := tg.MessagesGetFullChat(p.ChatID)
		if err != nil {
			return nil, fmt.Errorf("chat full: %w", err)
		}
		cf, ok := full.FullChat.(*telegram.ChatFullObj)
		if !ok || cf.Call == nil {
			return nil, errors.New("no active group call in this chat — start a voice chat first")
		}
		return cf.Call, nil
	default:
		return nil, fmt.Errorf("unsupported peer type %T", peer)
	}
}

// joinAndGetRemoteParams calls phone.JoinGroupCall and pulls the Telegram
// answer JSON out of the resulting Updates. Telegram puts the response in an
// UpdateGroupCallConnection update.
func joinAndGetRemoteParams(tg *telegram.Client, call telegram.InputGroupCall, localParams string) (string, error) {
	me, err := tg.GetMe()
	if err != nil {
		return "", fmt.Errorf("get me: %w", err)
	}
	updates, err := tg.PhoneJoinGroupCall(&telegram.PhoneJoinGroupCallParams{
		Call: call,
		JoinAs: &telegram.InputPeerUser{
			UserID:     me.ID,
			AccessHash: me.AccessHash,
		},
		Params: &telegram.DataJson{Data: localParams},
	})
	if err != nil {
		return "", fmt.Errorf("phone.JoinGroupCall: %w", err)
	}
	// Look for UpdateGroupCallConnection inside the Updates envelope.
	switch u := updates.(type) {
	case *telegram.UpdatesObj:
		for _, upd := range u.Updates {
			if conn, ok := upd.(*telegram.UpdateGroupCallConnection); ok && conn.Params != nil {
				return conn.Params.Data, nil
			}
		}
	}
	return "", errors.New("no UpdateGroupCallConnection in PhoneJoinGroupCall response")
}

func leaveGroupCall(tg *telegram.Client, call telegram.InputGroupCall) error {
	_, err := tg.PhoneLeaveGroupCall(call, 0)
	return err
}
