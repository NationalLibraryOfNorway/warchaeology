package filewalker

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	archivefs "github.com/nationallibraryofnorway/warchaeology/v5/internal/fs"
	"github.com/spf13/afero"
)

type FileWalker struct {
	ArchiveHandler func(context.Context, afero.Fs, string) (bool, error)
	SourceRoot     string
	Recursive      bool
	FollowSymlinks bool
	Suffixes       []string
	Fs             afero.Fs
	processedPaths StringSet
	closers        []io.Closer
	closed         bool
}

func WithRecursive(recursive bool) func(*FileWalker) {
	return func(w *FileWalker) {
		w.Recursive = recursive
	}
}

func WithFollowSymlinks(followSymlinks bool) func(*FileWalker) {
	return func(w *FileWalker) {
		w.FollowSymlinks = followSymlinks
	}
}

func WithSuffixes(suffixes []string) func(*FileWalker) {
	return func(w *FileWalker) {
		w.Suffixes = suffixes
	}
}

// WithFs transfers cleanup responsibility to the walker. Close releases the
// filesystem if it implements io.Closer.
func WithFs(fs afero.Fs) func(*FileWalker) {
	return func(w *FileWalker) {
		w.Fs = fs
		if c, ok := fs.(io.Closer); ok {
			w.closers = append(w.closers, c)
		}
	}
}

// Close releases mounts in reverse order after all asynchronous consumers finish.
func (fw *FileWalker) Close() error {
	if fw.closed {
		return nil
	}
	fw.closed = true
	var err error
	for _, v := range slices.Backward(fw.closers) {
		err = errors.Join(err, v.Close())
	}
	fw.closers = nil
	return err
}

func New(options ...func(*FileWalker)) *FileWalker {
	opts := &FileWalker{
		processedPaths: NewStringSet(),
	}
	for _, opt := range options {
		opt(opts)
	}
	return opts
}

func (fw *FileWalker) HasSuffix(path string) bool {
	if len(fw.Suffixes) == 0 {
		return true
	}
	for _, suffix := range fw.Suffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func (fw *FileWalker) Walk(ctx context.Context, path string, walkFn func(fs afero.Fs, path string, err error) error) error {
	if fw.closed {
		return os.ErrClosed
	}
	roots := []string(nil)
	if fw.SourceRoot != "" {
		roots = []string{fw.SourceRoot}
	}
	return fw.walkDir(ctx, fw.Fs, path, path, "", roots, walkFn)
}

func (fw *FileWalker) walkDir(ctx context.Context, currentFs afero.Fs, root string, dirName string, mountPrefix string, roots []string, walkFn func(fs afero.Fs, path string, err error) error) error {
	walkImpl := func(walkFn filepath.WalkFunc) error {
		// Use the custom walk with path.Join (forward slashes) for virtual
		// filesystems (zip, tar, ftp) where entry names use forward slashes.
		// Use afero.Walk for OS filesystems to preserve symlink handling
		// via lstatIfPossible.
		if currentFs.Name() != "OsFs" {
			return Walk(currentFs, dirName, walkFn)
		}
		return afero.Walk(currentFs, dirName, walkFn)
	}
	return walkImpl(func(path string, info fs.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return walkFn(currentFs, path, err)
		}

		logicalPath := path
		if mountPrefix != "" {
			logicalPath = mountPrefix + "!" + path
		}

		if info.IsDir() {
			// skip already processed directories
			if fw.processedPaths.Contains(logicalPath) {
				return filepath.SkipDir
			} else {
				fw.processedPaths.Add(logicalPath)
			}
			// always process the path that is equal to the root directory
			if root == path {
				return nil
			}
			// skip directories when recursive option is not set
			// An archive is one input: visit all of its members. Recursive only
			// controls directory traversal outside mounted archives.
			if !fw.Recursive && mountPrefix == "" && currentFs.Name() != "zipfs" && currentFs.Name() != "tarfs" {
				return filepath.SkipDir
			}
			return nil
		}

		// handle symlink
		if !info.Mode().IsRegular() {
			if !fw.FollowSymlinks {
				return nil
			}
			linkReader, ok := currentFs.(afero.LinkReader)
			if !ok {
				return afero.ErrNoReadlink
			}
			linkPath, err := linkReader.ReadlinkIfPossible(path)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(linkPath) {
				linkPath = filepath.Join(filepath.Dir(path), linkPath)
			}
			return fw.walkDir(ctx, currentFs, root, linkPath, mountPrefix, roots, walkFn)
		}

		if fw.ArchiveHandler != nil {
			if fw.processedPaths.Contains(logicalPath) {
				return nil
			}
			handled, e := fw.ArchiveHandler(ctx, &sourceFS{Fs: currentFs, roots: roots}, path)
			if e != nil {
				return walkFn(currentFs, path, e)
			}
			if handled {
				fw.processedPaths.Add(logicalPath)
				return nil
			}
		}
		mountedFs, resolveErr := archivefs.ResolveFilesystem(currentFs, path)
		if resolveErr != nil {
			return walkFn(currentFs, path, resolveErr)
		}
		if mountedFs != currentFs {
			if c, ok := mountedFs.(io.Closer); ok {
				fw.closers = append(fw.closers, c)
			}
			return fw.walkDir(ctx, mountedFs, "/", "/", logicalPath, append(append([]string(nil), roots...), path), walkFn)
		}

		// filter files by suffix
		if !fw.HasSuffix(path) {
			return nil
		}
		// skip already processed files
		if fw.processedPaths.Contains(logicalPath) {
			return nil
		} else {
			fw.processedPaths.Add(logicalPath)
		}

		return walkFn(&sourceFS{Fs: currentFs, roots: roots}, path, nil)
	})
}

// Walk walks an afero.Fs using forward-slash path joining (path.Join)
// instead of OS-native separators (filepath.Join). This is needed because
// afero.Walk hardcodes filepath.Join, which produces backslash paths on Windows
// that don't match virtual filesystem conventions (zip, tar, etc.).
func Walk(fs afero.Fs, root string, walkFn filepath.WalkFunc) error {
	info, err := fs.Stat(root)
	if err != nil {
		return walkFn(root, nil, err)
	}
	return walk(fs, root, info, walkFn)
}

func readDirNames(afs afero.Fs, dirname string) ([]string, error) {
	f, err := afs.Open(dirname)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func walk(afs afero.Fs, name string, info os.FileInfo, walkFn filepath.WalkFunc) error {
	err := walkFn(name, info, nil)
	if err != nil {
		if info.IsDir() && err == filepath.SkipDir {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}

	names, err := readDirNames(afs, name)
	if err != nil {
		return walkFn(name, info, err)
	}

	for _, child := range names {
		childPath := path.Join(name, child)
		childInfo, err := afs.Stat(childPath)
		if err != nil {
			if err := walkFn(childPath, nil, err); err != nil && err != filepath.SkipDir {
				return err
			}
		} else {
			if err := walk(afs, childPath, childInfo, walkFn); err != nil {
				if !childInfo.IsDir() || err != filepath.SkipDir {
					return err
				}
			}
		}
	}
	return nil
}
