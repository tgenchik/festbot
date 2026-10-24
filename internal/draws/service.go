// Package draws — ежедневные и финальный розыгрыши.
//
// Жизненный цикл: draft (итоги посчитаны) → confirmed (админ проверил) → notified.
// Уведомления уходят только после подтверждения, как требует ТЗ.
package draws

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgenchik/festbot/internal/campaign"
	"github.com/tgenchik/festbot/internal/outbox"
	"github.com/tgenchik/festbot/internal/store"
	"github.com/tgenchik/festbot/internal/tasks"
)

var (
	ErrAlreadyDrawn = errors.New("розыгрыш уже проведен")
	ErrBadStatus    = errors.New("розыгрыш не в статусе черновика")
	ErrNoEntries    = errors.New("нет участников")
)

type Config struct {
	MaxChances          int
	FinalWinners        int
	FinalReserves       int
	FinalPrize          string
	ClaimTTL            time.Duration
	RecheckSubscription bool
}

// MembershipChecker — проверка подписки (реализует subscription.Checker).
type MembershipChecker interface {
	Members(ctx context.Context, channelID int64, userIDs []int64) (map[int64]bool, error)
}

type Service struct {
	db       *pgxpool.Pool
	campaign *campaign.Repo
	checker  MembershipChecker
	cfg      Config
}

func NewService(db *pgxpool.Pool, c *campaign.Repo, checker MembershipChecker, cfg Config) *Service {
	return &Service{db: db, campaign: c, checker: checker, cfg: cfg}
}

// RunDaily проводит розыгрыш дня среди подтвержденных заявок. Результат — черновик.
func (s *Service) RunDaily(ctx context.Context, date time.Time) (int64, error) {
	day, err := s.campaign.ByDate(ctx, date)
	if err != nil {
		return 0, err
	}
	rows, err := s.db.Query(ctx, `SELECT user_id FROM daily_entries WHERE day = $1`, date)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}

	if s.cfg.RecheckSubscription && len(ids) > 0 {
		// Отписавшиеся после регистрации в розыгрыш не попадают.
		subscribed, err := s.checker.Members(ctx, day.ChannelID, ids)
		if err != nil {
			return 0, fmt.Errorf("перепроверка подписки: %w", err)
		}
		kept := ids[:0]
		for _, id := range ids {
			if subscribed[id] {
				kept = append(kept, id)
			}
		}
		ids = kept
	}

	cands := make([]Candidate, len(ids))
	for i, id := range ids {
		cands[i] = Candidate{UserID: id, Tickets: 1}
	}
	return s.save(ctx, "daily", &date, cands, day.WinnersCount, 0, day.PrizeTitle)
}

// RunFinal проводит финальный розыгрыш: билетов у участника столько, сколько активных дней.
func (s *Service) RunFinal(ctx context.Context) (int64, error) {
	rows, err := s.db.Query(ctx, `
		SELECT user_id, least(count(*), $1)::int FROM active_days GROUP BY user_id`, s.cfg.MaxChances)
	if err != nil {
		return 0, err
	}
	cands, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Candidate, error) {
		var c Candidate
		err := r.Scan(&c.UserID, &c.Tickets)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	return s.save(ctx, "final", nil, cands, s.cfg.FinalWinners, s.cfg.FinalReserves, s.cfg.FinalPrize)
}

func (s *Service) save(ctx context.Context, kind string, day *time.Time, cands []Candidate,
	winners, reserves int, prize string) (int64, error) {
	if len(cands) == 0 {
		return 0, ErrNoEntries
	}
	var seed [32]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return 0, err
	}
	picked := Pick(seed, cands, winners+reserves)

	tickets := 0
	for _, c := range cands {
		tickets += c.Tickets
	}

	var drawID int64
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO draws (kind, day, seed, participants_count, tickets_count)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			kind, day, seed[:], len(cands), tickets).Scan(&drawID)
		if store.IsUniqueViolation(err) {
			return ErrAlreadyDrawn
		}
		if err != nil {
			return err
		}

		// Снимок участников — для выгрузки и возможной проверки результата.
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"draw_participants"},
			[]string{"draw_id", "user_id", "tickets"},
			pgx.CopyFromSlice(len(cands), func(i int) ([]any, error) {
				return []any{drawID, cands[i].UserID, cands[i].Tickets}, nil
			}))
		if err != nil {
			return err
		}

		for i, uid := range picked {
			if _, err := tx.Exec(ctx, `
				INSERT INTO draw_winners (draw_id, user_id, position, is_reserve, prize_title)
				VALUES ($1, $2, $3, $4, $5)`, drawID, uid, i+1, i >= winners, prize); err != nil {
				return err
			}
		}
		return nil
	})
	return drawID, err
}

// Confirm — админ подтвердил итоги. В той же транзакции ставятся задачи на уведомления.
func (s *Service) Confirm(ctx context.Context, drawID int64, by string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var kind string
		err := tx.QueryRow(ctx, `
			UPDATE draws SET status = 'confirmed', confirmed_at = now(), confirmed_by = $2
			WHERE id = $1 AND status = 'draft' RETURNING kind`, drawID, by).Scan(&kind)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadStatus
		}
		if err != nil {
			return err
		}

		if kind == "final" {
			// Главный приз требует анкеты. Не ответил к дедлайну — приз уходит резерву.
			deadline := time.Now().Add(s.cfg.ClaimTTL)
			rows, err := tx.Query(ctx, `
				INSERT INTO prize_claims (draw_id, user_id, deadline_at)
				SELECT draw_id, user_id, $2 FROM draw_winners WHERE draw_id = $1 AND NOT is_reserve
				RETURNING user_id`, drawID, deadline)
			if err != nil {
				return err
			}
			users, err := pgx.CollectRows(rows, pgx.RowTo[int64])
			if err != nil {
				return err
			}
			for _, uid := range users {
				ref := tasks.ClaimRef{DrawID: drawID, UserID: uid}
				if err := outbox.Add(ctx, tx, tasks.TypePrizeExpire, ref, deadline); err != nil {
					return err
				}
			}
		}
		return outbox.Add(ctx, tx, tasks.TypeDrawNotify, tasks.DrawRef{DrawID: drawID}, time.Time{})
	})
}

func (s *Service) Cancel(ctx context.Context, drawID int64) error {
	tag, err := s.db.Exec(ctx, `UPDATE draws SET status = 'cancelled' WHERE id = $1 AND status = 'draft'`, drawID)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrBadStatus
	}
	return err
}

type Winner struct {
	UserID     int64  `json:"user_id"`
	Position   int    `json:"position"`
	IsReserve  bool   `json:"is_reserve"`
	PrizeTitle string `json:"prize_title"`
}

type Draw struct {
	ID                int64      `json:"id"`
	Kind              string     `json:"kind"`
	Day               *time.Time `json:"day"`
	Status            string     `json:"status"`
	Seed              string     `json:"seed"`
	ParticipantsCount int        `json:"participants_count"`
	TicketsCount      int        `json:"tickets_count"`
	CreatedAt         time.Time  `json:"created_at"`
	Winners           []Winner   `json:"winners"`
}

func (s *Service) Get(ctx context.Context, drawID int64) (Draw, error) {
	var d Draw
	var seed []byte
	err := s.db.QueryRow(ctx, `
		SELECT id, kind, day, status, seed, participants_count, tickets_count, created_at
		FROM draws WHERE id = $1`, drawID).
		Scan(&d.ID, &d.Kind, &d.Day, &d.Status, &seed, &d.ParticipantsCount, &d.TicketsCount, &d.CreatedAt)
	if err != nil {
		return d, err
	}
	d.Seed = fmt.Sprintf("%x", seed)
	rows, err := s.db.Query(ctx, `
		SELECT user_id, position, is_reserve, prize_title FROM draw_winners
		WHERE draw_id = $1 ORDER BY position`, drawID)
	if err != nil {
		return d, err
	}
	d.Winners, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Winner])
	return d, err
}

func (s *Service) MarkNotified(ctx context.Context, drawID int64) error {
	_, err := s.db.Exec(ctx,
		`UPDATE draws SET status = 'notified', notified_at = now() WHERE id = $1 AND status = 'confirmed'`, drawID)
	return err
}

// ExportCSV — выгрузка для команды MAX и партнера: все участники и отметка о выигрыше.
func (s *Service) ExportCSV(ctx context.Context, drawID int64, out io.Writer) error {
	rows, err := s.db.Query(ctx, `
		SELECT p.user_id, u.first_name, u.last_name, p.tickets,
		       coalesce(w.position::text, ''), coalesce(w.is_reserve::text, ''), coalesce(w.prize_title, '')
		FROM draw_participants p
		LEFT JOIN users u ON u.id = p.user_id
		LEFT JOIN draw_winners w ON w.draw_id = p.draw_id AND w.user_id = p.user_id
		WHERE p.draw_id = $1
		ORDER BY w.position NULLS LAST, p.user_id`, drawID)
	if err != nil {
		return err
	}
	defer rows.Close()

	w := csv.NewWriter(out)
	_ = w.Write([]string{"user_id", "first_name", "last_name", "tickets", "position", "is_reserve", "prize"})
	for rows.Next() {
		var uid int64
		var first, last, pos, reserve, prize *string
		var tickets int
		if err := rows.Scan(&uid, &first, &last, &tickets, &pos, &reserve, &prize); err != nil {
			return err
		}
		_ = w.Write([]string{strconv.FormatInt(uid, 10), deref(first), deref(last),
			strconv.Itoa(tickets), deref(pos), deref(reserve), deref(prize)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return err
	}
	return rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
