// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nationallibraryofnorway/gowarc/v4"
)

// RequestQuery follows the pinned cdxj-indexer extraction rules. AMF is treated
// as opaque binary; decoding AMF objects is intentionally deferred.
func RequestQuery(contentType, length string, body io.Reader) (string, error) {
	n := int64(8192)
	if length != "" {
		v, e := strconv.ParseInt(length, 10, 64)
		if e != nil {
			return "", nil
		}
		n = v
	}
	if n < 0 {
		n = 0
	}
	const maxQuery = 4096
	const prefix = "__wb_post_data="
	limited := io.LimitReader(body, n)
	structured := strings.HasPrefix(contentType, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(contentType, "multipart/") ||
		strings.HasPrefix(contentType, "application/json") ||
		strings.HasPrefix(contentType, "text/plain")
	if !structured {
		// Round up to a complete base64 quantum before clipping the encoded key.
		const sampleBytes = int64(3 * ((maxQuery - len(prefix) + 3) / 4))
		sample, err := io.ReadAll(io.LimitReader(limited, sampleBytes))
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(io.Discard, limited); err != nil {
			return "", err
		}
		q := prefix + base64.StdEncoding.EncodeToString(sample)
		return q[:min(len(q), maxQuery)], nil
	}
	// Keep the binary fallback prefix even if multipart parsing fails late.
	if strings.HasPrefix(contentType, "multipart/") {
		sample, err := io.ReadAll(io.LimitReader(limited, maxQuery))
		if err != nil {
			return "", err
		}
		stream := &multipartInput{Reader: io.MultiReader(bytes.NewReader(sample), limited)}
		q, ok := multipartQuery(contentType, stream, maxQuery)
		if _, err := io.Copy(io.Discard, stream); err != nil {
			return "", err
		}
		if stream.err != nil {
			return "", stream.err
		}
		if ok {
			return q, nil
		}
		q = prefix + base64.StdEncoding.EncodeToString(sample)
		return q[:min(len(q), maxQuery)], nil
	}
	// JSON duplicate keys and form validity can depend on the end of the body.
	// Hash oversized structured inputs rather than building a partial parse tree.
	const parseBudget = 256 << 10
	b, e := io.ReadAll(io.LimitReader(limited, parseBudget+1))
	if e != nil {
		return "", e
	}
	if len(b) > parseBudget {
		digest := sha256.New()
		digest.Write(b)
		if _, e = io.Copy(digest, limited); e != nil {
			return "", e
		}
		return fmt.Sprintf("__wb_post_sha256=%x", digest.Sum(nil)), nil
	}
	binary := func() string { return prefix + base64.StdEncoding.EncodeToString(b) }
	q := ""
	switch {
	case strings.HasPrefix(contentType, "application/x-www-form-urlencoded"):
		if utf8.Valid(b) {
			q = unquoteForm(string(b))
		} else {
			q = binary()
		}
	case strings.HasPrefix(contentType, "application/json"):
		q, _ = jsonQuery(b)
	case strings.HasPrefix(contentType, "text/plain"):
		q, e = jsonQuery(b)
		if e != nil {
			q = binary()
		}
	default:
		q = binary()
	}
	runes := []rune(q)
	if len(runes) > maxQuery {
		q = string(runes[:maxQuery])
	}
	return q, nil
}

// Preserve source I/O errors that mime/multipart may wrap as formatting errors.
type multipartInput struct {
	io.Reader
	err error
}

func (r *multipartInput) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

// multipartQuery stores only the escaped prefix that can enter the lookup key.
func multipartQuery(contentType string, body io.Reader, limit int) (string, bool) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return "", false
	}
	r := multipart.NewReader(body, params["boundary"])
	var q strings.Builder
	appendPrefix := func(s string) { q.WriteString(s[:min(len(s), limit-q.Len())]) }
	for {
		part, err := r.NextPart()
		if err == io.EOF {
			return q.String(), true
		}
		if err != nil {
			return "", false
		}
		if q.Len() > 0 {
			appendPrefix("&")
		}
		name := part.FormName()
		appendPrefix(url.QueryEscape(name[:min(len(name), limit-q.Len())]))
		appendPrefix("=")
		value, err := io.ReadAll(io.LimitReader(part, int64(limit-q.Len())))
		if err != nil {
			return "", false
		}
		appendPrefix(url.QueryEscape(string(value)))
		if _, err := io.Copy(io.Discard, part); err != nil {
			return "", false
		}
	}
}

func unquoteForm(s string) string { // Python unquote_plus tolerates invalid percent escapes.
	s = strings.ReplaceAll(s, "+", " ")
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if n, e := strconv.ParseUint(s[i+1:i+3], 16, 8); e == nil {
				out.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return strings.ToValidUTF8(out.String(), "�")
}

func jsonQuery(b []byte) (string, error) {
	var pairs []string
	counts := map[string]int{}
	add := func(name string, t jsontext.Token) {
		if name == "" {
			return
		}
		counts[name]++
		key := name
		if counts[name] > 1 {
			key = fmt.Sprintf("%s.%d_", name, counts[name])
		}
		value := t.String()
		if t.Kind() == '0' && strings.ContainsAny(value, ".eE") {
			if f, e := strconv.ParseFloat(value, 64); e == nil {
				format := byte('g')
				if math.Abs(f) >= 1e-4 && math.Abs(f) < 1e16 || f == 0 {
					format = 'f'
				}
				value = strconv.FormatFloat(f, format, -1, 64)
				if !strings.ContainsAny(value, ".eE") {
					value += ".0"
				}
			}
		}
		switch t.Kind() {
		case 'n':
			value = "None"
		case 't':
			value = "True"
		case 'f':
			value = "False"
		}
		pairs = append(pairs, url.QueryEscape(key)+"="+url.QueryEscape(value))
	}
	parse := func(b []byte) error {
		d := jsontext.NewDecoder(bytes.NewReader(b), jsontext.AllowDuplicateNames(true))
		root, err := readQueryNode(d, "")
		if err != nil {
			return err
		}
		root.visit(add)
		if _, e := d.ReadToken(); e != io.EOF {
			return fmt.Errorf("trailing JSON data")
		}
		return nil
	}
	if e := parse(b); e != nil {
		pairs = nil
		clear(counts)
		if !bytes.Contains(b, []byte{'\n'}) {
			return "", e
		}
		for line := range bytes.SplitSeq(b, []byte{'\n'}) {
			if e = parse(line); e != nil {
				return "", e
			}
		}
	}
	return strings.Join(pairs, "&"), nil
}

// queryNode retains object order while applying Python's last-value rule for duplicate keys.
type queryNode struct {
	name     string
	token    jsontext.Token
	children []queryNode
}

func readQueryNode(d *jsontext.Decoder, name string) (node queryNode, err error) {
	t, err := d.ReadToken()
	if err != nil {
		return node, err
	}
	node.name = name
	switch t.Kind() {
	case '{':
		positions := make(map[string]int)
		for d.PeekKind() != '}' {
			key, err := d.ReadToken()
			if err != nil {
				return node, err
			}
			name := key.String()
			child, err := readQueryNode(d, name)
			if err != nil {
				return node, err
			}
			if i, ok := positions[name]; ok {
				node.children[i] = child
			} else {
				positions[name] = len(node.children)
				node.children = append(node.children, child)
			}
		}
	case '[':
		for d.PeekKind() != ']' {
			child, err := readQueryNode(d, name)
			if err != nil {
				return node, err
			}
			node.children = append(node.children, child)
		}
	default:
		node.token = t.Clone()
		return node, nil
	}
	_, err = d.ReadToken()
	return node, err
}

func (node queryNode) visit(add func(string, jsontext.Token)) {
	if node.token.Kind() != 0 {
		add(node.name, node.token)
	}
	for _, child := range node.children {
		child.visit(add)
	}
}

func RequestMetadata(rec gowarc.Record) (method, query string, err error) {
	b, ok := rec.WarcRecord.Block().(gowarc.HttpRequestBlock)
	if !ok {
		return "", "", nil
	}
	method, _, _ = strings.Cut(string(b.ProtocolHeaderBytes()), " ")
	if method != http.MethodPost && method != http.MethodPut {
		return method, "", nil
	}
	r, err := b.PayloadBytes()
	if err != nil {
		return "", "", err
	}
	if b.HttpHeader().Get("Content-Length") == "" && len(b.HttpHeader().Values("Content-Length")) > 0 {
		return method, "", nil
	}
	query, err = RequestQuery(b.HttpHeader().Get("Content-Type"), b.HttpHeader().Get("Content-Length"), r)
	return
}

func Augment(c *Capture, method, query string) {
	if method != http.MethodPost && method != http.MethodPut {
		return
	}
	c.Method = method
	c.RequestBody = query
	sep := "?"
	target, _, _ := strings.Cut(c.GetUri(), "#")
	if strings.Contains(target, "?") {
		sep = "&"
	}
	raw := target + sep + "__wb_method=" + method
	if query != "" {
		raw += "&" + query
	}
	c.Ssu = CanonicalKey(raw)
}
