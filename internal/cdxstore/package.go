// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// PackageInfo describes a package and preserves its original manifest metadata.
type PackageInfo struct {
	SourceFingerprint string          `json:"sourceFingerprint,omitempty"`
	Locator           string          `json:"source"`
	Manifest          json.RawMessage `json:"manifest,omitempty"`
	Children          []string        `json:"children,omitempty"`
	Version           int             `json:"version"`
	Fingerprint       string          `json:"fingerprint"`
	Sources           []string        `json:"sources"`
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
func (d *DB) FinishPackage(ctx context.Context, locator, fingerprint string, sources []string, metadata ...PackageInfo) error {
	if d.readOnly {
		return ErrReadOnly
	}
	old, e := d.packageState(ctx, locator)
	if e != nil {
		return e
	}
	next := PackageInfo{Version: 2, Locator: locator, Fingerprint: fingerprint, Sources: sources}
	if len(metadata) > 0 {
		next.SourceFingerprint = metadata[0].SourceFingerprint
		next.Manifest = metadata[0].Manifest
		next.Children = metadata[0].Children
	}
	childSet := map[string]bool{}
	for _, child := range next.Children {
		childSet[child] = true
	}
	for _, child := range old.Children {
		if !childSet[child] {
			if e = d.removePackage(ctx, child, 0); e != nil {
				return e
			}
		}
	}
	keep := map[string]bool{}
	for _, s := range sources {
		keep[s] = true
	}
	for _, s := range old.Sources {
		if !keep[s] {
			c, e := d.BeginFile(ctx, s)
			if e != nil {
				return e
			}
			if e = d.FinishFile(c, s, 0, time.Time{}); e != nil {
				return e
			}
			if e = d.kv.Delete(ctx, "s/"+identity(s)); e != nil {
				return e
			}
			if e = d.kv.Delete(ctx, "container/"+identity(s)); e != nil {
				return e
			}
		}
	}
	for _, s := range sources {
		if e = d.kv.Put(ctx, "container/"+identity(s), []byte(locator)); e != nil {
			return e
		}
	}
	b, e := json.Marshal(next)
	if e != nil {
		return e
	}
	return d.batch(ctx, []mutation{{key: "package/" + identity(locator), value: b}})
}

// BeginPackage invalidates cached completion before any source generation changes.
func (d *DB) BeginPackage(ctx context.Context, locator string, sources []string, children ...string) error {
	if d.readOnly {
		return ErrReadOnly
	}
	old, e := d.packageState(ctx, locator)
	if e != nil {
		return e
	}
	seen := map[string]bool{}
	for _, s := range old.Sources {
		seen[s] = true
	}
	for _, s := range sources {
		if !seen[s] {
			old.Sources = append(old.Sources, s)
			seen[s] = true
		}
	}
	old.Fingerprint = ""
	old.Version = 2
	old.Locator = locator
	known := map[string]bool{}
	for _, child := range old.Children {
		known[child] = true
	}
	for _, child := range children {
		if !known[child] {
			old.Children = append(old.Children, child)
			known[child] = true
		}
	}
	b, e := json.Marshal(old)
	if e != nil {
		return e
	}
	return d.batch(ctx, []mutation{{key: "package/" + identity(locator), value: b}})
}

func (d *DB) removePackage(ctx context.Context, locator string, depth int) error {
	if depth >= 16 {
		return fmt.Errorf("package nesting exceeds 16 levels")
	}
	p, e := d.packageState(ctx, locator)
	if e != nil {
		return e
	}
	for _, child := range p.Children {
		if e = d.removePackage(ctx, child, depth+1); e != nil {
			return e
		}
	}
	p.Children = nil
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	if e = d.kv.Put(ctx, "package/"+identity(locator), b); e != nil {
		return e
	}
	if e = d.FinishPackage(ctx, locator, "", nil); e != nil {
		return e
	}
	return d.kv.Delete(ctx, "package/"+identity(locator))
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
