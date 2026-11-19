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
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/hooks"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/stat"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/wacz"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipfs"
	"github.com/spf13/afero"
)

const (
	waczRawIndex     = "warc"
	waczArchiveDir   = "archive/"
	multiWaczProfile = "multi-wacz-package"
)

type waczPackage struct {
	roots             []string
	members           map[string]*zip.File
	names, children   []string
	indexes           []*zip.File
	aliases           map[string]string
	manifest          json.RawMessage
	sourceFingerprint string
}

func (p *waczPackage) sourceFor(member string) string {
	return filewalker.MemberLocator(p.roots, "/"+member)
}

func (o *IndexOptions) handleWACZ(ctx context.Context, fs afero.Fs, name string) (_ bool, err error) {
	if !strings.EqualFold(path.Ext(name), ".wacz") {
		return false, nil
	}
	if !o.FileWalker.HasSuffix(name) {
		return true, nil
	}
	var update *cdxstore.PackageUpdate
	if db, ok := o.Sink.(*cdxstore.DB); ok {
		update = db.NewPackageUpdate()
		defer func() { err = errors.Join(err, update.Discard(context.WithoutCancel(ctx))) }()
	}
	_, err = filewalker.PrePostHookContext(ctx, fs, name, o.OpenInputFileHook, o.CloseInputFileHook, func(fs afero.Fs, path string) (stat.Result, error) {
		return nil, o.indexWACZ(ctx, fs, path, nil, update)
	})
	if errors.Is(err, hooks.ErrSkipFile) {
		return true, nil
	}
	if err == nil && update != nil {
		err = update.Publish(ctx)
	}
	return true, err
}

func (o *IndexOptions) indexWACZ(ctx context.Context, fs afero.Fs, name string, outer *filewalker.FileVersion, update *cdxstore.PackageUpdate) (err error) {
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
	f, err := fs.Open(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	version := filewalker.VersionOf(info)
	if outer == nil {
		if len(source.Roots) == 0 {
			outer = &version
		} else if container, present, e := filewalker.ContainerVersion(locator); e != nil {
			return e
		} else if present {
			outer = &container
		}
	}
	zr, err := zip.NewReader(f, info.Size())
	if err != nil {
		return err
	}
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "wacz-8/%d/%d/%s", version.Size, version.Modified.UnixNano(), o.WaczIndex)
	if outer != nil {
		_, _ = fmt.Fprintf(digest, "/%d/%d", outer.Size, outer.Modified.UnixNano())
	}
	_, _ = fmt.Fprintf(digest, "/%#v/%t/%t/%d/%d/%d/%s/%t", o.Filter, o.DefaultRecords, o.Force, o.Offset, o.Nth, o.Limit, o.DirRoot, o.PostAppend)
	digest.Write([]byte(o.WaczConfig))
	pkg, err := inspectWACZ(zr, append(source.Roots, source.Path), digest)
	if err != nil {
		return err
	}
	pkg.sourceFingerprint = wacz.Fingerprint(info, zr)
	fingerprint := fmt.Sprintf("%x", digest.Sum(nil))
	db, _ := o.Sink.(*cdxstore.DB)
	if db != nil {
		if db.IndexOnly() {
			return fmt.Errorf("WACZ requires a native-content database")
		}
		if same, err := db.PackageMatches(ctx, locator, fingerprint); err != nil || same {
			return err
		}
	}

	mounted := filewalker.WithSourceRoots(zipfs.NewWACZ(zr), pkg.roots)
	var stage *cdxstore.CaptureStage
	if len(pkg.names) > 0 {
		dir, err := os.MkdirTemp(o.tmpDir, "wacz-index-")
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
		stage, err = cdxstore.OpenCaptureStage(ctx, dir)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, stage.Close()) }()
		if err = o.stageWACZ(ctx, pkg, mounted, stage); err != nil {
			return err
		}
	}
	if err = checkArchiveVersion(f, version, pkg.sourceFor(""), outer); err != nil {
		return err
	}
	if err = o.stageWACZTree(ctx, pkg, mounted, stage, update, locator, fingerprint, outer); err != nil {
		return err
	}
	return checkArchiveVersion(f, version, pkg.sourceFor(""), outer)
}

func inspectWACZ(zr *zip.Reader, roots []string, digest hash.Hash) (*waczPackage, error) {
	pkg := &waczPackage{roots: roots, members: make(map[string]*zip.File), aliases: make(map[string]string)}
	for _, z := range zr.File {
		if z.FileInfo().IsDir() {
			continue
		}
		if path.Clean(z.Name) != z.Name || strings.HasPrefix(z.Name, "/") || strings.HasPrefix(z.Name, "../") || strings.Contains(z.Name, "\\") {
			return nil, fmt.Errorf("unsafe WACZ member %q", z.Name)
		}
		if _, ok := pkg.members[z.Name]; ok {
			return nil, fmt.Errorf("duplicate WACZ member %q", z.Name)
		}
		pkg.members[z.Name] = z
		_, _ = fmt.Fprintf(digest, "/%s/%d/%d/%d", z.Name, z.CRC32, z.UncompressedSize64, z.CompressedSize64)
		if isCDXMember(z.Name) {
			pkg.indexes = append(pkg.indexes, z)
		}
		if strings.HasPrefix(z.Name, waczArchiveDir) {
			pkg.addAlias(path.Base(z.Name), z.Name)
			if isWARCMember(z.Name) {
				pkg.names = append(pkg.names, z.Name)
			}
		}
	}
	sort.Strings(pkg.names)

	var metadata waczManifest
	if manifest := pkg.members["datapackage.json"]; manifest != nil {
		raw, err := readWACZManifest(manifest, &metadata)
		if err != nil {
			return nil, err
		}
		pkg.manifest = raw
		digest.Write(raw)
		for _, resource := range metadata.Resources {
			if strings.HasPrefix(resource.Path, waczArchiveDir) && resource.Name != "" {
				pkg.addAlias(resource.Name, resource.Path)
			}
		}
	}
	children, err := waczChildren(metadata, pkg.members)
	if err != nil {
		return nil, err
	}
	if len(pkg.names) == 0 && len(children) == 0 && metadata.Profile != multiWaczProfile {
		return nil, fmt.Errorf("WACZ contains no WARCs or child WACZ packages")
	}
	pkg.children = children
	return pkg, nil
}

func isCDXMember(name string) bool {
	if !strings.HasPrefix(name, "indexes/") {
		return false
	}
	name = strings.TrimSuffix(name, ".gz")
	return strings.HasSuffix(name, ".cdx") || strings.HasSuffix(name, ".cdxj")
}

func isWARCMember(name string) bool {
	return strings.HasSuffix(strings.TrimSuffix(name, ".gz"), ".warc")
}

func (p *waczPackage) addAlias(alias, member string) {
	if previous, ok := p.aliases[alias]; ok && previous != member {
		p.aliases[alias] = ""
		return
	}
	p.aliases[alias] = member
}

func readWACZManifest(manifest *zip.File, metadata *waczManifest) ([]byte, error) {
	r, err := manifest.Open()
	if err != nil {
		return nil, err
	}
	const maxManifest = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(r, maxManifest+1))
	if err = errors.Join(err, r.Close()); err != nil {
		return nil, err
	}
	if len(raw) > maxManifest {
		return nil, fmt.Errorf("WACZ manifest exceeds 16 MiB")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return nil, fmt.Errorf("WACZ manifest must be a JSON object")
	}
	if err = json.Unmarshal(raw, metadata); err != nil {
		return nil, fmt.Errorf("invalid WACZ manifest: %w", err)
	}
	return raw, nil
}

func (o *IndexOptions) stageWACZ(ctx context.Context, pkg *waczPackage, mounted afero.Fs, stage *cdxstore.CaptureStage) error {
	if len(pkg.indexes) > 0 && o.WaczIndex != waczRawIndex {
		for _, index := range pkg.indexes {
			if err := pkg.stageCDX(ctx, index, stage); err != nil {
				return fmt.Errorf("%s: %w", index.Name, err)
			}
		}
	} else {
		copyOptions := *o
		copyOptions.Sink = stage
		for _, name := range pkg.names {
			if _, err := copyOptions.handleFile(ctx, mounted, "/"+name, nil); err != nil {
				return err
			}
		}
	}
	return stage.Flush(ctx)
}

func (p *waczPackage) stageCDX(ctx context.Context, index *zip.File, stage *cdxstore.CaptureStage) (err error) {
	r, err := index.Open()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	var input io.Reader = r
	if strings.HasSuffix(index.Name, ".gz") {
		g, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, g.Close()) }()
		input = g
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		capture, err := cdx.ParseIndexedLine(scanner.Bytes())
		if err != nil {
			return err
		}
		member, err := p.warcMember(capture)
		if err != nil {
			return err
		}
		capture.Source = p.sourceFor(member)
		capture.Ref = fmt.Sprintf("warcfile:%s#%d", capture.GetSource(), capture.GetOffset())
		if err := stage.WriteCapture(ctx, capture); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func (p *waczPackage) warcMember(c *cdx.Capture) (string, error) {
	member := c.GetFilename()
	if !strings.HasPrefix(member, waczArchiveDir) {
		if resolved, exists := p.aliases[member]; exists {
			if resolved == "" {
				return "", fmt.Errorf("ambiguous WARC member %q", member)
			}
			member = resolved
		} else {
			member = waczArchiveDir + member
		}
	}
	zf, ok := p.members[member]
	if !ok || !strings.HasPrefix(member, waczArchiveDir) || !isWARCMember(member) {
		return "", fmt.Errorf("missing or unsafe WARC member %q", c.GetFilename())
	}
	offset, length := uint64(c.GetOffset()), uint64(c.GetRle())
	if offset > zf.UncompressedSize64 || length > zf.UncompressedSize64-offset {
		return "", fmt.Errorf("record outside %s", member)
	}
	return member, nil
}

func (o *IndexOptions) stageWACZTree(ctx context.Context, pkg *waczPackage, mounted afero.Fs, stage *cdxstore.CaptureStage, update *cdxstore.PackageUpdate, locator, fingerprint string, outer *filewalker.FileVersion) error {
	info := cdxstore.PackageInfo{Locator: locator, Fingerprint: fingerprint, SourceFingerprint: pkg.sourceFingerprint, Manifest: pkg.manifest}
	for _, name := range pkg.names {
		source := pkg.sourceFor(name)
		info.Sources = append(info.Sources, source)
		if update != nil {
			version := filewalker.VersionOf(pkg.members[name].FileInfo())
			if err := update.StageMember(ctx, stage, source, version, outer); err != nil {
				return err
			}
		} else if err := stage.ScanSource(ctx, source, func(c *cdx.Capture) error {
			return o.Sink.WriteCapture(ctx, c)
		}); err != nil {
			return err
		}
	}
	for _, child := range pkg.children {
		info.Children = append(info.Children, pkg.sourceFor(child))
		if err := o.indexWACZ(ctx, mounted, "/"+child, outer, update); err != nil {
			return fmt.Errorf("%s: %w", child, err)
		}
	}
	if update != nil {
		update.AddPackage(info)
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
	if manifest.Profile == multiWaczProfile && manifest.Resources == nil {
		return nil, fmt.Errorf("multi-WACZ manifest requires a resources array")
	}
	var children []string
	seen := map[string]bool{}
	for _, resource := range manifest.Resources {
		member := resource.Path
		if member == "" {
			member = resource.URL
		}
		if manifest.Profile != multiWaczProfile && !strings.EqualFold(path.Ext(member), ".wacz") && !strings.EqualFold(path.Ext(resource.Name), ".wacz") {
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
	if len(children) == 0 && manifest.Profile != multiWaczProfile {
		for name := range members {
			if strings.EqualFold(path.Ext(name), ".wacz") {
				children = append(children, name)
			}
		}
		sort.Strings(children)
	}
	return children, nil
}
