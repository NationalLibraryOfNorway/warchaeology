/*
 * Copyright 2022 National Library of Norway.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *       http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package loader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nationallibraryofnorway/gowarc/v3"
)

type StorageRefResolver interface {
	StorageRefById(ctx context.Context, warcId string) (storageRef string, err error)
}

type RecordLoader interface {
	RecordByStorageRef(context.Context, string) (gowarc.WarcRecord, error)
}

type API interface {
	StorageRefResolver
	RecordLoader
}

type Loader struct {
	StorageRefResolver
	RecordLoader

	NoUnpack bool
}

type ResolveRevisitError struct {
	Profile   string
	TargetURI string
	Date      string
}

func (e ResolveRevisitError) Error() string {
	return fmt.Sprintf("Resolving via Warc-Refers-To-Date and Warc-Refers-To-Target-URI failed: %s", e.String())
}

func (e ResolveRevisitError) String() string {
	return fmt.Sprintf("Warc-Refers-To-Date: %s, Warc-Refers-To-Target-URI: %s, Warc-Profile: %s", e.Date, e.TargetURI, e.Profile)
}

type WarcRefersToNotFoundError struct {
	WarcRefersTo string
}

func (e WarcRefersToNotFoundError) Error() string {
	return fmt.Sprintf("record referred via WARC-Refers-To not found: %s", e.WarcRefersTo)
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
	defer func() {
		if err != nil {
			err = errors.Join(err, record.Close())
		}
	}()
	if l.NoUnpack {
		return record, nil
	}

	var rtrRecord gowarc.WarcRecord

	//nolint:exhaustive
	switch record.Type() {
	case gowarc.Revisit:
		slog.Debug("Loader found a revisit record",
			"storageRef", storageRef,
			"warcRefersTo", record.WarcHeader().Get(gowarc.WarcRefersTo),
			"warcRefersToTargetURI", record.WarcHeader().Get(gowarc.WarcRefersToTargetURI),
			"warcRefersToDate", record.WarcHeader().Get(gowarc.WarcRefersToDate),
		)
		warcRefersTo := record.WarcHeader().GetId(gowarc.WarcRefersTo)
		if warcRefersTo == "" {
			warcRefersToTargetURI := record.WarcHeader().Get(gowarc.WarcRefersToTargetURI)
			warcRefersToDate := record.WarcHeader().Get(gowarc.WarcRefersToDate)
			if warcRefersToTargetURI == "" {
				return nil, fmt.Errorf("failed to resolve revisit record: neither WARC-Refers-To nor Warc-Refers-To-Target-URI")
			}
			if warcRefersToDate == "" {
				warcRefersToDate = record.WarcHeader().Get(gowarc.WarcDate)
			}
			return nil, ResolveRevisitError{
				Profile:   record.WarcHeader().Get(gowarc.WarcProfile),
				TargetURI: warcRefersToTargetURI,
				Date:      warcRefersToDate,
			}
		}

		var revisitOf gowarc.WarcRecord
		storageRef, err = l.StorageRefById(ctx, warcRefersTo)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve WARC-Refers-To [%s]: %w", warcRefersTo, err)
		}
		if storageRef == "" {
			return nil, WarcRefersToNotFoundError{WarcRefersTo: warcRefersTo}
		}
		revisitOf, err = l.load(ctx, storageRef, seen)
		if err != nil {
			return nil, err
		}

		rtrRecord, err = ReconstructIdentical(record, revisitOf)
		closeErr := errors.Join(record.Close(), revisitOf.Close())
		if err != nil || closeErr != nil {
			if rtrRecord != nil {
				_ = rtrRecord.Close()
			}
			return nil, errors.Join(err, closeErr)
		}
	case gowarc.Continuation:
		slog.Warn("Not implemented: storage ref resolved to a continuation record")
		// TODO continuation not implemented
		fallthrough
	default:
		rtrRecord = record
	}

	return rtrRecord, nil
}
