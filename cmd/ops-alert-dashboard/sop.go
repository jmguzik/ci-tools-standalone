package main

import (
	"context"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/openshift/ci-tools-standalone/internal/opsalerts"
)

// maxSOPBytes bounds both upstream reads and browser document size.
const maxSOPBytes = 256 * 1024

// sopHTTPClient permits deterministic document reads without external services in tests.
type sopHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// sopResponse retains the canonical source link, including its section fragment.
type sopResponse struct {
	URL      string `json:"url"`
	Markdown string `json:"markdown"`
}

// newSOPClient shares a bounded client across explicit document expansions.
func newSOPClient() *http.Client {
	return &http.Client{Timeout: 8 * time.Second, CheckRedirect: refuseSOPRedirect}
}

// refuseSOPRedirect prevents a source response from choosing another fetch destination.
func refuseSOPRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// sop serves only documents already linked by the selected retained occurrence.
func (d *dashboard) sop(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	id, source := query.Get("occurrence_id"), query.Get("url")
	linked := false
	for _, occurrence := range d.service.Snapshot().Occurrences {
		if occurrence.Identity.ID == id && slices.Contains(occurrence.Message.SOPURLs, source) {
			linked = true
			break
		}
	}
	raw, allowed := opsalerts.SOPRawURL(source)
	if id == "" || !linked || !allowed {
		writeJSON(w, http.StatusNotFound, apiError{Error: "SOP not found for this occurrence", Code: "not_found"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError{Error: "SOP is temporarily unavailable", Code: "sop_unavailable"})
		return
	}
	response, err := d.sopClient.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError{Error: "SOP is temporarily unavailable", Code: "sop_unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, apiError{Error: "SOP is temporarily unavailable", Code: "sop_unavailable"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSOPBytes+1))
	if err != nil || len(data) > maxSOPBytes {
		writeJSON(w, http.StatusBadGateway, apiError{Error: "SOP is temporarily unavailable", Code: "sop_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, sopResponse{URL: source, Markdown: string(data)})
}
