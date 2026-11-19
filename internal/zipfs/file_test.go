// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package zipfs

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/spf13/afero"
)

func testArchive(t *testing.T, corrupt bool) afero.Fs {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for _, name := range []string{"explicit/", "explicit/file", "implicit/file"} {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if name != "explicit/" {
			if _, err := io.WriteString(f, "abcdef"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := b.Bytes()
	if corrupt {
		pos := bytes.Index(data, []byte("abcdef"))
		data[pos] = 'x'
	}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return New(r)
}
func TestReadAtBoundsAndChecksum(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		fs := testArchive(t, corrupt)
		f, err := fs.Open("/explicit/file")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b := make([]byte, 6)
		n, err := f.ReadAt(b, 0)
		if corrupt {
			if !errors.Is(err, zip.ErrChecksum) {
				t.Fatalf("checksum = %v", err)
			}
			continue
		}
		if n != 6 || err != nil || string(b) != "abcdef" {
			t.Fatalf("read = %q %d %v", b, n, err)
		}
		if n, err := f.ReadAt(b, 100); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("past EOF = %d %v", n, err)
		}
		if _, err := f.ReadAt(b, -1); err == nil {
			t.Fatal("negative read accepted")
		}
		if pos, err := f.Seek(100, io.SeekStart); pos != 100 || err != nil {
			t.Fatalf("seek = %d %v", pos, err)
		}
		if n, err := f.Read(b); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read past EOF = %d %v", n, err)
		}
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				buf := make([]byte, 6)
				if _, err := f.ReadAt(buf, 0); err != nil || string(buf) != "abcdef" {
					t.Errorf("parallel read = %q %v", buf, err)
				}
			})
		}
		wg.Wait()
	}
}
func TestDirectoryContracts(t *testing.T) {
	fs := testArchive(t, false)
	for _, name := range []string{"/", "/explicit", "/implicit"} {
		f, err := fs.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for {
			entries, err := f.Readdir(1)
			count += len(entries)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		want := 1
		if name == "/" {
			want = 2
		}
		if count != want {
			t.Fatalf("%s count = %d", name, count)
		}
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			t.Fatalf("directory seek accepted: %s", name)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEmptyArchiveHasRoot(t *testing.T) {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	root, err := New(r).Open("/")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, err := root.Readdir(-1)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty root = %v, %v", entries, err)
	}
}

type countedReader struct {
	*bytes.Reader
	bytesRead int
}

func (r *countedReader) ReadAt(p []byte, off int64) (int, error) {
	n, e := r.Reader.ReadAt(p, off)
	r.bytesRead += n
	return n, e
}
func TestStoredMemberLateReadIsBounded(t *testing.T) {
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	entry, e := w.CreateHeader(&zip.FileHeader{Name: "large.warc", Method: zip.Store})
	if e != nil {
		t.Fatal(e)
	}
	payload := make([]byte, 8<<20)
	copy(payload[len(payload)-4:], "tail")
	entry.Write(payload)
	w.Close()
	backing := &countedReader{Reader: bytes.NewReader(b.Bytes())}
	zr, e := zip.NewReader(backing, int64(b.Len()))
	if e != nil {
		t.Fatal(e)
	}
	backing.bytesRead = 0
	f, e := New(zr).Open("large.warc")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	out := make([]byte, 4)
	if _, e = f.ReadAt(out, int64(len(payload)-4)); e != nil {
		t.Fatal(e)
	}
	if string(out) != "tail" || backing.bytesRead > 4096 {
		t.Fatalf("read=%q bytes=%d", out, backing.bytesRead)
	}
}

func TestBrowsertrixChildCRCPlaceholder(t *testing.T) {
	for _, name := range []string{"child.wacz", "data.warc"} {
		for _, wacz := range []bool{false, true} {
			var b bytes.Buffer
			writer := zip.NewWriter(&b)
			entry, e := writer.CreateRaw(&zip.FileHeader{Name: name, Method: zip.Store, CRC32: 0, CompressedSize64: 6, UncompressedSize64: 6})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = entry.Write([]byte("abcdef")); e != nil {
				t.Fatal(e)
			}
			if e = writer.Close(); e != nil {
				t.Fatal(e)
			}
			zr, e := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
			if e != nil {
				t.Fatal(e)
			}
			fs := New(zr)
			if wacz {
				fs = NewWACZ(zr)
			}
			f, e := fs.Open(name)
			if e != nil {
				t.Fatal(e)
			}
			out := make([]byte, 6)
			_, e = f.ReadAt(out, 0)
			f.Close()
			if wacz && name == "child.wacz" {
				if e != nil {
					t.Fatal(e)
				}
			} else if !errors.Is(e, zip.ErrChecksum) {
				t.Fatalf("name=%s wacz=%v error=%v", name, wacz, e)
			}
		}
	}
}
