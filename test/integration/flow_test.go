//go:build integration

// Сквозной тест основной механики на реальном Postgres:
//
//	DATABASE_URL=... go test -tags integration ./test/integration/
//
// ВНИМАНИЕ: тест пересоздает схему public в указанной базе.
package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/tgenchik/festbot/internal/campaign"
	"github.com/tgenchik/festbot/internal/clock"
	"github.com/tgenchik/festbot/internal/draws"
	"github.com/tgenchik/festbot/internal/entries"
	"github.com/tgenchik/festbot/internal/prizes"
	"github.com/tgenchik/festbot/internal/promo"
	"github.com/tgenchik/festbot/internal/stats"
	"github.com/tgenchik/festbot/internal/store"
	"github.com/tgenchik/festbot/internal/users"
	"github.com/tgenchik/festbot/migrations"
)

// allSubscribed — подменяет проверку подписки через MAX API.
type allSubscribed struct{}

func (allSubscribed) Members(_ context.Context, _ int64, ids []int64) (map[int64]bool, error) {
	m := map[int64]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m, nil
}

func TestFullFlow(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	migrate(t, url)

	db, err := store.Connect(ctx, url)
	must(t, err)
	defer db.Close()

	camp := campaign.NewRepo(db)
	usr := users.NewRepo(db)
	promoSvc := promo.NewService(db)
	ent := entries.NewService(db, 10)
	prz := prizes.NewService(db, time.Hour)
	drw := draws.NewService(db, camp, allSubscribed{}, draws.Config{
		MaxChances: 10, FinalWinners: 2, FinalReserves: 2, FinalPrize: "Билеты", ClaimTTL: time.Hour,
		RecheckSubscription: true,
	})

	today := clock.DateOf(time.Now())
	yesterday := today.AddDate(0, 0, -1)
	p1, err := camp.CreatePartner(ctx, campaign.Partner{Name: "Партнер 1", ChannelID: 1, ChannelURL: "https://max.ru/p1"})
	must(t, err)
	p2, err := camp.CreatePartner(ctx, campaign.Partner{Name: "Партнер 2", ChannelID: 2, ChannelURL: "https://max.ru/p2"})
	must(t, err)
	must(t, camp.UpsertDay(ctx, campaign.DayInput{Date: yesterday, PartnerID: p1, WinnersCount: 3, PrizeTitle: "Мерч"}))
	must(t, camp.UpsertDay(ctx, campaign.DayInput{Date: today, PartnerID: p2, WinnersCount: 2, PrizeTitle: "Сертификат"}))

	const nUsers, nCodes = 50, 30
	codes := make([]string, nCodes)
	for i := range codes {
		codes[i] = fmt.Sprintf("CODE%03d", i)
	}
	added, err := promoSvc.Load(ctx, p2, append(codes, "CODE000")) // дубль должен отсеяться
	must(t, err)
	if added != nCodes {
		t.Fatalf("loaded %d codes, want %d", added, nCodes)
	}
	for uid := int64(1); uid <= nUsers; uid++ {
		must(t, usr.Touch(ctx, uid, "User", fmt.Sprint(uid)))
	}

	// 1. Конкурентная выдача: каждый жмет «Получить промокод» дважды одновременно.
	var mu sync.Mutex
	issued := map[string]int64{}
	soldOut := 0
	var wg sync.WaitGroup
	for uid := int64(1); uid <= nUsers; uid++ {
		for range 2 {
			wg.Add(1)
			go func(uid int64) {
				defer wg.Done()
				res, err := promoSvc.Issue(ctx, uid, p2, today)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case errors.Is(err, promo.ErrSoldOut):
					soldOut++
				case err != nil:
					t.Errorf("issue: %v", err)
				default:
					if prev, ok := issued[res.Code]; ok && prev != uid {
						t.Errorf("code %s issued to %d and %d", res.Code, prev, uid)
					}
					issued[res.Code] = uid
				}
			}(uid)
		}
	}
	wg.Wait()
	if len(issued) != nCodes {
		t.Fatalf("issued %d distinct codes, want %d", len(issued), nCodes)
	}
	if soldOut != (nUsers-nCodes)*2 {
		t.Fatalf("sold out answers = %d, want %d: someone with a code was told codes ran out",
			soldOut, (nUsers-nCodes)*2)
	}

	// 2. Заявки вчера и сегодня; повтор — без новой заявки; шанс за день один.
	for uid := int64(1); uid <= 10; uid++ {
		_, err := ent.Register(ctx, uid, yesterday)
		must(t, err)
	}
	res, err := ent.Register(ctx, 1, today)
	must(t, err)
	if res.Already || res.Chances != 2 {
		t.Fatalf("register: %+v, want new entry and 2 chances (yesterday + today with promo)", res)
	}
	res, err = ent.Register(ctx, 1, today)
	must(t, err)
	if !res.Already || res.Chances != 2 {
		t.Fatalf("repeat register: %+v", res)
	}

	// 3. Дневной розыгрыш: черновик, повтор запрещен, подтверждение ставит задачу в outbox.
	dailyID, err := drw.RunDaily(ctx, yesterday)
	must(t, err)
	if _, err := drw.RunDaily(ctx, yesterday); !errors.Is(err, draws.ErrAlreadyDrawn) {
		t.Fatalf("second daily draw: %v, want ErrAlreadyDrawn", err)
	}
	d, err := drw.Get(ctx, dailyID)
	must(t, err)
	if len(d.Winners) != 3 || d.ParticipantsCount != 10 {
		t.Fatalf("daily draw: %+v", d)
	}
	must(t, drw.Confirm(ctx, dailyID, "tester"))
	if err := drw.Confirm(ctx, dailyID, "tester"); !errors.Is(err, draws.ErrBadStatus) {
		t.Fatalf("double confirm: %v", err)
	}

	// 4. Финал: 2 победителя + 2 резерва, анкеты и передача приза резерву.
	finalID, err := drw.RunFinal(ctx)
	must(t, err)
	must(t, drw.Confirm(ctx, finalID, "tester"))
	f, err := drw.Get(ctx, finalID)
	must(t, err)
	if len(f.Winners) != 4 || f.Winners[2].IsReserve != true || f.Winners[1].IsReserve {
		t.Fatalf("final winners: %+v", f.Winners)
	}
	w1, w2 := f.Winners[0].UserID, f.Winners[1].UserID

	must(t, prz.Start(ctx, finalID, w1))
	if id, ok, _ := prz.AwaitingName(ctx, w1); !ok || id != finalID {
		t.Fatal("winner should be awaiting name")
	}
	if err := prz.SaveName(ctx, finalID, w1, "Иван"); !errors.Is(err, prizes.ErrBadName) {
		t.Fatalf("single word name accepted: %v", err)
	}
	must(t, prz.SaveName(ctx, finalID, w1, "  Иван   Петров "))
	must(t, prz.Consent(ctx, finalID, w1, true))

	must(t, prz.Start(ctx, finalID, w2))
	must(t, prz.SaveName(ctx, finalID, w2, "Анна Иванова"))
	must(t, prz.Consent(ctx, finalID, w2, false)) // отказ → приз первому резервисту

	var promoted int64
	must(t, db.QueryRow(ctx, `SELECT user_id FROM draw_winners WHERE draw_id = $1 AND promoted_at IS NOT NULL`, finalID).
		Scan(&promoted))
	if promoted != f.Winners[2].UserID {
		t.Fatalf("promoted %d, want first reserve %d", promoted, f.Winners[2].UserID)
	}
	// Резервист не ответил: делаем дедлайн прошедшим и запускаем истечение.
	_, err = db.Exec(ctx, `UPDATE prize_claims SET deadline_at = now() - interval '1 second' WHERE user_id = $1`, promoted)
	must(t, err)
	must(t, prz.Expire(ctx, finalID, promoted))
	var claims int
	must(t, db.QueryRow(ctx, `SELECT count(*) FROM prize_claims WHERE draw_id = $1`, finalID).Scan(&claims))
	if claims != 4 { // 2 основных + 2 резервиста по очереди
		t.Fatalf("claims = %d, want 4", claims)
	}

	var outboxN int
	must(t, db.QueryRow(ctx, `SELECT count(*) FROM outbox`).Scan(&outboxN))
	if outboxN == 0 {
		t.Fatal("outbox is empty")
	}

	// 5. Статистика.
	rep, err := stats.Build(ctx, db, time.UTC)
	must(t, err)
	if rep.TotalUsers != nUsers || len(rep.Promo) != 2 || rep.Promo[1].Remaining != 0 || rep.Promo[1].Issued != nCodes {
		t.Fatalf("stats: %+v", rep)
	}
	t.Logf("promo: issued=%d sold_out_responses=%d outbox=%d", len(issued), soldOut, outboxN)
}

func migrate(t *testing.T, url string) {
	t.Helper()
	sqlDB, err := sql.Open("pgx", url)
	must(t, err)
	defer sqlDB.Close()
	_, err = sqlDB.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)
	must(t, err)
	goose.SetBaseFS(migrations.FS)
	must(t, goose.SetDialect("postgres"))
	must(t, goose.Up(sqlDB, "."))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
