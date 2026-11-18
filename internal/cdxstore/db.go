package cdxstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"strings"
	"sync"

	index "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx/schema"
	"google.golang.org/protobuf/proto"
)

const formatVersion = "warchaeology-captures-v2;whatwg-url-surt-v0.9.0"

var ErrReadOnly = errors.New("database is read-only")

type DB struct {
	closing    bool
	deleted    map[string]bool
	visibility sync.RWMutex
	kv         kv
	readOnly   bool
	mu         sync.Mutex
	tasks      map[string]context.CancelFunc
	wg         sync.WaitGroup
}

func Open(ctx context.Context, o Options) (*DB, error) {
	k, e := openKV(ctx, o)
	if e != nil {
		return nil, e
	}
	fail := func(e error) (*DB, error) { return nil, errors.Join(e, k.Close()) }
	v, e := k.Get(ctx, "!format")
	if e != nil {
		return fail(e)
	}
	if len(v) == 0 {
		nonempty := false
		if e = k.Scan(ctx, "", "", func(_ string, _ []byte) error { nonempty = true; return errStop }); e != nil && !errors.Is(e, errStop) {
			return fail(e)
		}
		if nonempty || o.ReadOnly {
			return fail(fmt.Errorf("unversioned capture database; reindex into a new database"))
		}
		if e = k.Put(ctx, "!format", []byte(formatVersion)); e != nil {
			return fail(e)
		}
	} else if string(v) != formatVersion {
		return fail(fmt.Errorf("incompatible capture database; reindex with this version"))
	}
	d := &DB{kv: k, readOnly: o.ReadOnly, tasks: map[string]context.CancelFunc{}, deleted: map[string]bool{}}
	pending, e := k.Get(ctx, resetKey)
	if e != nil {
		return fail(e)
	}
	if len(pending) > 0 {
		if o.ReadOnly {
			return fail(errors.New("interrupted index reset requires writable recovery"))
		}
		if e = d.finishReset(ctx); e != nil {
			return fail(e)
		}
	}
	return d, nil
}

var errStop = errors.New("stop scan")

func (d *DB) Close() error {
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return nil
	}
	d.closing = true
	for _, c := range d.tasks {
		c()
	}
	d.mu.Unlock()
	d.wg.Wait()
	return d.kv.Close()
}
func (d *DB) Flush(ctx context.Context) error { return ctx.Err() }
func identity(source string) string {
	h := sha256.Sum256([]byte(source))
	return hex.EncodeToString(h[:])
}
func captureKey(c *index.Capture) string {
	return "c/" + c.GetSsu() + " " + c.GetSts().AsTime().UTC().Format("20060102150405") + " " + identity(c.GetSource()) + " " + fmt.Sprintf("%020d", c.GetOffset()) + " " + c.GetGeneration()
}
func (d *DB) WriteCapture(ctx context.Context, c *index.Capture) error {
	if d.readOnly {
		return ErrReadOnly
	}
	if g, ok := ctx.Value(generationKey{}).(string); ok {
		c.Generation = g
	}
	b, e := proto.Marshal(c)
	if e != nil {
		return e
	}
	if e = d.kv.Put(ctx, captureKey(c), b); e != nil {
		return e
	}
	if c.GetRid() != "" {
		return d.kv.Put(ctx, "i/"+c.GetRid()+"/"+identity(c.GetSource())+"/"+fmt.Sprintf("%020d", c.GetOffset())+"/"+c.GetGeneration(), b)
	}
	return nil
}

func (d *DB) Scan(ctx context.Context, lower, upper string, emit func(*index.Capture) error) error {
	d.visibility.RLock()
	defer d.visibility.RUnlock()
	if upper == "" {
		upper = "\xff"
	}
	return d.kv.Scan(ctx, "c/"+lower, "c/"+upper, func(_ string, v []byte) error {
		c := &index.Capture{}
		if e := proto.Unmarshal(v, c); e != nil {
			return e
		}
		visible, e := d.visible(ctx, c)
		if e != nil {
			return e
		}
		if !visible {
			return nil
		}
		return emit(c)
	})
}

// CapturesByID visits every occurrence, including duplicate IDs in one file.
func (d *DB) CapturesByID(ctx context.Context, id string, emit func(*index.Capture) error) error {
	return d.kv.Scan(ctx, "i/"+id+"/", "i/"+id+"0", func(_ string, b []byte) error {
		c := &index.Capture{}
		if err := proto.Unmarshal(b, c); err != nil {
			return err
		}
		visible, err := d.visible(ctx, c)
		if err != nil || !visible {
			return err
		}
		return emit(c)
	})
}

func (d *DB) StorageRefById(ctx context.Context, id string) (ref string, err error) {
	err = d.CapturesByID(ctx, id, func(c *index.Capture) error {
		if ref != "" && ref != c.GetRef() {
			return fmt.Errorf("ambiguous record ID")
		}
		ref = c.GetRef()
		return nil
	})
	return
}

func (d *DB) GetStorageRef(ctx context.Context, id string) (string, error) {
	return d.StorageRefById(ctx, id)
}
func (d *DB) PathByFilename(name string) (string, error) {
	fi, e := d.GetFileInfo(context.Background(), name)
	if e != nil {
		return "", e
	}
	if fi == nil {
		return "", fmt.Errorf("file not indexed")
	}
	return fi.GetPath(), nil
}
func (d *DB) GetFileInfo(ctx context.Context, name string) (*schema.FileInfo, error) {
	return d.sourceInfo(ctx, name)
}
func (d *DB) Delete(ctx context.Context) error {
	if d.readOnly {
		return ErrReadOnly
	}
	if err := d.kv.Put(ctx, resetKey, []byte{1}); err != nil {
		return err
	}
	return d.finishReset(ctx)
}

const resetKey = "!reset"

func (d *DB) finishReset(ctx context.Context) error {
	var keys []string
	if e := d.kv.Scan(ctx, "", "", func(k string, _ []byte) error {
		if k != "!format" && k != resetKey {
			keys = append(keys, k)
		}
		return nil
	}); e != nil {
		return e
	}
	for _, k := range keys {
		if e := d.kv.Delete(ctx, k); e != nil {
			return e
		}
	}
	return d.kv.Delete(ctx, resetKey)
}
func queryFromRequest(req index.Request) index.Query {
	key := req.Ssurt()
	q := index.Query{Key: key, Match: "exact", Lower: key + " ", Upper: key + "!", Limit: req.Limit(), Closest: req.Closest()}
	if key == "" {
		q.Lower = ""
		q.Upper = ""
		q.Match = "prefix"
	}
	switch req.MatchType() {
	case index.MatchTypePrefix:
		q.Match = "prefix"
		q.Lower = key
		q.Upper = key + "\xff"
	case index.MatchTypeHost:
		q.Match = "host"
		q.Lower = strings.SplitN(key, ")", 2)[0] + ")"
		q.Upper = q.Lower + "\xff"
	case index.MatchTypeDomain:
		q.Match = "domain"
		q.Lower = strings.SplitN(key, ")", 2)[0]
		q.Upper = q.Lower + "\xff"
	}
	switch req.Sort() {
	case index.SortDesc:
		q.Order = "reverse"
	case index.SortClosest:
		q.Order = "closest"
	}
	return q
}
