// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package arc

import (
	"path/filepath"
	"testing"

	"github.com/nationallibraryofnorway/warchaeology/v5/cmd/convert/internal/conversion"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/testcheck"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcwriterconfig"
	"github.com/spf13/afero"
)

var (
	testDataDir = filepath.Join("..", "..", "..", "testdata")
)

func TestConvertArcFile(t *testing.T) {
	tests := []struct {
		name      string
		wantError bool
	}{
		{
			name: filepath.Join(testDataDir, "arc", "ARC-SAMPLE-20060928223931-00000-gojoblack.arc.gz"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filename, err := filepath.Abs(tt.name)
			if err != nil {
				t.Fatalf("failed to get absolute path: %v", err)
			}

			warcWriterConfig, err := warcwriterconfig.New("test",
				warcwriterconfig.WithBufferTmpDir(t.TempDir()),
				warcwriterconfig.WithOutDir(t.TempDir()),
				warcwriterconfig.WithCompress(false),
			)
			if err != nil {
				t.Fatalf("failed to create warc writer config: %v", err)
			}

			o := &ConvertArcOptions{
				WarcWriterConfig: warcWriterConfig,
			}
			defer testcheck.Close(t, o)

			result, err := (conversion.RunOptions{Writer: o.WarcWriterConfig}).ConvertFile(t.Context(), afero.NewOsFs(), filename, o.handleFile)
			if err != nil {
				if !tt.wantError {
					t.Fatal(err)
				}
			}
			if !tt.wantError && result.ErrorCount() > 0 {
				for _, err := range result.Errors() {
					t.Error(err)
				}
			} else if tt.wantError && result.ErrorCount() == 0 {
				t.Errorf("readFile() expected error, got none")
			}
		})
	}
}
