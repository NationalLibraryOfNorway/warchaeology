// Copyright 2021 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bufio"
	"fmt"
	"io"
	"net/http"

	"github.com/nationallibraryofnorway/gowarc/v4"
)

// RenderRecord renders gowarc.WarcRecord rec as a binary stream.
func RenderRecord(w http.ResponseWriter, rec gowarc.WarcRecord) (int64, error) {
	w.Header().Add("Content-Type", "application/octet-stream")

	marshaler := gowarc.NewMarshaler()

	var err error
	var written int64
	var n int64

	continuation := rec
	for continuation != nil {
		continuation, n, err = marshaler.Marshal(w, continuation, 0)
		written += n
		if err != nil {
			break
		}
	}
	return written, err
}

func RenderRedirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	// Write status line
	w.WriteHeader(http.StatusFound)
}

// RenderHTTP streams the archived entity after applying HTTP transfer decoding.
func RenderHTTP(w http.ResponseWriter, raw io.Reader) error {
	response, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	defer func() { _ = response.Body.Close() }()
	h := response.Header
	// Write headers
	for key, values := range h {
		for i, value := range values {
			if i == 0 {
				w.Header().Set(key, value)
			} else {
				w.Header().Add(key, value)
			}
		}
	}

	// Write status line
	w.WriteHeader(response.StatusCode)

	// Write HTTP payload
	_, err = io.Copy(w, response.Body)
	if err != nil {
		return fmt.Errorf("failed to write HTTP payload: %w", err)
	}

	return nil
}
