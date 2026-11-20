// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"google.golang.org/protobuf/proto"
)

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
	return p.Version == 2 && p.Fingerprint == fingerprint, e
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
	info.Version = 2
	u.packages = append(u.packages, info)
}

func (u *PackageUpdate) Publish(ctx context.Context) error {
	if len(u.packages) == 0 {
		return nil
	}
	return u.db.publishFiles(ctx, u.files, u.packages)
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

func (d *DB) packageMutations(ctx context.Context, packages []PackageInfo) ([]mutation, map[string]bool, error) {
	if len(packages) == 0 {
		return nil, nil, nil
	}
	var ops []mutation
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
		ops = append(ops, mutation{key: "package/" + identity(locator), remove: true})
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
		ops = append(ops, mutation{key: "package/" + identity(next.Locator), value: b})
	}
	for source := range removed {
		ops = append(ops, mutation{key: "container/" + identity(source), remove: true})
	}
	return ops, removed, nil
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
