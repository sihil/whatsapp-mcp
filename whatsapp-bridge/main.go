package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"bytes"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	wmstore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db                 *sql.DB
	mergedLIDs         sync.Map    // LIDs already folded into their phone number (see lid_merge.go)
	syncingCommunities atomic.Bool // a community sync pass is running (see group_links.go)
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", "file:store/messages.db?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			mime_type TEXT,
			edited_at TIMESTAMP,
			deleted_at TIMESTAMP,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE TABLE IF NOT EXISTS jid_aliases (
			old_jid TEXT PRIMARY KEY,
			new_jid TEXT NOT NULL
		);
	` + contactsSchema + ";" + senderIndexSchema + ";" + groupEventsSchema + ";" + groupLinksSchema + ";")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}
	if err := migrateMutations(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateMedia(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateGroupLinks(db); err != nil {
		db.Close()
		return nil, err
	}

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database.
//
// Upsert rather than INSERT OR REPLACE: an empty name must not erase the name
// already on the row (GetChatName now returns "" instead of inventing a phone
// number, so this is the common case for unknown contacts). It also avoids
// REPLACE's delete-then-insert, which fires foreign key actions against the
// messages rows that reference this chat.
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		 ON CONFLICT(jid) DO UPDATE SET
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END,
			last_message_time = excluded.last_message_time`,
		jid, keepChatName(jid, name), lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	media mediaInfo) error {
	// Only store if there's actual content or media
	if content == "" && media.Type == "" {
		return nil
	}

	// Upsert rather than INSERT OR REPLACE so a re-delivered message (history
	// sync) cannot wipe edited_at/deleted_at: a tombstone is never resurrected,
	// and an edited message keeps its edited text.
	_, err := store.db.Exec(
		`INSERT INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, mime_type, url, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, chat_jid) DO UPDATE SET
			sender = excluded.sender,
			content = CASE WHEN messages.edited_at IS NOT NULL THEN messages.content ELSE excluded.content END,
			timestamp = excluded.timestamp, is_from_me = excluded.is_from_me,
			media_type = excluded.media_type, filename = excluded.filename,
			mime_type = COALESCE(excluded.mime_type, messages.mime_type), url = excluded.url,
			media_key = excluded.media_key, file_sha256 = excluded.file_sha256,
			file_enc_sha256 = excluded.file_enc_sha256, file_length = excluded.file_length
		WHERE messages.deleted_at IS NULL`,
		id, chatJID, sender, content, timestamp, isFromMe, media.Type, media.Filename, nullIfEmpty(media.MimeType),
		media.URL, media.MediaKey, media.FileSHA256, media.FileEncSHA256, media.FileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// GetMessageForReply looks up a stored message by id and chat JID, returning
// the fields needed to build a quoted-reply ContextInfo. Returns
// sql.ErrNoRows if no such message exists in that chat.
func (store *MessageStore) GetMessageForReply(id, chatJID string) (sender string, content string, mediaType string, isFromMe bool, err error) {
	err = store.db.QueryRow(
		"SELECT sender, content, media_type, is_from_me FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&sender, &content, &mediaType, &isFromMe)
	return sender, content, mediaType, isFromMe, err
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
	ReplyTo   string `json:"reply_to,omitempty"`
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, messageStore *MessageStore, recipient string, message string, mediaPath string, replyTo string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waProto.Message{}

	// Resolve the quoted-reply context up front so both the text and the
	// media branches can attach it.
	var replyCtx *waProto.ContextInfo
	if replyTo != "" {
		var errMsg string
		replyCtx, errMsg = resolveReplyContext(client, messageStore, recipientJID, replyTo)
		if errMsg != "" {
			return false, errMsg
		}
	}

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		fmt.Println("Media uploaded", resp)

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				ContextInfo:   replyCtx,
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				ContextInfo:   replyCtx,
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				ContextInfo:   replyCtx,
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(mediaPath[strings.LastIndex(mediaPath, "/")+1:]),
				Caption:       proto.String(message),
				ContextInfo:   replyCtx,
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else if replyCtx != nil {
		msg.ExtendedTextMessage = &waProto.ExtendedTextMessage{
			Text:        proto.String(message),
			ContextInfo: replyCtx,
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	resp, err := client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	// No echo reaches the sending device: store the row ourselves so the
	// message can be listed, edited and deleted (see mutation_store.go).
	if err := recordSentMessage(context.Background(), client.Store.LIDs, messageStore, recipientJID,
		client.Store.GetJID(), resp.ID, resp.Timestamp, msg); err != nil {
		fmt.Printf("Failed to store sent message %s: %v\n", resp.ID, err)
	}

	return true, fmt.Sprintf("Message sent to %s (ID: %s)", recipient, resp.ID)
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Canonicalize identities before storing (see identity.go). The chat's
	// alt JID comes from the stanza: SenderAlt for incoming messages,
	// RecipientAlt for own-outgoing DMs.
	ctx := context.Background()
	chatAlt := msg.Info.SenderAlt
	if msg.Info.IsFromMe {
		chatAlt = msg.Info.RecipientAlt
	}
	canonicalChat := canonicalChatJID(ctx, client.Store.LIDs, msg.Info.Chat, chatAlt)
	chatJID := canonicalChat.String()
	sender := canonicalSenderJID(ctx, client.Store.LIDs, msg.Info.Sender, msg.Info.SenderAlt, client.Store.GetJID(), msg.Info.IsFromMe)
	if err := messageStore.MergeLIDs(ctx, messageLIDs(msg.Info.MessageSource, canonicalChat, sender)); err != nil {
		logger.Warnf("Failed to merge LID rows into their phone numbers: %v", err)
	}

	// Every incoming stanza carries the sender's self-chosen display name. For
	// a group participant who is not in the address book it is the only name
	// available, so record it against the canonical sender JID.
	if !msg.Info.IsFromMe && msg.Info.PushName != "" {
		if err := messageStore.StorePushName(sender, msg.Info.PushName); err != nil {
			logger.Warnf("Failed to store push name for %s: %v", sender, err)
		}
	}

	// Reactions, edits and revokes update existing rows; they are not messages
	// and must not bump the chat's last message time (see mutation_events.go).
	if handleMutationEvent(client, messageStore, msg, chatJID, sender, logger) {
		return
	}

	// Get appropriate chat name (pass nil for conversation since we don't
	// have one for regular messages). May be empty when the contact store
	// knows nothing; StoreChat keeps any name already on the row.
	name := GetChatName(client, messageStore, canonicalChat, chatJID, nil, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	media := extractMediaInfo(msg.Message)
	mediaType, filename := media.Type, media.Filename

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		media,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Size     uint64 `json:"size,omitempty"`
}

// GroupMembersRequest represents the request body for the group members API
type GroupMembersRequest struct {
	GroupJID string `json:"group_jid"`
}

// GroupMember represents a single group participant
type GroupMember struct {
	JID          string `json:"jid"`
	PhoneNumber  string `json:"phone_number,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	IsAdmin      bool   `json:"is_admin"`
	IsSuperAdmin bool   `json:"is_super_admin"`
}

// GroupMembersResponse represents the response for the group members API
type GroupMembersResponse struct {
	Success     bool          `json:"success"`
	Message     string        `json:"message"`
	GroupName   string        `json:"group_name,omitempty"`
	Members     []GroupMember `json:"members,omitempty"`
	MemberCount int           `json:"member_count"`
}

// altJIDResolver is the subset of whatsmeow's device store used to map a JID
// to its counterpart in the other addressing namespace (phone number vs LID).
type altJIDResolver interface {
	GetAltJID(ctx context.Context, jid types.JID) (types.JID, error)
}

// resolveStoredChatJID returns the chat key under which messageID is stored:
// the given key when the row exists there, otherwise the mapped alternate-
// namespace key when one is known. Callers may hold a stale key from the
// other namespace; the store is canonicalized to a single form.
func resolveStoredChatJID(ctx context.Context, alts altJIDResolver, store *MessageStore, messageID string, chatJID types.JID) types.JID {
	var exists int
	err := store.db.QueryRow(
		"SELECT 1 FROM messages WHERE id = ? AND chat_jid = ?", messageID, chatJID.String(),
	).Scan(&exists)
	if err != sql.ErrNoRows {
		return chatJID
	}
	// The startup migration records every chat key it renames (legacy-server
	// renames, @lid merges) in jid_aliases; that maps straight to the current
	// key, so consult it before the PN<->LID store.
	var aliased string
	if store.db.QueryRow(
		"SELECT new_jid FROM jid_aliases WHERE old_jid = ?", chatJID.String(),
	).Scan(&aliased) == nil {
		if jid, parseErr := types.ParseJID(aliased); parseErr == nil {
			return jid
		}
	}
	if alt, altErr := alts.GetAltJID(ctx, chatJID); altErr == nil && !alt.IsEmpty() {
		return alt
	}
	return chatJID
}

// resolveReplyContext looks up the reply target message and builds the
// quoted-reply ContextInfo. Returns a non-empty errMsg on failure.
func resolveReplyContext(client *whatsmeow.Client, messageStore *MessageStore, recipientJID types.JID, replyTo string) (ctx *waProto.ContextInfo, errMsg string) {
	chatKey := resolveStoredChatJID(context.Background(), client.Store, messageStore, replyTo, recipientJID)
	sender, content, mediaType, isFromMe, err := messageStore.GetMessageForReply(replyTo, chatKey.String())
	if err == sql.ErrNoRows {
		return nil, "Reply target message not found in chat"
	}
	if err != nil {
		return nil, fmt.Sprintf("Error looking up reply target: %v", err)
	}

	participant := resolveReplyParticipant(context.Background(), client.Store.LIDs,
		sender, isFromMe, chatKey, client.Store.GetJID().ToNonAD())

	return buildReplyContextInfo(replyTo, participant, content, mediaType), ""
}

// buildReplyContextInfo builds the ContextInfo for a quoted reply. Content-less
// rows (media messages without a caption) quote a placeholder derived from the
// stored media type.
func buildReplyContextInfo(id string, participant types.JID, content, mediaType string) *waProto.ContextInfo {
	quotedText := content
	if quotedText == "" {
		if mediaType != "" {
			quotedText = "[" + mediaType + "]"
		} else {
			quotedText = "[media]"
		}
	}

	return &waProto.ContextInfo{
		StanzaID:    proto.String(id),
		Participant: proto.String(participant.String()),
		QuotedMessage: &waProto.Message{
			Conversation: proto.String(quotedText),
		},
	}
}

// Function to get group members from WhatsApp
// The roster is also written through to the contacts table: this is the one
// place the bridge learns names for participants who have never sent a message.
func getGroupMembers(client *whatsmeow.Client, messageStore *MessageStore, groupJID string) (bool, string, string, []GroupMember) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp", "", nil
	}

	// Parse the JID
	jid, err := types.ParseJID(groupJID)
	if err != nil {
		return false, fmt.Sprintf("Invalid group JID: %v", err), "", nil
	}

	// Verify it's a group JID
	if jid.Server != "g.us" {
		return false, "JID is not a group (must end with @g.us)", "", nil
	}

	// Get group info from WhatsApp
	groupInfo, err := client.GetGroupInfo(context.Background(), jid)
	if err != nil {
		return false, fmt.Sprintf("Failed to get group info: %v", err), "", nil
	}

	// Convert participants to our GroupMember struct
	members := make([]GroupMember, 0, len(groupInfo.Participants))
	learned := make([]ContactNames, 0, len(groupInfo.Participants))
	for _, participant := range groupInfo.Participants {
		member := GroupMember{
			JID:          participant.JID.String(),
			IsAdmin:      participant.IsAdmin,
			IsSuperAdmin: participant.IsSuperAdmin,
			DisplayName:  participant.DisplayName,
		}

		// Resolve LID participants to their phone-number JID when a mapping
		// exists; it feeds both the reported number and the contact lookup.
		lookupJID := participant.JID
		if participant.JID.Server == types.HiddenUserServer {
			if pn, err := client.Store.LIDs.GetPNForLID(context.Background(), participant.JID); err == nil && !pn.IsEmpty() {
				lookupJID = pn
			}
		}
		member.PhoneNumber = lookupJID.User

		// Look up contact info for better display name
		contact, err := client.Store.Contacts.GetContact(context.Background(), lookupJID)
		if err == nil {
			if name := bestContactName(contact); name != "" {
				member.DisplayName = name
			}
			// lookupJID is already PN-canonical, matching messages.sender.
			learned = append(learned, contactNamesOf(lookupJID.String(), contact))
		}

		members = append(members, member)
	}

	// One transaction for the whole roster, not one per participant.
	if err := messageStore.StoreContacts(learned); err != nil {
		fmt.Printf("Warning: failed to store contact names for group %s: %v\n", groupJID, err)
	}

	return true, "Successfully retrieved group members", groupInfo.Name, members
}

// Register the REST API handlers that expose the WhatsApp client
// functionality, then open the gate the server has been holding shut.
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, ready *atomic.Bool) {
	// Handler for sending messages
	http.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		fmt.Println("Received request to send message", req.Message, req.MediaPath)

		// Send the message
		success, message := sendWhatsAppMessage(client, messageStore, req.Recipient, req.Message, req.MediaPath, req.ReplyTo)
		fmt.Println("Message sent", success, message)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	})

	// Handler for downloading media
	http.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		media, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		w.Header().Set("Content-Type", "application/json")

		if err != nil {
			errMsg := err.Error()
			fmt.Printf("Media download failed for message %s in %s: %s\n", req.MessageID, req.ChatJID, errMsg)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", media.Type),
			Filename: media.Filename,
			Path:     path,
			MimeType: media.MimeType,
			Size:     media.FileLength,
		})
	})

	// Handler for getting group members
	// Everyone in a community's linked groups. Answers like /api/group/members.
	http.HandleFunc("/api/community/members", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			CommunityJID string `json:"community_jid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		jid, err := types.ParseJID(req.CommunityJID)
		if err != nil || jid.Server != types.GroupServer {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(GroupMembersResponse{Message: "community_jid must be a group JID ending @g.us"})
			return
		}
		ctx := r.Context()
		jids, err := client.GetLinkedGroupsParticipants(ctx, jid)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(GroupMembersResponse{Message: fmt.Sprintf("Failed to get community members: %v", err)})
			return
		}
		members := make([]GroupMember, 0, len(jids))
		var learned []ContactNames
		for _, participant := range jids {
			member, names := communityMember(ctx, client.Store.LIDs, client.Store.Contacts, participant)
			members = append(members, member)
			if names != nil {
				learned = append(learned, *names)
			}
		}
		if err := messageStore.StoreContacts(learned); err != nil {
			fmt.Printf("Warning: failed to store contact names for community %s: %v\n", jid, err)
		}
		var name string
		messageStore.db.QueryRowContext(ctx, `SELECT name FROM group_links WHERE group_jid = ?`, jid.String()).Scan(&name)
		json.NewEncoder(w).Encode(GroupMembersResponse{
			Success: true, Message: "Successfully retrieved community members",
			GroupName: name, Members: members, MemberCount: len(members),
		})
	})

	http.HandleFunc("/api/group/members", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req GroupMembersRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.GroupJID == "" {
			http.Error(w, "Group JID is required", http.StatusBadRequest)
			return
		}

		// Basic JID format validation
		if !strings.Contains(req.GroupJID, "@") {
			http.Error(w, "Invalid JID format", http.StatusBadRequest)
			return
		}

		// Get group members
		success, message, groupName, members := getGroupMembers(client, messageStore, req.GroupJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Determine appropriate status code
		if !success {
			// Client errors (invalid input) get 400, server errors get 500
			if strings.Contains(message, "Invalid group JID") ||
				strings.Contains(message, "not a group") ||
				strings.Contains(message, "Invalid JID format") {
				w.WriteHeader(http.StatusBadRequest)
			} else {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}

		// Send response
		json.NewEncoder(w).Encode(GroupMembersResponse{
			Success:     success,
			Message:     message,
			GroupName:   groupName,
			Members:     members,
			MemberCount: len(members),
		})
	})

	registerMutationHandlers(client, messageStore)

	// The server has been answering 503 since startup (see serveGated).
	ready.Store(true)
	fmt.Println("REST API is accepting requests")
}

// pairDisplayName returns the client display name sent with a pairing-code
// request. WhatsApp's server validates it against a short list of
// "Browser (OS)" names, so only the OS half can honestly vary — the browser
// half must stay in sync with the whatsmeow.PairClientChrome constant passed
// to PairPhone.
func pairDisplayName(goos string) string {
	switch goos {
	case "darwin":
		return "Chrome (Mac OS)"
	case "windows":
		return "Chrome (Windows)"
	default:
		return "Chrome (Linux)"
	}
}

func main() {
	// Optional pairing-code auth: pass a phone number to pair via an 8-character
	// code instead of scanning a QR.
	pairPhone := flag.String("pair-phone", "", "phone number (international format, digits only, no +) to pair via 8-character code instead of QR")
	flag.Parse()
	usingPairCode := *pairPhone != ""

	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Claim the REST port before opening the session (see listenLocal).
	listener, err := listenLocal(8080)
	if err != nil {
		logger.Errorf("Another bridge appears to be running: %v", err)
		os.Exit(1)
	}
	apiReady := serveGated(listener, http.DefaultServeMux)

	// Linked Devices label (takes effect at pairing time, so existing sessions
	// keep their old label until re-paired). The phone ignores Os and shows
	// "Other device" unless PlatformType is set to a known value.
	wmstore.DeviceProps.Os = proto.String("WhatsApp MCP Bridge")
	wmstore.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Only ack messages after our event handlers have persisted them, and keep
	// sent messages in the whatsmeow DB so decryption-retry requests survive restarts.
	client.SynchronousAck = true
	client.UseRetryMessageStore = true

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Canonicalize legacy JID formats in the message store (see identity.go).
	// Runs on every startup: re-runs pick up PN<->LID mappings the store has
	// learned since the rows were written.
	if changed, err := canonicalizeMessageStore(context.Background(), messageStore.db, client.Store.LIDs, client.Store.GetJID()); err != nil {
		logger.Warnf("Failed to canonicalize message store: %v", err)
	} else if changed > 0 {
		logger.Infof("Canonicalized %d legacy rows in the message store", changed)
	}

	// Seed contact names so group senders can be attributed by the read layer,
	// and clear phone-number chat names left by the old GetChatName fallback
	// (see contacts.go). Idempotent; runs on every startup.
	if synced, scrubbed, err := migrateContacts(
		context.Background(), messageStore.db, client.Store.Contacts, client.Store.LIDs,
	); err != nil {
		logger.Warnf("Failed to sync contact names: %v", err)
	} else {
		logger.Infof("Synced %d contact names, cleared %d phone-number chat names", synced, scrubbed)
	}

	// Setup event handling for messages and history sync
	fatalChan := make(chan string, 1)
	client.AddEventHandler(func(evt interface{}) {
		if reason, terminal := terminalEventReason(evt); terminal {
			select {
			case fatalChan <- reason:
			default:
			}
			return
		}
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.GroupInfo:
			changes := groupEventsFromInfo(context.Background(), client.Store.LIDs, v)
			if err := messageStore.StoreGroupEvents(changes); err != nil {
				logger.Warnf("Failed to store membership changes in %s: %v", v.JID, err)
			}
			if err := messageStore.ApplyOwnMembership(changes, client.Store.GetJID().ToNonAD().String()); err != nil {
				logger.Warnf("Failed to record own membership of %s: %v", v.JID, err)
			}
			if links, err := messageStore.ApplyGroupLinkChanges(context.Background(), client.Store.LIDs, v); err != nil {
				logger.Warnf("Failed to apply community link changes in %s: %v", v.JID, err)
			} else if err := messageStore.StoreGroupEvents(links); err != nil {
				logger.Warnf("Failed to store community link changes in %s: %v", v.JID, err)
			}

		case *events.JoinedGroup:
			if err := messageStore.PutGroupInfo(&v.GroupInfo); err != nil {
				logger.Warnf("Failed to record community structure of %s: %v", v.JID, err)
			}

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			// Asks WhatsApp, so not on the event goroutine.
			go syncCommunities(client, messageStore, logger)

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		if usingPairCode {
			// PairPhone needs the login socket ready; per whatsmeow's docs, the
			// first QR channel item signals that. Request the code exactly once —
			// re-requesting would invalidate the previous code.
			if evt, ok := <-qrChan; !ok || evt.Event != "code" {
				logger.Errorf("Login failed before a pairing code could be requested (event %q): %v", evt.Event, evt.Error)
				return
			}
			code, perr := client.PairPhone(context.Background(), *pairPhone, true, whatsmeow.PairClientChrome, pairDisplayName(runtime.GOOS))
			if perr != nil {
				logger.Errorf("Failed to get pairing code: %v", perr)
				return
			}
			fmt.Printf("\nPairing code: %s\nOn your phone: WhatsApp > Settings > Linked Devices > Link a Device > Link with phone number instead\n", code)
		}

		// The channel emits fresh QR codes until pairing succeeds or a terminal
		// event (timeout, err-client-outdated, ...) closes it.
		authenticated := false
		var lastEvt whatsmeow.QRChannelItem
		for evt := range qrChan {
			lastEvt = evt
			if evt.Event == "code" && !usingPairCode {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				authenticated = true
				break
			}
		}
		if !authenticated {
			logger.Errorf("Login did not complete (last event %q): %v", lastEvt.Event, lastEvt.Error)
			return
		}
		fmt.Println("\nSuccessfully connected and authenticated!")
	} else {
		// Already logged in, just connect
		if usingPairCode {
			logger.Warnf("Ignoring -pair-phone: a session already exists; delete store/whatsapp.db to re-pair")
		}
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server
	startRESTServer(client, messageStore, apiReady)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for a termination signal, or for the session to be lost for good
	select {
	case <-exitChan:
	case reason := <-fatalChan:
		// Exit non-zero so a supervisor (launchd KeepAlive) restarts the bridge.
		logger.Errorf("Exiting: %s", reason)
		client.Disconnect()
		messageStore.Close()
		os.Exit(1)
	}

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	// A stored name that is nothing but digits came from this function's old
	// phone-number fallback. Treat it as absent so the lookup runs again — but
	// only for a DM: a group or channel name is user-set, so digits there are a
	// real name.
	isGroup := jid.Server == types.GroupServer
	if err == nil && (isMultiPartyChat(chatJID) && existingName != "" ||
		!isNumericName(existingName)) {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if isGroup {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Use whatever the contact store knows, best name first. There is
		// deliberately no phone-number fallback: a bare number is not a name,
		// and storing one as if it were defeats the read layer's own fallback
		// chain. An empty name leaves the row's existing value untouched.
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil {
			name = bestContactName(contact)
			// Same names the read layer will want for this sender in groups.
			names := contactNamesOf(chatJID, contact)
			if storeErr := messageStore.StoreContactNames(
				names.JID, names.FullName, names.PushName, names.BusinessName,
			); storeErr != nil {
				logger.Warnf("Failed to store contact names for %s: %v", chatJID, storeErr)
			}
		}

		logger.Infof("Using contact name: %q", name)
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	// The payload's own PN<->LID table must be consulted before the LID
	// store: whatsmeow ingests it asynchronously, racing this handler.
	pnMap := historyPNMap(historySync.Data)
	if err := messageStore.MergeLIDs(context.Background(), pnMap); err != nil {
		logger.Warnf("Failed to merge LID rows into their phone numbers: %v", err)
	}
	ownJID := client.Store.GetJID()

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		// Try to parse the JID
		jid, err := types.ParseJID(*conversation.ID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", *conversation.ID, err)
			continue
		}

		// Canonicalize the chat key (see identity.go)
		jid = canonicalHistoryChatJID(context.Background(), client.Store.LIDs, jid, conversation.GetPnJID(), pnMap)
		chatJID := jid.String()

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				content := extractTextContent(msg.Message.Message)
				media := extractMediaInfo(msg.Message.Message)
				mediaType, filename := media.Type, media.Filename

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// A REVOKE stub is a deleted message and has nothing to store
				// (see mutation_events.go).
				if applyHistoryRevoke(context.Background(), client.Store.LIDs, messageStore, msg.Message, jid, ownJID, pnMap, logger) {
					continue
				}
				if changes, ok := groupEventsFromHistory(context.Background(), client.Store.LIDs, msg.Message, jid, ownJID, pnMap); ok {
					if err := messageStore.StoreGroupEvents(changes); err != nil {
						logger.Warnf("Failed to store history membership changes in %s: %v", chatJID, err)
					}
					continue
				}

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine the canonical sender (see identity.go)
				participant := ""
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if msg.Message.Key.Participant != nil {
						participant = *msg.Message.Key.Participant
					}
				}
				if participant == "" {
					participant = msg.Message.GetParticipant()
				}
				sender := canonicalHistorySenderJID(context.Background(), client.Store.LIDs,
					participant, isFromMe, jid, ownJID, pnMap)

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					media,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					// Reactions ride on the message and need its row to attach to.
					applyHistoryReactions(context.Background(), client.Store.LIDs, messageStore, msg.Message, jid, ownJID, pnMap, logger)
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// Found OpusHead, extract sample rate and pre-skip
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8 // Skip "OpusHead" marker
				// PreSkip is 2 bytes at offset 10
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					fmt.Printf("Found OpusHead: sampleRate=%d, preSkip=%d\n", sampleRate, preSkip)
				}
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rand.Seed(int64(duration))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rand.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
