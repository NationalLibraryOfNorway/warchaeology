package redupe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v3"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/workerpool"
	"github.com/spf13/afero"
)

type entry struct {
	Capture       *cdx.Capture `json:"capture"`
	Fingerprint   string       `json:"fingerprint"`
	Profile       string       `json:"profile,omitempty"`
	Segmented     bool         `json:"segmented,omitempty"`
	Truncated     bool         `json:"truncated,omitempty"`
	PayloadLength int64        `json:"payloadLength"`
	Context       []string     `json:"context,omitempty"`
	Issue         string       `json:"issue,omitempty"`
}

type sourceInfo struct {
	Container *sourceInfo `json:"container,omitempty"`
	Locator   string      `json:"locator"`
	Size      int64       `json:"size"`
	Modified  time.Time   `json:"modified"`
}

func hashString(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func (o *RedupeOptions) put(key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return o.state.Update(func(tx *badger.Txn) error { return tx.Set([]byte(key), data) })
}

func (o *RedupeOptions) get(key string, value any) error {
	return o.state.View(func(tx *badger.Txn) error {
		item, err := tx.Get([]byte(key))
		if err != nil {
			return err
		}
		return item.Value(func(data []byte) error { return json.Unmarshal(data, value) })
	})
}

func (o *RedupeOptions) scan(prefix string, visit func([]byte) error) error {
	return o.state.View(func(tx *badger.Txn) error {
		it := tx.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek([]byte(prefix)); it.ValidForPrefix([]byte(prefix)); it.Next() {
			if err := it.Item().Value(visit); err != nil {
				return err
			}
		}
		return nil
	})
}

func (o *RedupeOptions) entry(ref string) (*entry, error) {
	e := &entry{}
	err := o.get("x/record/"+hashString(ref), e)
	return e, err
}

func (o *RedupeOptions) inventory(ctx context.Context) (_ string, resultErr error) {
	if err := o.state.DropPrefix([]byte("x/")); err != nil {
		return "", err
	}
	sorter := cdx.NewSorter(o.workDir, true)
	defer func() { resultErr = errors.Join(resultErr, sorter.Close()) }()
	var mu sync.Mutex
	var failures error
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fail := func(err error) { mu.Lock(); failures = errors.Join(failures, err); mu.Unlock(); cancel() }
	pool := workerpool.New(ctx, o.Concurrency)
	for _, path := range o.Paths {
		err := o.FileWalker.Walk(ctx, path, func(fs afero.Fs, path string, err error) error {
			if err != nil {
				return err
			}
			return pool.Submit(func() {
				_, err := filewalker.PrePostHookContext(ctx, fs, path, o.OpenInputFileHook, o.CloseInputFileHook,
					func(fs afero.Fs, path string) (stat.Result, error) {
						return o.inventoryFile(ctx, fs, path, func(e *entry) error {
							c := e.Capture
							line := c.GetSts().AsTime().UTC().Format("20060102150405.000000000") + " " + hex.EncodeToString([]byte(c.GetSource())) + " " + fmt.Sprintf("%020d", c.GetOffset()) + " " + hex.EncodeToString([]byte(c.GetRef()))
							mu.Lock()
							defer mu.Unlock()
							return sorter.Add(ctx, line)
						})
					})
				// A hook requesting a skip is an error here: preservation forbids silent omission.
				if err != nil {
					fail(fmt.Errorf("%s: %w", path, err))
				}
			})
		})
		if err != nil {
			fail(err)
			break
		}
	}
	pool.CloseWait()
	if failures != nil {
		return "", failures
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ordered := filepath.Join(o.workDir, "ordered")
	f, err := os.Create(ordered)
	if err != nil {
		return "", err
	}
	err = errors.Join(sorter.WriteTo(ctx, f), f.Close())
	return ordered, err
}

func (o *RedupeOptions) inventoryFile(ctx context.Context, fs afero.Fs, path string, emit func(*entry) error) (result stat.Result, err error) {
	result = stat.NewResult(path)
	source, err := filewalker.SourceLocator(fs, path)
	if err != nil {
		return result, err
	}
	container, err := sourceContainer(source)
	if err != nil {
		return result, err
	}
	file, err := fs.Open(path)
	if err != nil {
		return result, err
	}
	info, err := file.Stat()
	if err != nil {
		return result, errors.Join(err, file.Close())
	}
	reader, err := gowarc.NewWarcFileReaderFromStream(file, 0, gowarc.WithAddMissingRecordId(false), gowarc.WithFixDigest(false))
	if err != nil {
		return result, errors.Join(err, file.Close())
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	fileCtx, err := o.catalog.BeginFile(ctx, source)
	if err != nil {
		return result, err
	}
	for rec, readErr := range reader.Records() {
		if readErr != nil {
			return result, errors.Join(readErr, rec.Close())
		}
		e, recordErr := inventoryRecord(rec, source, path)
		closeErr := rec.Close()
		if recordErr != nil || closeErr != nil {
			return result, errors.Join(recordErr, closeErr)
		}
		if err = o.catalog.WriteCapture(fileCtx, e.Capture); err != nil {
			return result, err
		}
		if err = o.put("x/record/"+hashString(e.Capture.GetRef()), e); err != nil {
			return result, err
		}
		if err = emit(e); err != nil {
			return result, err
		}
		result.IncrRecords()
	}
	after, err := file.Stat()
	if err != nil {
		return result, err
	}
	if info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return result, errors.New("source changed during inventory")
	}
	if err = o.put("x/source/"+hashString(source), sourceInfo{Locator: source, Size: info.Size(), Modified: info.ModTime(), Container: container}); err != nil {
		return result, err
	}
	return result, o.catalog.FinishFile(fileCtx, source, info.Size(), info.ModTime())
}

func inventoryRecord(rec gowarc.Record, source, path string) (*entry, error) {
	wr := rec.WarcRecord
	if err := wr.Block().Cache(); err != nil {
		return nil, err
	}
	c, err := cdx.NewCapture(rec, source, filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("%s at %d: invalid capture metadata: %w", source, rec.Offset, err)
	}
	e := &entry{Capture: c, Profile: wr.WarcHeader().Get(gowarc.WarcProfile), PayloadLength: wr.Block().Size()}
	e.Truncated = wr.Type() != gowarc.Revisit && wr.WarcHeader().Has(gowarc.WarcTruncated)
	for _, h := range *wr.WarcHeader() {
		if strings.HasPrefix(strings.ToLower(h.Name), "warc-segment-") {
			e.Segmented = true
		}
	}
	e.Segmented = e.Segmented || wr.Type() == gowarc.Continuation
	for _, name := range []string{gowarc.WarcConcurrentTo, gowarc.WarcWarcinfoID} {
		for _, value := range wr.WarcHeader().GetAll(name) {
			e.Context = append(e.Context, strings.Trim(value, "<>"))
		}
	}
	if length, err := wr.ContentLength(); err != nil || length != wr.Block().Size() {
		e.Issue = "invalid content length"
	}
	if wr.RecordId() == "" {
		e.Issue = "missing record ID"
	}
	if b, ok := wr.Block().(gowarc.ProtocolHeaderBlock); ok {
		e.PayloadLength -= int64(len(b.ProtocolHeaderBytes()))
	}
	h := sha256.New()
	if _, err = fmt.Fprintln(h, wr.Version().String()); err != nil {
		return nil, err
	}
	data, err := json.Marshal(wr.WarcHeader())
	if err != nil {
		return nil, err
	}
	if _, err = h.Write(data); err != nil {
		return nil, err
	}
	raw, err := wr.Block().RawBytes()
	if err != nil {
		return nil, err
	}
	if _, err = io.Copy(h, raw); err != nil {
		return nil, err
	}
	e.Fingerprint = hex.EncodeToString(h.Sum(nil))
	return e, nil
}

func (o *RedupeOptions) checkSources(ctx context.Context) error {
	return o.scan("x/source/", func(data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var source sourceInfo
		if err := json.Unmarshal(data, &source); err != nil {
			return err
		}
		if source.Container != nil {
			info, err := os.Stat(source.Container.Locator)
			if err != nil {
				return err
			}
			if info.Size() != source.Container.Size || !info.ModTime().Equal(source.Container.Modified) {
				return fmt.Errorf("source container changed: %s", source.Container.Locator)
			}
		}
		f, mounts, err := filewalker.OpenSource(source.Locator)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		err = errors.Join(err, f.Close(), mounts.Close())
		if err != nil {
			return err
		}
		if info.Size() != source.Size || !info.ModTime().Equal(source.Modified) {
			return fmt.Errorf("source changed: %s", source.Locator)
		}
		return nil
	})
}

func sourceContainer(locator string) (*sourceInfo, error) {
	var source filewalker.Source
	if err := json.Unmarshal([]byte(locator), &source); err != nil {
		return nil, err
	}
	if len(source.Roots) == 0 || strings.HasPrefix(source.Roots[0], "ftp://") {
		return nil, nil //nolint:nilnil // No local archive container exists for this source.
	}
	path := source.Roots[0]
	if _, local, ok := strings.Cut(path, "://"); ok {
		path = local
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return &sourceInfo{Locator: path, Size: info.Size(), Modified: info.ModTime()}, nil
}
