// Copyright 2020 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package timestamp_test

import (
	"testing"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

func TestUTCFormats(t *testing.T) {
	for _, raw := range []string{"2019-12-31T23:30:00.123456789Z", "2020-01-01T00:30:00.123456789+01:00", "2019-12-31T18:30:00.123456789-05:00"} {
		instant, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := timestamp.Format14(instant); got != "20191231233000" {
			t.Errorf("%s formatted as %s", raw, got)
		}
		if got := timestamp.FormatRFC3339(instant); got != "2019-12-31T23:30:00.123456789Z" {
			t.Errorf("%s formatted as %s", raw, got)
		}
		key := timestamp.FormatNano(instant)
		if key != "20191231233000.123456789" || key >= timestamp.FormatNano(instant.Add(time.Nanosecond)) {
			t.Errorf("incorrect ordering key %s", key)
		}
		if got, err := timestamp.ParseKey(key); err != nil || !got.Equal(instant) {
			t.Errorf("key round trip = %v, %v", got, err)
		}
	}
}

func TestParsePrecision(t *testing.T) {
	const full = "20240229135507"
	want := time.Date(2024, 2, 29, 13, 55, 7, 0, time.UTC)
	if got, err := timestamp.Parse14(full); err != nil || got != want {
		t.Fatalf("Parse14 = %v, %v", got, err)
	}
	for n := 4; n <= 14; n += 2 {
		s := full[:n]
		got, err := timestamp.ParsePartial(s)
		if err != nil || got.Location() != time.UTC || timestamp.Format14(got)[:n] != s {
			t.Errorf("ParsePartial(%q) = %v, %v", s, got, err)
		}
		if n < 14 {
			if _, err := timestamp.Parse14(s); err == nil {
				t.Errorf("Parse14 accepted %q", s)
			}
		}
	}
	for _, s := range []string{"", "202", "20240", "202313", "20230229", "20241231240000", full + ".123"} {
		if _, err := timestamp.ParsePartial(s); err == nil {
			t.Errorf("ParsePartial accepted %q", s)
		}
		if _, err := timestamp.Parse14(s); err == nil {
			t.Errorf("Parse14 accepted %q", s)
		}
	}
}
