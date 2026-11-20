// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

// Package wacz queries a leaf package without importing its captures.
package wacz

import (
	"archive/zip"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipfs"
	"github.com/spf13/afero"
)

const archiveDir = "archive/"
const indexDir = "indexes/"

var ErrChanged = errors.New("WACZ changed; restart direct serving")

type Reader struct {
	file       *os.File
	info       os.FileInfo
	fs         afero.Fs
	generation string
	members    map[string]*zip.File
	ids        map[string]*zip.File
	aliases    map[string]string
	shards     []shard
}

func Open(ctx context.Context, filename string) (_ *Reader, err error) {
	filename, err = filepath.Abs(filename)
	if err != nil {
		return nil, err
	}
	initial, err := os.Stat(filename)
	if err != nil {
		return nil, err
	}
	if !initial.Mode().IsRegular() {
		return nil, errors.New("direct mode requires a regular WACZ file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("direct mode requires a regular WACZ file")
	}
	z, err := zip.NewReader(file, info.Size())
	if err != nil {
		return nil, err
	}
	mounted, err := zipfs.NewWACZ(z)
	if err != nil {
		return nil, err
	}
	r := &Reader{file: file, info: info, fs: mounted, members: map[string]*zip.File{},
		ids: map[string]*zip.File{}, aliases: map[string]string{}}
	if err := r.catalogue(ctx, z); err != nil {
		return nil, err
	}
	return r, r.Check()
}

// Fingerprint identifies an opened package by its file metadata and ZIP directory.
func Fingerprint(info os.FileInfo, z *zip.Reader) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d/%d", info.Size(), info.ModTime().UnixNano())
	for _, member := range z.File {
		if !member.FileInfo().IsDir() {
			_, _ = fmt.Fprintf(hash, "/%q/%d/%d", member.Name, member.CRC32, member.UncompressedSize64)
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func (r *Reader) catalogue(ctx context.Context, z *zip.Reader) error {
	var summaries []*zip.File
	for _, member := range z.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if member.FileInfo().IsDir() {
			continue
		}
		name := member.Name
		if !safeName(name) || r.members[name] != nil {
			return fmt.Errorf("unsafe or duplicate WACZ member %q", name)
		}
		if strings.EqualFold(path.Ext(name), ".wacz") {
			return errors.New("direct mode does not support nested WACZ packages")
		}
		r.members[name] = member
		switch {
		case isWARC(name):
			r.ids[r.MemberID(name)] = member
			r.addAlias(path.Base(name), name)
		case isCDX(name):
			r.shards = append(r.shards, shard{member: member})
		case strings.HasPrefix(name, indexDir) && strings.HasSuffix(name, ".idx"):
			summaries = append(summaries, member)
		}
	}
	r.generation = Fingerprint(r.info, z)
	if len(r.ids) == 0 || len(r.shards) == 0 {
		return errors.New("direct mode requires WARC members and an embedded CDXJ index")
	}
	if err := r.readManifest(); err != nil {
		return err
	}
	r.attachSummaries(ctx, summaries)
	return ctx.Err()
}

func (r *Reader) readManifest() error {
	member := r.members["datapackage.json"]
	if member == nil {
		return nil
	}
	const maxSize = 16 << 20
	if member.UncompressedSize64 > maxSize {
		return errors.New("WACZ manifest exceeds 16 MiB")
	}
	f, err := member.Open()
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if len(raw) > maxSize || !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return errors.New("invalid WACZ manifest")
	}
	var manifest struct {
		Profile   string `json:"profile"`
		Resources []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("invalid WACZ manifest: %w", err)
	}
	if manifest.Profile == "multi-wacz-package" {
		return errors.New("direct mode does not support multi-WACZ packages")
	}
	for _, resource := range manifest.Resources {
		if resource.Name != "" && isWARC(resource.Path) && r.members[resource.Path] != nil {
			r.addAlias(resource.Name, resource.Path)
		}
	}
	return nil
}

func safeName(name string) bool {
	return name != "" && name != "." && name != ".." && path.Clean(name) == name &&
		!strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "../") && !strings.ContainsAny(name, "\\:\x00")
}

func isWARC(name string) bool {
	return strings.HasPrefix(name, archiveDir) && strings.HasSuffix(strings.TrimSuffix(name, ".gz"), ".warc")
}

func isCDX(name string) bool {
	if !strings.HasPrefix(name, indexDir) {
		return false
	}
	ext := path.Ext(strings.TrimSuffix(name, ".gz"))
	return ext == ".cdx" || ext == ".cdxj"
}

func (r *Reader) addAlias(alias, member string) {
	if prev, ok := r.aliases[alias]; ok && prev != member {
		r.aliases[alias] = ""
	} else {
		r.aliases[alias] = member
	}
}

func (r *Reader) MemberID(name string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(r.file.Name()+"\x00"+name)))
}

func (r *Reader) resolve(c *cdx.Capture) (string, error) {
	name := c.GetFilename()
	if !safeName(name) {
		return "", fmt.Errorf("unsafe WARC filename %q", name)
	}
	if !strings.HasPrefix(name, archiveDir) {
		if member, ok := r.aliases[name]; ok {
			name = member
		} else {
			name = archiveDir + name
		}
	}
	member := r.members[name]
	if member == nil || !isWARC(name) {
		return "", fmt.Errorf("missing or ambiguous WARC member %q", c.GetFilename())
	}
	offset, length := uint64(c.GetOffset()), uint64(c.GetRle())
	if offset > member.UncompressedSize64 || length > member.UncompressedSize64-offset {
		return "", fmt.Errorf("capture outside WARC member %q", name)
	}
	return name, nil
}

// Check detects changes to the opened object or replacement of its path.
func (r *Reader) Check() error {
	current, err := os.Stat(r.file.Name())
	if err != nil || !os.SameFile(r.info, current) ||
		!filewalker.VersionOf(r.info).Matches(filewalker.VersionOf(current)) {
		return ErrChanged
	}
	return nil
}

func (r *Reader) Close() error       { return r.file.Close() }
func (r *Reader) Generation() string { return r.generation }

func (r *Reader) OpenMember(id string) (afero.File, error) {
	member := r.ids[id]
	if member == nil {
		return nil, os.ErrNotExist
	}
	return r.fs.Open(member.Name)
}

func (r *Reader) Scan(ctx context.Context, lower, upper string, emit func(*cdx.Capture) error) error {
	var queue rowHeap
	for i := range r.shards {
		cursor := newCursor(ctx, &r.shards[i], lower, upper)
		defer cursor.stop()
		row, err := cursor.next()
		if err != nil {
			return err
		}
		if row != "" {
			heap.Push(&queue, queuedRow{row, i, cursor})
		}
	}
	for queue.Len() > 0 {
		item := heap.Pop(&queue).(queuedRow)
		capture, err := cdx.ParseIndexedLine([]byte(item.line))
		if err != nil {
			return err
		}
		member, err := r.resolve(capture)
		if err != nil {
			return err
		}
		capture.Source = member
		if err := emit(capture); err != nil {
			return err
		}
		item.line, err = item.cursor.next()
		if err != nil {
			return err
		}
		if item.line != "" {
			heap.Push(&queue, item)
		}
	}
	return nil
}

type queuedRow struct {
	line   string
	shard  int
	cursor cursor
}
type rowHeap []queuedRow

func (h *rowHeap) Len() int { return len(*h) }
func (heap *rowHeap) Less(i, j int) bool {
	h := *heap
	left, right := capturePrefix(h[i].line), capturePrefix(h[j].line)
	if left == right {
		return h[i].shard < h[j].shard
	}
	return left < right
}
func (heap *rowHeap) Swap(i, j int) { h := *heap; h[i], h[j] = h[j], h[i] }
func (h *rowHeap) Push(item any)    { *h = append(*h, item.(queuedRow)) }
func (h *rowHeap) Pop() any {
	last := len(*h) - 1
	item := (*h)[last]
	(*h)[last] = queuedRow{}
	*h = (*h)[:last]
	return item
}
