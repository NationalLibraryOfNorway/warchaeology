// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"testing"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
)

func TestParse(t *testing.T) {
	tests := []struct {
		query url.Values
		want  *SearchRequest
		err   error
	}{
		// invalid matchType should error
		{
			query: map[string][]string{
				"url":       {"http://example.test/01"},
				"matchType": {"not a matchtype"},
			},
			want: &SearchRequest{},
			err:  errors.New("invalid matchtype"),
		},
		// invalid sort should error
		{
			query: map[string][]string{
				"url":  {"http://example.test/02"},
				"sort": {"not a sort"},
			},
			want: &SearchRequest{},
			err:  errors.New("invalid sort"),
		},
		// invalid URL should error
		{
			query: map[string][]string{
				"url": {"htttp://\\example02"},
			},
			want: &SearchRequest{},
			err:  errors.New("invalid url"),
		},
		// limit should be set
		{
			query: map[string][]string{
				"url":   {"http://www.nb.no"},
				"limit": {"100"},
			},
			want: &SearchRequest{
				originalURI: "http://www.nb.no",
				limit:       100,
			},
		},
		// parse does not set default limit
		{
			query: map[string][]string{
				"url":   {"http://example.com/?q=1"},
				"limit": {"0"},
			},
			want: &SearchRequest{
				originalURI: "http://example.com/?q=1",
				limit:       0,
			},
		},
		// to cannot be before from and should error
		{
			query: map[string][]string{
				"url":  {"http://example.com/"},
				"from": {"20020101000000"},
				"to":   {"2001"},
			},
			want: &SearchRequest{},
			err:  errors.New("to should not be allowed before from"),
		},
		// test closest and sort closest
		{
			query: map[string][]string{
				"url":     {"http://example.com/"},
				"closest": {"20020101000000"},
				"sort":    {"closest"},
			},
			want: &SearchRequest{
				originalURI: "http://example.com/",
				closest:     "20020101000000",
				sort:        index.SortClosest,
			},
		},
	}

	for i, test := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			got, err := Parse(test.query)
			if err != nil {
				if test.err == nil {
					t.Errorf("unexpected error: %s", err)
				}
				return
			}
			if test.err != nil {
				t.Errorf("expected error: %v", test.err)
				return
			}

			// don't compare the following fields
			test.want.whatwgUrl = got.whatwgUrl
			test.want.ssurt = got.ssurt
			test.want.Values = got.Values

			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("Got: '%+v', Want: '%+v'", got, test.want)
			}
		})
	}
}
