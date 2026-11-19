// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/wacz"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipfs"
	"github.com/spf13/afero"
)

func (o *IndexOptions) handleWACZ(ctx context.Context, fs afero.Fs, name string) (bool, error) {
	if !strings.EqualFold(path.Ext(name), ".wacz") {
		return false, nil
	}
	if !o.FileWalker.HasSuffix(name) {
		return true, nil
	}
	_, err := filewalker.PrePostHookContext(ctx, fs, name, o.OpenInputFileHook, o.CloseInputFileHook, func(fs afero.Fs, path string) (stat.Result, error) {
		return nil, o.indexWACZ(ctx, fs, path)
	})
	if errors.Is(err, hooks.ErrSkipFile) {
		return true, nil
	}
	return true, err
}
func (o *IndexOptions) indexWACZ(ctx context.Context, fs afero.Fs, name string) (err error) {
	locator, err := filewalker.SourceLocator(fs, name)
	if err != nil {
		return err
	}
	var source filewalker.Source
	if err = json.Unmarshal([]byte(locator), &source); err != nil {
		return err
	}
	if len(source.Roots) >= 16 {
		return fmt.Errorf("WACZ nesting exceeds 16 levels")
	}
	roots := append(source.Roots, source.Path)
	f, err := fs.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return err
	}
	members := map[string]*zip.File{}
	var indexes []*zip.File
	hash := sha256.New()
	fmt.Fprintf(hash, "wacz-7/%d/%d/%s", info.Size(), info.ModTime().UnixNano(), o.WaczIndex)
	fmt.Fprintf(hash, "/%#v/%t/%t/%d/%d/%d/%s/%t", o.Filter, o.DefaultRecords, o.Force, o.Offset, o.Nth, o.Limit, o.DirRoot, o.PostAppend)
	hash.Write([]byte(o.WaczConfig))
	for _, z := range zr.File {
		if z.FileInfo().IsDir() {
			continue
		}
		if path.Clean(z.Name) != z.Name || strings.HasPrefix(z.Name, "/") || strings.HasPrefix(z.Name, "../") || strings.Contains(z.Name, "\\") {
			return fmt.Errorf("unsafe WACZ member %q", z.Name)
		}
		if _, ok := members[z.Name]; ok {
			return fmt.Errorf("duplicate WACZ member %q", z.Name)
		}
		members[z.Name] = z
		fmt.Fprintf(hash, "/%s/%d/%d/%d", z.Name, z.CRC32, z.UncompressedSize64, z.CompressedSize64)
		if strings.HasPrefix(z.Name, "indexes/") && (strings.HasSuffix(z.Name, ".cdx") || strings.HasSuffix(z.Name, ".cdxj") || strings.HasSuffix(z.Name, ".cdx.gz") || strings.HasSuffix(z.Name, ".cdxj.gz")) {
			indexes = append(indexes, z)
		}
	}
	aliases := map[string]string{}
	for n := range members {
		if strings.HasPrefix(n, "archive/") {
			b := path.Base(n)
			if _, exists := aliases[b]; exists {
				aliases[b] = ""
			} else {
				aliases[b] = n
			}
		}
	}
	var manifestJSON json.RawMessage
	var metadata waczManifest
	if manifest := members["datapackage.json"]; manifest != nil {
		r, e := manifest.Open()
		if e != nil {
			return e
		}
		raw, e := io.ReadAll(io.LimitReader(r, 16<<20+1))
		e = errors.Join(e, r.Close())
		if e != nil {
			return e
		}
		if len(raw) > 16<<20 {
			return fmt.Errorf("WACZ manifest exceeds 16 MiB")
		}

		if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
			return fmt.Errorf("WACZ manifest must be a JSON object")
		}
		if e = json.Unmarshal(raw, &metadata); e != nil {
			return fmt.Errorf("invalid WACZ manifest: %w", e)
		}
		manifestJSON = raw
		hash.Write(raw)
		for _, resource := range metadata.Resources {
			if strings.HasPrefix(resource.Path, "archive/") && resource.Name != "" {
				if previous, ok := aliases[resource.Name]; ok && previous != resource.Path {
					aliases[resource.Name] = ""
				} else {
					aliases[resource.Name] = resource.Path
				}
			}
		}
	}
	children, e := waczChildren(metadata, members)
	if e != nil {
		return e
	}
	fingerprint := fmt.Sprintf("%x", hash.Sum(nil))
	db, _ := o.Sink.(*cdxstore.DB)
	if db != nil {
		if db.IndexOnly() {
			return fmt.Errorf("WACZ requires a native-content database")
		}
		same, e := db.PackageMatches(ctx, locator, fingerprint)
		if e != nil {
			return e
		}
		if same {
			return nil
		}
	}
	sourceFor := func(member string) string {
		b, _ := json.Marshal(filewalker.Source{Roots: roots, Path: "/" + member})
		return string(b)
	}
	var names []string
	for n := range members {
		if strings.HasPrefix(n, "archive/") && (strings.HasSuffix(n, ".warc") || strings.HasSuffix(n, ".warc.gz")) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 && len(children) == 0 && metadata.Profile != "multi-wacz-package" {
		return fmt.Errorf("WACZ contains no WARCs or child WACZ packages")
	}
	var childLocators []string
	for _, child := range children {
		childLocators = append(childLocators, sourceFor(child))
	}
	packageMetadata := cdxstore.PackageInfo{SourceFingerprint: wacz.Fingerprint(info, zr), Manifest: manifestJSON, Children: childLocators}
	if len(names) == 0 {
		if db != nil {
			if err = db.BeginPackage(ctx, locator, nil, childLocators...); err != nil {
				return err
			}
		}
		mounted := filewalker.WithSourceRoots(zipfs.NewWACZ(zr), roots)
		for _, child := range children {
			if err = o.indexWACZ(ctx, mounted, "/"+child); err != nil {
				return fmt.Errorf("%s: %w", child, err)
			}
		}
		current, e := f.Stat()
		if e != nil {
			return e
		}
		if current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			return fmt.Errorf("WACZ changed during indexing")
		}
		if db != nil {
			return db.FinishPackage(ctx, locator, fingerprint, nil, packageMetadata)
		}
		return nil
	}
	// A disk-backed staging sink validates the entire index before native publication.
	dir, err := os.MkdirTemp(o.tmpDir, "wacz-index-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	stage, err := cdxstore.Open(ctx, cdxstore.Options{Format: "badger", Dir: dir, Scratch: true})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, stage.Close()) }()
	if len(indexes) > 0 && o.WaczIndex != "warc" {
		for _, z := range indexes {
			err = func() error {
				r, e := z.Open()
				if e != nil {
					return e
				}
				defer r.Close()
				var input io.Reader = r
				if strings.HasSuffix(z.Name, ".gz") {
					g, e := gzip.NewReader(r)
					if e != nil {
						return e
					}
					defer g.Close()
					input = g
				}
				scanner := bufio.NewScanner(input)
				scanner.Buffer(make([]byte, 4096), 1<<20)
				for scanner.Scan() {
					if e := ctx.Err(); e != nil {
						return e
					}
					c, e := cdx.ParseIndexedLine(scanner.Bytes())
					if e != nil {
						return fmt.Errorf("%s: %w", z.Name, e)
					}
					member := c.Filename
					if !strings.HasPrefix(member, "archive/") {
						if resolved, exists := aliases[member]; exists {
							if resolved == "" {
								return fmt.Errorf("ambiguous WARC member %q", member)
							}
							member = resolved
						} else {
							member = "archive/" + member
						}
					}
					zf, ok := members[member]
					if !ok || path.Clean(member) != member || !(strings.HasSuffix(member, ".warc") || strings.HasSuffix(member, ".warc.gz")) {
						return fmt.Errorf("missing or unsafe WARC member %q", c.Filename)
					}
					if c.Offset < 0 || uint64(c.Offset) > zf.UncompressedSize64 || uint64(c.Rle) > zf.UncompressedSize64-uint64(c.Offset) {
						return fmt.Errorf("record outside %s", member)
					}
					c.Source = sourceFor(member)
					c.Ref = fmt.Sprintf("warcfile:%s#%d", c.Source, c.Offset)
					if e = stage.WriteCapture(ctx, c); e != nil {
						return e
					}
				}
				return scanner.Err()
			}()
			if err != nil {
				return err
			}
		}
	} else {
		mounted := zipfs.NewWACZ(zr)
		for _, n := range names {
			// Preserve the native indexer's metadata, filters and request pairing.
			memberFS := filewalker.WithSourceRoots(mounted, roots)
			copyOptions := *o
			copyOptions.Sink = stage
			if _, err = copyOptions.handleFile(ctx, memberFS, "/"+n, nil); err != nil {
				return err
			}
		}
	}
	current, e := f.Stat()
	if e != nil {
		return e
	}
	if current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return fmt.Errorf("WACZ changed during indexing")
	}
	var sources []string
	for _, n := range names {
		sources = append(sources, sourceFor(n))
	}
	if db != nil {
		if err = db.BeginPackage(ctx, locator, sources, childLocators...); err != nil {
			return err
		}
	}
	for _, n := range names {
		src := sourceFor(n)
		writeCtx := ctx
		finisher, ok := o.Sink.(fileFinisher)
		if ok {
			writeCtx, err = finisher.BeginFile(ctx, src)
			if err != nil {
				return err
			}
		}
		err = stage.Scan(ctx, "", "", func(c *cdx.Capture) error {
			if c.Source != src {
				return nil
			}
			c.Generation = ""
			return o.Sink.WriteCapture(writeCtx, c)
		})
		if err != nil {
			return err
		}
		if ok {
			z := members[n]
			if err = finisher.FinishFile(writeCtx, src, int64(z.UncompressedSize64), z.Modified); err != nil {
				return err
			}
		}
	}
	mounted := filewalker.WithSourceRoots(zipfs.NewWACZ(zr), roots)
	for _, child := range children {
		if err = o.indexWACZ(ctx, mounted, "/"+child); err != nil {
			return fmt.Errorf("%s: %w", child, err)
		}
	}
	if db != nil {
		return db.FinishPackage(ctx, locator, fingerprint, sources, packageMetadata)
	}
	return nil
}

type waczManifest struct {
	Profile   string `json:"profile"`
	Resources []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		URL  string `json:"url"`
	} `json:"resources"`
}

func waczChildren(manifest waczManifest, members map[string]*zip.File) ([]string, error) {
	if manifest.Profile == "multi-wacz-package" && manifest.Resources == nil {
		return nil, fmt.Errorf("multi-WACZ manifest requires a resources array")
	}
	var children []string
	seen := map[string]bool{}
	for _, resource := range manifest.Resources {
		member := resource.Path
		if member == "" {
			member = resource.URL
		}
		if manifest.Profile != "multi-wacz-package" && !strings.EqualFold(path.Ext(member), ".wacz") && !strings.EqualFold(path.Ext(resource.Name), ".wacz") {
			continue
		}
		if member == "" {
			member = resource.Name
		}
		if path.Clean(member) != member || strings.HasPrefix(member, "/") || strings.HasPrefix(member, "../") || strings.ContainsAny(member, "\\\x00") || strings.Contains(member, ":") {
			return nil, fmt.Errorf("child WACZ must reference a local bundle member: %q", member)
		}
		if !strings.EqualFold(path.Ext(member), ".wacz") {
			return nil, fmt.Errorf("unsupported child package name %q", member)
		}
		if _, ok := members[member]; !ok {
			return nil, fmt.Errorf("manifest references missing child WACZ %q", member)
		}
		if seen[member] {
			return nil, fmt.Errorf("duplicate child WACZ reference %q", member)
		}
		seen[member] = true
		children = append(children, member)
	}
	if len(children) == 0 && manifest.Profile != "multi-wacz-package" {
		for name := range members {
			if strings.EqualFold(path.Ext(name), ".wacz") {
				children = append(children, name)
			}
		}
		sort.Strings(children)
	}
	return children, nil
}
