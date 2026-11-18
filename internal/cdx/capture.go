// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"github.com/nationallibraryofnorway/whatwg-url/surt"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Capture is shared by all exports and database backends. Source and Offset
// identify the reopenable source position; Filename is the public replay name.
type Capture = schema.Cdx

type Sink interface {
	WriteCapture(context.Context, *Capture) error
	Flush(context.Context) error
	Close() error
}

func CanonicalKey(raw string) string {
	key, err := surt.String(raw)
	if err != nil {
		return raw
	}
	return key
}

func NewCapture(rec gowarc.Record, source, filename string) (*Capture, error) {
	wr := rec.WarcRecord
	h := wr.WarcHeader()
	t, err := h.GetTime(gowarc.WarcDate)
	if err != nil {
		return nil, err
	}
	c := &Capture{
		Uri:      h.Get(gowarc.WarcTargetURI),
		Sts:      timestamppb.New(t),
		Srt:      wr.Type().String(),
		Rid:      wr.RecordId(),
		Rct:      h.GetId(gowarc.WarcConcurrentTo),
		Rle:      rec.Size,
		Offset:   rec.Offset,
		Source:   source,
		Filename: filename,
		Dig:      h.Get(gowarc.WarcPayloadDigest),
		Mct:      h.Get(gowarc.ContentType),
	}
	c.Ref = "warcfile:" + source + "#" + strconv.FormatInt(rec.Offset, 10)
	c.Ssu = CanonicalKey(c.GetUri())
	c.Cle, _ = h.GetInt64(gowarc.ContentLength)
	// Use the measured block size when available. Reading an uncached block
	// here would consume the payload needed for digest or request extraction.
	c.Ple = max(0, c.Cle)
	if wr.Block().IsCached() {
		c.Ple = wr.Block().Size()
	}
	if b, ok := wr.Block().(gowarc.ProtocolHeaderBlock); ok {
		c.Ple = max(0, c.Ple-int64(len(b.ProtocolHeaderBytes())))
	}
	if b, ok := wr.Block().(gowarc.HttpResponseBlock); ok {
		c.Hsc = int32(b.HttpStatusCode())
		c.Mct = b.HttpHeader().Get("Content-Type")
	}
	if wr.Type() == gowarc.Revisit {
		c.Ple = 0
		c.Mct = "warc/revisit"
		c.Hsc = RevisitStatus(wr)
		c.Roi = h.GetId(gowarc.WarcRefersTo)
		c.Rou = h.Get(gowarc.WarcRefersToTargetURI)
		if dt, e := h.GetTime(gowarc.WarcRefersToDate); e == nil {
			c.Rod = timestamppb.New(dt)
		}
	}
	c.Mct = MediaType(c.GetMct())
	if c.GetDig() == "" && wr.Type() != gowarc.Revisit {
		var r io.Reader
		if b, ok := wr.Block().(gowarc.PayloadBlock); ok {
			r, err = b.PayloadBytes()
		} else {
			r, err = wr.Block().RawBytes()
		}
		if err != nil {
			return nil, err
		}
		hash := sha1.New()
		if c.Ple, err = io.Copy(hash, r); err != nil {
			return nil, err
		}
		c.Dig = "sha1:" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hash.Sum(nil))
	}
	return c, nil
}

// RevisitStatus returns zero for a revisit without parseable HTTP headers.
func RevisitStatus(record gowarc.WarcRecord) int32 {
	if r, err := record.Block().RawBytes(); err == nil {
		if response, err := http.ReadResponse(bufio.NewReader(r), nil); err == nil {
			_ = response.Body.Close()
			return int32(response.StatusCode)
		}
	}
	return 0
}

func MediaType(contentType string) string {
	if i := strings.IndexAny(contentType, "; \t\r\n"); i >= 0 {
		return contentType[:i]
	}
	return contentType
}

func Properties(c *Capture) (map[string]any, error) {
	m := make(map[string]any)
	if len(c.GetExtraJson()) > 0 {
		if err := json.Unmarshal(c.GetExtraJson(), &m, exactNumbers); err != nil {
			return nil, err
		}
		if _, complete := m["url"]; complete {
			return m, nil
		}
	}
	m["url"] = c.GetUri()
	if c.GetMct() != "" {
		m["mime"] = c.GetMct()
	}
	if c.GetHsc() != 0 {
		m["status"] = strconv.Itoa(int(c.GetHsc()))
	}
	if c.GetDig() != "" {
		m["digest"] = c.GetDig()
	}
	m["length"] = strconv.FormatInt(c.GetRle(), 10)
	m["offset"] = strconv.FormatInt(c.GetOffset(), 10)
	m["filename"] = c.GetFilename()
	if c.GetMethod() != "" {
		m["method"] = c.GetMethod()
	}
	if c.GetMethod() != "" {
		m["requestBody"] = c.GetRequestBody()
	}
	return m, nil
}

func Line(c *Capture) ([]byte, error) {
	m, err := Properties(c)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	return []byte(c.GetSsu() + " " + timestamp.Format14(c.GetSts().AsTime()) + " " + string(b)), nil
}

func ParseLine(line []byte) (*Capture, error) {
	fields := strings.SplitN(string(line), " ", 3)
	if len(fields) != 3 {
		return nil, fmt.Errorf("invalid CDXJ line")
	}
	t, err := timestamp.Parse14(fields[1])
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err = json.Unmarshal([]byte(fields[2]), &m, exactNumbers); err != nil {
		return nil, err
	}
	str := func(k string) string { v, _ := m[k].(string); return v }
	c := &Capture{
		Ssu:         fields[0],
		Sts:         timestamppb.New(t),
		Uri:         str("url"),
		Mct:         str("mime"),
		Dig:         str("digest"),
		Filename:    str("filename"),
		Method:      str("method"),
		RequestBody: str("requestBody"),
		ExtraJson:   []byte(fields[2]),
	}
	c.Offset, _ = strconv.ParseInt(str("offset"), 10, 64)
	c.Rle, _ = strconv.ParseInt(str("length"), 10, 64)
	status, _ := strconv.Atoi(str("status"))
	c.Hsc = int32(status)
	return c, nil
}
