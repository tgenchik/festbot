// Package entries — заявки на ежедневные розыгрыши и накопление шансов.
package entries

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgenchik/festbot/internal/store"
)

// MarkActive отмечает день активным (получен промокод или подана заявка).
// Повторный вызов в тот же день ничего не меняет: один день = один шанс.
func MarkActive(ctx context.Context, q store.Q, userID int64, day time.Time) error {
	_, err := q.Exec(ctx,
		`INSERT INTO active_days (user_id, day) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, day)
	return err
}

func LogEvent(ctx context.Context, q store.Q, userID int64, typ string, day time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO events (user_id, type, day) VALUES ($1, $2, $3)`, userID, typ, day)
	return err
}

type Service struct {
	db         *pgxpool.Pool
	maxChances int
}

func NewService(db *pgxpool.Pool, maxChances int) *Service {
	return &Service{db: db, maxChances: maxChances}
}

type RegisterResult struct {
	Already bool // заявка на этот день уже была
	Chances int
}

// Register регистрирует пользователя в розыгрыше дня. Подписка уже проверена вызывающим.
func (s *Service) Register(ctx context.Context, userID int64, day time.Time) (RegisterResult, error) {
	var res RegisterResult
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO daily_entries (user_id, day) VALUES ($1, $2) ON CONFLICT DO NOTHING`, userID, day)
		if err != nil {
			return err
		}
		res.Already = tag.RowsAffected() == 0
		if err := MarkActive(ctx, tx, userID, day); err != nil {
			return err
		}
		res.Chances, err = s.chances(ctx, tx, userID)
		return err
	})
	return res, err
}

func (s *Service) IsRegistered(ctx context.Context, userID int64, day time.Time) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM daily_entries WHERE user_id = $1 AND day = $2)`, userID, day).Scan(&ok)
	return ok, err
}

func (s *Service) chances(ctx context.Context, q store.Q, userID int64) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM active_days WHERE user_id = $1`, userID).Scan(&n)
	return min(n, s.maxChances), err
}

// Summary — данные для экрана «Мои шансы».
type Summary struct {
	Chances    int
	MaxChances int
	Partners   []string // с кем пользователь уже взаимодействовал, по порядку дней
}

func (s *Service) Summary(ctx context.Context, userID int64) (Summary, error) {
	sum := Summary{MaxChances: s.maxChances}
	rows, err := s.db.Query(ctx, `
		SELECT p.name FROM active_days a
		JOIN campaign_days cd ON cd.day = a.day
		JOIN partners p ON p.id = cd.partner_id
		WHERE a.user_id = $1 ORDER BY a.day`, userID)
	if err != nil {
		return sum, err
	}
	sum.Partners, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return sum, err
	}
	sum.Chances, err = s.chances(ctx, s.db, userID)
	return sum, err
}
