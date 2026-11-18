package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipnum"
	"github.com/spf13/viper"
)

func TestZipnumSubcommands(t *testing.T) {
	t.Cleanup(viper.Reset)
	dir := t.TempDir()
	input := filepath.Join(dir, "part,2.cdxj")
	if err := os.WriteFile(input, []byte("com,example)/ 20200101000000 {\"url\":\"https://example.com/\",\"filename\":\"a.warc.gz\",\"offset\":\"0\",\"length\":\"100\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing*.cdxj")
	unpublished := filepath.Join(dir, "unpublished")
	if err := executeCaptureCommand(t, "zipnum", "build", input, missing, "--output-dir", unpublished); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("unmatched glob: %v", err)
	}
	if _, err := os.Stat(unpublished); !os.IsNotExist(err) {
		t.Fatalf("partial cluster: %v", err)
	}
	first, added := filepath.Join(dir, "first"), filepath.Join(dir, "added")
	merged, repartitioned := filepath.Join(dir, "merged"), filepath.Join(dir, "repartitioned")
	for _, tt := range []struct {
		args   []string
		output string
		count  int
	}{
		{[]string{"build", input, "--lines", "1", "--blocks", "1"}, first, 1},
		{[]string{"add", first, input}, added, 2},
		{[]string{"merge", first, added}, merged, 3},
		{[]string{"repartition", merged, "--lines", "2", "--blocks", "2"}, repartitioned, 3},
	} {
		viper.Reset()
		command := NewWarcCommand()
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		args := append([]string{"zipnum"}, tt.args...)
		command.SetArgs(append(args, "--output-dir", tt.output))
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		reader, err := zipnum.Open(tt.output)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		err = reader.Scan(context.Background(), "", "", func(*cdx.Capture) error { count++; return nil })
		closeErr := reader.Close()
		if err != nil || closeErr != nil || count != tt.count {
			t.Fatalf("%s: captures=%d want=%d, scan=%v close=%v", tt.args[0], count, tt.count, err, closeErr)
		}
	}
}


func executeCaptureCommand(t *testing.T, args ...string) error {
	t.Helper()
	viper.Reset()
	t.Cleanup(viper.Reset)
	command := NewWarcCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs(args)
	return command.Execute()
}
