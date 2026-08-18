package task

import (
	"context"
	"testing"
)

func TestSaveSubtasksTx_RollsBackOnError(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	if err := s.CreateTask(ctx, "t1", "g"); err != nil {
		t.Fatal(err)
	}

	if err := s.SaveSubtasksTx(ctx, "t1", []Subtask{{ID: "s1", Title: "a", Prompt: "p"}}); err != nil {
		t.Fatalf("first batch: %v", err)
	}

	if err := s.SaveSubtasksTx(ctx, "t1", []Subtask{
		{ID: "s2", Title: "b", Prompt: "p"},
		{ID: "s1", Title: "dup", Prompt: "p"},
	}); err == nil {
		t.Fatal("want duplicate-key error, got nil")
	}

	_, subs, err := s.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}

	if len(subs) != 1 || subs[0].ID != "s1" {
		t.Errorf("subtasks = %v, want 只有第一批的s1 （有第二批整体回滚）", subs)
	}
}

func TestCompleteSubtaskTx_Idempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	defer s.Close()

	planAndSave(t, ctx, s, "t1", []Subtask{{ID: "s1", Title: "a", Prompt: "p"}})
	if err := s.TransitionSubtask(ctx, "t1", "s1", StatusRunning); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := s.CompleteSubtaskTx(ctx, "t1", "s1", "out", 100); err != nil {
			t.Fatalf("CompleteSubtaskTx #%d: %v", i+1, err)
		}
	}

	task, subs, err := s.LoadTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}

	if subs[0].TokensUsed != 100 || task.TotalTokens != 100 {
		t.Errorf("tokens = %d/%d, want 100/100", subs[0].TokensUsed, task.TotalTokens)
	}
}
