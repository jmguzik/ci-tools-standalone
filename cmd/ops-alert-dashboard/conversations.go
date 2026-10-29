package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// conversationService shares the existing authenticated Slack archive without a second writer.
type conversationService interface {
	Conversations() opsalerts.ConversationSnapshot
	Converse(context.Context, string, opsalerts.ConversationRequest) (opsalerts.Conversation, error)
}

// conversationWatcher records bounded foreground interest without reading Slack in an HTTP request.
type conversationWatcher interface {
	TouchConversation(string) bool
}

// conversationSnapshotResponse binds each chat poll to the current authenticated identity.
type conversationSnapshotResponse struct {
	opsalerts.ConversationSnapshot
	User        string `json:"user"`
	CSRFToken   string `json:"csrf_token"`
	ChannelName string `json:"channel_name"`
}

// conversationResponse confirms one retained Slack message and its thread.
type conversationResponse struct {
	Conversation opsalerts.Conversation `json:"conversation"`
}

// conversations reads only the shared cache, so opening a popup never scans Slack.
func (d *dashboard) conversations(w http.ResponseWriter, r *http.Request, actor string) {
	if watcher, ok := d.service.(conversationWatcher); ok {
		watcher.TouchConversation(r.URL.Query().Get("active"))
	}
	snapshot := opsalerts.ConversationSnapshot{Conversations: []opsalerts.Conversation{}}
	if service, ok := d.service.(conversationService); ok {
		snapshot = service.Conversations()
	}
	writeJSON(w, http.StatusOK, conversationSnapshotResponse{ConversationSnapshot: snapshot, User: actor, CSRFToken: d.csrfToken(actor), ChannelName: d.channelName})
}

// converse authenticates one explicit operator message with the same origin and CSRF checks as actions.
func (d *dashboard) converse(w http.ResponseWriter, r *http.Request, actor string) {
	if r.Header.Get("Origin") != d.access.origin || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(d.csrfToken(actor))) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "same-origin CSRF validation failed", Code: "forbidden"})
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, apiError{Error: "application/json is required", Code: "validation"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request opsalerts.ConversationRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid conversation JSON", Code: "validation"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "exactly one message is required", Code: "validation"})
		return
	}
	service, ok := d.service.(conversationService)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, apiError{Error: "Chai conversations are unavailable", Code: "not_ready"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	conversation, err := service.Converse(ctx, actor, request)
	if err != nil {
		status, code := actionError(err)
		writeJSON(w, status, apiError{Error: err.Error(), Code: code})
		return
	}
	writeJSON(w, http.StatusOK, conversationResponse{Conversation: conversation})
}
