// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package searchui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
)

func TestSearchAndConsole(t *testing.T) {
	idx, err := fulltext.Open(filepath.Join(t.TempDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, idx)
	if err := idx.Publish(context.Background(), []fulltext.Document{{
		ID:        "one",
		URL:       "https://example.org",
		Text:      "northern lights",
		Timestamp: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}, nil); err != nil {
		t.Fatal(err)
	}
	handler := New(idx, nil, "")
	for _, tc := range []struct {
		path     string
		code     int
		contains string
	}{
		{"/search/status", 200, `{"enabled":true}`},
		{"/search?q=northern", 200, "https://example.org"},
		{"/search?q=northern&limit=101", 400, "invalid"},
		{"/search?q=northern&offset=no", 400, "invalid"},
		{"/search?q=northern&from=yesterday", 400, "RFC3339"},
		{"/search?q=northern&offset=9999&limit=100", 400, "invalid"},
		{"/", 200, "CAPTURE SIGNAL"},
		{"/console.js", 200, "textContent"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil))
			if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing CSP")
			}
		})
	}
}
