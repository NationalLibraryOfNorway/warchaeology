package redupe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/publication"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/version"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
)

type location struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
}

type manifestEntry struct {
	planned

	Output            location `json:"output"`
	SourceFingerprint string   `json:"sourceFingerprint"`
}

type completion struct {
	Report    Report            `json:"report"`
	Year      string            `json:"year"`
	Checksums map[string]string `json:"sha256"`
}

func writeJSON(path string, value any) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".redupe-json-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(value)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = publication.Rename(f.Name(), path, true); err != nil {
		return err
	}
	return publication.SyncDir(filepath.Dir(path))
}

func (o *RedupeOptions) publish(ctx context.Context, ordered string) error {
	output, err := filepath.Abs(o.OutputDir)
	if err != nil {
		return err
	}
	identity := o.report.Fingerprint + ":" + output
	var previous string
	if err = o.get("!run", &previous); err == nil {
		if identity != previous {
			return errors.New("source inventory or effective configuration changed; use a new index and output directory")
		}
	} else if !errors.Is(err, badger.ErrKeyNotFound) {
		return err
	}
	if err = o.put("!run", identity); err != nil {
		return err
	}
	years := make([]string, 0, len(o.report.Years))
	for y := range o.report.Years {
		years = append(years, y)
	}
	sort.Strings(years)
	for _, y := range years {
		if err = ctx.Err(); err != nil {
			return err
		}
		if o.report.Years[y].Failed > 0 {
			continue
		}
		final := filepath.Join(output, y)
		if _, err = os.Stat(final); err == nil {
			if err = verifyCompletion(final, o.report.Fingerprint); err != nil {
				return err
			}
			o.report.Years[y].Completed = true
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = o.publishYear(ctx, ordered, output, y); err != nil {
			return err
		}
	}
	return nil
}

func (o *RedupeOptions) publishYear(ctx context.Context, ordered, output, y string) (err error) {
	var stale string
	if err = o.get("!stage/"+y, &stale); err == nil {
		if filepath.Dir(stale) != output || !strings.HasPrefix(filepath.Base(stale), ".redupe-"+y+"-") {
			return errors.New("invalid saved staging path")
		}
		if err = os.RemoveAll(stale); err != nil {
			return err
		}
	} else if !errors.Is(err, badger.ErrKeyNotFound) {
		return err
	}
	stage, err := os.MkdirTemp(output, ".redupe-"+y+"-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	if err = o.put("!stage/"+y, stage); err != nil {
		return err
	}
	config := o.WriterConfig.ForDirectory(stage)
	config.WarcFileWriterOptions = append(config.WarcFileWriterOptions, gowarc.WithWarcInfoFunc(func(b gowarc.WarcRecordBuilder) error {
		_, err := b.WriteString("software: " + version.SoftwareVersion() + "\r\ndescription: redupe self-contained UTC year " + y + "\r\n")
		return err
	}))
	defer func() { err = errors.Join(err, config.Close()) }()
	manifest, err := os.Create(filepath.Join(stage, "manifest.jsonl"))
	if err != nil {
		return err
	}
	manifestClosed := false
	defer func() {
		if !manifestClosed {
			err = errors.Join(err, manifest.Close())
		}
	}()
	encoder := json.NewEncoder(manifest)
	err = orderedRecords(ordered, func(ref string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p planned
		if err := o.get("x/plan/"+hashString(ref), &p); errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		if p.Year != y {
			return nil
		}
		e, err := o.entry(ref)
		if err != nil {
			return err
		}
		loc, err := o.writePlanned(ctx, config, e, &p, y, stage)
		if err != nil {
			return err
		}
		return encoder.Encode(manifestEntry{p, loc, e.Fingerprint})
	})
	if err != nil {
		return err
	}
	err = errors.Join(config.Close(), manifest.Sync(), manifest.Close())
	manifestClosed = true
	if err != nil {
		return err
	}
	if err = o.verifyYear(ctx, stage, y); err != nil {
		return err
	}
	if err = o.checkSources(ctx); err != nil {
		return err
	}
	if err = syncOutputFiles(stage); err != nil {
		return err
	}
	checksums, err := directoryChecksums(stage)
	if err != nil {
		return err
	}
	o.report.Years[y].Completed = true
	if err = writeJSON(filepath.Join(stage, "completion.json"), completion{o.report, y, checksums}); err != nil {
		o.report.Years[y].Completed = false
		return err
	}
	if err = ctx.Err(); err != nil {
		o.report.Years[y].Completed = false
		return err
	}
	if err = publication.SyncDir(stage); err != nil {
		return err
	}
	if err = publication.Rename(stage, filepath.Join(output, y), false); err != nil {
		o.report.Years[y].Completed = false
		return err
	}
	return publication.SyncDir(output)
}

func directoryChecksums(dir string) (map[string]string, error) {
	sums := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("unexpected symlink in output")
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "completion.json" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		err = errors.Join(err, f.Close())
		if err != nil {
			return err
		}
		sums[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return sums, err
}

func verifyCompletion(dir, fingerprint string) error {
	data, err := os.ReadFile(filepath.Join(dir, "completion.json"))
	if err != nil {
		return fmt.Errorf("existing year has no completion report: %w", err)
	}
	var c completion
	if err = json.Unmarshal(data, &c); err != nil {
		return err
	}
	if c.Year != filepath.Base(dir) {
		return errors.New("completion report period differs from destination")
	}
	if c.Report.Fingerprint != fingerprint {
		return errors.New("completed year's inventory/configuration differs")
	}
	sums, err := directoryChecksums(dir)
	if err != nil {
		return err
	}
	if len(sums) != len(c.Checksums) {
		return errors.New("completed year's files changed")
	}
	for path, sum := range sums {
		if c.Checksums[path] != sum {
			return fmt.Errorf("completed output checksum mismatch: %s", path)
		}
	}
	return nil
}

func (o *RedupeOptions) verifyYear(ctx context.Context, dir, y string) error {
	var count int64
	verifiedPrefix := "x/verified/" + y + "/"
	if err := o.scratch.DropPrefix([]byte(verifiedPrefix)); err != nil {
		return err
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) (resultErr error) {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(path, ".warc") && !strings.HasSuffix(path, ".warc.gz")) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		r, err := gowarc.NewWarcFileReaderFromStream(f, 0)
		if err != nil {
			return errors.Join(err, f.Close())
		}
		defer func() { resultErr = errors.Join(resultErr, r.Close()) }()
		first := true
		for rec, err := range r.Records() {
			if err != nil {
				return errors.Join(err, rec.Close())
			}
			checkErr := func() (resultErr error) {
				defer func() { resultErr = errors.Join(resultErr, rec.Close()) }()
				if err := ctx.Err(); err != nil {
					return err
				}
				wr := rec.WarcRecord
				if first {
					first = false
					if wr.Type() != gowarc.Warcinfo {
						return errors.New("missing output warcinfo")
					}
					return verifyRecord(wr)
				}
				count++
				seenKey := verifiedPrefix + hashString(wr.RecordId())
				var seen bool
				if err := o.get(seenKey, &seen); err == nil {
					return errors.New("duplicate output record ID")
				} else if !errors.Is(err, badger.ErrKeyNotFound) {
					return err
				}
				if err := o.put(seenKey, true); err != nil {
					return err
				}
				var p planned
				if err := o.get("x/output-id/"+y+"/"+hashString(wr.RecordId()), &p); err != nil {
					return err
				}
				if err := verifyBlock(wr); err != nil {
					return err
				}
				if wr.Type() == gowarc.Revisit {
					targetID := wr.WarcHeader().GetId(gowarc.WarcRefersTo)
					var target planned
					if err := o.get("x/output-id/"+y+"/"+hashString(targetID), &target); err != nil {
						return fmt.Errorf("output revisit target is outside year: %w", err)
					}
					if target.Action != actionFull {
						return errors.New("output revisit does not target a full record")
					}
					var loc location
					if err := o.get("x/output/"+hashString(target.Ref), &loc); err != nil {
						return err
					}
					donor, err := (loader.FileStorageLoader{}).RecordByStorageRef(ctx, fmt.Sprintf("warcfile:%s#%d", filepath.Join(dir, filepath.FromSlash(loc.File)), loc.Offset))
					if err != nil {
						return err
					}
					defer func() { resultErr = errors.Join(resultErr, donor.Close()) }()
					full, err := loader.ReconstructIdentical(wr, donor)
					if err != nil {
						return err
					}
					defer func() { resultErr = errors.Join(resultErr, full.Close()) }()
					wr = full
				}
				if err := verifyRecord(wr); err != nil {
					return err
				}
				actual, _, err := payloadHash(wr)
				if err != nil {
					return err
				}
				if actual != p.PayloadSHA256 {
					return errors.New("output payload differs from plan")
				}
				return nil
			}()
			if checkErr != nil {
				return checkErr
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	yr := o.report.Years[y]
	if count != yr.Selected-yr.Aliases {
		return fmt.Errorf("output record count %d does not match inventory %d", count, yr.Selected-yr.Aliases)
	}
	return nil
}

func (o *RedupeOptions) writePlanned(ctx context.Context, config *warcwriterconfig.WarcWriterConfig, e *entry, p *planned, y, stage string) (_ location, resultErr error) {
	var loc location
	if p.Action == "alias" {
		if err := o.get("x/output/"+hashString(p.Donor), &loc); err != nil {
			return loc, err
		}
	} else {
		wr, err := o.reconstruct(ctx, e)
		if err != nil {
			return loc, err
		}
		defer func(original gowarc.WarcRecord) { resultErr = errors.Join(resultErr, original.Close()) }(wr)
		digest, _, err := payloadHash(wr)
		if err != nil {
			return loc, err
		}
		if digest != p.PayloadSHA256 {
			return loc, errors.New("payload changed after planning")
		}
		if p.Action == actionFull || p.Action == actionRevisit {
			wr.WarcHeader().Set(gowarc.WarcPayloadDigest, "sha256:"+digest)
		}
		if p.Action == actionRevisit {
			revisit, err := wr.ToRevisitRecord(p.Target)
			if err != nil {
				return loc, err
			}
			defer func() { resultErr = errors.Join(resultErr, revisit.Close()) }()
			wr = revisit
		}
		writer, err := config.GetWarcWriterContext(ctx, "", e.Capture.GetSts().AsTime())
		if err != nil {
			return loc, err
		}
		responses := writer.Write(wr)
		if len(responses) != 1 {
			return loc, errors.New("writer did not write exactly one record (segmentation is unsupported)")
		}
		if responses[0].Err != nil {
			return loc, responses[0].Err
		}
		name := responses[0].FileName
		if !filepath.IsAbs(name) {
			name = filepath.Join(stage, name)
		}
		name, err = filepath.Rel(stage, name)
		if err != nil {
			return loc, err
		}
		loc = location{filepath.ToSlash(name), responses[0].FileOffset}
		if err = o.put("x/output/"+hashString(p.Ref), loc); err != nil {
			return loc, err
		}
		if err = o.put("x/output-id/"+y+"/"+hashString(p.ID), *p); err != nil {
			return loc, err
		}
	}
	return loc, nil
}

func syncOutputFiles(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("unexpected non-regular output")
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		return errors.Join(f.Sync(), f.Close())
	})
}
