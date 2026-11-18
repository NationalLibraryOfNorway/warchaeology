package zipnum

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type gzipReadCloser struct {
	gz *gzip.Reader
	f  *os.File
}

type normalizedLineReader struct{ reader *bufio.Reader }

// newNormalizedLineReader supports records larger than a fixed scanner limit.
func newNormalizedLineReader(r io.Reader) *normalizedLineReader {
	return &normalizedLineReader{reader: bufio.NewReaderSize(r, 64*1024)}
}

// Next normalizes CRLF and unterminated final records to a single LF.
// A final record is returned before EOF so callers can process it normally.
func (r *normalizedLineReader) Next() ([]byte, error) {
	line, err := r.reader.ReadBytes('\n')
	if len(line) == 0 {
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	line = append(line, '\n')
	if err == io.EOF {
		err = nil
	}
	return line, err
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }

// Close releases both the gzip reader and its underlying file.
func (g *gzipReadCloser) Close() error {
	err := g.gz.Close()
	if fileErr := g.f.Close(); err == nil {
		err = fileErr
	}
	return err
}

// openInput opens plain files or concatenated gzip members based on the suffix.
func openInput(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, partExt) {
		gz, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
		gz.Multistream(true)
		return &gzipReadCloser{gz: gz, f: f}, nil
	}
	return f, nil
}

// readNormalizedLines delivers each record before handling its read error.
func readNormalizedLines(r io.Reader, yield func([]byte) error) error {
	lr := newNormalizedLineReader(r)
	for {
		line, err := lr.Next()
		if len(line) > 0 {
			if yieldErr := yield(line); yieldErr != nil {
				return yieldErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// rawCursor reads sorted files in sequence, keeping one record and one file open.
type rawCursor struct {
	path     string
	paths    []string
	input    io.ReadCloser
	reader   *normalizedLineReader
	previous []byte
	current  []byte
	err      error
	closed   bool
}

// newRawCursor opens the input and advances to the first retained record.
func newRawCursor(path string) (*rawCursor, error) {
	return newFileCursor([]string{path})
}

func newPartCursor(parts []sourcePart) (*rawCursor, error) {
	paths := make([]string, len(parts))
	for i, p := range parts {
		paths[i] = filepath.Join(p.root, partDir, p.name+partExt)
	}
	return newFileCursor(paths)
}

func newFileCursor(paths []string) (*rawCursor, error) {
	c := &rawCursor{paths: paths}
	c.advance()
	if c.err != nil {
		return nil, c.err
	}
	return c, nil
}

// advance validates ordering even for filtered records, and closes at EOF or error.
func (c *rawCursor) advance() {
	c.current = nil
	for c.err == nil && !c.closed {
		if c.input == nil {
			if len(c.paths) == 0 {
				c.closed = true
				return
			}
			c.path, c.paths = c.paths[0], c.paths[1:]
			c.input, c.err = openInput(c.path)
			if c.err != nil {
				c.err = fmt.Errorf("open %q: %w", c.path, c.err)
				c.closed = true
				return
			}
			c.reader = newNormalizedLineReader(c.input)
		}
		line, err := c.reader.Next()
		if err != nil && err != io.EOF {
			c.err = fmt.Errorf("read %q: %w", c.path, err)
			_ = c.Close()
			return
		}
		if len(line) == 0 {
			if err == io.EOF {
				if closeErr := c.input.Close(); closeErr != nil {
					c.err = fmt.Errorf("close %q: %w", c.path, closeErr)
				}
				c.input = nil
				c.reader = nil
				continue
			}
			return
		}
		if len(c.previous) > 0 && bytes.Compare(c.previous, line) > 0 {
			c.err = fmt.Errorf("input %q is not sorted", c.path)
			_ = c.Close()
			return
		}
		c.previous = append(c.previous[:0], line...)
		if lineFilter(line) && len(getPartitionKey(line)) > 0 {
			c.current = line
			return
		}
	}
}

func (c *rawCursor) Peek() []byte { return c.current }

// Pop consumes the current record and prepares the next one.
func (c *rawCursor) Pop() []byte {
	line := c.current
	c.advance()
	return line
}

func (c *rawCursor) Err() error { return c.err }

// Close releases the input once, including when advance already closed it.
func (c *rawCursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	if c.input != nil {
		err := c.input.Close()
		c.input = nil
		c.reader = nil
		return err
	}
	return nil
}

// readInputLines owns the input handle and adds path context to read/close errors.
func readInputLines(path string, yield func([]byte) error) (err error) {
	r, err := openInput(path)
	if err != nil {
		return err
	}
	err = readNormalizedLines(r, yield)
	if closeErr := r.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	return nil
}
