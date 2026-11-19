// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package storagecodec

import (
	"testing"
	"time"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestLossless(t *testing.T) {
	for _, id := range []string{"urn:uuid:ce151eae-2bb0-41a7-a1b5-a984d5e4fa70", "<urn:uuid:ce151eae-2bb0-41a7-a1b5-a984d5e4fa70>", "https://records.example/a", "urn:example:other"} {
		for _, digest := range []string{"sha1:AAECAw==", "sha1:000102030405060708090a0b0c0d0e0f10111213", "ABCDEF234567", "sha256:ABCDEF", "custom:a"} {
			c := &cdx.Capture{
				Rid:       id,
				Dig:       digest,
				Ssu:       "com,example)/",
				Uri:       "https://example.com/",
				Sts:       timestamppb.New(time.Unix(1234567890, 123456789)),
				Offset:    1 << 40,
				Rle:       1 << 50,
				Source:    "/source",
				Ref:       "warcfile:/source#1099511627776",
				ExtraJson: []byte(`{"number":9007199254740993}`),
			}
			b, e := Encode(c)
			if e != nil {
				t.Fatal(e)
			}
			got, e := Decode(b, c.GetSsu())
			if e != nil || !proto.Equal(c, got) {
				t.Fatalf("%v %v", got, e)
			}
		}
	}
}
func TestRejectCorruption(t *testing.T) {
	for _, b := range [][]byte{{}, {8, 2}, {8, 1, 18, 255}, {8, 1, 26, 1, 255}} {
		if _, e := Decode(b, ""); e == nil {
			t.Fatalf("accepted %x", b)
		}
	}
}
