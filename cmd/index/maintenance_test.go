// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/fulltext"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
)

func TestOfflineMaintenance(t *testing.T) {
	root := t.TempDir()
	dir, backup, restored := filepath.Join(root, "db"), filepath.Join(root, "backup"), filepath.Join(root, "restored")
	f := fixtures(t)[0]
	source := filepath.Join(root, f.Name)
	if err := os.WriteFile(source, f.Warc, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	o := options(db, source)
	if err := o.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	var before bytes.Buffer
	if err := db.Export(t.Context(), &before); err != nil {
		t.Fatal(err)
	}
	if before.Len() == 0 {
		t.Fatal("empty fixture")
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) error {
		cmd := NewCmdIndex()
		cmd.SetArgs(args)
		return cmd.ExecuteContext(t.Context())
	}
	if err := run("backup", dir, backup); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if err := run("backup", dir, backup); err == nil {
		t.Fatal("backup overwrote existing file")
	}
	unchanged, _ := os.ReadFile(backup)
	if !bytes.Equal(original, unchanged) {
		t.Fatal("backup changed")
	}
	if err := run("restore", backup, restored); err != nil {
		t.Fatal(err)
	}
	if err := run("compact", restored); err != nil {
		t.Fatal(err)
	}
	restoredDB, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: restored, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, restoredDB)
	var after bytes.Buffer
	if err := restoredDB.Export(t.Context(), &after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatal("restored captures differ")
	}
	if err := run("backup", filepath.Join(root, "missing"), filepath.Join(root, "bad")); err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "bad")); !os.IsNotExist(err) {
		t.Fatal("failed backup published")
	}
}

func TestFulltextBackup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "db")
	backup := filepath.Join(root, "backup.zip")
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: "badger", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := backupIndex(t.Context(), dir, backup); err == nil {
		t.Fatal("backup allowed with database open")
	}
	var raw bytes.Buffer
	if err := db.Backup(t.Context(), &raw); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	text, err := fulltext.Open(filepath.Join(dir, "fulltext"))
	if err != nil {
		t.Fatal(err)
	}
	if err := text.Publish(t.Context(), []fulltext.Document{{ID: "one", URL: "https://example.org/", Text: "northern lights", Timestamp: time.Now()}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := backupIndex(t.Context(), dir, backup); err == nil {
		t.Fatal("backup allowed with text index open")
	}
	if err := text.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backupIndex(t.Context(), dir, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(root, "restored")
	restoreConcurrently(t, backup, restored)
	text, err = fulltext.OpenReadOnly(filepath.Join(restored, "fulltext"))
	if err != nil {
		t.Fatal(err)
	}
	defer testcheck.Close(t, text)
	result, err := text.Search(t.Context(), fulltext.Request{Query: "northern"})
	if err != nil || result.Total != 1 {
		t.Fatalf("restored search: %v %v", result, err)
	}
	rawPath := filepath.Join(root, "raw.backup")
	if err := os.WriteFile(rawPath, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	restoreConcurrently(t, rawPath, filepath.Join(root, "raw-restored"))
}

func restoreConcurrently(t *testing.T, backup, restored string) {
	t.Helper()
	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			results <- restoreIndex(t.Context(), backup, restored)
		})
	}
	close(start)
	wg.Wait()
	close(results)
	completed := 0
	for err := range results {
		if err == nil {
			completed++
		} else if !strings.Contains(err.Error(), "restore destination must not exist") {
			t.Errorf("unexpected error from competing restore: %v", err)
		}
	}
	if completed != 1 {
		t.Fatalf("successful restores = %d, want one", completed)
	}
	db, err := cdxstore.Open(t.Context(), cdxstore.Options{Format: badgerFormat, Dir: restored, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsUnsafeBundle(t *testing.T) {
	for _, name := range []string{"../escape", "fulltext/../../escape", "/absolute", "fulltext/../escape", "unexpected"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			var b bytes.Buffer
			z := zip.NewWriter(&b)
			member, err := z.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = member.Write([]byte("bad"))
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(root, "bad.zip")
			if err := os.WriteFile(filename, b.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "restored")
			if err := restoreIndex(t.Context(), filename, target); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatal("failed restore published")
			}
		})
	}
}
