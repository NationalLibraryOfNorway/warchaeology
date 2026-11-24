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

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
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
	return testMount(t, r)
}
func testMount(t *testing.T, r *zip.Reader) afero.Fs {
	t.Helper()
	fs, err := New(r)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}
func TestReadAtBoundsAndChecksum(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		fs := testArchive(t, corrupt)
		f, err := fs.Open("/explicit/file")
		if err != nil {
			t.Fatal(err)
		}
		defer testcheck.Close(t, f)
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
	root, err := testMount(t, r).Open("/")
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, root)
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
	if _, e := entry.Write(payload); e != nil {
		t.Fatal(e)
	}
	if e := w.Close(); e != nil {
		t.Fatal(e)
	}
	backing := &countedReader{Reader: bytes.NewReader(b.Bytes())}
	zr, e := zip.NewReader(backing, int64(b.Len()))
	if e != nil {
		t.Fatal(e)
	}
	backing.bytesRead = 0
	f, e := testMount(t, zr).Open("large.warc")
	if e != nil {
		t.Fatal(e)
	}
	defer testcheck.Close(t, f)
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
			fs := testMount(t, zr)
			if wacz {
				fs, e = NewWACZ(zr)
				if e != nil {
					t.Fatal(e)
				}
			}
			f, e := fs.Open(name)
			if e != nil {
				t.Fatal(e)
			}
			out := make([]byte, 6)
			_, e = f.ReadAt(out, 0)
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
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

func TestMemberNameCollisions(t *testing.T) {
	for _, tc := range []struct {
		first, second string
		valid         bool
	}{
		{"a", "./a", false}, {"a", "a/b", false}, {"a/b", "a", false}, {"a/b", "a/", true}, {"a/", "a/b", true},
	} {
		t.Run(tc.first+"-"+tc.second, func(t *testing.T) {
			r := &zip.Reader{}
			for _, name := range []string{tc.first, tc.second} {
				r.File = append(r.File, &zip.File{FileHeader: zip.FileHeader{Name: name}})
			}
			_, err := New(r)
			if (err == nil) != tc.valid {
				t.Fatalf("mount: %v", err)
			}
		})
	}
}
