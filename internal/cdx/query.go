package index

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"google.golang.org/protobuf/proto"
)

// QuerySource supplies ordered captures without requiring writable metadata APIs.
const (
	matchDomain   = "domain"
	paramPageSize = "pageSize"
	outputJSON    = "json"
)

type QuerySource interface {
	Scan(context.Context, string, string, func(*Capture) error) error
}
type BlockSource interface {
	ScanPage(context.Context, string, string, int, int, func(*Capture) error) (int, error)
}
type Query struct {
	Predicate                                                  func(*Capture) bool
	Lower, Upper, Key, Match, From, To, Order, Closest, Output string
	Limit, Page, PageSize                                      int
	Paginate, ShowPages                                        bool
	Fields                                                     []string
	Filters                                                    []captureFilter
}
type captureFilter struct {
	field, value  string
	negate, exact bool
	regex         *regexp.Regexp
}

//nolint:gocyclo // Validate the complete query before any output is written.
func ParseQuery(v url.Values) (Query, error) {
	q := Query{Match: v.Get("matchType"), From: v.Get("from"), To: v.Get("to"), Order: v.Get("sort"), Closest: v.Get("closest"), Output: v.Get("output"), Limit: 0, PageSize: 10}
	raw := v.Get("url")
	if raw == "" {
		return q, fmt.Errorf("url is required")
	}
	if strings.HasPrefix(raw, "*.") {
		raw = raw[2:]
		q.Match = matchDomain
	}
	if before, ok := strings.CutSuffix(raw, "*"); ok {
		raw = before
		q.Match = "prefix"
	}
	q.Key = CanonicalKey(raw)
	if q.Match == "" {
		q.Match = "exact"
	}
	q.Lower = q.Key
	switch q.Match {
	case "exact":
		q.Lower += " "
	case "prefix":
	case "host":
		q.Lower = strings.SplitN(q.Key, ")", 2)[0] + ")"
	case matchDomain:
		q.Lower = strings.SplitN(q.Key, ")", 2)[0]
	default:
		return q, fmt.Errorf("invalid matchType")
	}
	q.Upper = prefixEnd(q.Lower)
	for _, p := range []struct {
		k   string
		dst *int
	}{{"limit", &q.Limit}, {"page", &q.Page}, {paramPageSize, &q.PageSize}} {
		if v.Has(p.k) {
			n, e := strconv.Atoi(v.Get(p.k))
			if e != nil || n < 0 || (p.k == paramPageSize && n == 0) {
				return q, fmt.Errorf("invalid %s", p.k)
			}
			*p.dst = n
		}
	}
	q.Paginate = v.Has("page") || v.Has(paramPageSize) || v.Has("showNumPages")
	q.ShowPages = v.Get("showNumPages") == "true"
	if q.Order != "" && q.Order != "reverse" && q.Order != "closest" {
		return q, fmt.Errorf("invalid sort")
	}
	if q.Order == "closest" && q.Closest == "" {
		return q, fmt.Errorf("closest timestamp required")
	}
	if q.Closest != "" {
		if _, err := timestamp.Parse(q.Closest); err != nil {
			return q, err
		}
	}
	for _, s := range []string{q.From, q.To} {
		if len(s) > 14 {
			return q, fmt.Errorf("invalid timestamp")
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return q, fmt.Errorf("invalid timestamp")
			}
		}
	}
	if q.Output != "" && q.Output != "cdxj" && q.Output != outputJSON {
		return q, fmt.Errorf("invalid output")
	}
	if f := v.Get("fields"); f != "" {
		q.Fields = strings.Split(f, ",")
	}
	for _, f := range v["filter"] {
		c := captureFilter{}
		if strings.HasPrefix(f, "!") {
			c.negate = true
			f = f[1:]
		}
		mode := byte(0)
		if strings.HasPrefix(f, "=") || strings.HasPrefix(f, "~") {
			mode = f[0]
			f = f[1:]
		}
		var ok bool
		c.field, c.value, ok = strings.Cut(f, ":")
		if !ok {
			return q, fmt.Errorf("invalid filter")
		}
		c.exact = mode == '='
		if mode == '~' {
			var e error
			c.regex, e = regexp.Compile("^(?:" + c.value + ")")
			if e != nil {
				return q, e
			}
		}
		q.Filters = append(q.Filters, c)
	}
	return q, nil
}
func prefixEnd(s string) string {
	b := []byte(s)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 255 {
			b[i]++
			return string(b[:i+1])
		}
	}
	return ""
}
func (q Query) Accept(c *Capture) bool {
	if q.Predicate != nil && !q.Predicate(c) {
		return false
	}
	if q.Match == matchDomain {
		host, _, _ := strings.Cut(q.Key, ")")
		ch, _, _ := strings.Cut(c.GetSsu(), ")")
		if ch != host && !strings.HasPrefix(ch, host+",") {
			return false
		}
	}
	ts := c.GetSts().AsTime().UTC().Format("20060102150405")
	if q.From != "" && ts < q.From+strings.Repeat("0", 14-len(q.From)) {
		return false
	}
	if q.To != "" && ts > q.To+strings.Repeat("9", 14-len(q.To)) {
		return false
	}
	m, e := Properties(c)
	if e != nil {
		return false
	}
	m["urlkey"] = c.GetSsu()
	m["timestamp"] = ts
	for _, f := range q.Filters {
		s := fmt.Sprint(m[f.field])
		ok := strings.Contains(s, f.value)
		if f.exact {
			ok = s == f.value
		}
		if f.regex != nil {
			ok = f.regex.MatchString(s)
		}
		if ok == f.negate {
			return false
		}
	}
	return true
}

var errLimit = errors.New("query limit reached")

func (q Query) Run(ctx context.Context, src QuerySource, emit func(*Capture) error) (_ int, resultErr error) {
	var target time.Time
	if q.Order == "closest" {
		var err error
		target, err = timestamp.Parse(q.Closest)
		if err != nil {
			return 0, err
		}
	}
	count := 0
	send := func(c *Capture) error {
		if q.Limit > 0 && count >= q.Limit {
			return errLimit
		}
		if e := emit(c); e != nil {
			return e
		}
		count++
		return nil
	}
	var sorter *Sorter
	if q.Order != "" {
		sorter = NewSorter("", false)
		defer func() { resultErr = errors.Join(resultErr, sorter.Close()) }()
	}
	seq := uint64(0)
	visit := func(c *Capture) error {
		if !q.Accept(c) {
			return nil
		}
		if sorter == nil {
			return send(c)
		}
		raw, e := proto.Marshal(c)
		b := base64.StdEncoding.EncodeToString(raw)
		if e != nil {
			return e
		}
		key := c.GetSsu() + " " + c.GetSts().AsTime().UTC().Format("20060102150405.000000000")
		if q.Order == "reverse" {
			var sb strings.Builder
			for _, x := range []byte(key) {
				fmt.Fprintf(&sb, "%02x", 255-x)
			}
			key = sb.String() + "ff"
		} else {
			seconds, nanos := timestamp.Distance(c.GetSts().AsTime(), target)
			key = fmt.Sprintf("%020d.%09d", seconds, nanos)
		}
		seq++
		return sorter.Add(ctx, fmt.Sprintf("%s\t%020d\t%s", key, seq, b))
	}
	pages := 0
	var err error
	if q.Paginate {
		b, ok := src.(BlockSource)
		if !ok {
			return 0, fmt.Errorf("block pagination requires zipnum")
		}
		cb := visit
		if q.ShowPages {
			cb = nil
		}
		pages, err = b.ScanPage(ctx, q.Lower, q.Upper, q.Page, q.PageSize, cb)
	} else {
		err = src.Scan(ctx, q.Lower, q.Upper, visit)
	}
	if errors.Is(err, errLimit) {
		err = nil
	}
	if err != nil || q.ShowPages {
		return pages, err
	}
	if sorter != nil {
		r, w := io.Pipe()
		done := make(chan error, 1)
		go func() { e := sorter.WriteTo(ctx, w); _ = w.CloseWithError(e); done <- e }()
		br := bufio.NewReader(r)
		for {
			line, e := readSortLine(br)
			if e == io.EOF {
				break
			}
			if e != nil {
				err = e
				break
			}
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) != 3 {
				err = fmt.Errorf("invalid sort record")
				break
			}
			raw, e := base64.StdEncoding.DecodeString(parts[2])
			c := &Capture{}
			if e == nil {
				e = proto.Unmarshal(raw, c)
			}
			if e != nil {
				err = e
				break
			}
			if e = send(c); e != nil {
				if !errors.Is(e, errLimit) {
					err = e
				}
				break
			}
		}
		_ = r.Close()
		sortErr := <-done
		if err == nil && sortErr != nil && !errors.Is(sortErr, io.ErrClosedPipe) {
			err = sortErr
		}
	}
	return pages, err
}
func CDXHandler(src QuerySource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, e := ParseQuery(r.URL.Query())
		if e != nil {
			http.Error(w, e.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if q.Output == outputJSON || q.ShowPages {
			w.Header().Set("Content-Type", "application/x-ndjson")
		}
		wrote := false
		pages, e := q.Run(r.Context(), src, func(c *Capture) error {
			m, e := Properties(c)
			if e != nil {
				return e
			}
			ts := c.GetSts().AsTime().Format("20060102150405")
			if q.Output == outputJSON {
				m["urlkey"] = c.GetSsu()
				m["timestamp"] = ts
			}
			if len(q.Fields) > 0 {
				selected := map[string]any{}
				for _, f := range q.Fields {
					if v, ok := m[f]; ok {
						selected[f] = v
					}
				}
				m = selected
			}
			b, e := json.Marshal(m)
			if e != nil {
				return e
			}
			if q.Output != outputJSON {
				b = append([]byte(c.GetSsu()+" "+ts+" "), b...)
			}
			wrote = true
			_, e = w.Write(append(b, '\n'))
			return e
		})
		if e != nil {
			if !wrote {
				http.Error(w, e.Error(), http.StatusInternalServerError)
			}
			return
		}
		if q.ShowPages {
			_ = json.NewEncoder(w).Encode(map[string]int{"pages": pages, paramPageSize: q.PageSize})
		}
	}
}
