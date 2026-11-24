// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fulltext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/nationallibraryofnorway/gowarc/v3"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
)

var errNoTextResource = errors.New("capture does not contain a text resource")

func plainText(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "text/plain"
}

// Extract reads only an indexed text resource. Bounds apply to decoded text,
// independently of compressed WARC size; oversized text is never silently cut.
func Extract(ctx context.Context, records loader.RecordLoader, c *cdx.Capture, maxBytes int64) (_ *Document, resultErr error) {
	if c == nil || !plainText(c.GetMct()) {
		return nil, errNoTextResource
	}
	if c.GetRef() == "" || (c.GetSrt() != "" && c.GetSrt() != "resource") {
		return nil, errNoTextResource
	}
	original, browsertrix := strings.CutPrefix(c.GetUri(), "urn:text:")
	parsed, err := url.Parse(original)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		if browsertrix {
			return nil, errors.New("text resource has no absolute HTTP original URL")
		}
		return nil, errNoTextResource
	}
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	rec, err := records.RecordByStorageRef(ctx, c.GetRef())
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rec.Close()) }()
	if rec.Type() != gowarc.Resource {
		return nil, errNoTextResource
	}
	body, err := rec.Block().RawBytes()
	if err != nil {
		return nil, err
	}
	text, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(text)) > maxBytes {
		return nil, fmt.Errorf("text resource exceeds %d decoded bytes", maxBytes)
	}
	if !utf8.Valid(text) {
		return nil, errors.New("text resource is not UTF-8")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(c.GetRef()))
	return &Document{
		ID:         hex.EncodeToString(sum[:]),
		URL:        original,
		Host:       strings.ToLower(parsed.Hostname()),
		Text:       string(text),
		Timestamp:  c.GetSts().AsTime(),
		Source:     c.GetSource(),
		Generation: c.GetGeneration(),
		CaptureURI: c.GetUri(),
		CaptureKey: c.GetSsu(),
		Ref:        c.GetRef(),
		Snippet:    snippet(string(text)),
	}, nil
}

func snippet(text string) string {
	r := []rune(strings.Join(strings.Fields(text), " "))
	if len(r) > 320 {
		return string(r[:320]) + "…"
	}
	return string(r)
}
