package index

import (
	"bufio"
	"container/heap"
	"context"
	"errors"
	"io"
	"os"
	"sort"
)

// Sorter bounds run memory to 8 MiB and merge fan-in to 32 files. Call Close
// even when Add or WriteTo fails, to remove all temporary files.
type Sorter struct {
	dir    string
	lines  []string
	bytes  int
	runs   []string
	files  []string
	Unique bool
}

func NewSorter(dir string, unique bool) *Sorter { return &Sorter{dir: dir, Unique: unique} }

func (s *Sorter) Add(ctx context.Context, line string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.lines = append(s.lines, line)
	s.bytes += len(line)
	if s.bytes >= 8<<20 {
		return s.spill()
	}
	return nil
}

func (s *Sorter) temporary() (*os.File, error) {
	f, e := os.CreateTemp(s.dir, "warc-sort-*")
	if e == nil {
		s.files = append(s.files, f.Name())
	}
	return f, e
}

func (s *Sorter) spill() (err error) {
	if len(s.lines) == 0 {
		return nil
	}
	sort.Strings(s.lines)
	f, err := s.temporary()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	w := bufio.NewWriter(f)
	last := ""
	for i, l := range s.lines {
		if !s.Unique || i == 0 || l != last {
			if _, err = w.WriteString(l + "\n"); err != nil {
				return err
			}
		}
		last = l
	}
	if err = w.Flush(); err != nil {
		return err
	}
	s.runs = append(s.runs, f.Name())
	s.lines = nil
	s.bytes = 0
	return nil
}

type mergeItem struct {
	line string
	i    int
}

//nolint:recvcheck // container/heap uses pointer Push/Pop and slice accessors.
type mergeHeap []mergeItem

func (h mergeHeap) Len() int { return len(h) }

func (h mergeHeap) Less(i, j int) bool {
	if h[i].line == h[j].line {
		return h[i].i < h[j].i
	}
	return h[i].line < h[j].line
}

func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *mergeHeap) Push(x any) { *h = append(*h, x.(mergeItem)) }

func (h *mergeHeap) Pop() any { x := (*h)[len(*h)-1]; *h = (*h)[:len(*h)-1]; return x }

func (s *Sorter) merge(ctx context.Context, paths []string, out io.Writer) (err error) {
	readers := make([]*bufio.Reader, 0, len(paths))
	files := []*os.File{}
	defer func() {
		for _, f := range files {
			err = errors.Join(err, f.Close())
		}
	}()
	h := &mergeHeap{}
	next := func(i int) error {
		line, e := readSortLine(readers[i])
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		heap.Push(h, mergeItem{line, i})
		return nil
	}
	for _, p := range paths {
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		files = append(files, f)
		readers = append(readers, bufio.NewReader(f))
		if e = next(len(readers) - 1); e != nil {
			return e
		}
	}
	last := ""
	first := true
	for h.Len() > 0 {
		if e := ctx.Err(); e != nil {
			return e
		}
		x := heap.Pop(h).(mergeItem)
		if first || !s.Unique || x.line != last {
			if _, e := io.WriteString(out, x.line+"\n"); e != nil {
				return e
			}
		}
		first = false
		last = x.line
		if e := next(x.i); e != nil {
			return e
		}
	}
	return nil
}

func readSortLine(r *bufio.Reader) (string, error) {
	b, e := r.ReadString('\n')
	if len(b) > 0 {
		if b[len(b)-1] == '\n' {
			b = b[:len(b)-1]
		}
		return b, nil
	}
	return "", e
}

func (s *Sorter) WriteTo(ctx context.Context, w io.Writer) error {
	if e := s.spill(); e != nil {
		return e
	}
	for len(s.runs) > 32 {
		var next []string
		for i := 0; i < len(s.runs); i += 32 {
			f, e := s.temporary()
			if e != nil {
				return e
			}
			bw := bufio.NewWriter(f)
			e = s.merge(ctx, s.runs[i:min(i+32, len(s.runs))], bw)
			e = errors.Join(e, bw.Flush(), f.Close())
			if e != nil {
				return e
			}
			next = append(next, f.Name())
		}
		s.runs = next
	}
	return s.merge(ctx, s.runs, w)
}

func (s *Sorter) Close() error {
	var err error
	for _, p := range s.files {
		err = errors.Join(err, os.Remove(p))
	}
	s.files = nil
	return err
}
