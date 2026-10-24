// Package clock отвечает на вопрос «какой сейчас день акции».
// День считается в часовом поясе кампании, а хранится как DATE (полночь UTC),
// чтобы одинаково сравниваться в Go и Postgres.
package clock

import "time"

type Clock struct {
	loc *time.Location
	now func() time.Time
}

func New(loc *time.Location) Clock { return Clock{loc: loc, now: time.Now} }

// Today — текущая дата акции.
func (c Clock) Today() time.Time { return DateOf(c.now().In(c.loc)) }

// Yesterday — вчерашняя дата акции (для ночного розыгрыша).
func (c Clock) Yesterday() time.Time { return c.Today().AddDate(0, 0, -1) }

func DateOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
