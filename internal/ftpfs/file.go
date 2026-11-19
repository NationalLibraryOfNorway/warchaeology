// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package ftpfs

import (
	"errors"
	"io"
	"math"
	"net/textproto"
	"os"
	"path"
	"sync"
	"syscall"

	"github.com/jackc/puddle/v2"
	"github.com/jlaffaye/ftp"
)

type File struct {
	mu        sync.Mutex
	resp      io.ReadCloser
	info      *FileInfo
	resource  *puddle.Resource[connection]
	pos       int64
	streamPos int64
	entries   []*ftp.Entry
	dirPos    int
	listed    bool
}

func FileOpen(res *puddle.Resource[connection], name string) (*File, error) {
	entry, err := statEntry(res.Value(), name)
	if err != nil {
		res.Release()
		return nil, err
	}
	return &File{resource: res, info: &FileInfo{entry: entry, fullName: name}}, nil
}

func statEntry(c connection, name string) (*ftp.Entry, error) {
	entry, err := c.GetEntry(name)
	if err == nil {
		return entry, nil
	}
	var protocolErr *textproto.Error
	if !errors.As(err, &protocolErr) || (protocolErr.Code != 500 && protocolErr.Code != 502 && protocolErr.Code != 504) {
		return nil, err
	}
	// Older servers lack MLST. List the parent, not the requested directory:
	// a directory containing one child must not be mistaken for that child.
	clean := path.Clean(name)
	if clean == "." || clean == "/" {
		if _, err := c.List(clean); err != nil {
			return nil, err
		}
		return &ftp.Entry{Name: clean, Type: ftp.EntryTypeFolder}, nil
	}
	entries, err := c.List(path.Dir(clean))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name == path.Base(clean) {
			return entry, nil
		}
	}
	return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
}

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.closeStream()
	if f.resource != nil {
		f.resource.Release()
		f.resource = nil
	}
	return err
}
func (f *File) closeStream() error {
	if f.resp == nil {
		return nil
	}
	err := f.resp.Close()
	f.resp = nil
	return err
}
func (f *File) ftpClient() connection { return f.resource.Value() }
func (f *File) Name() string          { return f.info.fullName }
func (f *File) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resource == nil {
		return nil, os.ErrClosed
	}
	return f.info, nil
}
func (f *File) Sync() error          { return nil }
func (f *File) Truncate(int64) error { return syscall.EROFS }
func (f *File) readable() error {
	if f.resource == nil {
		return os.ErrClosed
	}
	if f.info.IsDir() {
		return syscall.EISDIR
	}
	return nil
}
func (f *File) Read(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.readable(); err != nil {
		return 0, err
	}
	if len(b) == 0 {
		return 0, nil
	}
	if f.pos >= f.info.Size() {
		return 0, io.EOF
	}
	if f.resp != nil && f.streamPos != f.pos {
		if err := f.closeStream(); err != nil {
			return 0, err
		}
	}
	if f.resp == nil {
		r, err := f.ftpClient().retrieve(f.info.fullName, uint64(f.pos))
		if err != nil {
			return 0, err
		}
		f.resp = r
		f.streamPos = f.pos
	}
	n, err := f.resp.Read(b)
	f.pos += int64(n)
	f.streamPos += int64(n)
	return n, err
}
func (f *File) ReadAt(b []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.readable(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(b) == 0 {
		return 0, nil
	}
	if off >= f.info.Size() {
		return 0, io.EOF
	}
	if err := f.closeStream(); err != nil {
		return 0, err
	}
	r, err := f.ftpClient().retrieve(f.info.fullName, uint64(off))
	if err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r, b)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	// A positional read never changes the sequential cursor.
	closeErr := r.Close()
	if closeErr != nil {
		return n, errors.Join(err, closeErr)
	}
	return n, err
}
func (f *File) Readdir(count int) ([]os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resource == nil {
		return nil, os.ErrClosed
	}
	if !f.info.IsDir() {
		return nil, syscall.ENOTDIR
	}
	if !f.listed {
		entries, err := f.ftpClient().List(f.info.fullName)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.Name != "." && entry.Name != ".." {
				f.entries = append(f.entries, entry)
			}
		}
		f.listed = true
	}
	if count > 0 && f.dirPos == len(f.entries) {
		return nil, io.EOF
	}
	end := len(f.entries)
	if count > 0 && count < end-f.dirPos {
		end = f.dirPos + count
	}
	entries := make([]os.FileInfo, 0, end-f.dirPos)
	for _, entry := range f.entries[f.dirPos:end] {
		entries = append(entries, &FileInfo{entry: entry, fullName: path.Join(f.info.fullName, entry.Name)})
	}
	f.dirPos = end
	return entries, nil
}
func (f *File) Readdirnames(n int) ([]string, error) {
	entries, err := f.Readdir(n)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, err
}
func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.readable(); err != nil {
		return 0, err
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.pos
	case io.SeekEnd:
		base = f.info.Size()
	default:
		return 0, syscall.EINVAL
	}
	if offset > 0 && base > math.MaxInt64-offset {
		return 0, syscall.EINVAL
	}
	pos := base + offset
	if pos < 0 {
		return 0, syscall.EINVAL
	}
	f.pos = pos
	return pos, nil
}
func (f *File) Write([]byte) (int, error)          { return 0, syscall.EROFS }
func (f *File) WriteAt([]byte, int64) (int, error) { return 0, syscall.EROFS }
func (f *File) WriteString(string) (int, error)    { return 0, syscall.EROFS }
