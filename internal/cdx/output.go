package index

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protodelim"
)

type Output struct {
	mu       sync.Mutex
	writer   io.Writer
	file     *os.File
	gzip     *gzip.Writer
	sorter   *Sorter
	target   string
	protobuf bool
	flushed  bool
	closed   bool
}

func NewOutput(stdout io.Writer, filename, format, tmp string, sorted bool) (*Output, error) {
	o := &Output{writer: stdout, protobuf: format == "cdxpb"}
	if filename != "-" && filename != "" {
		f, e := os.CreateTemp(filepath.Dir(filename), ".warc-index-*")
		if e != nil {
			return nil, e
		}
		o.file = f
		o.target = filename
		o.writer = f
		if strings.HasSuffix(filename, ".gz") {
			o.gzip = gzip.NewWriter(f)
			o.writer = o.gzip
		}
	}
	if !o.protobuf && (sorted || o.file != nil) {
		o.sorter = NewSorter(tmp, true)
	}
	return o, nil
}
func (o *Output) WriteCapture(ctx context.Context, c *Capture) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if e := ctx.Err(); e != nil {
		return e
	}
	if o.closed || o.flushed {
		return os.ErrClosed
	}
	if o.protobuf {
		_, e := protodelim.MarshalTo(o.writer, c)
		return e
	}
	b, e := Line(c)
	if e != nil {
		return e
	}
	if o.sorter != nil {
		return o.sorter.Add(ctx, string(b))
	}
	_, e = o.writer.Write(append(b, '\n'))
	return e
}
func (o *Output) Flush(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.flushed {
		return nil
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if o.sorter != nil {
		if e := o.sorter.WriteTo(ctx, o.writer); e != nil {
			return e
		}
	}
	if o.gzip != nil {
		if e := o.gzip.Close(); e != nil {
			return e
		}
		o.gzip = nil
	}
	if o.file != nil {
		if e := o.file.Sync(); e != nil {
			return e
		}
		if e := o.file.Close(); e != nil {
			return e
		}
		if e := os.Rename(o.file.Name(), o.target); e != nil {
			return e
		}
		o.file = nil
	}
	o.flushed = true
	return nil
}
func (o *Output) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	var e error
	if o.sorter != nil {
		e = o.sorter.Close()
	}
	if o.gzip != nil {
		e = errors.Join(e, o.gzip.Close())
	}
	if o.file != nil {
		e = errors.Join(e, o.file.Close(), os.Remove(o.file.Name()))
	}
	return e
}
