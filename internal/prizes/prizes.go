// Package prizes — получение главного приза: анкета победителя, согласие и
// передача приза резервному победителю, если основной не ответил или отказался.
package prizes

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgenchik/festbot/internal/outbox"
	"github.com/tgenchik/festbot/internal/tasks"
)

var (
	ErrClosed  = errors.New("получение приза закрыто")
	ErrBadName = errors.New("некорректные имя и фамилия")
)

type Service struct {
	db  *pgxpool.Pool
	ttl time.Duration
}

func NewService(db *pgxpool.Pool, ttl time.Duration) *Service { return &Service{db: db, ttl: ttl} }

const openStatuses = `('pending', 'awaiting_name', 'awaiting_consent')`

// Start — победитель нажал «Получить приз», ждем от него имя и фамилию.
func (s *Service) Start(ctx context.Context, drawID, userID int64) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE prize_claims SET status = 'awaiting_name', updated_at = now()
		WHERE draw_id = $1 AND user_id = $2 AND status IN `+openStatuses+` AND deadline_at > now()`,
		drawID, userID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrClosed
	}
	return err
}

// AwaitingName — если пользователь сейчас заполняет анкету, вернет id розыгрыша.
// Так бот понимает, что входящий текст — это имя, без отдельного хранилища состояний.
func (s *Service) AwaitingName(ctx context.Context, userID int64) (int64, bool, error) {
	var drawID int64
	err := s.db.QueryRow(ctx, `
		SELECT draw_id FROM prize_claims
		WHERE user_id = $1 AND status = 'awaiting_name' AND deadline_at > now()
		ORDER BY updated_at DESC LIMIT 1`, userID).Scan(&drawID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	return drawID, err == nil, err
}

func (s *Service) SaveName(ctx context.Context, drawID, userID int64, name string) error {
	name = strings.Join(strings.Fields(name), " ")
	if len(strings.Fields(name)) < 2 || utf8.RuneCountInString(name) > 100 {
		return ErrBadName
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE prize_claims SET full_name = $3, status = 'awaiting_consent', updated_at = now()
		WHERE draw_id = $1 AND user_id = $2 AND status = 'awaiting_name' AND deadline_at > now()`,
		drawID, userID, name)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrClosed
	}
	return err
}

// Consent — ответ на запрос согласия. Отказ сразу передает приз резервисту.
func (s *Service) Consent(ctx context.Context, drawID, userID int64, agree bool) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		status, consentAt := "declined", "NULL"
		if agree {
			status, consentAt = "claimed", "now()"
		}
		tag, err := tx.Exec(ctx, `
			UPDATE prize_claims SET status = $3, consent_at = `+consentAt+`, updated_at = now()
			WHERE draw_id = $1 AND user_id = $2 AND status = 'awaiting_consent' AND deadline_at > now()`,
			drawID, userID, status)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrClosed
		}
		if !agree {
			return s.promoteReserve(ctx, tx, drawID)
		}
		return nil
	})
}

// Expire вызывается задачей по дедлайну. Если анкета не заполнена — приз резервисту.
func (s *Service) Expire(ctx context.Context, drawID, userID int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE prize_claims SET status = 'expired', updated_at = now()
			WHERE draw_id = $1 AND user_id = $2 AND status IN `+openStatuses+` AND deadline_at <= now()`,
			drawID, userID)
		if err != nil || tag.RowsAffected() == 0 {
			return err // уже получил, отказался или дедлайн еще не наступил
		}
		return s.promoteReserve(ctx, tx, drawID)
	})
}

func (s *Service) promoteReserve(ctx context.Context, tx pgx.Tx, drawID int64) error {
	var userID int64
	err := tx.QueryRow(ctx, `
		UPDATE draw_winners SET promoted_at = now()
		WHERE (draw_id, user_id) = (
			SELECT draw_id, user_id FROM draw_winners
			WHERE draw_id = $1 AND is_reserve AND promoted_at IS NULL
			ORDER BY position LIMIT 1 FOR UPDATE
		) RETURNING user_id`, drawID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // резервисты закончились — приз решает организатор вручную
	}
	if err != nil {
		return err
	}
	deadline := time.Now().Add(s.ttl)
	if _, err := tx.Exec(ctx,
		`INSERT INTO prize_claims (draw_id, user_id, deadline_at) VALUES ($1, $2, $3)`,
		drawID, userID, deadline); err != nil {
		return err
	}
	ref := tasks.ClaimRef{DrawID: drawID, UserID: userID}
	if err := outbox.Add(ctx, tx, tasks.TypePrizeNotify, ref, time.Time{}); err != nil {
		return err
	}
	return outbox.Add(ctx, tx, tasks.TypePrizeExpire, ref, deadline)
}

// Claim — данные одной заявки на приз (для уведомления резервиста).
type Claim struct {
	PrizeTitle string
	Deadline   time.Time
}

func (s *Service) Get(ctx context.Context, drawID, userID int64) (Claim, error) {
	var c Claim
	err := s.db.QueryRow(ctx, `
		SELECT w.prize_title, c.deadline_at FROM prize_claims c
		JOIN draw_winners w USING (draw_id, user_id)
		WHERE c.draw_id = $1 AND c.user_id = $2`, drawID, userID).Scan(&c.PrizeTitle, &c.Deadline)
	return c, err
}

// ExportCSV — анкеты победителей для передачи партнеру или билетному оператору.
// Только тех, кто дал согласие на передачу данных.
func (s *Service) ExportCSV(ctx context.Context, drawID int64, out io.Writer) error {
	rows, err := s.db.Query(ctx, `
		SELECT c.user_id, c.full_name, w.prize_title, c.consent_at
		FROM prize_claims c JOIN draw_winners w USING (draw_id, user_id)
		WHERE c.draw_id = $1 AND c.status = 'claimed' ORDER BY w.position`, drawID)
	if err != nil {
		return err
	}
	defer rows.Close()
	w := csv.NewWriter(out)
	_ = w.Write([]string{"user_id", "full_name", "prize", "consent_at"})
	for rows.Next() {
		var uid int64
		var name, prize string
		var consentAt time.Time
		if err := rows.Scan(&uid, &name, &prize, &consentAt); err != nil {
			return err
		}
		_ = w.Write([]string{strconv.FormatInt(uid, 10), name, prize, consentAt.Format(time.RFC3339)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return rows.Err()
}
