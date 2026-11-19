// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipnum

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// meta is the cluster.json schema. Field order also determines encoded JSON order.
//
//nolint:tagliatelle // Preserve the published zipnum metadata format.
type meta struct {
	CreatedAt     string   `json:"created_at"`
	Duration      string   `json:"duration"`
	Source        []string `json:"source"`
	LinesPerBlock int      `json:"lines_per_block"`
	BlocksPerPart int      `json:"blocks_per_part"`
}

// readMeta reads strict metadata and reports read errors before close errors.
func readMeta(path string) (m meta, err error) {
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	m, err = decodeMetadata(f)
	if closeErr := closeIO(f); err == nil {
		err = closeErr
	}
	return m, err
}

// writeMetadata exclusively creates and syncs a cluster metadata file.
func writeMetadata(m meta, path string) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(m)
	if err == nil {
		err = syncFile(f)
	}
	if closeErr := closeIO(f); err == nil {
		err = closeErr
	}
	return err
}

// writeMetadata records the build layout, elapsed time, and source provenance.
func (z *zipnum) writeMetadata(start time.Time, sources []string) error {
	return writeMetadata(meta{
		CreatedAt:     start.UTC().Format(time.RFC3339),
		Duration:      time.Since(start).String(),
		Source:        sources,
		LinesPerBlock: z.linesPerBlock,
		BlocksPerPart: z.blocksPerPart,
	}, filepath.Join(z.output, metaFile))
}

// decodeMetadata rejects unknown fields and trailing JSON values.
func decodeMetadata(r io.Reader) (m meta, err error) {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&m)
	if err == nil {
		var trailing any
		if trailingErr := decoder.Decode(&trailing); trailingErr != io.EOF {
			if trailingErr == nil {
				err = fmt.Errorf("metadata contains multiple JSON values")
			} else {
				err = trailingErr
			}
		}
	}
	return m, err
}
