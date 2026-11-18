package zipfs

import (
	"archive/zip"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/afero"
)

type File struct {
	mu             sync.Mutex
	dirPos         int
	fs             *Fs
	zipfile        *zip.File
	pseudodir      *pseudoDir
	reader         io.ReadCloser
	offset         int64
	isdir, closed  bool
	readerOffset   int64
	checksum       hash.Hash32
	checksumOffset int64
}

func (f *File) Close() (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
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
	if f.zipfile.Method == zip.Store {
		raw, err := f.zipfile.OpenRaw()
		if err != nil {
			return 0, err
		}
		reader, ok := raw.(io.ReaderAt)
		if !ok {
			return 0, syscall.EIO
		}
		n, err := reader.ReadAt(p, off)
		if off == 0 {
			f.checksum = crc32.NewIEEE()
			f.checksumOffset = 0
		}
		if f.checksum != nil {
			if off != f.checksumOffset {
				f.checksum = nil
			} else {
				f.checksum.Write(p[:n])
				f.checksumOffset += int64(n)
				unknownChildCRC := f.fs.allowMissingNestedCRC && f.zipfile.CRC32 == 0 && strings.EqualFold(path.Ext(f.zipfile.Name), ".wacz")
				if f.checksumOffset == size && !unknownChildCRC && f.checksum.Sum32() != f.zipfile.CRC32 {
					return n, zip.ErrChecksum
				}
			}
		}
		return n, err
	}
	if f.reader == nil || off < f.readerOffset {
		if f.reader != nil {
			_ = f.reader.Close()
		}
		r, e := f.zipfile.Open()
		if e != nil {
			return 0, e
		}
		f.reader = r
		f.readerOffset = 0
	}
	if off > f.readerOffset {
		n, e := io.CopyN(io.Discard, f.reader, off-f.readerOffset)
		f.readerOffset += n
		if e != nil {
			return 0, e
		}
	}
	n, e := io.ReadFull(f.reader, p[:int(end-off)])
	f.readerOffset += int64(n)
	if f.readerOffset == size && e == nil {
		var tail [1]byte
		_, check := f.reader.Read(tail[:])
		if check != io.EOF {
			e = check
		}
	}
	if e == nil && n < len(p) {
		e = io.EOF
	}
	return n, e
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
