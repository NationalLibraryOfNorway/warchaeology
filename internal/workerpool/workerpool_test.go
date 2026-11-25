// Copyright 2024 National Library of Norway.
// SPDX-License-Identifier: Apache-2.0

package workerpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestCreatePool(t *testing.T) {
	pool := New(context.Background(), 1)
	pool.CloseWait()
}

func TestSubmitJob(t *testing.T) {
	pool := New(context.Background(), 1)
	pool.Jobs <- func() {}
	pool.CloseWait()
}

func TestSubmitToClosedWorkQueue(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic")
		}
	}()
	queue := New(context.Background(), 1)
	queue.CloseWait()
	queue.Jobs <- func() {}
}

func TestWorkerPool(t *testing.T) {
	const concurrency = 16
	const jobs = 1024
	var executed atomic.Int32

	queue := New(context.Background(), concurrency)
	for range jobs {
		queue.Jobs <- func() { executed.Add(1) }
	}
	queue.CloseWait()

	queueLength := len(queue.Jobs)
	if queueLength != 0 {
		t.Errorf("expected queue to be empty, but got %d jobs", queueLength)
	}
	if executed.Load() != int32(jobs) {
		t.Errorf("expected %d jobs to have been executed, but got %d", jobs, executed.Load())
	}
}

func TestSubmitCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := New(ctx, 1)
	started := make(chan struct{})
	release := make(chan struct{})
	if err := pool.Submit(func() { close(started); <-release }); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := pool.Submit(func() {}); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- pool.Submit(func() { t.Error("cancelled job ran") }) }()
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Errorf("submission = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("submission blocked after cancellation")
	}
	close(release)
	pool.CloseWait()
	syncPool := New(ctx, 0)
	defer syncPool.CloseWait()
	if err := syncPool.Submit(func() { t.Error("synchronous cancelled job ran") }); err != context.Canceled {
		t.Errorf("sync submit = %v", err)
	}
}
