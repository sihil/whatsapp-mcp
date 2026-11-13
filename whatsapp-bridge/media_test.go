package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

const signedURL = "https://mmg.whatsapp.net/v/t62.7119-24/891_n.enc?ccb=11-4&oh=01_sig&oe=6B150DBA&_nc_sid=5e03e0&mms3=true"

func TestDirectPathFromURLKeepsSignature(t *testing.T) {
	// whatsmeow appends "&hash=..." to the direct path, and the media servers
	// answer 403 without oh/oe, so the query has to survive.
	got := directPathFromURL(signedURL)
	want := "/v/t62.7119-24/891_n.enc?ccb=11-4&oh=01_sig&oe=6B150DBA&_nc_sid=5e03e0&mms3=true"
	if got != want {
		t.Errorf("directPathFromURL = %q, want %q", got, want)
	}
}

func TestDirectPathFromURLWithoutQuery(t *testing.T) {
	if got := directPathFromURL("https://mmg.whatsapp.net/v/a.enc"); got != "/v/a.enc" {
		t.Errorf("directPathFromURL = %q", got)
	}
}

func TestCaptionsAreContent(t *testing.T) {
	cases := map[string]*waE2E.Message{
		"image":    {ImageMessage: &waE2E.ImageMessage{Caption: proto.String("the rota")}},
		"video":    {VideoMessage: &waE2E.VideoMessage{Caption: proto.String("the rota")}},
		"document": {DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("the rota")}},
		"document with caption": {DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{Caption: proto.String("the rota")}},
		}},
	}
	for name, msg := range cases {
		if got := extractTextContent(msg); got != "the rota" {
			t.Errorf("%s: content = %q, want the caption", name, got)
		}
	}
}

func TestExtractMediaInfoKeepsMimeTypeAndName(t *testing.T) {
	msg := &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
		Message: &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			FileName:   proto.String("Split Plan.pdf"),
			Mimetype:   proto.String("application/pdf"),
			URL:        proto.String(signedURL),
			FileLength: proto.Uint64(440388),
		}},
	}}
	got := extractMediaInfo(msg)
	if got.Type != "document" || got.Filename != "Split Plan.pdf" || got.MimeType != "application/pdf" ||
		got.URL != signedURL || got.FileLength != 440388 {
		t.Errorf("extractMediaInfo = %+v", got)
	}

	img := extractMediaInfo(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg")}})
	if img.Type != "image" || img.MimeType != "image/jpeg" {
		t.Errorf("image media info = %+v", img)
	}
}

func TestMediaCachePathIsPerMessage(t *testing.T) {
	a := mediaCachePath(tChatGrp, "MSG1", "image_20260712_203448.jpg")
	b := mediaCachePath(tChatGrp, "MSG2", "image_20260712_203448.jpg")
	if a == b {
		t.Fatalf("two messages share a cache path: %s", a)
	}
	if !strings.HasSuffix(a, "MSG1_image_20260712_203448.jpg") {
		t.Errorf("cache path %q lost the message ID or the filename", a)
	}
}

func TestMediaCachePathStaysInChatDir(t *testing.T) {
	dir := filepath.Dir(mediaCachePath(tChatGrp, "MSG1", "x"))
	for _, name := range []string{"../../../.zshrc", "/etc/passwd", "a/../../b", ".."} {
		p := mediaCachePath(tChatGrp, "MSG1", name)
		if filepath.Dir(p) != dir {
			t.Errorf("filename %q escapes the chat directory: %s", name, p)
		}
	}
	if p := mediaCachePath(tChatGrp, "../MSG1", "x"); filepath.Dir(p) != dir {
		t.Errorf("message ID escapes the chat directory: %s", p)
	}
}

func TestStoreMessageKeepsMimeType(t *testing.T) {
	s := newMutationStore(t)
	must(t, s.StoreChat(tChatGrp, "", t0))
	must(t, s.StoreMessage("M", tChatGrp, tPeer, "", t0, false,
		mediaInfo{Type: "document", Filename: "a.pdf", MimeType: "application/pdf", URL: signedURL}))
	got, err := s.GetMediaInfo("M", tChatGrp)
	if err != nil {
		t.Fatal(err)
	}
	if got.MimeType != "application/pdf" || got.Filename != "a.pdf" || got.URL != signedURL {
		t.Errorf("GetMediaInfo = %+v", got)
	}
}

func TestMigrateMediaAddsMimeType(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	must(t, func() error {
		_, err := db.Exec(`CREATE TABLE messages (id TEXT, chat_jid TEXT, media_type TEXT, PRIMARY KEY (id, chat_jid));
			INSERT INTO messages VALUES ('A', 'c', 'image')`)
		return err
	}())
	for i := 0; i < 2; i++ {
		if err := migrateMedia(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var mime sql.NullString
	if err := db.QueryRow("SELECT mime_type FROM messages").Scan(&mime); err != nil {
		t.Fatalf("mime_type missing: %v", err)
	}
	if mime.Valid {
		t.Errorf("old row gained a mime type: %q", mime.String)
	}
}
