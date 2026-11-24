// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fulltext

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestExtractBrowsertrixResource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "text.warc")
	payload := "Senja blåbær og nordlys"
	raw := fmt.Sprintf("WARC/1.1\r\nWARC-Type: resource\r\nWARC-Record-ID: <urn:uuid:1789da6c-6444-4f9a-b5ca-cb591e39713a>\r\nWARC-Date: 2026-01-01T20:15:59Z\r\nWARC-Target-URI: urn:text:https://example.org/\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s\r\n\r\n", len(payload), payload)
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	c := &cdx.Capture{
		Uri:    "urn:text:https://example.org/",
		Mct:    "text/plain",
		Ref:    "warcfile:" + path + "#0",
		Source: path,
		Sts:    timestamppb.New(time.Now()),
	}
	doc, err := Extract(context.Background(), loader.FileStorageLoader{}, c, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Text != payload || doc.URL != "https://example.org/" || doc.Host != "example.org" {
		t.Fatalf("unexpected document: %+v", doc)
	}
	if _, err := Extract(context.Background(), loader.FileStorageLoader{}, c, 4); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected bounded extraction error, got %v", err)
	}
}
