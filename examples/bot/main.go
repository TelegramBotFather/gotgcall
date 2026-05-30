// Command bot is a SKELETON example for wiring gotgcall with gogram MTProto.
//
// The exact extraction of the response JSON from phone.JoinGroupCall depends
// on your MTProto layer version and gogram API. This file shows the call
// sequence; fill in the marked TODOs against the version of gogram you use.
//
//	APP_ID=... APP_HASH=... SESSION=session.dat CHAT=-1001234 \
//	go run . ./song.mp3
package main

import (
	"errors"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/annihilatorrrr/gotgcall"

	"github.com/amarnathcjd/gogram/telegram"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: bot <file-or-url>")
	}
	source := os.Args[1]
	appID, _ := strconv.Atoi(os.Getenv("APP_ID"))
	appHash := os.Getenv("APP_HASH")
	session := envOr("SESSION", "session.dat")
	chatRaw := os.Getenv("CHAT")
	if appID == 0 || appHash == "" || chatRaw == "" {
		log.Fatal("APP_ID, APP_HASH, CHAT env vars are required")
	}
	chatID, err := strconv.ParseInt(chatRaw, 10, 64)
	if err != nil {
		log.Fatalf("CHAT must be an int64 chat id: %v", err)
	}
	tg, err := telegram.NewClient(telegram.ClientConfig{
		AppID:   int32(appID),
		AppHash: appHash,
		Session: session,
	})
	if err != nil {
		log.Fatalf("create tg client: %v", err)
	}
	if err = tg.Connect(); err != nil {
		log.Fatalf("connect: %v", err)
	}
	client, err := gotgcall.New(
		gotgcall.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))),
	)
	if err != nil {
		log.Fatalf("new gotgcall: %v", err)
	}
	defer client.Close()
	// Fires on EOF, ffmpeg crash, or Stop. err == nil for clean EOF/Stop.
	client.OnStreamEnd(func(chat int64, t gotgcall.StreamType, d gotgcall.Device, err error) {
		log.Printf("stream end chat=%d type=%s device=%s err=%v", chat, t, d, err)
	})

	// ICE/DTLS state transitions: Connecting → Connected → Failed/Closed.
	client.OnConnectionChange(func(chat int64, info gotgcall.NetworkInfo) {
		log.Printf("conn chat=%d state=%s", chat, info.State)
	})

	// Server-side state change (admin muted/unmuted us, video disabled).
	// Wired from the gogram updates loop below via NotifyUpgrade.
	client.OnUpgrade(func(chat int64, state gotgcall.MediaState) {
		log.Printf("UPGRADE chat=%d muted=%v video_stopped=%v", chat, state.Muted, state.VideoStopped)
	})

	// Hook gogram updates → NotifyUpgrade. Whenever Telegram sends
	// UpdateGroupCallParticipants and our peer is in the list with new
	// muted/video_stopped flags, forward to the library.
	tg.AddRawHandler(&telegram.UpdateGroupCallParticipants{}, func(u telegram.Update, _ *telegram.Client) error {
		upd, ok := u.(*telegram.UpdateGroupCallParticipants)
		if !ok {
			return nil
		}
		for _, p := range upd.Participants {
			// In real code, compare p.Peer to your own user/channel id. For
			// brevity, we accept any participant change here.
			_ = p
			client.NotifyUpgrade(chatID, gotgcall.MediaState{
				// Fill these from the participant struct fields supplied by
				// your gogram version. Field names vary by layer.
				// Example:
				//   Muted:        p.Muted,
				//   VideoStopped: p.VideoStopped,
			})
		}
		return nil
	})
	// 1. Local-side JSON.
	localParams, err := client.CreateCall(chatID)
	if err != nil {
		log.Fatalf("create call: %v", err)
	}
	log.Printf("local params: %d bytes", len(localParams))
	// 2. Drive Telegram via your MTProto layer.
	//    See gogram docs for the exact response shape on your version.
	//    Typical sequence:
	//
	//      inputCall := /* lookup phone.GetGroupCall(chat) */
	//      me, _   := tg.GetMe()
	//      updates, _ := tg.PhoneJoinGroupCall(&telegram.PhoneJoinGroupCallParams{
	//          Call:   inputCall,
	//          JoinAs: &telegram.InputPeerUser{UserID: me.ID, AccessHash: me.AccessHash},
	//          Params: &telegram.DataJson{Data: localParams},
	//      })
	//      remoteParams := /* extract phone.GroupCall.Params.Data from updates */
	remoteParams, err := joinAndGetRemoteParams(tg, chatID, localParams)
	if err != nil {
		log.Fatalf("join + extract: %v", err)
	}
	// 3. Finish handshake.
	if err = client.Connect(chatID, remoteParams); err != nil {
		log.Fatalf("client.Connect: %v", err)
	}
	// 4. Stream.
	if err = client.SetStreamSources(chatID, gotgcall.FromFile(source)); err != nil {
		log.Fatalf("set source: %v", err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	// 5. Leave. phone.LeaveGroupCall takes a `Source` int that the server
	//    uses to identify which participant is leaving. In practice 0 works
	//    fine (Telegram infers from the joined session); if you want to be
	//    explicit, pass client.AudioSSRC(chatID).
	_ = client.Stop(chatID)
	_ = leaveGroupCall(tg, chatID, 0)
	log.Println("done")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// joinAndGetRemoteParams is a stub that the integrator should implement
// against the gogram version they ship. The library is intentionally
// MTProto-lib-agnostic; this glue lives in your bot code, not in gotgcall.
func joinAndGetRemoteParams(_ *telegram.Client, _ int64, _ string) (string, error) {
	return "", errors.New("TODO: implement against your gogram version; see comments above")
}

func leaveGroupCall(_ *telegram.Client, _ int64, _ uint32) error {
	return errors.New("TODO: implement phone.LeaveGroupCall against your gogram version")
}
