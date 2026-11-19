// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package keyvalue

// This file contains implementations of some of
// the interfaces from the index package.

import (
	"context"
	"encoding/json"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"google.golang.org/protobuf/encoding/protojson"
)

// IdResponse implements the index.IdResponse interface.
type IdResponse struct {
	Key   string
	Value string
	Error error
}

func (ir IdResponse) GetId() string {
	return ir.Key
}

func (ir IdResponse) GetValue() string {
	return ir.Value
}

func (ir IdResponse) GetError() error {
	return ir.Error
}

// Assert IdResponse implements the index.IdResponse interface.
var _ index.IdResponse = IdResponse{}

// CdxResponse implements the index.CdxResponse interface.
type CdxResponse struct {
	Key   CdxKey
	Value *schema.Cdx
	Error error
}

func (cr CdxResponse) GetKey() CdxKey {
	return cr.Key
}

func (cr CdxResponse) GetCdx() *schema.Cdx {
	return cr.Value
}

func (cr CdxResponse) GetError() error {
	return cr.Error
}

func (cr CdxResponse) MarshalJSON() ([]byte, error) {
	res := &struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value,omitempty"`
		Error string          `json:"error,omitempty"`
	}{
		Key: cr.Key.String(),
	}
	if cr.Value != nil {
		cdx, err := protojson.Marshal(cr.Value)
		if err != nil {
			return nil, err
		}
		res.Value = cdx
	}

	if cr.Error != nil {
		res.Error = cr.Error.Error()
	}
	return json.Marshal(res)
}

// Assert CdxResponse implements the index.CdxResponse interface.
var _ index.CdxResponse = CdxResponse{}

// FileInfoResponse implements the index.FileInfoResponse interface.
type FileInfoResponse struct {
	FileInfo *schema.FileInfo
	Error    error
}

func (fir FileInfoResponse) GetFileInfo() *schema.FileInfo {
	return fir.FileInfo
}

func (fir FileInfoResponse) GetError() error {
	return fir.Error
}

// Assert FileInfoResponse implements the index.FileInfoResponse interface.
var _ index.FileInfoResponse = FileInfoResponse{}

// ReportResponse implements the index.ReportResponse interface.
type ReportResponse struct {
	Value *schema.Report
	Error error
}

func (rr ReportResponse) GetReport() *schema.Report {
	return rr.Value
}

func (rr ReportResponse) GetError() error {
	return rr.Error
}

// Assert ReportResponse implements the index.ReportResponse interface.
var _ index.ReportResponse = ReportResponse{}

type DebugRequest struct {
	index.Request

	Key string
}

type DebugAPI interface {
	Debug(context.Context, DebugRequest, chan<- CdxResponse) error
}

type Deleter interface {
	Delete(context.Context) error
}
