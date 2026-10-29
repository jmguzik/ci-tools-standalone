package main

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// actionPreviewResponse freezes the current targets while leaving future rule matching explicit.
type actionPreviewResponse struct {
	Occurrences []opsalerts.Occurrence `json:"occurrences"`
	Until       *time.Time             `json:"until,omitempty"`
}

// preview evaluates identifiers on the server without creating Slack actions or rules.
func (d *dashboard) preview(w http.ResponseWriter, r *http.Request, actor string) {
	if r.Header.Get("Origin") != d.access.origin || !hmac.Equal([]byte(r.Header.Get("X-CSRF-Token")), []byte(d.csrfToken(actor))) {
		writeJSON(w, http.StatusForbidden, apiError{Error: "same-origin CSRF validation failed", Code: "forbidden"})
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, apiError{Error: "application/json is required", Code: "validation"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request opsalerts.PreviewRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "invalid preview JSON", Code: "validation"})
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, apiError{Error: "exactly one preview is required", Code: "validation"})
		return
	}
	occurrences, err := d.service.Preview(request)
	if err != nil {
		status, code := actionError(err)
		writeJSON(w, status, apiError{Error: err.Error(), Code: code})
		return
	}
	response := actionPreviewResponse{Occurrences: occurrences}
	if request.Kind == "acknowledge" || request.Kind == "snooze" {
		now := time.Now().UTC()
		until := time.Date(now.Year(), now.Month(), now.Day(), 6, 0, 0, 0, time.UTC)
		if !until.After(now) {
			until = until.Add(24 * time.Hour)
		}
		response.Until = &until
	}
	writeJSON(w, http.StatusOK, response)
}
