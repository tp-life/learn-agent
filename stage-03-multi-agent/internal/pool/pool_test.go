package pool

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestRun_RespectsConcurrencyLimit(t *testing.T) {
	const limit = 3

	p := New(limit, time.Second)

	var inFlight, peak atomic.Int32
	jobs := make([]Job, 10)
	for i := range jobs {
		i := i
		jobs[i] = Job{
			ID: fmt.Sprintf("job-%d", i),
			Run: func(ctx context.Context) (string, error) {
				cur := inFlight.Add(1)
				defer inFlight.Add(-1)

				for {
					m := peak.Load()
					if cur <= m || peak.CompareAndSwap(m, cur) {
						break
					}
				}
				select {
				case <-time.After(20 * time.Millisecond):
					return "ok", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			},
		}
	}

	results := p.Run(context.Background(), jobs)
	if len(results) != len(jobs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(jobs))
	}

	for i, r := range results {
		if r.Err != nil {
			t.Errorf("result[%d].Err = %v, want nil", i, r.Err)
		}

		if r.ID != jobs[i].ID {
			t.Errorf("results[%d].ID = %q, want %q (顺序应与 jobs 一致)", i, r.ID, jobs[i].ID)
		}
	}

	if got := peak.Load(); got > limit {
		t.Errorf("并发峰值 =%d, 超过上限 %d", got, limit)
	}
	if got := peak.Load(); got < 2 {
		t.Errorf("并发峰值 = %d, 疑似没有真正并行（测试失败）", got)
	}
}

func TestRun_JobTimeout(t *testing.T) {
	p := New(2, 50*time.Millisecond)

	jobs := []Job{
		{ID: "slow", Run: func(ctx context.Context) (string, error) {
			select {
			case <-time.After(5 * time.Second):
				return "done", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}},
		{ID: "fast", Run: func(ctx context.Context) (string, error) {
			return "quick", nil
		}},
	}

	start := time.Now()
	results := p.Run(context.Background(), jobs)
	elapsed := time.Since(start)

	if !errors.Is(results[0].Err, context.DeadlineExceeded) {
		t.Errorf("slow job Err = %v, want context.DeadlineExceeded", results[0].Err)
	}

	if results[1].Err != nil || results[1].Value != "quick" {
		t.Errorf("fast job = %+v, want {Value: quick}", results[1])
	}

	if elapsed > 2*time.Second {
		t.Errorf("RUN 耗时 %v, 超时取消没有生效 (应该 ~50ms 就返回)", elapsed)
	}
}

func TestRun_PartialFailure(t *testing.T) {
	p := New(3, time.Second)
	boom := errors.New("boom")

	jobs := []Job{
		{ID: "a", Run: func(ctx context.Context) (string, error) { return "va", nil }},
		{ID: "b", Run: func(ctx context.Context) (string, error) { return "", boom }},
		{ID: "c", Run: func(ctx context.Context) (string, error) {
			time.Sleep(30 * time.Millisecond)
			return "vc", nil
		}},
	}

	results := p.Run(context.Background(), jobs)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}

	if results[0].Value != "va" || results[0].Err != nil {
		t.Errorf("results[0] = %+v, want {ID: a Value: va}", results[0])
	}

	if !errors.Is(results[1].Err, boom) {
		t.Errorf("results[1].Err = %v, want boom", results[1].Err)
	}

	if results[2].Value != "vc" || results[2].Err != nil {
		t.Errorf("results[2] = %+v, want {ID:c Value: vc} (b 的失败不应该影响 c)", results[2])
	}
}

func TestRun_ContextCancelled(t *testing.T) {
	p := New(1, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	var started atomic.Int32

	jobs := make([]Job, 3)
	for i := range jobs {
		i := i
		jobs[i] = Job{
			ID: fmt.Sprintf("job-%d", i),
			Run: func(ctx context.Context) (string, error) {
				started.Add(1)
				select {
				case <-time.After(5 * time.Second):
					return "done", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			},
		}
	}

	done := make(chan []Result, 1)
	go func() { done <- p.Run(ctx, jobs) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case results := <-done:
		if len(results) != 3 {
			t.Fatalf("len(results) = %d, want 3 (取消也要给每个 job 一个结局)", len(results))
		}

		if !errors.Is(results[0].Err, context.Canceled) {
			t.Errorf("results[0].Err = %v, want context.Canceled", results[0].Err)
		}

		if got := started.Load(); got != 1 {
			t.Errorf("实际开工的job数 = %d, want 1 (排队 job 不应再执行)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run 未在 2s 内返回， ctx 取消没有生效")
	}
}
