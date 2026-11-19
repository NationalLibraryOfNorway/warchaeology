// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warcserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"google.golang.org/protobuf/types/known/timestamppb"

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

func TestRevisitOffsetSelectsUTCCapture(t *testing.T) {
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Dir: t.TempDir(), Format: "badger"})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, db)
	const uri = "https://example.org/"
	for _, date := range []string{"2019-12-31T23:30:00Z", "2020-01-01T00:30:00Z"} {
		instant, err := time.Parse(time.RFC3339, date)
		if err != nil {
			t.Fatal(err)
		}
		c := &cdx.Capture{Uri: uri, Ssu: cdx.CanonicalKey(uri), Sts: timestamppb.New(instant), Ref: date}
		if err := db.WriteCapture(t.Context(), c); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ref, err := (Handler{Index: db}).resolveRevisit(ctx, uri, "2020-01-01T00:30:00+01:00")
	if err != nil || ref != "2019-12-31T23:30:00Z" {
		t.Fatalf("resolved capture %q, %v", ref, err)
	}
}
