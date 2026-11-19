// Copyright 2026 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package cdxstore

import "context"

type contextLocker interface {
	Lock()
	Unlock()
	TryLock() bool
}

// Wait in the lock's normal queue (including RWMutex writer preference), while
// allowing an upload to time out. A canceled waiter only releases the lock; it
// never touches database state after ownership was abandoned.
func lockContext(ctx context.Context, lock contextLocker) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lock.TryLock() {
		return nil
	}
	acquired := make(chan struct{})
	go func() {
		lock.Lock()
		select {
		case acquired <- struct{}{}:
		case <-ctx.Done():
			lock.Unlock()
		}
	}()
	select {
	case <-acquired:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
