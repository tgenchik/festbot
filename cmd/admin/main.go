// Админка: HTTP API для команды акции.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/redis/go-redis/v9"

	"github.com/tgenchik/festbot/internal/admin"
	"github.com/tgenchik/festbot/internal/broadcast"
	"github.com/tgenchik/festbot/internal/campaign"
	"github.com/tgenchik/festbot/internal/config"
	"github.com/tgenchik/festbot/internal/devchat"
	"github.com/tgenchik/festbot/internal/draws"
	"github.com/tgenchik/festbot/internal/prizes"
	"github.com/tgenchik/festbot/internal/promo"
	"github.com/tgenchik/festbot/internal/store"
	"github.com/tgenchik/festbot/internal/subscription"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("admin stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireBot(); err != nil { // нужен для перепроверки подписок при ручном розыгрыше
		return err
	}
	if cfg.AdminPassword == "" {
		return errors.New("ADMIN_PASSWORD is required")
	}
	db, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	var checker draws.MembershipChecker
	if cfg.DevMode {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword})
		defer rdb.Close()
		checker = devchat.NewChecker(devchat.NewStore(rdb))
	} else {
		api, err := maxbot.NewApi(cfg.BotToken)
		if err != nil {
			return err
		}
		checker = subscription.NewChecker(api.Chats)
	}

	camp := campaign.NewRepo(db)
	handler := admin.Router(admin.Deps{
		Log:      log,
		DB:       db,
		Campaign: camp,
		Promo:    promo.NewService(db),
		Draws: draws.NewService(db, camp, checker, draws.Config{
			MaxChances:          cfg.MaxChances,
			FinalWinners:        cfg.FinalWinners,
			FinalReserves:       cfg.FinalReserves,
			FinalPrize:          cfg.FinalPrize,
			ClaimTTL:            cfg.ClaimTTL,
			RecheckSubscription: cfg.RecheckSubscription,
		}),
		Prizes:    prizes.NewService(db, cfg.ClaimTTL),
		Broadcast: broadcast.NewService(db),
		Location:  cfg.Location,
		User:      cfg.AdminUser,
		Password:  cfg.AdminPassword,
	})

	srv := &http.Server{Addr: cfg.AdminAddr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("admin listening", "addr", cfg.AdminAddr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
