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

package warcserver

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/julienschmidt/httprouter"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/server/api"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	whatwgUrl "github.com/nationallibraryofnorway/whatwg-url/url"
)

func parseClosest(u string, closest string) (*api.SearchRequest, error) {
	if _, err := timestamp.Parse(closest); err != nil {
		return nil, err
	}
	uri, err := whatwgUrl.Parse(u)
	if err != nil {
		return nil, err
	}
	return api.ClosestRequest(closest, u, uri), nil
}

func parseValues(values url.Values) (req *api.SearchRequest, err error) {
	req = &api.SearchRequest{
		FilterMap: map[string]string{
			"status": "hsc",
			"mime":   "mct",
			"url":    "uri",
		},
	}
	err = req.Parse(values)
	return
}

func parseResourceRequest(r *http.Request) (uri string, closest string) {
	params := httprouter.ParamsFromContext(r.Context())

	// closest parameter
	p0 := params.ByName("timestamp")
	// remove trailing 'id_'
	closest = strings.TrimSuffix(p0, "id_")

	// url parameter
	p1 := params.ByName("url")
	// remove leading '/'
	uri = strings.TrimPrefix(p1, "/")
	if prefix, ok := strings.CutSuffix(r.URL.Path, p1); ok {
		escaped := r.URL.EscapedPath()
		// Each percent triplet represents one byte of the decoded route prefix.
		for i := 0; i < len(prefix); i++ {
			if escaped[0] == '%' {
				escaped = escaped[3:]
			} else {
				escaped = escaped[1:]
			}
		}
		uri = strings.TrimPrefix(escaped, "/")
	}

	// we must add on any query parameters
	if q := r.URL.RawQuery; len(q) > 0 {
		uri += "?" + q
	}
	// and fragment
	if len(r.URL.Fragment) > 0 {
		// and fragment
		uri += "#" + r.URL.Fragment
	}

	return
}
