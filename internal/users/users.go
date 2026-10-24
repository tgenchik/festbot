// Package users — учет пользователей бота.
package users

import (
	"context"

	"github.com/tgenchik/festbot/internal/store"
)

type Repo struct{ db store.Q }

func NewRepo(db store.Q) *Repo { return &Repo{db: db} }

// Touch создает пользователя при первом обращении и обновляет last_seen_at.
func (r *Repo) Touch(ctx context.Context, id int64, firstName, lastName string) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO users (id, first_name, last_name) VALUES ($1, $2, $3)
		ON CONFLICT (id) DO UPDATE
		SET last_seen_at = now(), unreachable = false,
		    first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name`,
		id, firstName, lastName)
	return err
}

func (r *Repo) MarkUnreachable(ctx context.Context, id int64) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET unreachable = true WHERE id = $1`, id)
	return err
}

// ReachableAfter — страница получателей рассылки (keyset-пагинация по id).
func (r *Repo) ReachableAfter(ctx context.Context, afterID int64, limit int) ([]int64, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id FROM users WHERE NOT unreachable AND id > $1 ORDER BY id LIMIT $2`,
		afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
