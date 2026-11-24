// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/timestamp"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/zipnum"
	"github.com/spf13/cobra"
)

type hydrateOptions struct {
	input, warcRoot, donorDir, indexFile, outputDir string
	allowUnresolved                                 bool
	diagnostics                                     io.Writer
}

func newCmdHydrate() *cobra.Command {
	var o hydrateOptions
	cmd := &cobra.Command{
		Use:   "hydrate INPUT",
		Short: "Add verified revisit payload locations to an existing index for pywb",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.input, o.diagnostics = args[0], cmd.ErrOrStderr()
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()
			cmd.SilenceUsage = true
			return o.run(ctx)
		},
	}
	cmd.Flags().StringVar(&o.warcRoot, "warc-root", "", "root directory of the WARCs used for replay")
	cmd.Flags().StringVar(&o.donorDir, "donor-index-dir", "", "additional read-only native donor catalogue")
	cmd.Flags().StringVar(&o.indexFile, "index-file", "", "output CDXJ filename (use .gz for gzip)")
	cmd.Flags().StringVar(&o.outputDir, "output-dir", "", "new ZipNum cluster directory")
	cmd.Flags().BoolVar(&o.allowUnresolved, "allow-unresolved", false, "copy unresolved revisit rows unchanged")
	cmd.MarkFlagsOneRequired("index-file", "output-dir")
	cmd.MarkFlagsMutuallyExclusive("index-file", "output-dir")
	return cmd
}

func (o hydrateOptions) run(ctx context.Context) (err error) {
	if (o.indexFile == "") == (o.outputDir == "") || o.indexFile == "-" || o.outputDir == "-" {
		return fmt.Errorf("specify one named --index-file or --output-dir")
	}
	if o.warcRoot == "" {
		return fmt.Errorf("--warc-root is required")
	}
	output := o.indexFile
	if output == "" {
		output = o.outputDir
	}
	input, err := existingPath(o.input)
	if err != nil {
		return err
	}
	if err := checkHydratePaths(input, output); err != nil {
		return err
	}
	if o.donorDir != "" {
		if err := checkHydratePaths(o.donorDir, output); err != nil {
			return err
		}
	}
	info, err := os.Stat(input)
	if err != nil {
		return err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("input must be a CDXJ file or ZipNum cluster directory")
	}
	rootPath, err := existingPath(o.warcRoot)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	tmp, err := os.MkdirTemp("", "warc-hydrate-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	rows, err := os.Create(filepath.Join(tmp, "input.cdxj"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()

	var out *cdx.Output
	var sorter *cdx.Sorter
	var writeRow func([]byte) error
	if o.indexFile != "" {
		out, err = cdx.NewReplayOutput(output)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, out.Close()) }()
		writeRow = func(row []byte) error { return out.WriteRow(ctx, row) }
	} else {
		sorter = cdx.NewSorter(tmp, false)
		defer func() { err = errors.Join(err, sorter.Close()) }()
		writeRow = func(row []byte) error { return sorter.Add(ctx, string(row[:len(row)-1])) }
	}
	layout, err := o.hydrate(ctx, input, info.IsDir(), tmp, mountedWARCs{root, rootPath}, rows, writeRow)
	if err != nil {
		return err
	}
	if out != nil {
		return out.Flush(ctx)
	}
	sorted, err := os.Create(filepath.Join(tmp, "sorted.cdxj"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sorted.Close()) }()
	if err := sorter.WriteTo(ctx, sorted); err != nil {
		return err
	}
	if _, err := sorted.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return zipnum.WriteSorted(ctx, output, input, layout, sorted)
}

func (o hydrateOptions) hydrate(ctx context.Context, input string, cluster bool, tmp string, archives mountedWARCs, rows *os.File, writeRow func([]byte) error) (layout zipnum.Layout, err error) {
	catalogue, err := openHydrateCatalogue(ctx, tmp, o.donorDir, archives)
	if err != nil {
		return layout, err
	}
	defer func() { err = errors.Join(err, catalogue.close()) }()
	buffer := bufio.NewWriter(rows)
	var number int
	indexRow := func(row []byte) error {
		number++
		c, err := cdx.ParseReplayLine(row[:len(row)-1])
		if err != nil {
			return fmt.Errorf("invalid CDXJ row %d: %w", number, err)
		}
		if err := catalogue.add(ctx, c); err != nil {
			return err
		}
		_, err = buffer.Write(row)
		return err
	}
	if cluster {
		layout, err = zipnum.ScanRows(ctx, input, indexRow)
	} else {
		err = scanCDXJFile(ctx, input, indexRow)
	}
	if err != nil {
		return layout, err
	}
	if err := buffer.Flush(); err != nil {
		return layout, err
	}
	if _, err := rows.Seek(0, io.SeekStart); err != nil {
		return layout, err
	}
	return layout, o.hydrateRows(ctx, catalogue, rows, writeRow)
}

func (o hydrateOptions) hydrateRows(ctx context.Context, catalogue *hydrateCatalogue, rows io.Reader, writeRow func([]byte) error) error {
	resolver := loader.Loader{
		StorageRefResolver: catalogue,
		TargetCaptures:     catalogue.capturesByTarget,
		RecordLoader:       catalogue.archives,
	}
	var resolved, hydrated, skipped, unresolved int
	err := scanCDXJRows(ctx, rows, func(row []byte) error {
		c, err := cdx.ParseReplayLine(row[:len(row)-1])
		if err != nil {
			return err
		}
		fields, err := cdx.Properties(c)
		if err != nil {
			return err
		}
		if c.GetMct() != "warc/revisit" && !hasOriginalFields(fields) {
			skipped++
			return writeRow(row)
		}
		catalogue.archives.locate(c, false)
		donor, err := resolver.RevisitDonor(ctx, c)
		if cause := ctx.Err(); cause != nil {
			return cause
		}
		var lookupErr hydrateLookupError
		switch {
		case errors.As(err, &lookupErr):
			return err
		case errors.Is(err, loader.ErrRedirectRevisit):
			skipped++
		case err != nil:
			unresolved++
			if _, err := fmt.Fprintf(o.diagnostics, "unresolved key=%q timestamp=%s filename=%q offset=%d reason=%q\n",
				c.GetSsu(), timestamp.Format14(c.GetSts().AsTime()), c.GetFilename(), c.GetOffset(), err); err != nil {
				return err
			}
		case catalogue.archives.matchesOriginal(ctx, fields, donor):
			hydrated++
		default:
			resolved++
			if err := setPywbOriginal(c, donor); err != nil {
				return err
			}
			line, err := cdx.Line(c)
			if err != nil {
				return err
			}
			row = append(line, '\n')
		}
		return writeRow(row)
	})
	_, reportErr := fmt.Fprintf(o.diagnostics, "resolved=%d already-hydrated=%d skipped=%d unresolved=%d\n", resolved, hydrated, skipped, unresolved)
	if err := errors.Join(err, reportErr, ctx.Err()); err != nil {
		return err
	}
	if unresolved > 0 && !o.allowUnresolved {
		return fmt.Errorf("%d unresolved revisit rows", unresolved)
	}
	return nil
}

func hasOriginalFields(fields map[string]any) bool {
	for _, key := range []string{"orig.filename", "orig.offset", "orig.length"} {
		switch fields[key] {
		case nil, "", "-":
		default:
			return true
		}
	}
	return false
}

func cdxNumber(value any) (int64, error) {
	var text string
	switch n := value.(type) {
	case string:
		text = n
	case jsontext.Value:
		text = string(n)
	}
	return strconv.ParseInt(text, 10, 64)
}

func scanCDXJFile(ctx context.Context, path string, yield func([]byte) error) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	buffer := bufio.NewReader(file)
	magic, _ := buffer.Peek(2)
	if bytes.Equal(magic, []byte{0x1f, 0x8b}) || strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(buffer)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, gz.Close()) }()
		return scanCDXJRows(ctx, gz, yield)
	}
	return scanCDXJRows(ctx, buffer, yield)
}

func scanCDXJRows(ctx context.Context, source io.Reader, yield func([]byte) error) error {
	reader := bufio.NewReader(source)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := reader.ReadBytes('\n')
		if len(row) > 0 {
			row = bytes.TrimSuffix(row, []byte("\n"))
			row = bytes.TrimSuffix(row, []byte("\r"))
			if err := yield(append(row, '\n')); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func existingPath(path string) (string, error) {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func checkHydratePaths(source, output string) error {
	source, err := existingPath(source)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	// Resolve the existing prefix, including links to parents of a new output.
	target := output
	for prefix := output; ; prefix = filepath.Dir(prefix) {
		resolved, err := existingPath(prefix)
		if err == nil {
			suffix, err := filepath.Rel(prefix, output)
			if err != nil {
				return err
			}
			target = filepath.Join(resolved, suffix)
			break
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(prefix) == prefix {
			return err
		}
	}
	for _, candidate := range []string{output, target} {
		if insidePath(source, candidate) || insidePath(candidate, source) {
			return fmt.Errorf("output %q overlaps input %q", output, source)
		}
	}
	if dst, err := os.Stat(output); err == nil {
		src, err := os.Stat(source)
		if err != nil {
			return err
		}
		if os.SameFile(src, dst) {
			return fmt.Errorf("output %q overlaps input %q", output, source)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func insidePath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}
