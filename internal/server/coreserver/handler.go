/*
 * Copyright 2020 National Library of Norway.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package coreserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"time"

	"github.com/julienschmidt/httprouter"
	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/keyvalue"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/api"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/handlers"
	"google.golang.org/protobuf/encoding/protojson"
)

var lf = []byte("\n")

type Handler struct {
	Index    index.API
	Loader   loader.API
	DebugAPI keyvalue.DebugAPI
}

func (h Handler) Routes() []api.Route {
	return []api.Route{
		{Method: http.MethodGet, Path: "/id", Handler: h.listIds},
		{Method: http.MethodGet, Path: "/id/:urn", Handler: h.getStorageRefByURN},
		{Method: http.MethodGet, Path: "/file", Handler: h.listFiles},
		{Method: http.MethodGet, Path: "/file/:filename", Handler: h.getFileInfoByFilename},
		{Method: http.MethodGet, Path: "/cdx", Handler: h.search},
		{Method: http.MethodGet, Path: "/record/:urn", Handler: h.loadRecordByUrn},
		{Method: http.MethodGet, Path: "/debug", Handler: h.debug},
		{Method: http.MethodPost, Path: "/report", Handler: h.createReport},
		{Method: http.MethodDelete, Path: "/report/:id", Handler: h.deleteReport},
		{Method: http.MethodGet, Path: "/report/:id", Handler: h.getReport},
		{Method: http.MethodGet, Path: "/report", Handler: h.listReports},
	}
}

func (h Handler) debug(w http.ResponseWriter, r *http.Request) {
	if h.DebugAPI == nil {
		http.Error(w, "Debug API not implemented", http.StatusNotImplemented)
		return
	}

	coreAPI, err := api.Parse(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := keyvalue.DebugRequest{
		Key:     r.URL.Query().Get("key"),
		Request: coreAPI,
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	response := make(chan keyvalue.CdxResponse)

	if err := h.DebugAPI.Debug(ctx, req, response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	start := time.Now()
	count := 0
	defer func() {
		slog.Debug("Found items", "count", count, "duration", time.Since(start))
	}()

	enc := json.NewEncoder(w)
	for res := range response {
		err = enc.Encode(res)
		if err != nil {
			slog.Warn("Failed to marshal result", "error", err)
			continue
		}
		count++
	}
}

func (h Handler) search(w http.ResponseWriter, r *http.Request) {
	coreAPI, err := api.Parse(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	start := time.Now()
	count := 0
	defer func() {
		slog.Debug("Search", "count", count, "duration", time.Since(start), "request", fmt.Sprintf("%+v", coreAPI))
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	response := make(chan index.CdxResponse)

	if err = h.Index.Search(ctx, coreAPI, response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Search failed", "request", coreAPI, "error", err)
		return
	}

	for res := range response {
		if res.GetError() != nil {
			slog.Warn("Failed result", "error", res.GetError())
			continue
		}
		v, err := protojson.Marshal(res.GetCdx())
		if err != nil {
			slog.Warn("Failed to marshal result", "error", err)
			continue
		}
		_, err = io.Copy(w, bytes.NewReader(v))
		if err != nil {
			slog.Warn("Failed to write result", "error", err)
			return
		}
		_, _ = w.Write(lf)
		count++
	}
}

type storageRef struct {
	Id       string `json:"id"`
	Filename string `json:"filename"`
	Offset   int64  `json:"offset"`
}

func (h Handler) listIds(w http.ResponseWriter, r *http.Request) {
	coreAPI, err := api.Parse(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	response := make(chan index.IdResponse)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	if err := h.Index.ListStorageRef(ctx, coreAPI, response); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to list ids", "request", coreAPI, "error", err)
		return
	}
	start := time.Now()
	count := 0
	defer func() {
		slog.Debug("Found items", "count", count, "duration", time.Since(start))
	}()

	for res := range response {
		if res.GetError() != nil {
			slog.Warn("Failed result", "error", res.GetError())
			continue
		}
		filename, offset, err := index.ParseStorageRef(res.GetValue())
		if err != nil {
			slog.Warn("Failed to parse storage ref", "error", err, "storageRef", res.GetValue())
			continue
		}
		ref := &storageRef{
			Id:       res.GetId(),
			Filename: filename,
			Offset:   offset,
		}
		v, err := json.Marshal(ref)
		if err != nil {
			slog.Warn("Failed to marshal storage ref", "error", err, "storageRef", ref)
			continue
		}
		_, err = io.Copy(w, bytes.NewReader(v))
		if err != nil {
			slog.Warn("Failed to write storage ref", "error", err, "storageRef", ref)
			return
		}
		_, _ = w.Write(lf)
		count++
	}
}

func (h Handler) getStorageRefByURN(w http.ResponseWriter, r *http.Request) {
	params := httprouter.ParamsFromContext(r.Context())
	urn := params.ByName("urn")

	storageRef, err := h.Loader.StorageRefById(r.Context(), urn)
	if err != nil {
		httpErr := fmt.Sprintf("failed to resolve storage ref: %s: %v", urn, err)
		http.Error(w, httpErr, http.StatusInternalServerError)
		slog.Error("Failed to resolve storage ref", "urn", urn, "error", err)
		return
	}
	_, err = fmt.Fprintln(w, storageRef)
	if err != nil {
		slog.Warn("Failed to write storage ref", "error", err, "storageRef", storageRef)
	}
}

//nolint:dupl // Typed streams have separate wire formats and backend methods.
func (h Handler) listFiles(w http.ResponseWriter, r *http.Request) {
	coreAPI, err := api.Parse(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	responses := make(chan index.FileInfoResponse)

	if err := h.Index.ListFileInfo(ctx, coreAPI, responses); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to list files", "request", coreAPI, "error", err)
		return
	}

	start := time.Now()
	count := 0
	defer func() {
		slog.Debug("Found items", "count", count, "duration", time.Since(start))
	}()

	for res := range responses {
		if res.GetError() != nil {
			slog.Warn("Failed result", "error", res.GetError())
			continue
		}
		v, err := protojson.Marshal(res.GetFileInfo())
		if err != nil {
			slog.Warn("Failed to marshal file info", "error", err)
			continue
		}
		_, err = io.Copy(w, bytes.NewReader(v))
		if err != nil {
			slog.Warn("Failed to write file info", "error", err)
			return
		}
		_, _ = w.Write(lf)
		count++
	}
}

func (h Handler) getFileInfoByFilename(w http.ResponseWriter, r *http.Request) {
	params := httprouter.ParamsFromContext(r.Context())
	filename := params.ByName("filename")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	fileInfo, err := h.Index.GetFileInfo(ctx, filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to get file info", "filename", filename, "error", err)
		return
	}
	b, err := protojson.Marshal(fileInfo)
	if err != nil {
		slog.Warn("Failed to marshal file info", "error", err, "filename", filename, "fileInfo", fileInfo)
		return
	}
	_, err = io.Copy(w, bytes.NewReader(b))
	if err != nil {
		slog.Warn("Failed to write file info", "error", err)
	}
	_, _ = w.Write(lf)
}

func (h Handler) loadRecordByUrn(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	params := httprouter.ParamsFromContext(ctx)
	warcId := params.ByName("urn")

	storageRef, err := h.Index.GetStorageRef(ctx, warcId)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to get storage ref", "warcId", warcId, "error", err)
		return
	}

	record, err := h.Loader.RecordByStorageRef(ctx, storageRef)
	if record != nil {
		defer func() {
			if err := record.Close(); err != nil {
				slog.Warn("Failed to close replay source", "error", err)
			}
		}()
	}
	if err != nil {
		err := fmt.Errorf("failed to load record '%s': %w", warcId, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to load record", "urn", warcId, "error", err)
		return
	}
	if record == nil {
		http.NotFound(w, r)
		return
	}
	_, err = handlers.RenderRecord(w, record)
	if err != nil {
		slog.Warn("Failed to write record", "error", err, "urn", warcId)
	}
}

func (h Handler) createReport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	coreAPI, err := api.Parse(r.Form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	report, err := h.Index.CreateReport(ctx, coreAPI)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to generate report", "request", coreAPI, "error", err)
		return
	}
	b, err := protojson.Marshal(report)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to marshal report", "error", err)
	}

	w.WriteHeader(http.StatusAccepted)
	_, err = io.Copy(w, bytes.NewReader(b))
	if err != nil {
		slog.Warn("Failed to write report", "error", err)
		return
	}
	_, _ = w.Write(lf)
}

func (h Handler) getReport(w http.ResponseWriter, r *http.Request) {
	params := httprouter.ParamsFromContext(r.Context())
	id := params.ByName("id")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	report, err := h.Index.GetReport(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to get report", "id", id, "error", err)
		return
	}
	if report == nil {
		http.NotFound(w, r)
		return
	}

	b, err := protojson.Marshal(report)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to marshal report", "error", err)
	}
	_, err = io.Copy(w, bytes.NewReader(b))
	if err != nil {
		slog.Warn("Failed to write report", "error", err)
		return
	}
	_, _ = w.Write(lf)
}

//nolint:dupl // Typed streams have separate wire formats and backend methods.
func (h Handler) listReports(w http.ResponseWriter, r *http.Request) {
	coreAPI, err := api.Parse(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	responses := make(chan index.ReportResponse)

	if err := h.Index.ListReports(ctx, coreAPI, responses); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to list reports", "request", coreAPI, "error", err)
		return
	}

	start := time.Now()
	count := 0
	defer func() {
		slog.Debug("Found items", "count", count, "duration", time.Since(start))
	}()

	for res := range responses {
		if res.GetError() != nil {
			slog.Warn("Failed report result", "error", res.GetError())
			continue
		}
		v, err := protojson.Marshal(res.GetReport())
		if err != nil {
			slog.Warn("Failed to marshal report", "error", err)
			continue
		}
		_, err = io.Copy(w, bytes.NewReader(v))
		if err != nil {
			slog.Warn("Failed to write report", "error", err)
			return
		}
		_, _ = w.Write(lf)
		count++
	}
}

func (h Handler) deleteReport(w http.ResponseWriter, r *http.Request) {
	params := httprouter.ParamsFromContext(r.Context())
	id := params.ByName("id")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	err := h.Index.DeleteReport(ctx, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		slog.Error("Failed to delete report", "id", id, "error", err)
		return
	}
}
