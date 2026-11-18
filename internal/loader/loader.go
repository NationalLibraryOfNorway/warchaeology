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
	CapturesByID(context.Context, string, func(*cdx.Capture) error) error
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

	TargetCaptures func(context.Context, string, time.Time, func(*cdx.Capture) error) error
	NoUnpack       bool
}

type WarcRefersToNotFoundError struct {
	WarcRefersTo string
}

var ErrRedirectRevisit = errors.New("redirect revisit uses its own HTTP headers")

func (e WarcRefersToNotFoundError) Error() string {
	return fmt.Sprintf("record referred via WARC-Refers-To not found: %s", e.WarcRefersTo)
}

func (l *Loader) CaptureByID(ctx context.Context, id string) (*cdx.Capture, error) {
	choice := captureChoice{target: "record ID " + id}
	if err := l.CapturesByID(ctx, id, func(c *cdx.Capture) error {
		choice.add(ctx, l.RecordLoader, c)
		return nil
	}); err != nil {
		return nil, err
	}
	if choice.err != nil {
		return nil, choice.err
	}
	return choice.first, nil
}

// captureChoice compares physical records while retaining a single candidate.
type captureChoice struct {
	target      string
	first       *cdx.Capture
	fingerprint string
	err         error
}

func (choice *captureChoice) add(ctx context.Context, records RecordLoader, c *cdx.Capture) {
	if choice.err != nil {
		return
	}
	if choice.first == nil {
		choice.first = c
		return
	}
	if choice.fingerprint == "" {
		choice.fingerprint, choice.err = CaptureFingerprint(ctx, records, choice.first)
		if choice.err != nil {
			return
		}
	}
	hash, err := CaptureFingerprint(ctx, records, c)
	if err == nil && hash != choice.fingerprint {
		err = fmt.Errorf("ambiguous %s", choice.target)
	}
	choice.err = err
}

// CaptureFingerprint compares the WARC header and complete block at an indexed position.
func CaptureFingerprint(ctx context.Context, records RecordLoader, c *cdx.Capture) (string, error) {
	record, err := records.RecordByStorageRef(WithCapture(ctx, c), c.GetRef())
	if err != nil {
		return "", err
	}
	hash, err := warcdigest.RecordFingerprint(record)
	return hash, errors.Join(err, record.Close())
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
	return l.load(ctx, storageRef, map[string]bool{}, nil, nil)
}

// RevisitDonor returns the physical response capture supplying a verified payload.
func (l *Loader) RevisitDonor(ctx context.Context, revisit *cdx.Capture) (*cdx.Capture, error) {
	var donor *cdx.Capture
	record, err := l.load(WithCapture(ctx, revisit), revisit.GetRef(), map[string]bool{}, &donor, nil)
	if err != nil {
		return nil, err
	}
	return donor, record.Close()
}

type revisitTarget struct {
	id, uri string
	date    time.Time
}

func (target *revisitTarget) check(record gowarc.WarcRecord) error {
	header := record.WarcHeader()
	if (target.id != "" && record.RecordId() != target.id) ||
		(target.uri != "" && header.Get(gowarc.WarcTargetURI) != target.uri) {
		return fmt.Errorf("revisit target identity does not match donor")
	}
	if !target.date.IsZero() {
		actual, err := header.GetTime(gowarc.WarcDate)
		if err != nil || !actual.Equal(target.date) {
			return fmt.Errorf("revisit target date does not match donor")
		}
	}
	return nil
}

func (l *Loader) load(ctx context.Context, storageRef string, seen map[string]bool, terminal **cdx.Capture, expected *revisitTarget) (result gowarc.WarcRecord, err error) {
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
	if expected != nil {
		if err := expected.check(record); err != nil {
			return nil, errors.Join(err, record.Close())
		}
	}
	if l.NoUnpack || record.Type() != gowarc.Revisit {
		if terminal != nil {
			capture := CaptureFromContext(ctx)
			block, http := record.Block().(gowarc.HttpResponseBlock)
			if record.Type() != gowarc.Response || !http || len(seen) == 1 || record.WarcHeader().Get(gowarc.WarcTruncated) != "" {
				return nil, errors.Join(fmt.Errorf("expected a complete HTTP response donor for the revisit"), record.Close())
			}
			capture.Mct = cdx.MediaType(block.HttpHeader().Get("Content-Type"))
			*terminal = capture
		}
		return record, nil
	}
	defer func() {
		err = errors.Join(err, record.Close())
		if err != nil && result != nil {
			err = errors.Join(err, result.Close())
			result = nil
		}
	}()

	if terminal != nil && len(seen) == 1 {
		if status := cdx.RevisitStatus(record); status >= 300 && status < 400 {
			return nil, ErrRedirectRevisit
		}
	}
	if err := checkIdenticalProfile(record); err != nil {
		return nil, err
	}
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
	if terminal != nil && digest == "" {
		return nil, fmt.Errorf("revisit has no payload digest")
	}
	switch {
	case id != "":
		target, err = l.CaptureByID(ctx, id)
	case l.TargetCaptures != nil && uri != "" && !date.IsZero() && digest != "":
		target, err = l.captureByTarget(ctx, uri, date, digest, terminal != nil)
	default:
		return nil, fmt.Errorf("revisit requires a record ID or an exact target URI, date and payload digest")
	}
	if err != nil {
		return nil, err
	}
	if target.GetRef() == "" {
		return nil, WarcRefersToNotFoundError{WarcRefersTo: id + uri}
	}
	donor, err := l.load(WithCapture(ctx, target), target.GetRef(), seen, terminal, &revisitTarget{id, uri, date})
	if err != nil {
		return nil, err
	}
	defer func() {
		if result != donor {
			err = errors.Join(err, donor.Close())
		}
	}()
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
	if terminal != nil {
		// The exporter needs the verified payload location. Forward the physical
		// donor so each enclosing revisit can verify its own digest.
		return donor, nil
	}
	return ReconstructIdentical(record, donor)
}

func (l *Loader) captureByTarget(ctx context.Context, uri string, date time.Time, digest string, pywb bool) (*cdx.Capture, error) {
	choice := captureChoice{target: "revisit target"}
	matchedDigest := false
	var verifyErr error
	err := l.TargetCaptures(ctx, uri, date, func(c *cdx.Capture) error {
		// Native captures can carry the exact date. CDXJ generally
		// provides only a timestamp truncated to the second.
		if verifyErr != nil || (pywb && c.GetSrt() != "" && c.GetSrt() != "response" && c.GetSrt() != "revisit") {
			return nil
		}
		actual := c.GetSts().AsTime()
		label := c.GetDig()
		if actual.Nanosecond() == 0 || (pywb && c.GetSrt() == "") {
			record, err := l.RecordLoader.RecordByStorageRef(WithCapture(ctx, c), c.GetRef())
			if err != nil {
				verifyErr = err
				return nil
			}
			header := record.WarcHeader()
			actual, err = header.GetTime(gowarc.WarcDate)
			if physical := header.Get(gowarc.WarcPayloadDigest); physical != "" {
				label = physical
			}
			eligible := !pywb || record.Type() == gowarc.Response || record.Type() == gowarc.Revisit
			if verifyErr = errors.Join(err, record.Close()); verifyErr != nil || !eligible {
				return nil
			}
		}
		if !actual.Equal(date) {
			return nil
		}
		matches := pywb && warcdigest.Equal(digest, label)
		if matches && !matchedDigest {
			// As with the indexed lookup, prefer matches for the known digest.
			choice = captureChoice{target: choice.target}
			matchedDigest = true
		}
		if matches == matchedDigest {
			choice.add(ctx, l.RecordLoader, c)
		}
		return nil
	})
	if err = errors.Join(err, verifyErr, choice.err); err != nil {
		return nil, err
	}
	return choice.first, nil
}
