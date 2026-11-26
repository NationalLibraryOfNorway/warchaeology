// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
)

// One report per WARC or CDXJ member. Source is a locator including its mount chain.
type longKeyReport struct {
	source, position, policy string
	skipped, maxBytes        int
	examples                 []string
	logger                   *slog.Logger
}

func (r *longKeyReport) check(key string, position int64) (bool, error) {
	if len(key) <= cdxstore.InlineURLKeyLimit || (r.policy != "skip" && r.policy != "error") {
		return false, nil
	}
	r.maxBytes = max(r.maxBytes, len(key))
	if r.policy == "skip" {
		r.skipped++
		if len(r.examples) == 3 {
			return true, nil
		}
	}
	preview := key[:min(96, len(key))]
	if end := strings.IndexAny(preview, "?#"); end >= 0 {
		preview = preview[:end]
	}
	sample := fmt.Sprintf("%s=%d bytes=%d preview=%s", r.position, position, len(key), strconv.QuoteToASCII(preview))
	if r.policy == "error" {
		return false, fmt.Errorf("URL key exceeds %d bytes (%s)", cdxstore.InlineURLKeyLimit, sample)
	}
	r.examples = append(r.examples, sample)
	if r.skipped == 1 {
		var loc filewalker.Source
		if err := json.Unmarshal([]byte(r.source), &loc); err != nil {
			r.logger = slog.With("source", r.source)
		} else if len(loc.Roots) == 0 {
			r.logger = slog.With("path", loc.Path)
		} else {
			r.logger = slog.With("archive", loc.Roots[0], "member", loc.Path)
			if len(loc.Roots) > 1 {
				r.logger = r.logger.With("nested", loc.Roots[1:])
			}
		}
		r.logger.Warn("Skipping capture with oversized URL key", r.position, position, "bytes", len(key), "preview", preview)
	}
	return true, nil
}

func (r *longKeyReport) summary() {
	if r.skipped > 0 {
		r.logger.Warn("Skipped captures with oversized URL keys", "skipped", r.skipped, "maxBytes", r.maxBytes, "examples", r.examples)
	}
}
