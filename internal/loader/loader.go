// Copyright 2022 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nationallibraryofnorway/gowarc/v3"
	cdx "github.com/nationallibraryofnorway/warchaeology/v5/internal/cdx"
	"github.com/nationallibraryofnorway/warchaeology/v5/internal/warcdigest"
)

type StorageRefResolver interface {
	CaptureByID(context.Context, string) (*cdx.Capture, error)
}

type RecordLoader interface {
	RecordByStorageRef(context.Context, string) (gowarc.WarcRecord, error)
}

type API interface {
	StorageRefById(context.Context, string) (string, error)
	RecordByID(context.Context, string) (gowarc.WarcRecord, error)
	RecordLoader
}

type Loader struct {
	StorageRefResolver
	RecordLoader

	ResolveTarget func(context.Context, string, time.Time) (*cdx.Capture, error)
	NoUnpack      bool
}

type WarcRefersToNotFoundError struct {
	WarcRefersTo string
}

func (e WarcRefersToNotFoundError) Error() string {
	return fmt.Sprintf("record referred via WARC-Refers-To not found: %s", e.WarcRefersTo)
}

func (l *Loader) StorageRefById(ctx context.Context, id string) (string, error) {
	c, err := l.CaptureByID(ctx, id)
	return c.GetRef(), err
}

func (l *Loader) RecordByID(ctx context.Context, id string) (gowarc.WarcRecord, error) {
	c, err := l.CaptureByID(ctx, id)
	if err != nil || c == nil {
		return nil, err
	}
	record, err := l.RecordByStorageRef(WithCapture(ctx, c), c.GetRef())
	if err == nil && record.RecordId() != id {
		return nil, errors.Join(fmt.Errorf("record ID does not match indexed capture"), record.Close())
	}
	return record, err
}

func (l *Loader) RecordByStorageRef(ctx context.Context, storageRef string) (gowarc.WarcRecord, error) {
	return l.load(ctx, storageRef, map[string]bool{})
}

func (l *Loader) load(ctx context.Context, storageRef string, seen map[string]bool) (result gowarc.WarcRecord, err error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if seen[storageRef] || len(seen) >= 32 {
		return nil, fmt.Errorf("revisit reference cycle or chain too deep")
	}
	seen[storageRef] = true
	record, err := l.RecordLoader.RecordByStorageRef(ctx, storageRef)
	if err != nil {
		return nil, err
	}
	if l.NoUnpack || record.Type() != gowarc.Revisit {
		return record, nil
	}
	defer func() {
		err = errors.Join(err, record.Close())
		if err != nil && result != nil {
			err = errors.Join(err, result.Close())
			result = nil
		}
	}()

	header := record.WarcHeader()
	id := header.GetId(gowarc.WarcRefersTo)
	uri := header.Get(gowarc.WarcRefersToTargetURI)
	dateText := header.Get(gowarc.WarcRefersToDate)
	var date time.Time
	if dateText != "" {
		date, err = time.Parse(time.RFC3339Nano, dateText)
		if err != nil {
			return nil, fmt.Errorf("invalid revisit target date: %w", err)
		}
	}
	var target *cdx.Capture
	digest := header.Get(gowarc.WarcPayloadDigest)
	switch {
	case id != "":
		target, err = l.CaptureByID(ctx, id)
	case l.ResolveTarget != nil && uri != "" && !date.IsZero() && digest != "":
		target, err = l.ResolveTarget(ctx, uri, date)
	default:
		return nil, fmt.Errorf("revisit requires a record ID or an exact target URI, date and payload digest")
	}
	if err != nil {
		return nil, err
	}
	if target.GetRef() == "" {
		return nil, WarcRefersToNotFoundError{WarcRefersTo: id + uri}
	}
	donor, err := l.load(WithCapture(ctx, target), target.GetRef(), seen)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, donor.Close()) }()
	donorHeader := donor.WarcHeader()
	if (id != "" && donor.RecordId() != id) ||
		(uri != "" && donorHeader.Get(gowarc.WarcTargetURI) != uri) {
		return nil, fmt.Errorf("revisit target identity does not match donor")
	}
	if !date.IsZero() {
		actual, dateErr := donorHeader.GetTime(gowarc.WarcDate)
		if dateErr != nil || !actual.Equal(date) {
			return nil, fmt.Errorf("revisit target date does not match donor")
		}
	}
	if digest != "" {
		if err := donor.Block().Cache(); err != nil {
			return nil, err
		}
		var body io.Reader
		if payload, ok := donor.Block().(gowarc.PayloadBlock); ok {
			body, err = payload.PayloadBytes()
		} else {
			body, err = donor.Block().RawBytes()
		}
		if err != nil {
			return nil, err
		}
		if err = warcdigest.Verify(digest, body); err != nil {
			return nil, err
		}
	}
	return ReconstructIdentical(record, donor)
}
