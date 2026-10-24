// Package promo — пул промокодов и их атомарная выдача.
package promo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgenchik/festbot/internal/entries"
	"github.com/tgenchik/festbot/internal/store"
)

var ErrSoldOut = errors.New("промокоды закончились")

type Service struct{ db *pgxpool.Pool }

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db} }

type Result struct {
	Code  string
	Fresh bool // false — пользователь уже получал код этого партнера
}

// Issue выдает пользователю код партнера дня. Повторный вызов возвращает тот же код.
// FOR UPDATE SKIP LOCKED позволяет параллельным запросам брать разные свободные коды
// без блокировок друг друга, поэтому один код никогда не уйдет двоим.
func (s *Service) Issue(ctx context.Context, userID, partnerID int64, day time.Time) (Result, error) {
	res, err := s.issue(ctx, userID, partnerID, day)
	if store.IsUniqueViolation(err) {
		// Двойной клик: параллельная транзакция успела выдать код этому же пользователю.
		return s.issue(ctx, userID, partnerID, day)
	}
	return res, err
}

func (s *Service) issue(ctx context.Context, userID, partnerID int64, day time.Time) (Result, error) {
	var res Result
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// Двойной клик одного пользователя выполняется по очереди: иначе второй запрос,
		// не дождавшись первого, мог бы ответить «коды закончились».
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, userID); err != nil {
			return err
		}
		if err := entries.LogEvent(ctx, tx, userID, "promo_request", day); err != nil {
			return err
		}
		err := tx.QueryRow(ctx,
			`SELECT code FROM promo_codes WHERE partner_id = $1 AND user_id = $2`, partnerID, userID).
			Scan(&res.Code)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		err = tx.QueryRow(ctx, `
			UPDATE promo_codes SET user_id = $2, issued_at = now()
			WHERE id = (
				SELECT id FROM promo_codes
				WHERE partner_id = $1 AND user_id IS NULL
				ORDER BY id LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING code`, partnerID, userID).Scan(&res.Code)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSoldOut
		}
		if err != nil {
			return err
		}
		res.Fresh = true
		return entries.MarkActive(ctx, tx, userID, day)
	})
	if errors.Is(err, ErrSoldOut) {
		// Событие запроса откатилось вместе с транзакцией — запишем его отдельно.
		_ = entries.LogEvent(ctx, s.db, userID, "promo_request", day)
	}
	return res, err
}

// Load загружает пачку кодов партнера (дубликаты пропускаются). Возвращает число добавленных.
func (s *Service) Load(ctx context.Context, partnerID int64, codes []string) (int64, error) {
	var added int64
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO promo_codes (partner_id, code)
			SELECT $1, unnest($2::text[])
			ON CONFLICT (partner_id, code) DO NOTHING`, partnerID, codes)
		added = tag.RowsAffected()
		return err
	})
	return added, err
}
