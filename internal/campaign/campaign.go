// Package campaign — расписание акции: какой партнер в какой день.
package campaign

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tgenchik/festbot/internal/store"
)

var ErrNoDay = errors.New("на эту дату партнер не назначен")

type Day struct {
	Date             time.Time
	PartnerID        int64
	PartnerName      string
	ChannelID        int64
	ChannelURL       string
	PromoWhere       string
	PromoValidUntil  *time.Time
	WinnersCount     int
	PrizeTitle       string
	PrizeInstruction string
}

type Repo struct{ db store.Q }

func NewRepo(db store.Q) *Repo { return &Repo{db: db} }

func (r *Repo) ByDate(ctx context.Context, date time.Time) (Day, error) {
	var d Day
	err := r.db.QueryRow(ctx, `
		SELECT cd.day, p.id, p.name, p.channel_id, p.channel_url, p.promo_where,
		       p.promo_valid_until, cd.winners_count, cd.prize_title, cd.prize_instruction
		FROM campaign_days cd JOIN partners p ON p.id = cd.partner_id
		WHERE cd.day = $1`, date).
		Scan(&d.Date, &d.PartnerID, &d.PartnerName, &d.ChannelID, &d.ChannelURL, &d.PromoWhere,
			&d.PromoValidUntil, &d.WinnersCount, &d.PrizeTitle, &d.PrizeInstruction)
	if errors.Is(err, pgx.ErrNoRows) {
		return Day{}, ErrNoDay
	}
	return d, err
}

// Bounds — первый и последний день акции и их количество.
func (r *Repo) Bounds(ctx context.Context) (first, last time.Time, total int, err error) {
	err = r.db.QueryRow(ctx,
		`SELECT coalesce(min(day), '-infinity'), coalesce(max(day), '-infinity'), count(*) FROM campaign_days`).
		Scan(&first, &last, &total)
	return
}

type Partner struct {
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	ChannelID       int64      `json:"channel_id"`
	ChannelURL      string     `json:"channel_url"`
	PromoWhere      string     `json:"promo_where"`
	PromoValidUntil *time.Time `json:"promo_valid_until"`
}

func (r *Repo) CreatePartner(ctx context.Context, p Partner) (int64, error) {
	var id int64
	err := r.db.QueryRow(ctx, `
		INSERT INTO partners (name, channel_id, channel_url, promo_where, promo_valid_until)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		p.Name, p.ChannelID, p.ChannelURL, p.PromoWhere, p.PromoValidUntil).Scan(&id)
	return id, err
}

type DayInput struct {
	Date             time.Time `json:"date"`
	PartnerID        int64     `json:"partner_id"`
	WinnersCount     int       `json:"winners_count"`
	PrizeTitle       string    `json:"prize_title"`
	PrizeInstruction string    `json:"prize_instruction"`
}

func (r *Repo) UpsertDay(ctx context.Context, d DayInput) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO campaign_days (day, partner_id, winners_count, prize_title, prize_instruction)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (day) DO UPDATE SET partner_id = EXCLUDED.partner_id,
		    winners_count = EXCLUDED.winners_count, prize_title = EXCLUDED.prize_title,
		    prize_instruction = EXCLUDED.prize_instruction`,
		d.Date, d.PartnerID, d.WinnersCount, d.PrizeTitle, d.PrizeInstruction)
	return err
}
