// Copyright 2022 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package filter

import (
	"slices"
	"strings"

	"github.com/nationallibraryofnorway/gowarc/v3"
)

type RecordFilter struct {
	ids         []string
	RecordTypes gowarc.RecordType
	hasStatus   bool
	fromStatus  int
	toStatus    int
	mime        []string
}

func New(options ...func(*RecordFilter)) *RecordFilter {
	f := &RecordFilter{}
	for _, option := range options {
		option(f)
	}
	return f
}

func WithCodeRange(from, to int) func(*RecordFilter) {
	return func(f *RecordFilter) {
		f.hasStatus = true
		f.fromStatus = from
		f.toStatus = to
	}
}

func WithRecordTypes(recordTypes gowarc.RecordType) func(*RecordFilter) {
	return func(f *RecordFilter) {
		f.RecordTypes = recordTypes
	}
}

func WithRecordIds(ids []string) func(*RecordFilter) {
	return func(f *RecordFilter) {
		for id := range ids {
			f.ids = append(f.ids, strings.Trim(ids[id], "<>"))
		}
	}
}
func WithMimeType(mimeTypes []string) func(*RecordFilter) {
	return func(f *RecordFilter) {
		f.mime = mimeTypes
	}
}

func (f *RecordFilter) Accept(wr gowarc.WarcRecord) bool {
	if !f.acceptIdentity(wr) {
		return false
	}

	// Check HTTP response code
	if v, ok := wr.Block().(gowarc.HttpResponseBlock); ok {
		if f.hasStatus && (v.HttpStatusCode() < f.fromStatus || v.HttpStatusCode() >= f.toStatus) {
			return false
		}
	}

	// Check document mime-type
	if len(f.mime) > 0 {
		switch v := wr.Block().(type) {
		case gowarc.HttpRequestBlock:
			if v.HttpHeader() == nil {
				return false
			}
			contentType := v.HttpHeader().Get(gowarc.ContentType)
			for _, r := range f.mime {
				if strings.Contains(contentType, r) {
					return true
				}
			}
			return false
		case gowarc.HttpResponseBlock:
			if v.HttpHeader() == nil {
				return false
			}
			contentType := v.HttpHeader().Get(gowarc.ContentType)
			for _, r := range f.mime {
				if strings.Contains(contentType, r) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}

	return true
}

func (f *RecordFilter) AcceptCapture(wr gowarc.WarcRecord, mime string, status int) bool {
	if !f.acceptIdentity(wr) {
		return false
	}
	if f.hasStatus && (status == 0 || status < f.fromStatus || status >= f.toStatus) {
		return false
	}
	for _, m := range f.mime {
		if strings.Contains(mime, m) {
			return true
		}
	}
	return len(f.mime) == 0
}

func (f *RecordFilter) acceptIdentity(wr gowarc.WarcRecord) bool {
	return (len(f.ids) == 0 || slices.Contains(f.ids, wr.RecordId())) && (f.RecordTypes == 0 || wr.Type()&f.RecordTypes != 0)
}
