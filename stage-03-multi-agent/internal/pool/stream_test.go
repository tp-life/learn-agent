package pool

import (
	"context"
	"testing"
	"time"
)

func TestRunStream_EarlyResultFirst(t *testing.T) {
	p := New(2, time.Second)
	jobs := []Job{
		{ID: "slow", Run: func(ctx context.Context) (string, error) {
			time.Sleep(100 * time.Millisecond)
			return "vslow", nil
		}},
		{ID: "fast", Run: func(ctx context.Context) (string, error) {
			return "vfast", nil
		}},
	}

	ch := p.RunStream(context.Background(), jobs)

	first := <-ch
	if first.ID != "fast" {
		t.Errorf("第一个到达的结果 = %q, want fast (流式应先出快结果)", first.ID)
	}

	got := map[string]Result{first.ID: first}
	for r := range ch {
		got[r.ID] = r
	}

	if len(got) != 2 {
		t.Fatalf("收到 %d 个结果， want 2", len(got))
	}

	if got["slow"].Value != "vslow" || got["slow"].Err != nil {
		t.Errorf("slow = %+v, want {Value: slow}", got["slow"])
	}
}

func TestRunStream_ConcurrencyLimit(t *testing.T) {
	p := New(1, time.Second)
	jobs := []Job{
		{ID: "a", Run: func(ctx context.Context) (string, error) {
			time.Sleep(30 * time.Millisecond)
			return "va", nil
		}},
		{ID: "b", Run: func(ctx context.Context) (string, error) { return "vb", nil }},
	}

	start := time.Now()
	n := 0
	for range p.RunStream(context.Background(), jobs) {
		n++
	}
	if n != 2 {
		t.Fatalf("收到 %d 个结果， want 2", n)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Errorf("总耗时 %v < 30ms, 疑似并发执行 （上限 1 应串行）", elapsed)
	}
}
