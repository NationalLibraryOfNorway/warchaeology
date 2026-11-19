// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/gzip"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/spf13/afero"
)

func archiveBytes(t *testing.T, kind string) []byte {
	t.Helper()
	var b bytes.Buffer
	if kind == "zip" {
		w := zip.NewWriter(&b)
		f, err := w.Create("implicit/record.warc")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, "content"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		var dst io.Writer = &b
		var gz *gzip.Writer
		if kind == "tgz" {
			gz = gzip.NewWriter(&b)
			dst = gz
		}
		w := tar.NewWriter(dst)
		if err := w.WriteHeader(&tar.Header{Name: "implicit/record.warc", Size: 7, Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, "content"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if gz != nil {
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b.Bytes()
}

func TestLiteralArchivePaths(t *testing.T) {
	for _, kind := range []string{"zip", "tar", "tgz"} {
		for _, base := range []string{"relative", "/absolute", "percent%zz%2F", "hash#fragment", "query?value", "plus+space name", "Unicode-å", `C:\archive`, "C://archive"} {
			t.Run(kind+"/"+base, func(t *testing.T) {
				fs := afero.NewMemMapFs()
				name := base + "." + kind
				if err := fs.MkdirAll("C:", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := afero.WriteFile(fs, name, archiveBytes(t, kind), 0o644); err != nil {
					t.Fatal(err)
				}
				mounted, err := ResolveFilesystem(fs, name)
				if err != nil {
					t.Fatal(err)
				}
				if c, ok := mounted.(io.Closer); ok {
					t.Cleanup(func() {
						if err := c.Close(); err != nil {
							t.Error(err)
						}
					})
				}
				// Reads must not share a cursor, including simultaneous reads.
				var wg sync.WaitGroup
				for range 4 {
					wg.Go(func() {
						data, err := afero.ReadFile(mounted, "/implicit/record.warc")
						if err != nil || string(data) != "content" {
							t.Errorf("read = %q, %v", data, err)
						}
					})
				}
				wg.Wait()
				entries, err := afero.ReadDir(mounted, "/")
				if err != nil || len(entries) != 1 || !entries[0].IsDir() {
					t.Fatalf("root = %v, %v", entries, err)
				}
				if _, err := mounted.Create("/write"); err == nil {
					t.Fatal("mount must be read-only")
				}
			})
		}
	}
}

type trackingFS struct {
	afero.Fs

	opened, closed int
}
type trackingFile struct {
	afero.File

	owner *trackingFS
}

func (f *trackingFS) Open(name string) (afero.File, error) {
	file, err := f.Fs.Open(name)
	if err != nil {
		return nil, err
	}
	f.opened++
	return &trackingFile{File: file, owner: f}, nil
}
func (f *trackingFile) Close() error { f.owner.closed++; return f.File.Close() }

func TestMountFailuresAndOwnership(t *testing.T) {
	for _, kind := range []string{"zip", "tar", "tgz"} {
		for _, bad := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/bad", false: "/good"}[bad], func(t *testing.T) {
				fs := &trackingFS{Fs: afero.NewMemMapFs()}
				data := archiveBytes(t, kind)
				if bad {
					if kind == "tar" {
						data = data[:514]
					} else {
						data = data[:len(data)/3]
					}
				}
				if err := afero.WriteFile(fs.Fs, "a."+kind, data, 0o644); err != nil {
					t.Fatal(err)
				}
				mounted, err := ResolveFilesystem(fs, "a."+kind)
				if bad && err == nil {
					t.Fatal("corrupt archive accepted")
				}
				if !bad && err != nil {
					t.Fatal(err)
				}
				if !bad && kind == "zip" {
					if fs.closed != 0 {
						t.Fatal("backing file closed before consumer")
					}
					c := mounted.(io.Closer)
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if fs.opened != 1 || fs.closed != 1 {
					t.Fatalf("open/close = %d/%d", fs.opened, fs.closed)
				}
			})
		}
	}
}
func TestFTPComponents(t *testing.T) {
	for _, tt := range []struct{ input, addr, user, password string }{
		{"ftp://blåbær.no", "xn--blbr-roah.no:21", "", ""},
		{"ftp://example.org", "example.org:21", "", ""},
		{"ftp://u:p@example.org:2121/", "example.org:2121", "u", "p"},
		{"ftp://[2001:db8::1]:2121", "[2001:db8::1]:2121", "", ""},
		{"ftp://[::1]", "[::1]:21", "", ""},
		{"ftp://u%40x:p%2525+%20@example.org", "example.org:21", "u@x", "p%25+ "},
	} {
		t.Run(tt.input, func(t *testing.T) {
			u, err := url.Parse(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			addr, user, password, err := ftpEndpoint(u)
			if err != nil || addr != tt.addr || user != tt.user || password != tt.password {
				t.Fatalf("components = %q %q %q, %v", addr, user, password, err)
			}
		})
	}
	for _, input := range []string{"ftp://u%zz:p@host", "ftp://u:p%@host"} {
		_, err := ResolveFilesystem(afero.NewMemMapFs(), input)
		if err == nil || !strings.Contains(err.Error(), "invalid URL escape") {
			t.Fatalf("%q: %v", input, err)
		}
	}
}
func TestLiteralArchivePrefixes(t *testing.T) {
	for _, kind := range []string{"zip", "tar", "tgz"} {
		for _, name := range []string{
			"archive.data", "/absolute.data", "space name.data",
			"percent%20%2F%zz.data", "query?value#fragment.data",
			"user:password@archive:123", "plus+Unicode-å",
			"relative/archive.data", "C:/archive.data", `C:\archive.data`,
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				fs := afero.NewMemMapFs()
				if err := fs.MkdirAll(filepath.Dir(name), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := afero.WriteFile(fs, name, archiveBytes(t, kind), 0o644); err != nil {
					t.Fatal(err)
				}
				mounted, err := ResolveFilesystem(fs, kind+"://"+name)
				if err != nil {
					t.Fatal(err)
				}
				if c, ok := mounted.(io.Closer); ok {
					t.Cleanup(func() {
						if err := c.Close(); err != nil {
							t.Error(err)
						}
					})
				}
				data, err := afero.ReadFile(mounted, "/implicit/record.warc")
				if err != nil || string(data) != "content" {
					t.Fatalf("read = %q, %v", data, err)
				}
			})
		}
	}
}

type remoteTrackingFS struct{ *trackingFS }

func (*remoteTrackingFS) Name() string { return "ftpfs" }
func TestFTPArchiveReleasesSourceBeforeConsumption(t *testing.T) {
	fs := &remoteTrackingFS{&trackingFS{Fs: afero.NewMemMapFs()}}
	if err := afero.WriteFile(fs.Fs, "remote.zip", archiveBytes(t, "zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mounted, err := ResolveFilesystem(fs, "remote.zip", WithTempDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer testcheck.Close(t, mounted.(io.Closer))
		if fs.closed != 1 {
			t.Fatal("FTP pool slot retained by mounted archive")
		}
		data, err := afero.ReadFile(mounted, "/implicit/record.warc")
		if err != nil || string(data) != "content" {
			t.Fatalf("staged read = %q %v", data, err)
		}
	}()
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("staging files after close = %v, %v", files, err)
	}
}

func TestInvalidFTPEndpoints(t *testing.T) {
	for _, locator := range []string{
		"ftp:///path", "ftp://host:65536", "ftp://host:-1",
		"ftp://host:abc", "ftp://[not-ipv6]", "ftp://bad^host",
		"ftp://u%zz:p@host", "ftp://u:p%@host",
	} {
		t.Run(locator, func(t *testing.T) {
			if _, err := ResolveFilesystem(afero.NewMemMapFs(), locator); err == nil {
				t.Fatal("expected invalid FTP endpoint error")
			}
		})
	}
}

func TestFTPEndpointOnly(t *testing.T) {
	for _, suffix := range []string{"", "/"} {
		mounted, err := ResolveFilesystem(afero.NewMemMapFs(), "ftp://u%3F:p%23@example.org"+suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := mounted.(io.Closer).Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct{ suffix, message string }{
		{"/archives/2025", "command arguments"},
		{"//", "command arguments"},
		{"/%2F", "command arguments"},
		{"/%2e", "command arguments"},
		{"?mode=passive", "query"},
		{"?", "query"},
		{"/?", "query"},
		{"#part1", "fragment"},
		{"#", "fragment"},
		{"/#", "fragment"},
	} {
		t.Run(tt.suffix, func(t *testing.T) {
			_, err := ResolveFilesystem(afero.NewMemMapFs(), "ftp://user:secret@example.org"+tt.suffix)
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("error = %v, want %q", err, tt.message)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error exposes credentials")
			}
		})
	}
}
