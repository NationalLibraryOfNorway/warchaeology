// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package fs

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/spf13/afero"
)

// readTar loads a read-only mount. MemMapFs gives each Open an independent
// cursor, supports concurrent readers, and supplies implicit parent directories.
// Archive names use slash separators regardless of the host OS.
func readTar(r io.Reader) (afero.Fs, error) {
	fs := afero.NewMemMapFs()
	type member struct {
		original  string
		directory bool
	}
	members := map[string]member{"/": {original: "/", directory: true}}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return &tarMount{Fs: afero.NewReadOnlyFs(fs)}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tar header: %w", err)
		}
		// Do not let platform-specific separators alias distinct archive entries.
		if strings.Contains(hdr.Name, "\\") {
			return nil, fmt.Errorf("unsupported backslash in tar entry %q", hdr.Name)
		}
		name := path.Clean("/" + hdr.Name)
		for current := name; ; current = path.Dir(current) {
			directory := current != name || hdr.Typeflag == tar.TypeDir
			if previous, ok := members[current]; ok {
				if !directory || !previous.directory {
					return nil, fmt.Errorf("tar entries %q and %q conflict at %q", previous.original, hdr.Name, current)
				}
				break
			}
			members[current] = member{hdr.Name, directory}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := fs.MkdirAll(name, hdr.FileInfo().Mode().Perm()); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := fs.MkdirAll(path.Dir(name), 0o755); err != nil {
				return nil, err
			}
			f, err := fs.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, hdr.FileInfo().Mode().Perm())
			if err != nil {
				return nil, err
			}
			_, copyErr := io.CopyN(f, tr, hdr.Size)
			if err := errors.Join(copyErr, f.Close()); err != nil {
				return nil, fmt.Errorf("read tar entry %q: %w", hdr.Name, err)
			}
			if err := fs.Chtimes(name, hdr.AccessTime, hdr.ModTime); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported tar entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
}

type tarMount struct {
	afero.Fs
}

func (*tarMount) Name() string { return "tarfs" }
