// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package index

import (
	"errors"
	"time"

	"github.com/dgraph-io/badger/v4"
)

const (
	maxConflictRetries = 32
	conflictBackoff    = time.Millisecond
)

func runWithConflictRetry(op func() error) error {
	var err error

	for attempt := range maxConflictRetries {
		err = op()
		if !errors.Is(err, badger.ErrConflict) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * conflictBackoff)
	}

	return err
}
