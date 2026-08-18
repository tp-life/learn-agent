package task

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "task.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return s, path
}

func planAndSave(t *testing.T, ctx context.Context, s *Store, taskID string, subs []Subtask) {
	t.Helper()
	if err := s.CreateTask(ctx, taskID, "测试目标"); err != nil {
		t.Fatalf("createTask: %v", err)
	}

	for _, to := range []Status{StatusPlanning, StatusRunning} {
		if err := s.Transition(ctx, taskID, to); err != nil {
			t.Fatalf("Transition -> %s: %v", to, err)
		}
	}

	if err := s.SaveSubtasks(ctx, taskID, subs); err != nil {
		t.Fatalf("SaveSubtasks: %v", err)
	}
}

func TestTransition_RejectsIllegal(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	if err := s.CreateTask(ctx, "t1", "goal"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	for _, to := range []Status{StatusRunning, StatusDone, StatusWaitingHuman} {
		if err := s.Transition(ctx, "t1", to); err == nil {
			t.Errorf("pending -> %s: want error got nil", to)
		}
	}

	for _, to := range []Status{StatusPlanning, StatusRunning, StatusWaitingHuman, StatusRunning, StatusDone} {
		if err := s.Transition(ctx, "t1", to); err != nil {
			t.Fatalf("legal transition -> %s: %v", to, err)
		}
	}

	if err := s.Transition(ctx, "t1", StatusRunning); err == nil {
		t.Errorf("done -> running: want error, got nil (终态不可迁出)")
	}

	if err := s.Transition(ctx, "ghost", StatusPlanning); err == nil {
		t.Error("transition on missing task: want eror, got nil")
	}
}

func TestCompleteSubtask_Idempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	planAndSave(t, ctx, s, "t1", []Subtask{
		{ID: "s1", Title: "调研", Prompt: "...", IdempotencyKey: "t1:s1"},
	})
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatalf("TransitionSubtask: %v", err)
	}

	if err := s.CompleteSubtask(ctx, "t1", "s1", "调研结论", 100); err != nil {
		t.Fatalf("CompleteSubtask: %v", err)
	}

	task, subs, err := s.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}

	if subs[0].Status != StatusDone || subs[0].Output != "调研结论" {
		t.Errorf("subtask = %+v, want done with output", subs[0])
	}

	if subs[0].TokensUsed != 100 {
		t.Errorf("TokensUsed = %d, want 100", subs[0].TokensUsed)
	}

	if task.TotalTokens != 100 {
		t.Errorf("TotalTokens = %d, want 100", task.TotalTokens)
	}

	if subs[0].Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 ", subs[0].Attempts)
	}

	if subs[0].IdempotencyKey != "t1:s1" {
		t.Errorf("IdempotencyKey = %q, want t1:s1", subs[0].IdempotencyKey)
	}
}

func TestCrashRecovery(t *testing.T) {
	ctx := context.Background()
	s, path := newTestStore(t)

	planAndSave(t, ctx, s, "t1", []Subtask{
		{ID: "s1", Title: "调研", Prompt: "...", IdempotencyKey: "t1:s1"},
		{ID: "s2", Title: "写稿", Prompt: "...", IdempotencyKey: "t1:s2"},
		{ID: "s3", Title: "评审", Prompt: "...", IdempotencyKey: "t1:s3"},
	})

	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteSubtask(ctx, "t1", "s1", "调研结论", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionSubtask(ctx, "t1", "s2", StatusRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	ids, err := s2.ListResumable(ctx)
	if err != nil {
		t.Fatalf("ListResumable: %v", err)
	}
	if len(ids) != 1 || ids[0] != "t1" {
		t.Fatalf("ListResumable = %v, want [t1]", ids)
	}

	task, subs, err := s2.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatalf("LoadTask: %v", err)
	}

	if task.Status != StatusRunning {
		t.Errorf("task status = %s, want running", task.Status)
	}

	if len(subs) != 3 {
		t.Fatalf("got %d subtasks, want 3", len(subs))
	}

	if subs[0].Status != StatusDone || subs[0].Output != "调研结论" || subs[0].TokensUsed != 100 {
		t.Errorf("s1 = %+v, want done with checkpoint intact", subs[0])
	}

	if subs[1].Status != StatusRunning {
		t.Errorf("s2 status =%s, wnat running ", subs[0].Status)
	}

	if subs[2].Status != StatusPending {
		t.Errorf("s3 status = %s, want pending", subs[2].Status)
	}

	if err := s2.TransitionSubtask(ctx, "t1", "s2", StatusPending); err != nil {
		t.Fatalf("reset interrupted s2: %v", err)
	}

	if err := s2.TransitionSubtask(ctx, "t1", "s2", StatusRunning); err != nil {
		t.Fatalf("resume s2 :%v", err)
	}

	if err := s2.CompleteSubtask(ctx, "t1", "s2", "初稿", 200); err != nil {
		t.Fatal(err)
	}

	_, subs, err = s2.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if subs[1].Attempts != 2 {
		t.Errorf("s2 Attempts = %d, want 2 (被打断1次+重跑1次)", subs[1].Attempts)
	}
}

func TestListResumable_ExcludesTerminal(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	if err := s.CreateTask(ctx, "t1", "g"); err != nil {
		t.Fatal(err)
	}

	for _, to := range []Status{StatusPlanning, StatusRunning, StatusDone} {
		if err := s.Transition(ctx, "t1", to); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.CreateTask(ctx, "t2", "g"); err != nil {
		t.Fatal(err)
	}

	if err := s.Transition(ctx, "t2", StatusFailed); err != nil {
		t.Fatal(err)
	}

	if err := s.CreateTask(ctx, "t3", "g"); err != nil {
		t.Fatal(err)
	}

	ids, err := s.ListResumable(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(ids) != 1 || ids[0] != "t3" {
		t.Errorf("ListResumable = %v, want [t3]", ids)
	}
}

func TestFailSubtask_AndRequeue(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	planAndSave(t, ctx, s, "t1", []Subtask{{ID: "s1", Title: "x", Prompt: "p"}})
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatal(err)
	}

	if err := s.FailSubtask(ctx, "t1", "s1", "LLM 超时"); err != nil {
		t.Fatalf("FailSubtask: %v", err)
	}

	if err := s.CompleteSubtask(ctx, "t1", "s1", "x", 1); err == nil || !strings.Contains(err.Error(), "illegal") {
		t.Errorf("complete a failed subtask: want illegal-transition error, got %v", err)
	}

	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatal(err)
	}

	_, subs, err := s.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}

	if subs[0].Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", subs[0].Attempts)
	}

	if subs[0].Output != "LLM 超时" {
		t.Errorf("output = %q, want 保留失败现场", subs[0].Output)
	}
}
