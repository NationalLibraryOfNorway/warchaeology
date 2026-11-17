package workerpool

import (
	"context"
	"sync"
)

type WorkerPool struct {
	Jobs        chan<- func()
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
				select {
				case <-ctx.Done():
					return
				default:
					job()
				}
			}
		})
	}

	return pool
}

func (pool *WorkerPool) CloseWait() {
	close(pool.Jobs)
	pool.waitGroup.Wait()
}

// Submit returns the context error if cancellation prevents accepting work.
func (pool *WorkerPool) Submit(fn func()) error {
	if err := pool.ctx.Err(); err != nil {
		return err
	}
	if pool.concurrency == 0 {
		fn()
		return nil
	}
	select {
	case <-pool.ctx.Done():
		return pool.ctx.Err()
	case pool.Jobs <- fn:
		return nil
	}
}
