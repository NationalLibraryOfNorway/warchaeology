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
