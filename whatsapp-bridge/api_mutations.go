package main

// REST handlers for reacting to, editing and deleting messages. The pure
// checks (checkEditable, checkDeletable) decide what WhatsApp would refuse
// anyway, so the caller gets a clear message instead of a silent no-op.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// ReactRequest is the body of POST /api/react. An empty emoji removes the reaction.
type ReactRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji"`
}

// EditRequest is the body of POST /api/edit.
type EditRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
	Message   string `json:"message"`
}

// DeleteRequest is the body of POST /api/delete.
type DeleteRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
}

// checkEditable returns why m cannot be edited, or "" when it can.
func checkEditable(m StoredMessage, newText string, now time.Time) string {
	switch {
	case !m.IsFromMe:
		return "Only your own messages can be edited"
	case m.Deleted:
		return "Message was deleted"
	case m.MediaType != "":
		return "Only text messages can be edited"
	case newText == "":
		return "New message text is required"
	case !withinEditWindow(m.Time, now):
		return "Message is older than 15 minutes and can no longer be edited"
	}
	return ""
}

// checkDeletable returns why m cannot be deleted for everyone, or "" when it can.
func checkDeletable(m StoredMessage, now time.Time) string {
	switch {
	case !m.IsFromMe:
		return "Only your own messages can be deleted"
	case m.Deleted:
		return "Message is already deleted"
	case !withinDeleteWindow(m.Time, now):
		return "Message is older than 2 days 12 hours and can no longer be deleted for everyone"
	}
	return ""
}

// mutationTarget resolves the chat key and loads the target message. A non-zero
// status means the request failed and msg says why.
func mutationTarget(client *whatsmeow.Client, store *MessageStore, chatJID, messageID string) (chat types.JID, m StoredMessage, status int, msg string) {
	if !client.IsConnected() {
		return chat, m, http.StatusInternalServerError, "Not connected to WhatsApp"
	}
	if !strings.Contains(chatJID, "@") {
		chatJID += "@" + types.DefaultUserServer
	}
	parsed, err := types.ParseJID(chatJID)
	if err != nil {
		return chat, m, http.StatusBadRequest, fmt.Sprintf("Invalid chat JID: %v", err)
	}
	ctx := context.Background()
	chat = resolveStoredChatJID(ctx, client.Store, store, messageID, parsed)
	m, err = store.GetStoredMessage(ctx, messageID, chat.String())
	if err == sql.ErrNoRows {
		return chat, m, http.StatusBadRequest, "message not found"
	}
	if err != nil {
		return chat, m, http.StatusInternalServerError, fmt.Sprintf("Error looking up message: %v", err)
	}
	return chat, m, 0, ""
}

func reactToMessage(client *whatsmeow.Client, store *MessageStore, req ReactRequest) (int, string) {
	chat, m, status, msg := mutationTarget(client, store, req.ChatJID, req.MessageID)
	if status != 0 {
		return status, msg
	}
	if m.Deleted {
		return http.StatusBadRequest, "Message was deleted"
	}
	ctx := context.Background()
	own := client.Store.GetJID().ToNonAD()
	target := own
	if !m.IsFromMe {
		target = resolveReplyParticipant(ctx, client.Store.LIDs, m.Sender, false, chat, own)
	}
	if _, err := client.SendMessage(ctx, chat, client.BuildReaction(chat, target, req.MessageID, req.Emoji)); err != nil {
		return http.StatusInternalServerError, fmt.Sprintf("Error sending reaction: %v", err)
	}
	// The sending device gets no echo, so record our own reaction here.
	if err := store.SetReaction(req.MessageID, chat.String(), own.String(), req.Emoji, time.Now()); err != nil {
		fmt.Printf("Failed to store own reaction: %v\n", err)
	}
	if req.Emoji == "" {
		return 0, "Reaction removed"
	}
	return 0, "Reaction sent"
}

func editMessage(client *whatsmeow.Client, store *MessageStore, req EditRequest) (int, string) {
	chat, m, status, msg := mutationTarget(client, store, req.ChatJID, req.MessageID)
	if status != 0 {
		return status, msg
	}
	now := time.Now()
	if why := checkEditable(m, req.Message, now); why != "" {
		return http.StatusBadRequest, why
	}
	edit := client.BuildEdit(chat, req.MessageID, &waE2E.Message{Conversation: proto.String(req.Message)})
	if _, err := client.SendMessage(context.Background(), chat, edit); err != nil {
		return http.StatusInternalServerError, fmt.Sprintf("Error sending edit: %v", err)
	}
	// Key the store change on the row's own sender: is_from_me is already
	// verified, and a legacy row may spell the own JID differently.
	if applied, err := store.ApplyEdit(chat.String(), req.MessageID, m.Sender, req.Message, now); err != nil || !applied {
		fmt.Printf("Edit of %s was sent but not stored (applied=%v): %v\n", req.MessageID, applied, err)
	}
	return 0, "Message edited"
}

func deleteMessage(client *whatsmeow.Client, store *MessageStore, req DeleteRequest) (int, string) {
	chat, m, status, msg := mutationTarget(client, store, req.ChatJID, req.MessageID)
	if status != 0 {
		return status, msg
	}
	now := time.Now()
	if why := checkDeletable(m, now); why != "" {
		return http.StatusBadRequest, why
	}
	if _, err := client.SendMessage(context.Background(), chat, client.BuildRevoke(chat, types.EmptyJID, req.MessageID)); err != nil {
		return http.StatusInternalServerError, fmt.Sprintf("Error sending delete: %v", err)
	}
	if applied, err := store.ApplyRevoke(chat.String(), req.MessageID, m.Sender, nil, now); err != nil || !applied {
		fmt.Printf("Delete of %s was sent but not stored (applied=%v): %v\n", req.MessageID, applied, err)
	}
	return 0, "Message deleted"
}

// serveMutation registers a POST handler that decodes a T, validates it and
// runs the action. It answers {"success","message"}: 400 for bad input, 500 for
// send failure. Each request decodes into its own T.
func serveMutation[T any](path string, valid func(T) bool, run func(T) (int, string)) {
	http.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req T
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if !valid(req) {
			http.Error(w, "Chat JID, message ID and any required text are needed", http.StatusBadRequest)
			return
		}
		status, message := run(req)
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: status == 0, Message: message})
	})
}

// registerMutationHandlers adds /api/react, /api/edit and /api/delete.
func registerMutationHandlers(client *whatsmeow.Client, store *MessageStore) {
	serveMutation("/api/react",
		func(r ReactRequest) bool { return r.ChatJID != "" && r.MessageID != "" },
		func(r ReactRequest) (int, string) { return reactToMessage(client, store, r) })
	serveMutation("/api/edit",
		func(r EditRequest) bool { return r.ChatJID != "" && r.MessageID != "" && r.Message != "" },
		func(r EditRequest) (int, string) { return editMessage(client, store, r) })
	serveMutation("/api/delete",
		func(r DeleteRequest) bool { return r.ChatJID != "" && r.MessageID != "" },
		func(r DeleteRequest) (int, string) { return deleteMessage(client, store, r) })
}
