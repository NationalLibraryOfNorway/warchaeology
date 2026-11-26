// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcdigest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"

	"github.com/nationallibraryofnorway/gowarc/v4"
)

// RecordFingerprint hashes the WARC version, headers and complete raw block.
func RecordFingerprint(wr gowarc.WarcRecord) (string, error) {
	h := sha256.New()
	if _, err := fmt.Fprintln(h, wr.Version().String()); err != nil {
		return "", err
	}
	data, err := json.Marshal(wr.WarcHeader())
	if err != nil {
		return "", err
	}
	if _, err = h.Write(data); err != nil {
		return "", err
	}
	raw, err := wr.Block().RawBytes()
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(h, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
