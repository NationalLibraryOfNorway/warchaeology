// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/nationallibraryofnorway/gowarc/v3"
)

func checkIdenticalProfile(record gowarc.WarcRecord) error {
	profile := record.WarcHeader().Get(gowarc.WarcProfile)
	if profile != gowarc.ProfileIdenticalPayloadDigestV1_0 && profile != gowarc.ProfileIdenticalPayloadDigestV1_1 {
		return fmt.Errorf("unsupported revisit profile %q", profile)
	}
	return nil
}

// ReconstructIdentical builds an independently owned record. Merge aliases the
// donor's block and digest, which is unsuitable for writing a reconstructed WARC.
// The caller retains ownership of both inputs.
func ReconstructIdentical(record, donor gowarc.WarcRecord) (gowarc.WarcRecord, error) {
	if err := checkIdenticalProfile(record); err != nil {
		return nil, err
	}
	if donor.Type() != gowarc.Response && donor.Type() != gowarc.Resource {
		return nil, fmt.Errorf("unsupported donor type %s", donor.Type())
	}
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
	if donor.Type() == gowarc.Response {
		header, ok := record.Block().(gowarc.ProtocolHeaderBlock)
		if !ok || len(header.ProtocolHeaderBytes()) == 0 {
			return nil, fmt.Errorf("revisit lacks capture-specific HTTP headers")
		}
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(header.ProtocolHeaderBytes())), nil)
		if err != nil {
			return nil, fmt.Errorf("invalid capture-specific HTTP headers: %w", err)
		}
		_ = response.Body.Close()
		if response.StatusCode == http.StatusNotModified {
			return nil, fmt.Errorf("cannot reconstruct a 304 response with a payload")
		}

		if _, err := builder.Write(header.ProtocolHeaderBytes()); err != nil {
			return nil, err
		}
	}
	var payload io.Reader
	var err error
	if block, ok := donor.Block().(gowarc.PayloadBlock); ok {
		payload, err = block.PayloadBytes()
	} else {
		payload, err = donor.Block().RawBytes()
	}
	if err != nil {
		return nil, err
	}
	if _, err = builder.ReadFrom(payload); err != nil {
		return nil, err
	}
	result, _, err := builder.Build()
	if err != nil {
		return nil, err
	}
	success = true
	return result, nil
}
