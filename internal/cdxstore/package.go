// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"path"
	"strings"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/wacz"
	"google.golang.org/protobuf/proto"
)

const packageVersion = 3

// PackageInfo describes a package and preserves its original manifest metadata.
type PackageInfo struct {
	Locator           string         `json:"source"`
	Manifest          jsontext.Value `json:"manifest,omitzero"`
	Children          []string       `json:"children,omitempty"`
	Version           int            `json:"version"`
	Fingerprint       string         `json:"fingerprint"`
	SourceFingerprint string         `json:"sourceFingerprint,omitempty"`
	Sources           []string       `json:"sources"`
}

func (d *DB) packageState(ctx context.Context, locator string) (PackageInfo, error) {
	var p PackageInfo
	b, e := d.kv.Get(ctx, "package/"+identity(locator))
	if e == nil && len(b) > 0 {
		e = json.Unmarshal(b, &p)
	}
	return p, e
}
func (d *DB) PackageMatches(ctx context.Context, locator, fingerprint string) (bool, error) {
	p, e := d.packageState(ctx, locator)
	if e != nil || p.Version != packageVersion || p.Fingerprint != fingerprint {
		return false, e
	}
	dirty, err := d.kv.Get(ctx, "package-dirty/"+identity(locator))
	return len(dirty) == 0, err
}

// PackageUpdate holds unpublished source generations for one package tree.
type PackageUpdate struct {
	db       *DB
	files    []pendingFile
	packages []PackageInfo
}

func (d *DB) NewPackageUpdate() *PackageUpdate {
	return &PackageUpdate{db: d}
}

func (u *PackageUpdate) StageMember(ctx context.Context, stage *CaptureStage, source string, version filewalker.FileVersion, container *filewalker.FileVersion) error {
	ctx, err := u.db.BeginFile(ctx, source)
	if err != nil {
		return err
	}
	g := ctx.Value(generationKey{}).(*generation)
	u.files = append(u.files, pendingFile{source: source, generation: g, version: version, container: container})
	var previous *cdx.Capture
	if err := stage.ScanSource(ctx, source, func(c *cdx.Capture) error {
		if previous != nil && captureBase(previous) == captureBase(c) {
			if !proto.Equal(previous, c) {
				return fmt.Errorf("conflicting capture values in %s at offset %d", source, c.GetOffset())
			}
			return nil
		}
		previous = proto.CloneOf(c)
		return u.db.WriteCapture(ctx, c)
	}); err != nil {
		return err
	}
	return g.flush(ctx, u.db)
}

func (u *PackageUpdate) AddPackage(info PackageInfo) {
	info.Version = packageVersion
	u.packages = append(u.packages, info)
}

func (u *PackageUpdate) Publish(ctx context.Context) error {
	if len(u.packages) == 0 {
		return nil
	}
	// Packages are staged in postorder, so the final package is the root.
	return u.db.publishFiles(ctx, u.packages[len(u.packages)-1].Locator, u.files, u.packages)
}

func (u *PackageUpdate) Discard(ctx context.Context) error {
	var err error
	for _, file := range u.files {
		if !file.generation.published {
			err = errors.Join(err, u.db.cleanGeneration(ctx, generationMarker(file.source, file.generation.id)))
		}
	}
	return err
}

func packageAncestors(locator string) ([]string, error) {
	if !strings.HasPrefix(locator, "{") {
		return nil, nil
	}
	var src filewalker.Source
	if err := json.Unmarshal([]byte(locator), &src); err != nil {
		return nil, err
	}
	var parents []string
	for i, member := range src.Roots {
		if !strings.EqualFold(path.Ext(member), ".wacz") {
			continue
		}
		parent, err := filewalker.MemberLocator(src.Roots[:i], member)
		if err != nil {
			return nil, err
		}
		parents = append(parents, parent)
	}
	return parents, nil
}

// A separately published member invalidates each enclosing package's reuse
// fingerprint. Keep its membership and manifest for replacement and removal.
func (d *DB) invalidateAncestors(ctx context.Context, locator string) ([]mutation, error) {
	parents, err := packageAncestors(locator)
	if err != nil {
		return nil, err
	}
	var ops []mutation
	for _, parent := range parents {
		p, err := d.packageState(ctx, parent)
		if err != nil {
			return nil, err
		}
		if p.Fingerprint == "" {
			continue
		}
		p.Fingerprint = ""
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		ops = append(ops, mutation{key: "package/" + identity(parent), value: raw})
	}
	return ops, nil
}

func (d *DB) packageMutations(ctx context.Context, locator string, packages []PackageInfo) ([]mutation, map[string]bool, error) {
	ops, err := d.invalidateAncestors(ctx, locator)
	if err != nil || len(packages) == 0 {
		return ops, nil, err
	}
	removed := map[string]bool{}
	var removeTree func(string, int) error
	removeTree = func(locator string, depth int) error {
		if depth >= 16 {
			return fmt.Errorf("package nesting exceeds 16 levels")
		}
		p, err := d.packageState(ctx, locator)
		if err != nil {
			return err
		}
		for _, child := range p.Children {
			if err := removeTree(child, depth+1); err != nil {
				return err
			}
		}
		for _, source := range p.Sources {
			removed[source] = true
		}
		ops = append(ops, mutation{key: "package/" + identity(locator), remove: true}, mutation{key: "package-dirty/" + identity(locator), remove: true})
		return nil
	}
	for _, next := range packages {
		old, err := d.packageState(ctx, next.Locator)
		if err != nil {
			return nil, nil, err
		}
		children := make(map[string]bool, len(next.Children))
		for _, child := range next.Children {
			children[child] = true
		}
		for _, child := range old.Children {
			if !children[child] {
				if err := removeTree(child, 0); err != nil {
					return nil, nil, err
				}
			}
		}
		for _, source := range old.Sources {
			removed[source] = true
		}
		for _, source := range next.Sources {
			delete(removed, source)
			ops = append(ops, mutation{key: "container/" + identity(source), value: []byte(next.Locator)})
		}
		b, err := json.Marshal(next)
		if err != nil {
			return nil, nil, err
		}
		ops = append(ops, mutation{key: "package/" + identity(next.Locator), value: b}, mutation{key: "package-dirty/" + identity(next.Locator), remove: true})
	}
	for source := range removed {
		ops = append(ops, mutation{key: "container/" + identity(source), remove: true})
	}
	return ops, removed, nil
}

// PackageDonor resolves a CDX original filename inside the capture's own leaf
// package, including aliases declared in its manifest.
func (d *DB) PackageDonor(ctx context.Context, source, filename string) (ArchiveInfo, error) {
	parent, err := d.kv.Get(ctx, "container/"+identity(source))
	if err != nil {
		return ArchiveInfo{}, err
	}
	p, err := d.packageState(ctx, string(parent))
	if err != nil {
		return ArchiveInfo{}, err
	}
	var manifest struct {
		Resources []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"resources"`
	}
	if len(p.Manifest) > 0 {
		if err := json.Unmarshal(p.Manifest, &manifest); err != nil {
			return ArchiveInfo{}, err
		}
	}
	aliases := wacz.MemberAliases{}
	members := make(map[string]string, len(p.Sources))
	for _, locator := range p.Sources {
		var member filewalker.Source
		if err := json.Unmarshal([]byte(locator), &member); err != nil {
			return ArchiveInfo{}, err
		}
		name := strings.TrimPrefix(member.Path, "/")
		members[name] = locator
		aliases.Add(path.Base(name), name)
	}
	for _, resource := range manifest.Resources {
		if resource.Name != "" && members[resource.Path] != "" {
			aliases.Add(resource.Name, resource.Path)
		}
	}
	name, err := aliases.Resolve(filename)
	if err != nil {
		return ArchiveInfo{}, err
	}
	locator, ok := members[name]
	if !ok {
		return ArchiveInfo{}, fmt.Errorf("missing WARC donor %q in package", filename)
	}
	return d.ArchiveSource(ctx, ArchiveID(locator))
}

// ListPackages streams persisted package metadata, including manifests and membership.
func (d *DB) ListPackages(ctx context.Context, emit func(PackageInfo) error) error {
	return d.kv.Scan(ctx, "package/", "package0", func(_ string, b []byte) error {
		var p PackageInfo
		if e := json.Unmarshal(b, &p); e != nil {
			return e
		}
		return emit(p)
	})
}
