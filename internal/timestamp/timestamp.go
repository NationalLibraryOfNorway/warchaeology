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

package timestamp

import (
	"fmt"
	"time"
)

const cdxLayout = "20060102150405"

func TimeTo14(t time.Time) string {
	return t.Format(cdxLayout)
}

func To14(s string) (string, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", err
	}

	return t.Format(cdxLayout), nil
}

func Parse(s string) (time.Time, error) {
	l := len(s)
	if l > 14 {
		return time.Time{}, fmt.Errorf("invalid CDX timestamp: %s", s)
	}
	if l%2 != 0 {
		return time.Time{}, fmt.Errorf("invalid CDX timestamp: %s", s)
	}
	if l < 4 {
		return time.Time{}, fmt.Errorf("invalid CDX timestamp: %s", s)
	}

	return time.Parse(cdxLayout[:l], s)
}

// absInt64 returns the absolute value of an int64
//
// Source: https://github.com/cavaliercoder/go-abs
func absInt64(n int64) int64 {
	y := n >> 63
	return (n ^ y) - y
}

// CompareClosest compares two timestamps and returns true if ts1 is closer to closest than ts2
func CompareClosest(closest int64) func(int64, int64) bool {
	return func(ts1 int64, ts2 int64) bool {
		return absInt64(closest-ts1) < absInt64(closest-ts2)
	}
}

// CompareAsc compares two timestamps and returns true if a is less than or equal to b
func CompareAsc(a int64, b int64) bool {
	return a <= b
}

// CompareDesc compares two timestamps and returns true if a is greater than b
func CompareDesc(a int64, b int64) bool {
	return a > b
}

// Distance returns the absolute difference across the full CDX date range.
func Distance(a, b time.Time) (seconds int64, nanos int) {
	if a.Before(b) {
		a, b = b, a
	}
	seconds, nanos = a.Unix()-b.Unix(), a.Nanosecond()-b.Nanosecond()
	if nanos < 0 {
		seconds--
		nanos += 1_000_000_000
	}
	return
}
