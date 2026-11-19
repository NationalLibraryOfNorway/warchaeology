// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package wacz

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
)

type block struct {
	key            string
	offset, length int64
}
type shard struct {
	member *zip.File
	blocks []block
}
type cursor struct {
	pull func() (string, error, bool)
	stop func()
}

func (c cursor) next() (string, error) {
	line, err, _ := c.pull()
	return line, err
}

func (r *Reader) attachSummaries(ctx context.Context, summaries []*zip.File) {
	byMember := map[string][]block{}
	duplicate := map[string]bool{}
	for _, file := range summaries {
		groups, err := readSummary(ctx, file)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("Streaming CDXJ because WACZ summary could not be used", "member", file.Name, "error", err)
			}
			continue
		}
		for name, blocks := range groups {
			if _, exists := byMember[name]; exists {
				duplicate[name] = true
			}
			byMember[name] = blocks
		}
	}
	for i := range r.shards {
		s := &r.shards[i]
		blocks := byMember[s.member.Name]
		if !duplicate[s.member.Name] && validBlocks(s.member, blocks) {
			s.blocks = blocks
		}
	}
}

func validBlocks(member *zip.File, blocks []block) bool {
	if len(blocks) == 0 || member.Method != zip.Store || !strings.HasSuffix(member.Name, ".gz") {
		return false
	}
	size := member.UncompressedSize64
	var end uint64
	previous := ""
	for _, b := range blocks {
		if b.offset < 0 || uint64(b.offset) != end || b.length <= 0 || uint64(b.length) > size-end ||
			capturePrefix(b.key+" ") != b.key || b.key < previous {
			return false
		}
		end += uint64(b.length)
		previous = b.key
	}
	return end == size
}

func readSummary(ctx context.Context, file *zip.File) (_ map[string][]block, err error) {
	f, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	s := lineScanner(f)
	groups := map[string][]block{}
	defaultName := ""
	for i := 0; s.Scan(); i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := s.Text()
		if i == 0 && strings.HasPrefix(line, "!meta 0 ") {
			var meta struct {
				Format   string `json:"format"`
				Filename string `json:"filename"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "!meta 0 ")), &meta); err != nil || meta.Format != "cdxj-gzip-1.0" {
				return nil, errors.New("unsupported CDXJ summary header")
			}
			defaultName = meta.Filename
			continue
		}
		name, b, err := readBlockLine(line, defaultName)
		if err != nil {
			return nil, err
		}
		if !safeName(name) {
			return nil, fmt.Errorf("unsafe CDXJ summary member %q", name)
		}
		if !strings.HasPrefix(name, indexDir) {
			name = path.Join(path.Dir(file.Name), name)
		}
		groups[name] = append(groups[name], b)
	}
	return groups, s.Err()
}

func readBlockLine(line, defaultName string) (string, block, error) {
	if fields := strings.Split(line, "\t"); len(fields) >= 4 {
		offset, offErr := strconv.ParseInt(fields[2], 10, 64)
		length, lenErr := strconv.ParseInt(fields[3], 10, 64)
		return fields[1], block{fields[0], offset, length}, errors.Join(offErr, lenErr)
	}
	key, data, ok := strings.Cut(line, " {")
	if !ok || key == "" {
		return "", block{}, errors.New("invalid CDXJ summary line")
	}
	var index struct {
		Filename string `json:"filename"`
		Offset   *int64 `json:"offset"`
		Length   int64  `json:"length"`
	}
	if err := json.Unmarshal([]byte("{"+data), &index); err != nil {
		return "", block{}, err
	}
	if index.Offset == nil {
		return "", block{}, errors.New("CDXJ summary line has no offset")
	}
	if index.Filename == "" {
		index.Filename = defaultName
	}
	return index.Filename, block{key, *index.Offset, index.Length}, nil
}

func lineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	return scanner
}

func capturePrefix(line string) string {
	key, rest, ok := strings.Cut(line, " ")
	if !ok {
		return ""
	}
	stamp, _, ok := strings.Cut(rest, " ")
	if !ok || len(stamp) != 14 {
		return ""
	}
	return key + " " + stamp
}

func newCursor(ctx context.Context, s *shard, lower, upper string) cursor {
	next, stop := iter.Pull2(func(yield func(string, error) bool) {
		previous := ""
		for input, err := range s.inputs(lower, upper) {
			if err != nil {
				yield("", err)
				return
			}
			done, err := scanInput(ctx, input, lower, upper, &previous, yield)
			if err != nil {
				yield("", fmt.Errorf("%s: %w", s.member.Name, err))
				return
			}
			if done {
				return
			}
		}
	})
	return cursor{next, stop}
}

type indexInput struct {
	io.ReadCloser

	firstKey string
}

func scanInput(ctx context.Context, input indexInput, lower, upper string, previous *string, yield func(string, error) bool) (done bool, err error) {
	defer func() { err = errors.Join(err, input.Close()) }()
	scanner := lineScanner(input)
	first := input.firstKey
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		line := scanner.Text()
		if first != "" && capturePrefix(line) != first {
			return true, errors.New("CDXJ block boundary disagrees with summary")
		}
		first = ""
		key := capturePrefix(line)
		if key == "" || key < *previous {
			return true, errors.New("invalid or unsorted CDXJ record")
		}
		*previous = key
		if upper != "" && key >= upper {
			return true, nil
		}
		if key >= lower && !yield(line, nil) {
			return true, nil
		}
	}
	if first != "" {
		return true, errors.New("empty CDXJ block")
	}
	return false, scanner.Err()
}

func (s *shard) inputs(lower, upper string) iter.Seq2[indexInput, error] {
	return func(yield func(indexInput, error) bool) {
		if len(s.blocks) == 0 {
			f, err := s.member.Open()
			if err != nil {
				yield(indexInput{}, err)
				return
			}
			if !strings.HasSuffix(s.member.Name, ".gz") {
				yield(indexInput{ReadCloser: f}, nil)
				return
			}
			gz, err := gzip.NewReader(f)
			if err != nil {
				yield(indexInput{}, errors.Join(err, f.Close()))
				return
			}
			yield(indexInput{ReadCloser: gzipStream{gz, f}}, nil)
			return
		}
		raw, err := s.member.OpenRaw()
		if err != nil {
			yield(indexInput{}, err)
			return
		}
		ra, ok := raw.(io.ReaderAt)
		if !ok {
			yield(indexInput{}, errors.New("stored CDXJ member is not seekable"))
			return
		}
		first := sort.Search(len(s.blocks), func(i int) bool { return s.blocks[i].key >= lower })
		for _, b := range s.blocks[max(0, first-1):] {
			if upper != "" && b.key >= upper {
				return
			}
			gz, err := gzip.NewReader(io.NewSectionReader(ra, b.offset, b.length))
			if err != nil {
				yield(indexInput{}, err)
				return
			}
			gz.Multistream(false)
			if !yield(indexInput{ReadCloser: gz, firstKey: b.key}, nil) {
				return
			}
		}
	}
}

type gzipStream struct {
	*gzip.Reader

	file io.Closer
}

func (g gzipStream) Close() error { return errors.Join(g.Reader.Close(), g.file.Close()) }
