// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcdigest"
)

func checkIdenticalProfile(record gowarc.WarcRecord) error {
	profile := record.WarcHeader().Get(gowarc.WarcProfile)
	if profile != gowarc.ProfileIdenticalPayloadDigestV1_0 && profile != gowarc.ProfileIdenticalPayloadDigestV1_1 {
		return fmt.Errorf("unsupported revisit profile %q", profile)
	}
	return nil
}

// ReconstructIdentical builds an independently owned record.
// The caller retains ownership of both inputs.
func ReconstructIdentical(record, donor gowarc.WarcRecord) (gowarc.WarcRecord, error) {
	if err := checkIdenticalProfile(record); err != nil {
		return nil, err
	}
	switch donor.Type() {
	case gowarc.Resource:
		return reconstructResource(record, donor)
	case gowarc.Response:
		if _, err := ResponseRevisitLength(record); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported donor type %s", donor.Type())
	}
	return record.Merge(donor)
}

// ResponseRevisitLength parses the capture-specific response headers and returns
// the entity length that Merge checks, or -1 when the length is unconstrained.
func ResponseRevisitLength(record gowarc.WarcRecord) (int64, error) {
	header, ok := record.Block().(gowarc.ProtocolHeaderBlock)
	if !ok || len(header.ProtocolHeaderBytes()) == 0 {
		return -1, errors.New("revisit lacks capture-specific HTTP headers")
	}
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header.ProtocolHeaderBytes())), nil)
	if err != nil {
		return -1, fmt.Errorf("invalid capture-specific HTTP headers: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return -1, errors.New("cannot reconstruct a 304 response with a payload")
	}
	if response.StatusCode < 200 || response.StatusCode == http.StatusNoContent || response.Header.Get("Content-Length") == "" {
		return -1, nil
	}
	return response.ContentLength, nil
}

// A resource can carry an HTTP message or a bare entity. Its revisit retains
// the capture's headers even when the donor used different framing.
func reconstructResource(record, donor gowarc.WarcRecord) (gowarc.WarcRecord, error) {
	builder := gowarc.NewRecordBuilder(donor.Type(), gowarc.WithVersion(record.Version()),
		gowarc.WithAddMissingDigest(true), gowarc.WithBlockErrorPolicy(gowarc.ErrFail))
	success := false
	defer func() {
		if !success {
			_ = builder.Close()
		}
	}()
	for _, h := range *record.WarcHeader() {
		switch strings.ToLower(h.Name) {
		case "warc-type", "content-length", "warc-block-digest", "warc-profile", "warc-refers-to", "warc-refers-to-target-uri", "warc-refers-to-date", "warc-truncated":
			continue
		}
		builder.AddWarcHeader(h.Name, h.Value)
	}
	if v := donor.WarcHeader().Get(gowarc.WarcTruncated); v != "" {
		builder.AddWarcHeader(gowarc.WarcTruncated, v)
	}
	framing, err := ParseResourceRevisitFraming(record)
	if err != nil {
		return nil, err
	}
	if _, err := builder.Write(framing.header); err != nil {
		return nil, err
	}
	var payload io.Reader
	if b, ok := donor.Block().(gowarc.PayloadBlock); ok {
		payload, err = b.PayloadBytes()
	} else {
		payload, err = donor.Block().RawBytes()
	}
	if err != nil {
		return nil, err
	}
	if framing.chunked {
		chunks := httputil.NewChunkedWriter(builder)
		if _, err := io.Copy(chunks, payload); err != nil {
			return nil, err
		}
		if err := chunks.Close(); err != nil {
			return nil, err
		}
		if _, err := builder.WriteString("\r\n"); err != nil {
			return nil, err
		}
	} else {
		n, err := builder.ReadFrom(payload)
		if err != nil {
			return nil, err
		}
		if framing.ContentLength >= 0 && n != framing.ContentLength {
			return nil, fmt.Errorf("resource revisit expects %d payload bytes, donor supplied %d", framing.ContentLength, n)
		}
	}
	result, _, err := builder.Build()
	if err != nil {
		return nil, err
	}
	success = true
	if block, ok := result.Block().(gowarc.HttpBlock); ok {
		label := record.WarcHeader().Get(gowarc.WarcPayloadDigest)
		if label != "" && !warcdigest.Equal(label, block.PayloadDigest()) {
			return nil, errors.Join(errors.New("resource revisit payload digest mismatch"), result.Close())
		}
	}
	return result, nil
}

// ResourceRevisitFraming describes the entity carried by a resource revisit.
type ResourceRevisitFraming struct {
	header        []byte
	chunked       bool
	ContentLength int64 // -1 when the capture does not specify an entity length.
}

func ParseResourceRevisitFraming(record gowarc.WarcRecord) (ResourceRevisitFraming, error) {
	framing := ResourceRevisitFraming{ContentLength: -1}
	if b, ok := record.Block().(gowarc.ProtocolHeaderBlock); ok {
		framing.header = b.ProtocolHeaderBytes()
	}
	mediaType, _, _ := strings.Cut(record.WarcHeader().Get(gowarc.ContentType), ";")
	if !strings.EqualFold(strings.TrimSpace(mediaType), gowarc.ApplicationHttp) {
		if len(framing.header) != 0 {
			return framing, errors.New("unexpected protocol headers on a non-HTTP resource revisit")
		}
		return framing, nil
	}
	r := bufio.NewReader(bytes.NewReader(framing.header))
	if bytes.HasPrefix(framing.header, []byte("HTTP/")) {
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			return framing, err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotModified {
			return framing, errors.New("cannot reconstruct a 304 response with a payload")
		}
		framing.chunked = len(resp.TransferEncoding) > 0 && resp.Body != http.NoBody
		framing.ContentLength = resp.ContentLength
	} else {
		req, err := http.ReadRequest(r)
		if err != nil {
			return framing, err
		}
		defer req.Body.Close()
		framing.chunked = len(req.TransferEncoding) > 0
		if req.Header.Get("Content-Length") != "" {
			framing.ContentLength = req.ContentLength
		}
	}
	return framing, nil
}
