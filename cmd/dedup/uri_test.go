package dedup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/index"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/spf13/afero"
)

func TestURITextPreserved(t *testing.T) {
	uri := "HTTP://Example.COM:80/a/../%FC?q=+&q=%2f&flag&empty=#fragment"
	input := filepath.Join(t.TempDir(), "input.warc")
	file, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		b := gowarc.NewRecordBuilder(gowarc.Response, gowarc.WithAddMissingDigest(true))
		b.AddWarcHeader(gowarc.WarcTargetURI, uri)
		b.AddWarcHeaderTime(gowarc.WarcDate, time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC))
		b.AddWarcHeader(gowarc.ContentType, "application/http;msgtype=response")
		if _, err := b.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 7168\r\n\r\n" + strings.Repeat("payload", 1024)); err != nil {
			t.Fatal(err)
		}
		rec, _, err := b.Build()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := gowarc.NewMarshaler().Marshal(file, rec, 0); err != nil {
			t.Fatal(err)
		}
		if err := rec.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	cfg, err := warcwriterconfig.New("uri", warcwriterconfig.WithOutDir(out), warcwriterconfig.WithBufferTmpDir(t.TempDir()), warcwriterconfig.WithCompress(false))
	if err != nil {
		t.Fatal(err)
	}

	idx, err := index.NewDigestIndex(t.TempDir(), false, true)
	if err != nil {
		_ = cfg.Close()
		t.Fatal(err)
	}

	o := &DedupOptions{WarcWriterConfig: cfg, DigestIndex: idx, RecordTypes: []gowarc.RecordType{gowarc.Response}, MinimumSizeGain: 0}
	defer o.Close()
	result, err := o.handleFile(afero.NewOsFs(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.ErrorCount() != 0 {
		t.Fatalf("record errors: %v", result.Errors())
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	var seen []gowarc.RecordType
	err = filepath.WalkDir(out, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		r, err := gowarc.NewWarcFileReader(name, 0)
		if err != nil {
			return err
		}
		defer r.Close()
		for rec, err := range r.Records() {
			if err != nil {
				return err
			}
			if rec.WarcRecord.Type() == gowarc.Warcinfo {
				rec.Close()
				continue
			}
			seen = append(seen, rec.WarcRecord.Type())
			if got := rec.WarcRecord.WarcHeader().Get(gowarc.WarcTargetURI); got != uri {
				t.Errorf("target URI = %q, want %q", got, uri)
			}
			if rec.WarcRecord.Type() == gowarc.Revisit {
				if got := rec.WarcRecord.WarcHeader().Get(gowarc.WarcRefersToTargetURI); got != uri {
					t.Errorf("reference URI = %q, want %q", got, uri)
				}
			}
			rec.Close()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("records = %v", seen)
	}
	if seen[1] != gowarc.Revisit {
		t.Fatalf("second record type = %v, want revisit", seen[1])
	}
}
