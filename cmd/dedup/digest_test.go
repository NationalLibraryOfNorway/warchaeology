// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package dedup

import (
	"testing"

	"github.com/nationallibraryofnorway/gowarc/v4"
)

func TestVerifiedPayloadKey(t *testing.T) {
	makeRecord := func(kind gowarc.RecordType, data string) gowarc.WarcRecord {
		t.Helper()
		b := gowarc.NewRecordBuilder(kind, gowarc.WithAddMissingDigest(true))
		if kind == gowarc.Response {
			b.AddWarcHeader(gowarc.ContentType, "application/http;msgtype=response")
		}
		if _, err := b.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
		r, _, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close() })
		return r
	}
	msg := "HTTP/1.1 200 OK\r\nX-Test: one\r\n\r\nbody"
	a := makeRecord(gowarc.Response, msg)
	b := makeRecord(gowarc.Response, "HTTP/1.1 200 OK\r\n\r\n"+msg)
	if a.WarcHeader().Get(gowarc.WarcBlockDigest) != b.WarcHeader().Get(gowarc.WarcPayloadDigest) {
		t.Fatal("invalid cross-scope fixture")
	}
	key, err := getDigest(a)
	if err != nil || key == "" {
		t.Fatalf("valid response: %s %v", key, err)
	}
	other := makeRecord(gowarc.Response, "HTTP/1.1 200 OK\r\nX-Test: two\r\n\r\nbody")
	if k, e := getDigest(other); e != nil || k != key {
		t.Fatal(k, e)
	}
	res := makeRecord(gowarc.Resource, "body")
	res.WarcHeader().Delete(gowarc.WarcPayloadDigest)
	if k, e := getDigest(res); e != nil || k == "" || k == key {
		t.Fatal(k, e)
	}
	b.WarcHeader().Set(gowarc.WarcPayloadDigest, a.WarcHeader().Get(gowarc.WarcPayloadDigest))
	if k, e := getDigest(b); k != "" || e == nil {
		t.Fatal("stale digest accepted", k, e)
	}
	a.WarcHeader().Delete(gowarc.WarcPayloadDigest)
	if k, e := getDigest(a); k != "" || e != nil {
		t.Fatal("block-only HTTP donor", k, e)
	}
}
