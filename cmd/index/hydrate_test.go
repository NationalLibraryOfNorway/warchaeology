// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipnum"
)

const replayTestRow = `org,example)/ 20260201000000 {"url":"https://example.org/","filename":"a.warc","offset":0,"length":42}` + "\n"

func TestHydrationCancellationLeavesDestinationAndCleansUp(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	if err := os.Mkdir(tmp, 0700); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(env, tmp)
	}
	input := filepath.Join(root, "source.cdxj")
	output := filepath.Join(root, "output.cdxj.gz")
	writeHydrateFixture(t, input, replayTestRow)
	writeHydrateFixture(t, output, "previous output")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o := hydrateOptions{input: input, warcRoot: root, indexFile: output, allowUnresolved: true, diagnostics: cancellingWriter{cancel}}
	if err := o.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("hydrate error = %v", err)
	}
	assertHydrateFile(t, input, replayTestRow)
	assertHydrateFile(t, output, "previous output")
	entries, err := os.ReadDir(tmp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary directory contains %v, error %v", entries, err)
	}
	if files, err := filepath.Glob(filepath.Join(root, ".warc-index-*")); err != nil || len(files) != 0 {
		t.Fatalf("staged output remains: %v, error %v", files, err)
	}
}

func TestHydrationRejectsMalformedInputInPartialMode(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "source.cdxj")
	output := filepath.Join(root, "output.cdxj")
	corrupt := replayTestRow + "org,example)/ 20260201000000 {\"filename\":\"a.warc\"}\n"
	writeHydrateFixture(t, input, corrupt)
	writeHydrateFixture(t, output, "previous output")
	o := hydrateOptions{input: input, warcRoot: root, indexFile: output, allowUnresolved: true, diagnostics: io.Discard}
	if err := o.run(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid CDXJ row 2") {
		t.Fatalf("hydrate error = %v", err)
	}
	assertHydrateFile(t, input, corrupt)
	assertHydrateFile(t, output, "previous output")

	cluster := filepath.Join(root, "cluster")
	if err := zipnum.WriteSorted(t.Context(), cluster, input, zipnum.Layout{}, strings.NewReader(replayTestRow)); err != nil {
		t.Fatal(err)
	}
	writeHydrateFixture(t, filepath.Join(cluster, "cluster.idx"), "malformed summary")
	o.input = cluster
	if err := o.run(t.Context()); err == nil {
		t.Fatal("hydrated cluster with malformed summary")
	}
	assertHydrateFile(t, output, "previous output")
}

func TestHydrationRejectsOverlappingPaths(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "source.cdxj")
	writeHydrateFixture(t, input, replayTestRow)
	o := hydrateOptions{input: input, warcRoot: root, indexFile: input, diagnostics: io.Discard}
	if err := o.run(t.Context()); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("hydrate error = %v", err)
	}
	assertHydrateFile(t, input, replayTestRow)

	cluster := filepath.Join(root, "source")
	if err := os.Mkdir(cluster, 0700); err != nil {
		t.Fatal(err)
	}
	o.input, o.indexFile = cluster, filepath.Join(cluster, "subdir", "index.cdxj")
	if err := o.run(t.Context()); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("hydrate error = %v", err)
	}
}

func writeHydrateFixture(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertHydrateFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Fatalf("read %s = %q, %v, want %q", path, got, err, want)
	}
}

type cancellingWriter struct{ cancel context.CancelFunc }

func (w cancellingWriter) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}
