package ftpfs

import (
	"context"
	"errors"
	"io"
	"net/textproto"
	"os"
	"strings"
	"testing"

	"github.com/jackc/puddle"
	"github.com/jlaffaye/ftp"
)

type fakeConnection struct {
	data                        string
	entry                       *ftp.Entry
	entries                     []*ftp.Entry
	entryErr, listErr, loginErr error
	quits, lists                int
}

func (c *fakeConnection) Login(string, string) error          { return c.loginErr }
func (c *fakeConnection) Quit() error                         { c.quits++; return nil }
func (c *fakeConnection) GetEntry(string) (*ftp.Entry, error) { return c.entry, c.entryErr }
func (c *fakeConnection) List(string) ([]*ftp.Entry, error)   { c.lists++; return c.entries, c.listErr }
func (c *fakeConnection) retrieve(_ string, off uint64) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(c.data[off:])), nil
}
func testPool(t *testing.T, c *fakeConnection) *puddle.Pool {
	t.Helper()
	p := puddle.NewPool(func(context.Context) (any, error) { return c, nil }, func(any) {}, 1)
	t.Cleanup(p.Close)
	return p
}
func testFile(t *testing.T, c *fakeConnection) *File {
	t.Helper()
	p := testPool(t, c)
	r, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f, err := FileOpen(r, "/file")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	return f
}
func TestReadSeekAndReadAt(t *testing.T) {
	f := testFile(t, &fakeConnection{data: "abcdef", entry: &ftp.Entry{Name: "file", Size: 6}})
	b := make([]byte, 2)
	if n, err := f.Read(b); n != 2 || err != nil || string(b) != "ab" {
		t.Fatalf("read = %q, %d, %v", b, n, err)
	}
	if n, err := f.ReadAt(b, 3); n != 2 || err != nil || string(b) != "de" {
		t.Fatalf("read at = %q, %d, %v", b, n, err)
	}
	if n, err := f.Read(b); n != 2 || err != nil || string(b) != "cd" {
		t.Fatalf("read after read-at = %q, %d, %v", b, n, err)
	}
	for _, tt := range []struct {
		off    int64
		whence int
		want   int64
	}{{1, io.SeekStart, 1}, {1, io.SeekCurrent, 2}, {1, io.SeekCurrent, 3}, {-2, io.SeekEnd, 4}} {
		if pos, err := f.Seek(tt.off, tt.whence); err != nil || pos != tt.want {
			t.Fatalf("seek = %d, %v, want %d", pos, err, tt.want)
		}
	}
	if n, err := f.ReadAt(b, 5); n != 1 || !errors.Is(err, io.EOF) || b[0] != 'f' {
		t.Fatalf("short read-at = %q, %d, %v", b, n, err)
	}
	if pos, err := f.Seek(0, io.SeekCurrent); pos != 4 || err != nil {
		t.Fatalf("cursor = %d, %v", pos, err)
	}
	if _, err := f.Seek(-7, io.SeekEnd); err == nil {
		t.Fatal("negative seek accepted")
	}
	if _, err := f.ReadAt(b, -1); err == nil {
		t.Fatal("negative offset accepted")
	}
	if _, err := f.Seek(100, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(b); !errors.Is(err, io.EOF) {
		t.Fatalf("past EOF: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Read(b); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed read: %v", err)
	}
}
func TestDirectoryIteration(t *testing.T) {
	c := &fakeConnection{entry: &ftp.Entry{Name: "file", Type: ftp.EntryTypeFolder}, entries: []*ftp.Entry{{Name: "."}, {Name: ".."}, {Name: "one"}, {Name: "sub", Type: ftp.EntryTypeFolder}}}
	f := testFile(t, c)
	for _, want := range []string{"one", "sub"} {
		names, err := f.Readdirnames(1)
		if err != nil || len(names) != 1 || names[0] != want {
			t.Fatalf("names = %v, %v", names, err)
		}
	}
	if _, err := f.Readdir(1); !errors.Is(err, io.EOF) {
		t.Fatalf("end = %v", err)
	}
	if entries, err := f.Readdir(-1); len(entries) != 0 || err != nil {
		t.Fatalf("read all at end = %v, %v", entries, err)
	}
	if c.lists != 1 {
		t.Fatalf("list calls = %d", c.lists)
	}
}
func TestLegacyDirectoryStat(t *testing.T) {
	c := &fakeConnection{entryErr: &textproto.Error{Code: 502}, entries: []*ftp.Entry{{Name: "directory", Type: ftp.EntryTypeFolder}}}
	e, err := statEntry(c, "/directory")
	if err != nil || e.Type != ftp.EntryTypeFolder {
		t.Fatalf("stat = %v, %v", e, err)
	}
	if _, err := statEntry(c, "/missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing = %v", err)
	}
}
func TestConnectionFailureCleanup(t *testing.T) {
	failure := errors.New("failure")
	c := &fakeConnection{loginErr: failure}
	if _, err := connect(func() (connection, error) { return c, nil }, "u", "p"); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if c.quits != 1 {
		t.Fatalf("quit calls = %d", c.quits)
	}
	c = &fakeConnection{entryErr: &textproto.Error{Code: 502}, listErr: failure}
	p := testPool(t, c)
	r, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FileOpen(r, "/missing"); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if p.Stat().AcquiredResources() != 0 {
		t.Fatal("failed open leaked its connection")
	}
}
