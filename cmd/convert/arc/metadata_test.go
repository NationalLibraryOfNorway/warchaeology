package arc

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nationallibraryofnorway/gowarc/v3"
	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/internal/flag"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestARCWarcinfoVersion(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)
	f := NewConvertArcFlags()
	cmd := &cobra.Command{Use: "arc"}
	f.AddFlags(cmd)
	if err := viper.BindPFlags(cmd.Flags()); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	viper.Set(flag.OutputDir, out)
	viper.Set(flag.WarcVersion, "1.0")
	viper.Set(flag.Compress, false)
	viper.Set(flag.TempDir, t.TempDir())
	opts, err := f.ToConvertArcOptions()
	if err != nil {
		t.Fatal(err)
	}
	defer opts.Close()
	input := filepath.Join(testDataDir, "arc", "ARC-SAMPLE-20060928223931-00000-gojoblack.arc.gz")
	if _, err := opts.handleFile(afero.NewOsFs(), input); err != nil {
		t.Fatal(err)
	}
	names, err := filepath.Glob(filepath.Join(out, "*.warc"))
	if err != nil || len(names) != 1 {
		t.Fatalf("output = %v, %v", names, err)
	}
	r, err := gowarc.NewWarcFileReader(names[0], 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rec, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	if rec.WarcRecord.Type() != gowarc.Warcinfo {
		t.Fatalf("first record = %v", rec.WarcRecord.Type())
	}
	body, err := rec.WarcRecord.Block().RawBytes()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Format: WARC File Format 1.0") {
		t.Fatalf("warcinfo = %s", data)
	}
}
