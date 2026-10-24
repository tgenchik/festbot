package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tgenchik/festbot/internal/bot"
	"github.com/tgenchik/festbot/internal/campaign"
	"github.com/tgenchik/festbot/internal/clock"
	"github.com/tgenchik/festbot/internal/config"
	"github.com/tgenchik/festbot/internal/devchat"
	"github.com/tgenchik/festbot/internal/draws"
	"github.com/tgenchik/festbot/internal/promo"
)

// serveDev запускает локальный чат вместо MAX (DEV_MODE=true).
func serveDev(ctx context.Context, log *slog.Logger, cfg config.Config, db *pgxpool.Pool,
	store *devchat.Store, b *bot.Bot) error {
	camp := campaign.NewRepo(db)
	clk := clock.New(cfg.Location)
	drw := draws.NewService(db, camp, devchat.NewChecker(store), drawsConfig(cfg))
	promoSvc := promo.NewService(db)

	actions := map[string]devchat.Action{
		"seed": func(ctx context.Context) (string, error) {
			today := clk.Today()
			if day, err := camp.ByDate(ctx, today); err == nil {
				return fmt.Sprintf("Партнер на сегодня уже есть: %s", day.PartnerName), nil
			} else if !errors.Is(err, campaign.ErrNoDay) {
				return "", err
			}
			until := today.AddDate(0, 0, 30)
			id, err := camp.CreatePartner(ctx, campaign.Partner{
				Name:            fmt.Sprintf("Партнер %s", today.Format("02.01")),
				ChannelID:       0,
				ChannelURL:      "https://max.ru",
				PromoWhere:      "на сайте партнера",
				PromoValidUntil: &until,
			})
			if err != nil {
				return "", err
			}
			if err := camp.UpsertDay(ctx, campaign.DayInput{
				Date: today, PartnerID: id, WinnersCount: 2,
				PrizeTitle: "фестивальный мерч", PrizeInstruction: "Мы напишем тебе в течение трех дней.",
			}); err != nil {
				return "", err
			}
			codes := make([]string, 5)
			for i := range codes {
				codes[i] = "FEST-" + rand.Text()[:6]
			}
			if _, err := promoSvc.Load(ctx, id, codes); err != nil {
				return "", err
			}
			return fmt.Sprintf("Партнер на %s заведен, 5 промокодов, 2 приза дня", today.Format("02.01")), nil
		},
		"daily": func(ctx context.Context) (string, error) {
			id, err := drw.RunDaily(ctx, clk.Today())
			if err != nil {
				return "", err
			}
			return confirmAndDescribe(ctx, drw, id, "Розыгрыш дня")
		},
		"final": func(ctx context.Context) (string, error) {
			id, err := drw.RunFinal(ctx)
			if err != nil {
				return "", err
			}
			return confirmAndDescribe(ctx, drw, id, "Финал")
		},
		"reset": func(ctx context.Context) (string, error) {
			if _, err := db.Exec(ctx, `TRUNCATE users, partners, campaign_days, promo_codes, daily_entries,
				active_days, events, draws, draw_participants, draw_winners, prize_claims, broadcasts, outbox
				RESTART IDENTITY CASCADE`); err != nil {
				return "", err
			}
			return "База и чаты очищены", store.ClearAll(ctx)
		},
	}

	srv := &http.Server{
		Addr:              cfg.BotAddr,
		Handler:           devchat.NewServer(store, b.Handle, actions, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("DEV MODE: локальный чат открыт", "url", "http://localhost"+cfg.BotAddr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func confirmAndDescribe(ctx context.Context, drw *draws.Service, id int64, title string) (string, error) {
	if err := drw.Confirm(ctx, id, "dev"); err != nil {
		return "", err
	}
	d, err := drw.Get(ctx, id)
	if err != nil {
		return "", err
	}
	main, reserve := 0, 0
	for _, w := range d.Winners {
		if w.IsReserve {
			reserve++
		} else {
			main++
		}
	}
	msg := fmt.Sprintf("%s №%d: участников %d, победителей %d", title, d.ID, d.ParticipantsCount, main)
	if reserve > 0 {
		msg += fmt.Sprintf(", в резерве %d", reserve)
	}
	return msg + ". Уведомления отправит воркер", nil
}

func drawsConfig(cfg config.Config) draws.Config {
	return draws.Config{
		MaxChances:          cfg.MaxChances,
		FinalWinners:        cfg.FinalWinners,
		FinalReserves:       cfg.FinalReserves,
		FinalPrize:          cfg.FinalPrize,
		ClaimTTL:            cfg.ClaimTTL,
		RecheckSubscription: cfg.RecheckSubscription,
	}
}
