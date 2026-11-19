// cdxstorage runs one engine/encoding/workload per isolated process.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"google.golang.org/protobuf/encoding/protowire"
)

type pair struct {
	K, V   []byte
	Delete bool
}
type engine struct {
	b      *badger.DB
	p      *pebble.DB
	mu     sync.Mutex
	waloff bool
}

func check(e error) {
	if e != nil {
		panic(e)
	}
}
func open(name, dir string, waloff bool) *engine {
	e := &engine{waloff: waloff}
	var err error
	if name == "badger" {
		e.b, err = badger.Open(badger.DefaultOptions(dir).WithLogger(nil).WithSyncWrites(!waloff).WithCompression(options.Snappy).WithBlockCacheSize(64 << 20).WithIndexCacheSize(16 << 20).WithMemTableSize(16 << 20).WithValueLogFileSize(64 << 20))
	} else {
		o := &pebble.Options{DisableWAL: waloff, MemTableSize: 16 << 20, Cache: pebble.NewCache(64 << 20)}
		// Pebble defaults every level to Snappy.
		e.p, err = pebble.Open(dir, o)
		o.Cache.Unref()
	}
	check(err)
	return e
}
func (e *engine) close() {
	if e.b != nil {
		check(e.b.Close())
	} else {
		check(e.p.Close())
	}
}
func (e *engine) write(ps []pair) {
	if e.b != nil {
		check(e.b.Update(func(t *badger.Txn) error {
			for _, p := range ps {
				var err error
				if p.Delete {
					err = t.Delete(p.K)
				} else {
					err = t.Set(p.K, p.V)
				}
				if err != nil {
					return err
				}
			}
			return nil
		}))
		return
	}
	b := e.p.NewBatch()
	defer b.Close()
	for _, p := range ps {
		if p.Delete {
			check(b.Delete(p.K, nil))
		} else {
			check(b.Set(p.K, p.V, nil))
		}
	}
	check(b.Commit(&pebble.WriteOptions{Sync: !e.waloff}))
}
func (e *engine) register(p pair) bool {
	if e.b != nil {
		for {
			added := false
			err := e.b.Update(func(t *badger.Txn) error {
				_, err := t.Get(p.K)
				if err == badger.ErrKeyNotFound {
					added = true
					return t.Set(p.K, p.V)
				}
				return err
			})
			if err == badger.ErrConflict {
				continue
			}
			check(err)
			return added
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, c, err := e.p.Get(p.K)
	if err == nil {
		check(c.Close())
		return false
	}
	if err != pebble.ErrNotFound {
		check(err)
	}
	e.write([]pair{p})
	return true
}
func (e *engine) scan(lo, hi []byte, fn func([]byte, []byte) bool) {
	if e.b != nil {
		check(e.b.View(func(t *badger.Txn) error {
			it := t.NewIterator(badger.DefaultIteratorOptions)
			defer it.Close()
			for it.Seek(lo); it.Valid(); it.Next() {
				x := it.Item()
				k := x.Key()
				if len(hi) > 0 && bytes.Compare(k, hi) >= 0 {
					break
				}
				stop := false
				check(x.Value(func(v []byte) error { stop = !fn(k, v); return nil }))
				if stop {
					break
				}
			}
			return nil
		}))
		return
	}
	it, err := e.p.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	check(err)
	defer it.Close()
	for ok := it.First(); ok; ok = it.Next() {
		if !fn(it.Key(), it.Value()) {
			break
		}
	}
	check(it.Error())
}
func (e *engine) compact() {
	if e.b != nil {
		check(e.b.Flatten(2))
		for e.b.RunValueLogGC(.5) == nil {
		}
	} else {
		check(e.p.Flush())
		check(e.p.Compact(context.Background(), []byte{0}, []byte{255}, true))
	}
}
func str(b []byte, n protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, n, protowire.BytesType)
	return protowire.AppendString(b, s)
}
func num(b []byte, n protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, n, protowire.VarintType)
	return protowire.AppendVarint(b, v)
}
func record(i, n, seed int, encoding, work string, changed bool) []pair {
	u := i % (n / 5)
	payload := i % (n / 3)
	d := sha256.Sum256(fmt.Appendf(nil, "%d/%d", seed, payload))
	file := fmt.Sprintf("crawl-%04d.warc.gz", i/1000)
	url := fmt.Sprintf("https://host%d.example/path/%d?q=%d", u%100, u, u%7)
	key := fmt.Sprintf("c/example,host%d)/path/%d?q=%d %s %08d", u%100, u, u%7, time.Unix(1577836800+int64(i)*31, 0).UTC().Format("20060102150405"), i)
	var v []byte
	if encoding == "current" {
		v = str(v, 1, url)
		v = str(v, 3, "sha256:"+hex.EncodeToString(d[:]))
		v = str(v, 15, key[2:])
		v = str(v, 18, file)
		v = str(v, 19, "/archive/"+file)
		v = str(v, 6, fmt.Sprintf("warcfile:/archive/%s#%d", file, i%1000*100003))
		v = str(v, 7, fmt.Sprintf("urn:uuid:%08x-0000-4000-8000-%012x", seed, i))
		v = str(v, 23, fmt.Sprintf(`{"url":%q,"digest":%q,"filename":%q,"offset":"%d","custom":9007199254740993}`, url, hex.EncodeToString(d[:]), file, i%1000*100003))
	} else {
		v = str(v, 1, url)
		v = str(v, 3, string(append([]byte{3}, d[:]...)))
		v = num(v, 18, uint64(i/1000))
		v = str(v, 23, `{"custom":9007199254740993}`)
		v = str(v, 7, fmt.Sprintf("urn:uuid:%08x-0000-4000-8000-%012x", seed, i))
	}
	v = num(v, 16, uint64(1577836800+i*31))
	v = num(v, 20, uint64(i%1000*100003))
	v = num(v, 10, uint64(100000+i%997))
	v = str(v, 5, "text/html")
	if i%10 == 0 {
		v = str(v, 5, "warc/revisit")
	}
	if i%17 == 0 {
		v = str(v, 21, "POST")
		v = str(v, 22, "a=1&b=two")
	}
	if changed {
		v = str(v, 30, "changed-value")
	}
	if work == "dedup" {
		key = "d/" + string(d[:])
		v = nil
		v = str(v, 1, fmt.Sprintf("https://donor.example/%d", payload))
		v = num(v, 2, uint64(payload))
		if encoding == "current" {
			key = "d/sha256:" + hex.EncodeToString(d[:])
			v = str(v, 3, fmt.Sprintf("<urn:uuid:%08x-0000-4000-8000-%012x>", seed, payload))
		}
		return []pair{{[]byte(key), v, false}}
	}
	out := []pair{{[]byte(key), v, false}}
	if work == "inventory" {
		ref := []byte(key)
		if encoding == "current" {
			ref = v
		}
		out = append(out, pair{fmt.Appendf(nil, "i/%08d", i), ref, false}, pair{fmt.Appendf(nil, "f/%08d", i), d[:], false}, pair{fmt.Appendf(nil, "p/%08d", i), fmt.Appendf(nil, "year=2020;donor=%d", payload), false})
	}
	if encoding == "compact" {
		out = append(out, pair{fmt.Appendf(nil, "dict/%04d", i/1000), []byte(file), false})
	}
	return out
}
func summary(e *engine) (int, string) {
	n := 0
	h := sha256.New()
	e.scan(nil, nil, func(k, v []byte) bool {
		n++
		fmt.Fprintf(h, "%d:", len(k))
		h.Write(k)
		fmt.Fprintf(h, "%d:", len(v))
		h.Write(v)
		return true
	})
	return n, hex.EncodeToString(h.Sum(nil))
}
func main() {
	name := flag.String("engine", "badger", "")
	enc := flag.String("encoding", "compact", "")
	work := flag.String("workload", "capture", "")
	dir := flag.String("dir", "", "")
	n := flag.Int("n", 1000000, "")
	seed := flag.Int("seed", 11, "")
	phase := flag.String("phase", "ingest", "")
	order := flag.String("order", "ingestion", "")
	off := flag.Bool("wal-off", false, "")
	flag.Parse()
	if *dir == "" || *n < 100 {
		panic("dir required, n >= 100")
	}
	if *off && *work != "inventory" {
		panic("WAL-off only for disposable inventory")
	}
	var sortedInput *os.File
	generationStart := time.Now()
	if *phase == "ingest" && *order == "sorted" && *work != "dedup" {
		var err error
		sortedInput, err = os.CreateTemp("", "cdxstorage-input-*")
		check(err)
		defer os.Remove(sortedInput.Name())
		defer sortedInput.Close()
		command := exec.Command("sort", "-S", "64M")
		command.Env = append(os.Environ(), "LC_ALL=C")
		command.Stdout = sortedInput
		input, err := command.StdinPipe()
		check(err)
		check(command.Start())
		writer := bufio.NewWriterSize(input, 1<<20)
		for i := range *n {
			for _, p := range record(i, *n, *seed, *enc, *work, false) {
				_, err = fmt.Fprintf(writer, "%x %x\n", p.K, p.V)
				check(err)
			}
		}
		check(writer.Flush())
		check(input.Close())
		check(command.Wait())
		_, err = sortedInput.Seek(0, 0)
		check(err)
	}
	generationSeconds := time.Since(generationStart).Seconds()
	e := open(*name, *dir, *off)
	start := time.Now()
	switch *phase {
	case "ingest":
		if *work == "dedup" {
			var added atomic.Int64
			var wg sync.WaitGroup
			for worker := range 8 {
				wg.Go(func() {
					for i := worker; i < *n; i += 8 {
						if e.register(record(i, *n, *seed, *enc, *work, false)[0]) {
							added.Add(1)
						}
					}
				})
			}
			wg.Wait()
			if added.Load() != int64(*n/3) {
				panic("donor count mismatch")
			}
		} else {
			batch := make([]pair, 0, 1000)
			add := func(p pair) {
				batch = append(batch, p)
				if len(batch) >= 500 {
					e.write(batch)
					batch = batch[:0]
				}
			}
			if sortedInput != nil {
				scanner := bufio.NewScanner(sortedInput)
				scanner.Buffer(make([]byte, 4096), 1<<20)
				for scanner.Scan() {
					k, v, _ := strings.Cut(scanner.Text(), " ")
					key, err := hex.DecodeString(k)
					check(err)
					value, err := hex.DecodeString(v)
					check(err)
					add(pair{K: key, V: value})
				}
				check(scanner.Err())
			} else {
				for i := range *n {
					for _, p := range record(i, *n, *seed, *enc, *work, false) {
						add(p)
					}
				}
			}
			if len(batch) > 0 {
				e.write(batch)
			}
		}

	case "compact":
		e.compact()
	case "churn":
		batch := []pair{}
		for i := 0; i < *n; i += 5 {
			ps := record(i, *n, *seed, *enc, *work, false)
			for _, p := range ps {
				if !bytes.HasPrefix(p.K, []byte("dict/")) {
					p.Delete = true
					batch = append(batch, p)
				}
			}
			if i%10 == 0 {
				batch = append(batch, record(i, *n, *seed, *enc, *work, true)...)
			}
			if len(batch) > 500 {
				e.write(batch)
				batch = nil
			}
		}
		if len(batch) > 0 {
			e.write(batch)
		}
	case "verify":
	default:
		panic("invalid phase")
	}
	elapsed := time.Since(start).Seconds()
	count, sum := summary(e)
	start = time.Now()
	for i := range 100 {
		p := record(i, *n, *seed, *enc, *work, false)[0]
		e.scan(p.K, append(append([]byte{}, p.K...), 0), func(k, v []byte) bool { return false })
	}
	q := time.Since(start).Seconds() / 100
	e.close()
	check(json.NewEncoder(os.Stdout).Encode(map[string]any{
		"order":              *order,
		"generation_seconds": generationSeconds,
		"engine":             *name,
		"encoding":           *enc,
		"workload":           *work,
		"seed":               *seed,
		"captures":           *n,
		"unique_urls":        *n / 5,
		"unique_payloads":    *n / 3,
		"wal_off":            *off && *name == "pebble",
		"disposable":         *off,
		"sync_writes":        !*off,
		"phase":              *phase,
		"seconds":            elapsed,
		"entries":            count,
		"checksum":           sum,
		"query_seconds":      q,
	}))
}
