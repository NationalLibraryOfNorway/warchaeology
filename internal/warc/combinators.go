// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package warc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"strings"

	"github.com/nationallibraryofnorway/gowarc/v4"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/diskspace"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filter"
)

// Records skips recoverable read failures before record selection.
func Records(ctx context.Context, reader *gowarc.WarcFileReader, force bool, path string) iter.Seq2[gowarc.Record, error] {
	return func(yield func(gowarc.Record, error) bool) {
		var last int64 = -1
		for {
			if err := ctx.Err(); err != nil {
				yield(gowarc.Record{}, err)
				return
			}
			rec, err := reader.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				closeErr := rec.Close()
				if !force || diskspace.Exhausted(err) || closeErr != nil || rec.Offset <= last {
					yield(rec, errors.Join(err, closeErr))
					return
				}
				slog.Warn("Skipping malformed record", "path", path, "offset", rec.Offset, "error", err)
				last = rec.Offset
				continue
			}
			if !yield(rec, nil) {
				return
			}
		}
	}
}

func Filter(seq iter.Seq2[gowarc.Record, error], accept func(gowarc.Record) bool) iter.Seq2[gowarc.Record, error] {
	if accept == nil {
		return seq
	}
	return func(yield func(gowarc.Record, error) bool) {
		for rec, err := range seq {
			if err != nil {
				yield(rec, err)
				return
			}
			if !accept(rec) {
				if err := rec.Close(); err != nil {
					yield(rec, err)
					return
				}
				continue
			}
			if !yield(rec, nil) {
				return
			}
		}
	}
}

func Limit(seq iter.Seq2[gowarc.Record, error], limit int) iter.Seq2[gowarc.Record, error] {
	if limit <= 0 {
		return seq
	}

	return func(yield func(gowarc.Record, error) bool) {
		count := 0
		for rec, err := range seq {
			if err != nil {
				yield(rec, err)
				return
			}
			count++
			if !yield(rec, nil) {
				return
			}
			if count >= limit {
				return
			}
		}
	}
}

func Nth(seq iter.Seq2[gowarc.Record, error], n int) iter.Seq2[gowarc.Record, error] {
	return func(yield func(gowarc.Record, error) bool) {
		if n <= 0 {
			return
		}

		count := 0
		for rec, err := range seq {
			if err != nil {
				yield(rec, err)
				return
			}
			count++
			if count == n {
				yield(rec, nil)
				return
			}
			if err := rec.Close(); err != nil {
				yield(rec, err)
				return
			}
		}
	}
}

func Skip(seq iter.Seq2[gowarc.Record, error], n int) iter.Seq2[gowarc.Record, error] {
	if n <= 0 {
		return seq
	}

	return func(yield func(gowarc.Record, error) bool) {
		skipped := 0
		for rec, err := range seq {
			if err != nil {
				yield(rec, err)
				return
			}
			if skipped < n {
				skipped++
				if err := rec.Close(); err != nil {
					yield(rec, err)
					return
				}
				continue
			}
			if !yield(rec, nil) {
				return
			}
		}
	}
}

func Compose(seq iter.Seq2[gowarc.Record, error], recordFilter *filter.RecordFilter, nth int, limit int) iter.Seq2[gowarc.Record, error] {
	if recordFilter != nil {
		seq = Filter(seq, ByRecordFilter(recordFilter))
	}
	if nth > 0 {
		return Nth(seq, nth)
	}
	if limit > 0 {
		return Limit(seq, limit)
	}
	return seq
}

func ByRecordFilter(recordFilter *filter.RecordFilter) func(gowarc.Record) bool {
	if recordFilter == nil {
		return func(gowarc.Record) bool { return true }
	}
	return func(record gowarc.Record) bool {
		if record.WarcRecord == nil {
			return false
		}
		return recordFilter.Accept(record.WarcRecord)
	}
}

func ByRecordType(types ...gowarc.RecordType) func(gowarc.Record) bool {
	var mask gowarc.RecordType
	for _, t := range types {
		mask |= t
	}

	return func(record gowarc.Record) bool {
		if record.WarcRecord == nil {
			return false
		}
		return record.WarcRecord.Type()&mask != 0
	}
}

func ByContentType(substr string) func(gowarc.Record) bool {
	substr = strings.ToLower(substr)
	return func(record gowarc.Record) bool {
		if record.WarcRecord == nil {
			return false
		}
		ct := strings.ToLower(record.WarcRecord.WarcHeader().Get(gowarc.ContentType))
		return strings.Contains(ct, substr)
	}
}

func ByTargetURI(substr string) func(gowarc.Record) bool {
	return func(record gowarc.Record) bool {
		if record.WarcRecord == nil {
			return false
		}
		uri := record.WarcRecord.WarcHeader().Get(gowarc.WarcTargetURI)
		return strings.Contains(uri, substr)
	}
}

func And(predicates ...func(gowarc.Record) bool) func(gowarc.Record) bool {
	return func(record gowarc.Record) bool {
		for _, predicate := range predicates {
			if !predicate(record) {
				return false
			}
		}
		return true
	}
}

func Or(predicates ...func(gowarc.Record) bool) func(gowarc.Record) bool {
	return func(record gowarc.Record) bool {
		for _, predicate := range predicates {
			if predicate(record) {
				return true
			}
		}
		return false
	}
}

func Not(predicate func(gowarc.Record) bool) func(gowarc.Record) bool {
	return func(record gowarc.Record) bool {
		return !predicate(record)
	}
}

// CheckCoverage verifies physical framing as records pass through, before filtering.
func CheckCoverage(seq iter.Seq2[gowarc.Record, error], file io.ReaderAt, size, offset int64) iter.Seq2[gowarc.Record, error] {
	return func(yield func(gowarc.Record, error) bool) {
		coverage := Coverage(offset)
		for rec, err := range seq {
			if err == nil {
				if e := coverage.Add(file, rec); e != nil {
					err = errors.Join(e, rec.Close())
				}
			}
			if !yield(rec, err) || err != nil {
				return
			}
		}
		if err := coverage.Finish(file, size); err != nil {
			yield(gowarc.Record{Offset: int64(coverage)}, err)
		}
	}
}

// Coverage tracks the physical end of parsed records, including gzip framing.
type Coverage int64

func (c *Coverage) Add(file io.ReaderAt, r gowarc.Record) error {
	if err := c.Finish(file, r.Offset); err != nil {
		return err
	}
	if r.Size <= 0 {
		return fmt.Errorf("unparsed archive bytes at offset %d", *c)
	}
	*c = Coverage(r.Offset + r.Size)
	return nil
}

// Some producers leave extra line endings between records or at EOF.
func (c Coverage) Finish(file io.ReaderAt, size int64) error {
	for int64(c) < size {
		var framing [256]byte
		n := min(int64(len(framing)), size-int64(c))
		b := framing[:n]
		if _, err := file.ReadAt(b, int64(c)); err != nil {
			return err
		}
		if len(bytes.Trim(b, "\r\n")) != 0 {
			break
		}
		c += Coverage(n)
	}
	if int64(c) != size {
		return fmt.Errorf("incomplete archive: parsed %d of %d bytes", c, size)
	}
	return nil
}
