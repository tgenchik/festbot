// Package stats — метрики из раздела 8 ТЗ.
package stats

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DayCount struct {
	Day   time.Time `json:"day"`
	Count int       `json:"count"`
}

type PartnerPromo struct {
	Partner   string `json:"partner"`
	Issued    int    `json:"issued"`
	Remaining int    `json:"remaining"`
}

type Report struct {
	TotalUsers    int            `json:"total_users"`
	NewUsersByDay []DayCount     `json:"new_users_by_day"`
	PromoRequests []DayCount     `json:"promo_requests_by_day"`
	Promo         []PartnerPromo `json:"promo_by_partner"`
	EntriesByDay  []DayCount     `json:"daily_entries_by_day"`
}

func Build(ctx context.Context, db *pgxpool.Pool, loc *time.Location) (Report, error) {
	var r Report
	if err := db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&r.TotalUsers); err != nil {
		return r, err
	}
	var err error
	if r.NewUsersByDay, err = dayCounts(ctx, db, `
		SELECT (started_at AT TIME ZONE $1)::date, count(*) FROM users GROUP BY 1 ORDER BY 1`, loc.String()); err != nil {
		return r, err
	}
	if r.PromoRequests, err = dayCounts(ctx, db, `
		SELECT day, count(*) FROM events WHERE type = 'promo_request' GROUP BY 1 ORDER BY 1`); err != nil {
		return r, err
	}
	if r.EntriesByDay, err = dayCounts(ctx, db, `
		SELECT day, count(*) FROM daily_entries GROUP BY 1 ORDER BY 1`); err != nil {
		return r, err
	}
	rows, err := db.Query(ctx, `
		SELECT p.name, count(c.user_id)::int, (count(c.id) - count(c.user_id))::int
		FROM partners p LEFT JOIN promo_codes c ON c.partner_id = p.id
		GROUP BY p.id ORDER BY p.id`)
	if err != nil {
		return r, err
	}
	r.Promo, err = pgx.CollectRows(rows, pgx.RowToStructByPos[PartnerPromo])
	return r, err
}

func dayCounts(ctx context.Context, db *pgxpool.Pool, sql string, args ...any) ([]DayCount, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[DayCount])
}
