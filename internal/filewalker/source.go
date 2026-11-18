package filewalker

import (
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"

	archivefs "github.com/nationallibraryofnorway/warchaeology/v5/internal/fs"
	"github.com/spf13/afero"
)

// Source describes a chain of archive mounts or an FTP endpoint, followed by a
// literal member path. JSON avoids ambiguity when filenames contain ! or #.
type Source struct {
	Roots []string `json:"roots,omitempty"`
	Path  string   `json:"path"`
}
type sourceFS struct {
	afero.Fs

	roots []string
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
	b, err := json.Marshal(Source{Roots: roots, Path: path})
	return string(b), err
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
