// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestRenderHTTP(t *testing.T) {
	for _, tc := range []struct {
		name          string
		chunked, gzip bool
	}{{"fixed", false, false}, {"chunked", true, false}, {"compressed chunks", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("hello world")
			var raw bytes.Buffer
			raw.WriteString("HTTP/1.1 201 Created\r\n")
			if tc.gzip {
				var compressed bytes.Buffer
				z := gzip.NewWriter(&compressed)
				if _, err := z.Write(payload); err != nil {
					t.Fatal(err)
				}
				if err := z.Close(); err != nil {
					t.Fatal(err)
				}
				payload = compressed.Bytes()
				raw.WriteString("Content-Encoding: gzip\r\n")
			}
			if tc.chunked {
				raw.WriteString("Transfer-Encoding: chunked\r\n\r\n")
				for _, chunk := range [][]byte{payload[:3], payload[3:]} {
					fmt.Fprintf(&raw, "%x\r\n", len(chunk))
					raw.Write(chunk)
					raw.WriteString("\r\n")
				}
				raw.WriteString("0\r\n\r\n")
			} else {
				fmt.Fprintf(&raw, "Content-Length: %d\r\n\r\n", len(payload))
				raw.Write(payload)
			}
			w := httptest.NewRecorder()
			if err := RenderHTTP(w, &raw); err != nil {
				t.Fatal(err)
			}
			if w.Code != 201 || !bytes.Equal(w.Body.Bytes(), payload) || (w.Header().Get("Content-Encoding") == "gzip") != tc.gzip {
				t.Fatalf("replay %d %v %q", w.Code, w.Header(), w.Body.Bytes())
			}
		})
	}
}
