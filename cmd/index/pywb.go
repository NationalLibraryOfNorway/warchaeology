// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"

	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/cdxstore"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/loader"
)

func (o *IndexOptions) exportPywb(ctx context.Context, db *cdxstore.DB) (err error) {
	out, err := cdx.NewOutput(o.stdout, o.output, "cdxj", o.tmpDir, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	records := loader.Loader{
		StorageRefResolver: db,
		TargetCaptures:     db.VisitCapturesByTarget,
		RecordLoader:       loader.FileStorageLoader{ValidateSource: db.ValidateArchiveSource},
	}
	files := make(map[string]string)
	register := func(c *cdx.Capture) error {
		filename, source := c.GetFilename(), c.GetSource()
		if previous, ok := files[filename]; ok && previous != source {
			return fmt.Errorf("ambiguous pywb archive filename %q; use unique filenames or --dir-root", filename)
		}
		files[filename] = source
		return nil
	}
	err = db.Scan(ctx, "", "", func(c *cdx.Capture) error {
		if len(c.GetExtraJson()) == 0 {
			return nil
		}
		if err := register(c); err != nil {
			return err
		}
		c.ExtraJson = nil
		// pywb replays redirect revisits using their own HTTP headers.
		if c.GetSrt() == "revisit" && (c.GetHsc() < 300 || c.GetHsc() >= 400) {
			donor, err := records.RevisitDonor(ctx, c)
			if err != nil {
				return fmt.Errorf("resolve pywb revisit %s at %s: %w", c.GetUri(), c.GetRef(), err)
			}
			if err := register(donor); err != nil {
				return err
			}
			if err := setPywbOriginal(c, donor); err != nil {
				return err
			}
		}
		return out.WriteCapture(ctx, c)
	})
	if err != nil {
		return err
	}
	return out.Flush(ctx)
}

func setPywbOriginal(c, donor *cdx.Capture) error {
	fields, err := cdx.Properties(c)
	if err != nil {
		return err
	}
	// pywb uses the explicit original location when the row carries resolved MIME.
	fields["mime"] = donor.GetMct()
	fields["orig.filename"] = donor.GetFilename()
	fields["orig.offset"] = strconv.FormatInt(donor.GetOffset(), 10)
	fields["orig.length"] = strconv.FormatInt(donor.GetRle(), 10)
	c.ExtraJson, err = json.Marshal(fields)
	return err
}
