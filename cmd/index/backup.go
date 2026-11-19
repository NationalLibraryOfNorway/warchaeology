// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/blevesearch/bleve/v2"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
)

const backupFormat = "warchaeology-backup-v1\n"

func backupIndex(ctx context.Context, dir, filename string) (err error) {
	if _, err = os.Stat(filepath.Join(dir, "MANIFEST")); err != nil {
		return err
	}
	// An exclusive Badger handle prevents a server from opening either index
	// while the two snapshots are taken.
	db, err := cdxstore.Open(ctx, cdxstore.Options{Format: badgerFormat, Dir: dir})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	stage, err := os.MkdirTemp(filepath.Dir(filename), ".warc-backup-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	textDir := filepath.Join(dir, "fulltext")
	if _, e := os.Stat(textDir); e == nil {
		text, e := bleve.OpenUsing(textDir, map[string]any{"bolt_timeout": "1s"})
		if e != nil {
			return e
		}
		copier, ok := text.(bleve.IndexCopyable)
		if !ok {
			_ = text.Close()
			return errors.New("text index does not support snapshots")
		}
		e = copier.CopyTo(bleve.FileSystemDirectory(filepath.Join(stage, "fulltext")))
		if e = errors.Join(e, text.Close()); e != nil {
			return e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	output, err := os.CreateTemp(filepath.Dir(filename), ".warc-backup-*.zip")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(output.Name())) }()
	z := zip.NewWriter(output)
	writeErr := func() error {
		header, e := z.Create("format")
		if e != nil {
			return e
		}
		if _, e = io.WriteString(header, backupFormat); e != nil {
			return e
		}
		data, e := z.Create("badger.backup")
		if e != nil {
			return e
		}
		if e = db.Backup(ctx, data); e != nil {
			return e
		}
		return filepath.WalkDir(stage, func(path string, entry fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if e = ctx.Err(); e != nil {
				return e
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("unexpected snapshot file: %s", path)
			}
			name, e := filepath.Rel(stage, path)
			if e != nil {
				return e
			}
			dest, e := z.Create(filepath.ToSlash(name))
			if e != nil {
				return e
			}
			source, e := os.Open(path)
			if e != nil {
				return e
			}
			_, e = io.Copy(dest, source)
			return errors.Join(e, source.Close())
		})
	}()
	err = errors.Join(writeErr, z.Close(), output.Sync(), output.Close())
	if err != nil {
		return err
	}
	return os.Link(output.Name(), filename)
}

func restoreIndex(ctx context.Context, filename, dir string) (err error) {
	input, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	var magic [4]byte
	_, err = io.ReadFull(input, magic[:])
	if err != nil {
		return err
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if string(magic[:]) != "PK\x03\x04" {
		return cdxstore.Restore(ctx, dir, input)
	}
	dir = filepath.Clean(dir)
	if _, err = os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		return errors.New("restore destination must not exist")
	}
	info, err := input.Stat()
	if err != nil {
		return err
	}
	z, err := zip.NewReader(input, info.Size())
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dir), 0750); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(dir), ".warc-restore-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	if err = unpackBackup(ctx, z, stage); err != nil {
		return err
	}
	format, err := os.ReadFile(filepath.Join(stage, "format"))
	if err != nil || string(format) != backupFormat {
		return errors.New("invalid backup format")
	}
	textDir := filepath.Join(stage, "fulltext")
	if _, e := os.Stat(textDir); errors.Is(e, os.ErrNotExist) {
		textDir = ""
	} else if e != nil {
		return e
	} else {
		idx, err := fulltext.OpenReadOnly(textDir)
		if err != nil {
			return err
		}
		if err := idx.Close(); err != nil {
			return err
		}
	}
	data, err := os.Open(filepath.Join(stage, "badger.backup"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, data.Close()) }()
	return cdxstore.RestoreWithFulltext(ctx, dir, data, textDir)
}

func unpackBackup(ctx context.Context, z *zip.Reader, stage string) error {
	seen := map[string]bool{}
	for _, entry := range z.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name
		if seen[name] || !filepath.IsLocal(name) || strings.Contains(name, "\\") || filepath.ToSlash(filepath.Clean(name)) != name || !entry.Mode().IsRegular() {
			return fmt.Errorf("invalid backup member: %s", name)
		}
		if name != "format" && name != "badger.backup" && !strings.HasPrefix(name, "fulltext/") {
			return fmt.Errorf("unknown backup member: %s", name)
		}
		seen[name] = true
		target := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		source, e := entry.Open()
		if e != nil {
			return e
		}
		dest, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			_ = source.Close()
			return e
		}
		_, e = io.Copy(dest, source)
		if e = errors.Join(e, source.Close(), dest.Close()); e != nil {
			return e
		}
	}
	if !seen["badger.backup"] || !seen["format"] {
		return errors.New("invalid backup format")
	}
	return nil
}
