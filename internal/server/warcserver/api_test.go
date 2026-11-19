// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/julienschmidt/httprouter"
)

func TestParseResourceRequest(t *testing.T) {
	tests := []struct {
		name    string        // test case name
		uri     string        // expected uri
		closest string        // expected timestamp
		request *http.Request // test request
	}{
		{
			name:    "Query parameters are not sorted during parsing",
			uri:     "http://example.com?d=4&a=1&c=3&b=2#hei",
			closest: "20210101000000",
			request: func() *http.Request {
				r, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://example.com", nil)

				ctx := context.WithValue(context.Background(), httprouter.ParamsKey, httprouter.Params{
					httprouter.Param{
						Key:   "url",
						Value: "/http://example.com?d=4&a=1&c=3&b=2#hei",
					},
					httprouter.Param{
						Key:   "timestamp",
						Value: "20210101000000id_",
					},
				})

				return r.WithContext(ctx)
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uri, closest := parseResourceRequest(tt.request)
			if uri != tt.uri {
				t.Errorf("got %s, want %s", uri, tt.uri)
			}
			if closest != tt.closest {
				t.Errorf("got %s, want %s", closest, tt.closest)
			}
		})
	}
}
