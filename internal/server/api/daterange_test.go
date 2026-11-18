/*
 * Copyright 2021 National Library of Norway.
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

package api

import (
	"math"
	"testing"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
)

type dateRangeTestData struct {
	name      string
	daterange *DateRange
	datestr   string
	expect    bool
}

func TestValidDateRangeContains(t *testing.T) {
	tests := []dateRangeTestData{
		{
			"date string in range returns true",
			&DateRange{
				from: 0,
				to:   60,
			},
			"19700101000010",
			true,
		},
		{
			"date string same as 'from' returns true",
			&DateRange{
				from: 0,
				to:   60,
			},
			"19700101000000",
			true,
		},
		{
			"date string same as 'to' returns true",
			&DateRange{
				from: 0,
				to:   60,
			},
			"19700101000100",
			true,
		},
		{
			"date string below range returns false",
			&DateRange{
				from: 59,
				to:   60,
			},
			"19700101000000",
			false,
		},
		{
			"date string above range returns false",
			&DateRange{
				from: 0,
				to:   1,
			},
			"19700101000200",
			false,
		},
		{
			"nil date range returns true",
			nil,
			"19700101000200",
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, err := timestamp.Parse(tt.datestr)
			if err != nil {
				t.Fatalf("Testing %s; Unexpected error: %s", tt.name, err)
			}
			contains := tt.daterange.Contains(ts.Unix())
			if contains != tt.expect {
				t.Errorf("Testing %s; Expected %t, got %t", tt.name, tt.expect, contains)
			}
		})
	}
}

func TestFromAndToParsing(t *testing.T) {
	tests := []struct {
		name         string
		fromAndTo    string
		expectedFrom int64
		expectedTo   int64
		expectError  bool
	}{
		{
			"valid full date string succeeds",
			"19700101000000",
			0,
			0,
			false,
		},
		{
			"missing seconds date string succeeds",
			"197001010000",
			0,
			59,
			false,
		},
		{
			"missing minutes date string succeeds",
			"1970010100",
			0,
			59*60 + 59,
			false,
		},
		{
			"missing hours date string succeeds",
			"19700101",
			0,
			23*60*60 + 60*59 + 59,
			false,
		},
		{
			"normal year date string succeeds",
			"197002",
			time.Date(1970, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
			time.Date(1970, 2, 28, 23, 59, 59, 0, time.UTC).Unix(),
			false,
		},
		{
			"leap year date string succeeds",
			"200002",
			time.Date(2000, 2, 1, 0, 0, 0, 0, time.UTC).Unix(),
			time.Date(2000, 2, 29, 23, 59, 59, 0, time.UTC).Unix(),
			false,
		},
		{
			"empty date string succeeds",
			"",
			time.Time{}.Unix(),
			math.MaxInt64,
			false,
		},
		{
			"odd number date string fails",
			"19975",
			0,
			0,
			true,
		},
		{
			"more than 14 number date string fails",
			"200801122359590",
			0,
			0,
			true,
		},
		{
			"invalid char date string fails",
			"199A",
			0,
			0,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, err := from(tt.fromAndTo)
			if err != nil && !tt.expectError {
				t.Errorf("Unexpected 'from' error: %s", err)
			}
			if from != tt.expectedFrom {
				t.Errorf("From expected %d, got %d", tt.expectedFrom, from)
			}
			to, err := to(tt.fromAndTo)
			if err != nil && !tt.expectError {
				t.Errorf("Unexpected 'to' error: %s", err)
			}
			if to != tt.expectedTo {
				t.Errorf("To expected %d, got %d", tt.expectedTo, to)
			}
		})
	}
}
