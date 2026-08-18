package task

import (
	"context"
	"fmt"
	"time"
)

func (s *Store) SaveSubtasksTx(ctx context.Context, taskID string, subs []Subtask) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("task: begin tx : %w", err)
	}
	defer tx.Rollback()

	for i, sub := range subs {
		if sub.ID == "" {
			return fmt.Errorf("task: subtasks[%d] has empty id", i)
		}

		now := time.Now()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO subtasks (id, task_id, title, prompt, status, idempotency_key, requires_approval,created_at, updated_at) 
				VALUES (?,?,?,?,?,?,?,?,?)`,
			sub.ID, taskID, sub.Title, sub.Prompt, StatusPending,
			sub.IdempotencyKey, sub.RequiresApproval, now, now)
		if err != nil {
			return fmt.Errorf("task: save subtask %s/%s: %w", taskID, sub.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("task: commit subtasks of %s:%w", taskID, err)
	}
	return nil
}

func (s *Store) CompleteSubtaskTx(ctx context.Context, taskID, subID, output string, token int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("task: begin tx: %w", err)
	}

	defer tx.Rollback()

	var from Status
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM subtasks WHERE task_id = ? AND id = ?`, taskID, subID).Scan(&from)
	if err != nil {
		return fmt.Errorf("task: read subtask %s/%s: %w", taskID, subID, err)
	}

	if from == StatusDone {
		return nil
	}

	if !canTransition(subtaskTransitions, from, StatusDone) {
		return fmt.Errorf("task: illegal subtask transition %s -> %s (%s/%s)", from, StatusDone, taskID, subID)
	}

	now := time.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE subtasks SET status = ?, output = ?, tokens_used = tokens_used + ?, updated_at = ? 
		WHERE task_id = ? AND id = ?`, StatusDone, output, token, now, taskID, subID); err != nil {
		return fmt.Errorf("task: complete subtask %s/%s: %w", taskID, subID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE tasks SET total_tokens = total_tokens + ?, updated_at = ? WHERE id = ?`, token, now, taskID); err != nil {
		return fmt.Errorf("task: add tokens to %s:%w", taskID, err)
	}

	return tx.Commit()
}
