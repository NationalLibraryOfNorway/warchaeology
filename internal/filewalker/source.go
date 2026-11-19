// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package filewalker

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	archivefs "github.com/nationallibraryofnorway/warchaeology/v5/internal/fs"
	"github.com/spf13/afero"
)

// Source describes a chain of archive mounts or an FTP endpoint, followed by a
// literal member path. JSON avoids ambiguity when filenames contain ! or #.
type Source struct {
	Roots []string `json:"roots,omitempty"`
	Path  string   `json:"path"`
}

// FileVersion identifies the file metadata used to derive byte offsets.
type FileVersion struct {
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	CRC      *uint32   `json:"crc,omitempty"`
}

func VersionOf(info fs.FileInfo) FileVersion {
	version := FileVersion{Size: info.Size(), Modified: info.ModTime().UTC()}
	if header, ok := info.Sys().(*zip.FileHeader); ok {
		version.CRC = new(header.CRC32)
	}
	return version
}

func (v FileVersion) Matches(other FileVersion) bool {
	if v.Size != other.Size || !v.Modified.Equal(other.Modified) {
		return false
	}
	return v.CRC == nil || (other.CRC != nil && *v.CRC == *other.CRC)
}

// LocalContainer returns the outer local archive for a mounted source.
func LocalContainer(locator string) (string, error) {
	if !strings.HasPrefix(locator, "{") {
		return "", nil
	}
	var src Source
	if err := json.Unmarshal([]byte(locator), &src); err != nil {
		return "", err
	}
	if len(src.Roots) == 0 {
		return "", nil
	}
	return localRootPath(src.Roots[0]), nil
}

// ContainerVersion snapshots the outer container, when a local one exists.
func ContainerVersion(locator string) (FileVersion, bool, error) {
	path, err := LocalContainer(locator)
	if err != nil || path == "" {
		return FileVersion{}, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return FileVersion{}, false, err
	}
	return VersionOf(info), true, nil
}

type sourceFS struct {
	afero.Fs

	roots []string
}

func MemberLocator(roots []string, member string) string {
	data, _ := json.Marshal(Source{Roots: roots, Path: member}) //nolint:errchkjson // Source contains only strings.
	return string(data)
}

func localRootPath(root string) string {
	scheme, local, ok := strings.Cut(root, "://")
	if ok {
		switch strings.ToLower(scheme) {
		case "ftp":
			return ""
		case "zip", "tar", "tgz":
			return local
		}
	}
	return root
}

func SourceLocator(fs afero.Fs, path string) (string, error) {
	roots := []string(nil)
	if f, ok := fs.(*sourceFS); ok {
		roots = f.roots
	}
	if len(roots) == 0 {
		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			return "", err
		}
	}
	roots = append([]string(nil), roots...)
	if len(roots) > 0 && !strings.HasPrefix(roots[0], "ftp://") {
		prefix, local, ok := strings.Cut(roots[0], "://")
		if !ok {
			local = roots[0]
			prefix = ""
		} else {
			prefix += "://"
		}
		abs, e := filepath.Abs(local)
		if e != nil {
			return "", e
		}
		roots[0] = prefix + abs
	}
	return MemberLocator(roots, path), nil
}

func OpenSource(locator string) (afero.File, io.Closer, error) {
	s := Source{Path: locator}
	if strings.HasPrefix(locator, "{") {
		if e := json.Unmarshal([]byte(locator), &s); e != nil {
			return nil, nil, e
		}
	}
	var fs = afero.NewOsFs()
	closers := sourceClosers{}
	for _, root := range s.Roots {
		mounted, e := archivefs.ResolveFilesystem(fs, root)
		if e != nil {
			return nil, nil, errors.Join(e, closers.Close())
		}
		if mounted != fs {
			if c, ok := mounted.(io.Closer); ok {
				closers = append(closers, c)
			}
		}
		fs = mounted
	}
	f, e := fs.Open(s.Path)
	if e != nil {
		return nil, nil, errors.Join(e, closers.Close())
	}
	return f, closers, nil
}

type sourceClosers []io.Closer

func (c sourceClosers) Close() error {
	var e error
	for _, v := range slices.Backward(c) {
		e = errors.Join(e, v.Close())
	}
	return e
}

// WithSourceRoots associates a mounted filesystem with its reopenable mount chain.
func WithSourceRoots(fs afero.Fs, roots []string) afero.Fs {
	return &sourceFS{Fs: fs, roots: append([]string(nil), roots...)}
}
