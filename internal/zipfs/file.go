package zipfs

import (
	"archive/zip"
	"io"
	"os"
	"path"
	"sort"
	"sync"
	"syscall"

	"github.com/spf13/afero"
)

type File struct {
	mu            sync.Mutex
	dirPos        int
	readErr       error
	fs            *Fs
	zipfile       *zip.File
	pseudodir     *pseudoDir
	reader        io.ReadCloser
	offset        int64
	isdir, closed bool
	buf           []byte
}

func (f *File) fillBuffer(end int64) error {
	if f.readErr != nil {
		return f.readErr
	}
	if f.reader == nil {
		r, err := f.zipfile.Open()
		if err != nil {
			return err
		}
		f.reader = r
	}
	size := int64(f.zipfile.UncompressedSize64)
	if end > size {
		end = size
	}
	if end > int64(len(f.buf)) {
		buf := make([]byte, end-int64(len(f.buf)))
		n, err := io.ReadFull(f.reader, buf)
		f.buf = append(f.buf, buf[:n]...)
		if err != nil {
			f.readErr = err
			return err
		}
	}
	if int64(len(f.buf)) == size {
		// Force validation of the checksum even for an exact-size read.
		var extra [1]byte
		_, err := f.reader.Read(extra[:])
		if err != nil && err != io.EOF {
			f.readErr = err
			return err
		}
	}
	return nil
}

func (f *File) Close() (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.buf = nil
	if f.reader != nil {
		err = f.reader.Close()
		f.reader = nil
	}
	return
}

func (f *File) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.readAt(p, f.offset)
	f.offset += int64(n)
	return n, err
}
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readAt(p, off)
}
func (f *File) readAt(p []byte, off int64) (int, error) {
	if f.closed {
		return 0, afero.ErrFileClosed
	}
	if f.isdir || f.pseudodir != nil {
		return 0, syscall.EISDIR
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	size := int64(f.zipfile.UncompressedSize64)
	if off >= size {
		return 0, io.EOF
	}
	end := size
	if int64(len(p)) < size-off {
		end = off + int64(len(p))
	}
	err := f.fillBuffer(end)
	if off >= int64(len(f.buf)) {
		return 0, err
	}
	n := copy(p, f.buf[off:])
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.isdir || f.pseudodir != nil {
		return 0, syscall.EISDIR
	}
	if f.closed {
		return 0, afero.ErrFileClosed
	}
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.offset
	case io.SeekEnd:
		offset += int64(f.zipfile.UncompressedSize64)
	default:
		return 0, syscall.EINVAL
	}
	if offset < 0 {
		return 0, afero.ErrOutOfRange
	}
	f.offset = offset
	return offset, nil
}

func (f *File) Write(p []byte) (n int, err error) { return 0, syscall.EPERM }

func (f *File) WriteAt(p []byte, off int64) (n int, err error) { return 0, syscall.EPERM }

func (f *File) Name() string {
	if f.zipfile == nil {
		if f.pseudodir != nil {
			return f.pseudodir.path
		}
		return "/"
	}
	return path.Join(splitpath(f.zipfile.Name))
}

func (f *File) getDirEntries() (map[string]*zip.File, error) {
	if !f.isdir && f.pseudodir == nil {
		return nil, syscall.ENOTDIR
	}
	name := f.Name()
	entries, ok := f.fs.files[name]
	if !ok {
		return nil, &os.PathError{Op: "readdir", Path: name, Err: syscall.ENOENT}
	}
	return entries, nil
}

func (f *File) Readdir(count int) ([]os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, afero.ErrFileClosed
	}
	entries, err := f.getDirEntries()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	if count > 0 && f.dirPos == len(names) {
		return nil, io.EOF
	}
	end := len(names)
	if count > 0 && count < end-f.dirPos {
		end = f.dirPos + count
	}
	result := make([]os.FileInfo, 0, end-f.dirPos)
	for _, name := range names[f.dirPos:end] {
		if entry := entries[name]; entry != nil {
			result = append(result, entry.FileInfo())
		} else {
			result = append(result, pseudoDir{name})
		}
	}
	f.dirPos = end
	return result, nil
}
func (f *File) Readdirnames(count int) ([]string, error) {
	entries, err := f.Readdir(count)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, err
}

func (f *File) Stat() (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, afero.ErrFileClosed
	}
	if f.zipfile == nil {
		if f.pseudodir != nil {
			return f.pseudodir, nil
		}
		return &pseudoRoot{}, nil
	}
	return f.zipfile.FileInfo(), nil
}

func (f *File) Sync() error { return nil }

func (f *File) Truncate(size int64) error { return syscall.EPERM }

func (f *File) WriteString(s string) (ret int, err error) { return 0, syscall.EPERM }
