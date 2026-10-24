// Package outbox — надежная постановка задач в Redis.
//
// Задача сначала записывается в Postgres в той же транзакции, что и бизнес-данные
// (например, подтверждение итогов розыгрыша). Relay переносит такие записи в asynq.
// TaskID = "outbox-<id>", поэтому повторная доставка после сбоя не создаст дубль.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgenchik/festbot/internal/store"
	"github.com/tgenchik/festbot/internal/tasks"
)

func Add(ctx context.Context, q store.Q, taskType string, payload any, processAt time.Time) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if processAt.IsZero() {
		processAt = time.Now()
	}
	_, err = q.Exec(ctx,
		`INSERT INTO outbox (task_type, payload, process_at) VALUES ($1, $2, $3)`, taskType, data, processAt)
	return err
}

type Relay struct {
	db     *pgxpool.Pool
	client *asynq.Client
	log    *slog.Logger
}

func NewRelay(db *pgxpool.Pool, client *asynq.Client, log *slog.Logger) *Relay {
	return &Relay{db: db, client: client, log: log}
}

func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.flush(ctx); err != nil && ctx.Err() == nil {
				r.log.Error("outbox flush", "err", err)
			}
		}
	}
}

func (r *Relay) flush(ctx context.Context) error {
	return pgx.BeginFunc(ctx, r.db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, task_type, payload, process_at FROM outbox
			WHERE dispatched_at IS NULL ORDER BY id LIMIT 100
			FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		type item struct {
			id        int64
			typ       string
			payload   []byte
			processAt time.Time
		}
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.typ, &it.payload, &it.processAt); err != nil {
				return err
			}
			items = append(items, it)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, it := range items {
			_, err := r.client.EnqueueContext(ctx, asynq.NewTask(it.typ, it.payload),
				asynq.TaskID(fmt.Sprintf("outbox-%d", it.id)),
				asynq.Queue(tasks.QueueFor(it.typ)),
				asynq.ProcessAt(it.processAt),
				asynq.MaxRetry(10),
				asynq.Retention(48*time.Hour)) // хранить ID выполненных задач, чтобы ловить дубли
			if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE outbox SET dispatched_at = now() WHERE id = $1`, it.id); err != nil {
				return err
			}
		}
		return nil
	})
}
