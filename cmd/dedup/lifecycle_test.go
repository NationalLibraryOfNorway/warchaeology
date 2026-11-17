package dedup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/filewalker"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type failingCloseFS struct {
	afero.Fs

	err    error
	closes int
}

func (fs *failingCloseFS) Close() error {
	fs.closes++
	return fs.err
}

func TestOptionsCloseReleasesIndexesAndReportsFilesystemError(t *testing.T) {
	dir := t.TempDir()
	o := &DedupOptions{}
	t.Cleanup(func() { _ = o.Close() })
	var err error
	o.DigestIndex, err = index.NewDigestIndex(dir, false, true)
	if err != nil {
		t.Fatal(err)
	}
	o.FileIndex, err = index.NewFileIndex(dir, false, true)
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("filesystem cleanup failed")
	fs := &failingCloseFS{Fs: afero.NewMemMapFs(), err: wantErr}
	o.FileWalker = filewalker.New(filewalker.WithFs(fs))
	if err := o.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("Close = %v, want %v", err, wantErr)
	}
	for _, name := range []string{"digest-index", "file-index"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists after Close: %v", name, err)
		}
	}
	// A repeated close must not remove files created after the first cleanup.
	recreated := filepath.Join(dir, "digest-index")
	if err := os.Mkdir(recreated, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(recreated); err != nil {
		t.Fatal(err)
	}
	if fs.closes != 1 {
		t.Fatalf("filesystem closed %d times", fs.closes)
	}
}

func TestConstructorFailureClosesIndexes(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	f := NewDedupFlags()
	cmd := &cobra.Command{Use: "dedup"}
	f.AddFlags(cmd)
	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	viper.Set(flag.IndexDir, dir)
	viper.Set(flag.KeepIndex, true)
	viper.Set(flag.OutputDir, t.TempDir())
	viper.Set(flag.SrcFileSystem, filepath.Join(t.TempDir(), "missing.zip"))
	o, err := f.ToDedupOptions()
	if err == nil {
		_ = o.Close()
		t.Fatal("expected archive open failure")
	}
	// Reopening retained indexes proves their database locks were released.
	digest, err := index.NewDigestIndex(dir, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer digest.Close()
	files, err := index.NewFileIndex(dir, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
}
