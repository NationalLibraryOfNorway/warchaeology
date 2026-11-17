package workerpool

import (
	"context"
	"sync"
)

type WorkerPool struct {
	Jobs        chan<- func()
	pending     sync.WaitGroup
	waitGroup   sync.WaitGroup
	concurrency int
	ctx         context.Context
}

func New(ctx context.Context, concurrency int) *WorkerPool {
	jobs := make(chan func(), concurrency)

	pool := &WorkerPool{
		Jobs:        jobs,
		concurrency: concurrency,
		ctx:         ctx,
	}

	for range concurrency {
		pool.waitGroup.Go(func() {
			for job := range jobs {
				job()
			}
		})
	}

	return pool
}

func (pool *WorkerPool) CloseWait() {
	close(pool.Jobs)
	pool.waitGroup.Wait()
}

// Wait waits for previously submitted jobs while keeping the pool open.
func (pool *WorkerPool) Wait() { pool.pending.Wait() }

// Submit returns the context error if cancellation prevents accepting work.
func (pool *WorkerPool) Submit(fn func()) error {
	if err := pool.ctx.Err(); err != nil {
		return err
	}
	if pool.concurrency == 0 {
		fn()
		return nil
	}
	pool.pending.Add(1)
	job := func() {
		defer pool.pending.Done()
		if pool.ctx.Err() == nil {
			fn()
		}
	}
	select {
	case <-pool.ctx.Done():
		pool.pending.Done()
		return pool.ctx.Err()
	case pool.Jobs <- job:
		return nil
	}
}
