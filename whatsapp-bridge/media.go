package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
)

// mediaInfo is what the store keeps about a message's attachment: enough to
// describe it in a listing and to download it later.
type mediaInfo struct {
	Type          string
	Filename      string
	MimeType      string
	URL           string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
}

// unwrapMessage returns the message inside a document-with-caption wrapper.
// Live events arrive unwrapped by whatsmeow; history sync and sent messages do
// not.
func unwrapMessage(msg *waProto.Message) *waProto.Message {
	if inner := msg.GetDocumentWithCaptionMessage().GetMessage(); inner != nil {
		return inner
	}
	return msg
}

// extractTextContent is a message's text, or its caption for an image, video
// or document.
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	msg = unwrapMessage(msg)
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		return msg.GetExtendedTextMessage().GetText()
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetCaption()
	case msg.GetVideoMessage() != nil:
		return msg.GetVideoMessage().GetCaption()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetCaption()
	}
	return ""
}

func extractMediaInfo(msg *waProto.Message) mediaInfo {
	if msg == nil {
		return mediaInfo{}
	}
	msg = unwrapMessage(msg)
	stamp := time.Now().Format("20060102_150405")
	if img := msg.GetImageMessage(); img != nil {
		return mediaInfo{"image", "image_" + stamp + ".jpg", img.GetMimetype(), img.GetURL(),
			img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()}
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return mediaInfo{"video", "video_" + stamp + ".mp4", vid.GetMimetype(), vid.GetURL(),
			vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()}
	}
	if aud := msg.GetAudioMessage(); aud != nil {
		return mediaInfo{"audio", "audio_" + stamp + ".ogg", aud.GetMimetype(), aud.GetURL(),
			aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()}
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + stamp
		}
		return mediaInfo{"document", filename, doc.GetMimetype(), doc.GetURL(),
			doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()}
	}
	return mediaInfo{}
}

// migrateMedia adds messages.mime_type to a store created before it. Rows
// stored earlier keep a NULL mime type unless history sync delivers them again.
func migrateMedia(db *sql.DB) error {
	var have int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'mime_type'",
	).Scan(&have); err != nil {
		return fmt.Errorf("reading messages schema: %w", err)
	}
	if have > 0 {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE messages ADD COLUMN mime_type TEXT"); err != nil {
		return fmt.Errorf("adding messages.mime_type: %w", err)
	}
	return nil
}

func (store *MessageStore) GetMediaInfo(id, chatJID string) (mediaInfo, error) {
	var m mediaInfo
	var mediaType, filename, mimeType, mediaURL sql.NullString
	var fileLength sql.NullInt64
	err := store.db.QueryRow(
		`SELECT media_type, filename, mime_type, url, media_key, file_sha256, file_enc_sha256, file_length
		 FROM messages WHERE id = ? AND chat_jid = ?`,
		id, chatJID,
	).Scan(&mediaType, &filename, &mimeType, &mediaURL, &m.MediaKey, &m.FileSHA256, &m.FileEncSHA256, &fileLength)
	m.Type, m.Filename, m.MimeType, m.URL = mediaType.String, filename.String, mimeType.String, mediaURL.String
	m.FileLength = uint64(fileLength.Int64)
	return m, err
}

// directPathFromURL turns a stored media URL into the direct path whatsmeow
// downloads from. The query carries the oh/oe signature: whatsmeow appends
// "&hash=..." to the path, and the media servers answer 403 without it.
func directPathFromURL(mediaURL string) string {
	u, err := url.Parse(mediaURL)
	if err != nil || u.Path == "" {
		return mediaURL
	}
	if u.RawQuery == "" {
		return u.Path
	}
	return u.Path + "?" + u.RawQuery
}

// mediaCachePath is where a message's downloaded media is kept. It is keyed by
// message ID because generated filenames repeat: they record when the bridge
// processed the message, and a history sync processes hundreds in one second.
// The filename is the sender's to choose, so only its base name is used.
func mediaCachePath(chatJID, messageID, filename string) string {
	safe := func(s string) string {
		s = strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(s)
		if s == "." || s == ".." {
			return "_"
		}
		return s
	}
	return filepath.Join("store", "media", safe(chatJID), safe(messageID)+"_"+safe(filename))
}

// MediaDownloader implements whatsmeow.DownloadableMessage.
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

func (d *MediaDownloader) GetDirectPath() string             { return d.DirectPath }
func (d *MediaDownloader) GetURL() string                    { return d.URL }
func (d *MediaDownloader) GetMediaKey() []byte               { return d.MediaKey }
func (d *MediaDownloader) GetFileLength() uint64             { return d.FileLength }
func (d *MediaDownloader) GetFileSHA256() []byte             { return d.FileSHA256 }
func (d *MediaDownloader) GetFileEncSHA256() []byte          { return d.FileEncSHA256 }
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType { return d.MediaType }

var whatsmeowMediaTypes = map[string]whatsmeow.MediaType{
	"image":    whatsmeow.MediaImage,
	"video":    whatsmeow.MediaVideo,
	"audio":    whatsmeow.MediaAudio,
	"document": whatsmeow.MediaDocument,
}

// downloadMedia fetches and decrypts a message's media into the cache, and
// returns the stored media info with the file's absolute path.
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (mediaInfo, string, error) {
	if jid, jidErr := types.ParseJID(chatJID); jidErr == nil {
		if resolved := resolveStoredChatJID(context.Background(), client.Store, messageStore, messageID, jid); resolved != jid {
			chatJID = resolved.String()
		}
	}

	// A tombstone has no media pointers left to download from.
	if m, err := messageStore.GetStoredMessage(context.Background(), messageID, chatJID); err == nil && m.Deleted {
		return mediaInfo{}, "", fmt.Errorf("message was deleted")
	}

	media, err := messageStore.GetMediaInfo(messageID, chatJID)
	if err != nil {
		return mediaInfo{}, "", fmt.Errorf("failed to find message: %v", err)
	}
	if media.Type == "" {
		return mediaInfo{}, "", fmt.Errorf("not a media message")
	}

	localPath := mediaCachePath(chatJID, messageID, media.Filename)
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return mediaInfo{}, "", fmt.Errorf("failed to get absolute path: %v", err)
	}
	if _, err := os.Stat(localPath); err == nil {
		return media, absPath, nil
	}

	if media.URL == "" || len(media.MediaKey) == 0 || len(media.FileSHA256) == 0 ||
		len(media.FileEncSHA256) == 0 || media.FileLength == 0 {
		return mediaInfo{}, "", fmt.Errorf("incomplete media information for download")
	}
	waMediaType, ok := whatsmeowMediaTypes[media.Type]
	if !ok {
		return mediaInfo{}, "", fmt.Errorf("unsupported media type: %s", media.Type)
	}

	data, err := client.Download(context.Background(), &MediaDownloader{
		URL:           media.URL,
		DirectPath:    directPathFromURL(media.URL),
		MediaKey:      media.MediaKey,
		FileLength:    media.FileLength,
		FileSHA256:    media.FileSHA256,
		FileEncSHA256: media.FileEncSHA256,
		MediaType:     waMediaType,
	})
	if err != nil {
		return mediaInfo{}, "", fmt.Errorf("failed to download media: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0700); err != nil {
		return mediaInfo{}, "", fmt.Errorf("failed to create media directory: %v", err)
	}
	// Written under a temporary name so a failed write never leaves a partial
	// file that the cache check above would later serve.
	tmp := localPath + ".part"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return mediaInfo{}, "", fmt.Errorf("failed to save media file: %v", err)
	}
	if err := os.Rename(tmp, localPath); err != nil {
		return mediaInfo{}, "", fmt.Errorf("failed to save media file: %v", err)
	}
	fmt.Printf("Downloaded %s media for message %s in %s to %s (%d bytes)\n",
		media.Type, messageID, chatJID, absPath, len(data))
	return media, absPath, nil
}

// nullIfEmpty stores an unknown value as NULL, so an upsert can tell it apart
// from a known one and keep what is already there.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
