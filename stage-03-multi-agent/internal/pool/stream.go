package pool

import (
	"context"
	"sync"
)

func (p *Pool) RunStream(ctx context.Context, jobs []Job) <-chan Result {
	out := make(chan Result, len(jobs))
	sem := make(chan struct{}, p.maxConcurrent)

	var wg sync.WaitGroup

	for i := range jobs {
		job := jobs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				out <- Result{ID: job.ID, Err: ctx.Err()}
				return
			}

			jctx, cancel := context.WithTimeout(ctx, p.jobTimeout)
			defer cancel()

			v, err := job.Run(jctx)
			out <- Result{ID: job.ID, Value: v, Err: err}
		}()
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}
