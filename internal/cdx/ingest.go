// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// CaptureURLKey is shared by ingestion, deletion and POST-aware queries.
func CaptureURLKey(raw, method, body string) (string, error) {
	if raw == "" || strings.ContainsFunc(raw, unicode.IsControl) {
		return "", fmt.Errorf("invalid original URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "", fmt.Errorf("absolute original URL required")
	}
	if strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https") {
		if u.Hostname() == "" {
			return "", fmt.Errorf("HTTP URL requires a host")
		}
		if port := u.Port(); port != "" {
			if _, err := strconv.ParseUint(port, 10, 16); err != nil {
				return "", fmt.Errorf("invalid HTTP port")
			}
		}
	}
	if method != "" {
		for _, r := range method {
			if r < 'A' || r > 'Z' {
				return "", fmt.Errorf("method must be an uppercase HTTP token")
			}
		}
	}
	if body != "" && method != http.MethodPost && method != http.MethodPut {
		return "", fmt.Errorf("requestBody requires POST or PUT")
	}
	c := &Capture{Uri: raw, Ssu: CanonicalKey(raw)}
	Augment(c, method, body)
	if len(c.GetSsu()) > 60000 {
		return "", fmt.Errorf("canonical URL key exceeds 60000 bytes")
	}
	return c.GetSsu(), nil
}

// ParseIngestLine deliberately ignores the supplied key. Extension numbers are
// kept as json.Number until their original decimal representation is marshaled.
func ParseIngestLine(line []byte) (*Capture, error) {
	return parseCaptureLine(line, false)
}

// ParseIndexedLine validates an embedded capture while retaining its stored lookup key.
func ParseIndexedLine(line []byte) (*Capture, error) {
	return parseCaptureLine(line, true)
}

func parseCaptureLine(line []byte, retainKey bool) (*Capture, error) {
	parts := bytes.SplitN(line, []byte(" "), 3)
	if len(parts) != 3 || len(parts[0]) == 0 || len(parts[1]) != 14 {
		return nil, fmt.Errorf("expected CDXJ key timestamp JSON object")
	}
	t, err := timestamp.Parse14(string(parts[1]))
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp: %w", err)
	}
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(parts[2]))
	d.UseNumber()
	if err = d.Decode(&m); err != nil || m == nil {
		return nil, fmt.Errorf("invalid CDXJ JSON object")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	text := func(k string, required bool) (string, error) {
		v, ok := m[k]
		if !ok && !required {
			return "", nil
		}
		s, ok := v.(string)
		if !ok || (required && s == "") {
			return "", fmt.Errorf("%s must be a nonempty string", k)
		}
		delete(m, k)
		return s, nil
	}
	c := &Capture{Sts: timestamppb.New(t)}
	if err = c.GetSts().CheckValid(); err != nil {
		return nil, fmt.Errorf("invalid timestamp: %w", err)
	}
	for _, f := range []struct {
		k        string
		p        *string
		required bool
	}{{"url", &c.Uri, true}, {"filename", &c.Filename, true}, {"mime", &c.Mct, false}, {"digest", &c.Dig, false}, {"method", &c.Method, false}, {"requestBody", &c.RequestBody, false}} {
		if *f.p, err = text(f.k, f.required); err != nil {
			return nil, err
		}
	}
	integer := func(k string, required bool) (int64, error) {
		v, ok := m[k]
		if !ok && !required {
			return 0, nil
		}
		var s string
		switch n := v.(type) {
		case string:
			s = n
		case json.Number:
			s = n.String()
		default:
			return 0, fmt.Errorf("%s must be an integer", k)
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("%s outside nonnegative int64 range", k)
		}
		delete(m, k)
		return n, nil
	}
	if c.Offset, err = integer("offset", true); err != nil {
		return nil, err
	}
	if c.Rle, err = integer("length", true); err != nil {
		return nil, err
	}
	status, err := integer("status", false)
	if err != nil || status > 999 {
		return nil, fmt.Errorf("invalid status")
	}
	c.Hsc = int32(status)
	if retainKey {
		if _, err = CaptureURLKey(c.GetUri(), c.GetMethod(), c.GetRequestBody()); err != nil {
			return nil, err
		}
		key := string(parts[0])
		if len(key) > 60000 || strings.ContainsFunc(key, unicode.IsControl) {
			return nil, fmt.Errorf("invalid stored URL key")
		}
		c.Ssu = key
	} else if c.Ssu, err = CaptureURLKey(c.GetUri(), c.GetMethod(), c.GetRequestBody()); err != nil {
		return nil, err
	}
	if strings.ContainsAny(c.GetFilename(), "\x00\r\n") {
		return nil, fmt.Errorf("invalid filename")
	}
	if len(m) > 0 {
		c.ExtraJson, err = json.Marshal(m)
	}
	return c, err
}
