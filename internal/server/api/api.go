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

package api

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	whatwgUrl "github.com/nationallibraryofnorway/whatwg-url/url"
)

type Route struct {
	Method  string
	Path    string
	Handler http.HandlerFunc
}

const (
	SortClosest = "closest"
	SortReverse = "reverse"
)

var sorts = []string{SortClosest, SortReverse}

const (
	MatchTypeHost     = "host"
	MatchTypeDomain   = "domain"
	MatchTypePrefix   = "prefix"
	MatchTypeExact    = "exact"
	MatchTypeVerbatim = "verbatim"
)

var matchTypes = []string{
	MatchTypeHost,
	MatchTypeDomain,
	MatchTypePrefix,
	MatchTypeExact,
	MatchTypeVerbatim,
}

const (
	OutputCdxj = "cdxj"
	OutputJson = "json"

	// The following outputs is not implemented:
	// OutputLink = "link"
	// OutputText = "text"

)

var outputs = []string{OutputCdxj, OutputJson}

const (
	ParamMatchType = "matchType"
	ParamUrl       = "url"
	ParamFrom      = "from"
	ParamTo        = "to"
	ParamLimit     = "limit"
	ParamSort      = "sort"
	ParamClosest   = "closest"
	ParamOutput    = "output"
	ParamFilter    = "filter"
	ParamFields    = "fields"
)

type SearchRequest struct {
	url.Values

	FilterMap map[string]string

	originalURI string
	whatwgUrl   *whatwgUrl.Url
	dateRange   *DateRange
	limit       int
	filter      Filter
	ssurt       string
	matchType   index.MatchType
	sort        index.Sort
	closest     string
	output      string
	fields      []string
}

func (c *SearchRequest) Url() *whatwgUrl.Url {
	return c.whatwgUrl
}

func (c *SearchRequest) OriginalURI() string { return c.originalURI }

func (c *SearchRequest) Closest() string {
	return c.closest
}

func (c *SearchRequest) Ssurt() string {
	return c.ssurt
}

func (c *SearchRequest) Sort() index.Sort {
	return c.sort
}

func (c *SearchRequest) DateRange() index.DateRange {
	return c.dateRange
}

func (c *SearchRequest) Filter() index.Filter {
	return c.filter
}

func (c *SearchRequest) Limit() int {
	return c.limit
}

func (c *SearchRequest) MatchType() index.MatchType {
	return c.matchType
}

func (c *SearchRequest) Output() string {
	return c.output
}

func (c *SearchRequest) Fields() []string {
	return c.fields
}

func (c *SearchRequest) SetLimit(limit int) {
	c.limit = limit
}

func (c *SearchRequest) SetMatchType(matchType index.MatchType) {
	c.matchType = matchType
}

var schemeRegExp = regexp.MustCompile(`^[a-z][a-z0-9+\-.]+(:.*)`)

func Parse(values url.Values) (req *SearchRequest, err error) {
	req = new(SearchRequest)
	err = req.Parse(values)
	return
}

func (c *SearchRequest) Parse(values url.Values) error {
	var err error

	c.Values = values

	// Match type
	matchType := values.Get(ParamMatchType)
	if matchType != "" {
		if !slices.Contains(matchTypes, matchType) {
			return fmt.Errorf("matchType must be one of %v, got: %s", matchTypes, matchType)
		}
		switch matchType {
		case MatchTypeExact:
			c.matchType = index.MatchTypeExact
		case MatchTypePrefix:
			c.matchType = index.MatchTypePrefix
		case MatchTypeHost:
			c.matchType = index.MatchTypeHost
		case MatchTypeDomain:
			c.matchType = index.MatchTypeDomain
		case MatchTypeVerbatim:
			c.matchType = index.MatchTypeVerbatim
		default:
			c.matchType = index.MatchTypeExact
		}
	}

	// URL
	urlStr := values.Get(ParamUrl)
	if urlStr != "" {
		if !schemeRegExp.MatchString(urlStr) {
			urlStr = "http://" + urlStr
		}
		u, err := whatwgUrl.Parse(urlStr)
		if err != nil {
			return fmt.Errorf("failed to parse url: %w", err)
		}
		c.originalURI = urlStr
		c.whatwgUrl = u
		c.ssurt = index.CanonicalKey(u.String())
	}

	// Date range
	from := values.Get(ParamFrom)
	to := values.Get(ParamTo)
	dateRange, err := NewDateRange(from, to)
	if err != nil {
		return fmt.Errorf("failed to parse date range: %w", err)
	}
	c.dateRange = dateRange

	// Limit
	limit := values.Get(ParamLimit)
	if limit != "" {
		l, err := strconv.Atoi(limit)
		if err != nil {
			return fmt.Errorf("limit must be a positive integer, was %s", limit)
		}
		c.limit = l
	}

	// Closest
	closest := values.Get(ParamClosest)
	if closest != "" {
		_, err := timestamp.Parse(closest)
		if err != nil {
			return fmt.Errorf("failed to parse closest: %w", err)
		}
		c.closest = closest
	}

	// Sort
	sort := values.Get(ParamSort)
	if sort != "" {
		if !slices.Contains(sorts, sort) {
			return fmt.Errorf("sort must be one of %v, was: %s", sorts, sort)
		} else if sort == SortClosest && closest == "" {
			sort = ""
		} else if sort == SortClosest && c.whatwgUrl == nil {
			return fmt.Errorf("%s=%s is not valid without url", ParamSort, SortClosest)
		}
		switch sort {
		case SortReverse:
			c.sort = index.SortDesc
		case SortClosest:
			c.sort = index.SortClosest
		default:
			c.sort = index.SortAsc
		}
	}

	output := values.Get(ParamOutput)
	if output != "" {
		if !slices.Contains(outputs, output) {
			return fmt.Errorf("output must be one of %v, was: %s", outputs, output)
		}
		c.output = output
	}

	filter, ok := values[ParamFilter]
	if ok {
		c.filter = ParseFilter(filter, c.FilterMap)
	}

	fields := values.Get(ParamFields)
	if fields != "" {
		c.fields = strings.Split(fields, ",")
	}

	return nil
}

func ClosestRequest(closest, originalURI string, url *whatwgUrl.Url) *SearchRequest {
	return &SearchRequest{
		whatwgUrl:   url,
		originalURI: originalURI,
		ssurt:       index.CanonicalKey(url.String()),
		limit:       1,
		sort:        index.SortClosest,
		closest:     closest,
		matchType:   index.MatchTypeVerbatim,
	}
}
