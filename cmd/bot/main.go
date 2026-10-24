// Бот: принимает апдейты MAX (вебхук в проде, long polling локально) и отвечает пользователям.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
	"github.com/redis/go-redis/v9"

	"github.com/tgenchik/festbot/internal/bot"
	"github.com/tgenchik/festbot/internal/campaign"
	"github.com/tgenchik/festbot/internal/clock"
	"github.com/tgenchik/festbot/internal/config"
	"github.com/tgenchik/festbot/internal/devchat"
	"github.com/tgenchik/festbot/internal/entries"
	"github.com/tgenchik/festbot/internal/messenger"
	"github.com/tgenchik/festbot/internal/prizes"
	"github.com/tgenchik/festbot/internal/promo"
	"github.com/tgenchik/festbot/internal/store"
	"github.com/tgenchik/festbot/internal/subscription"
	"github.com/tgenchik/festbot/internal/users"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("bot stopped", "err", err)
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
	if err := cfg.RequireBot(); err != nil {
		return err
	}
	db, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	var (
		api     *maxbot.Api
		sender  messenger.Sender
		checker bot.MembershipChecker
		store   *devchat.Store
	)
	if cfg.DevMode {
		rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword})
		defer rdb.Close()
		if err := rdb.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		store = devchat.NewStore(rdb)
		sender, checker = devchat.NewMessenger(store), devchat.NewChecker(store)
	} else {
		if api, err = maxbot.NewApi(cfg.BotToken); err != nil {
			return err
		}
		sender, checker = messenger.New(api), subscription.NewChecker(api.Chats)
	}

	b := bot.New(bot.Deps{
		Log:             log,
		Messenger:       sender,
		Clock:           clock.New(cfg.Location),
		Users:           users.NewRepo(db),
		Campaign:        campaign.NewRepo(db),
		Promo:           promo.NewService(db),
		Entries:         entries.NewService(db, cfg.MaxChances),
		Subscription:    checker,
		Prizes:          prizes.NewService(db, cfg.ClaimTTL),
		WelcomeImageURL: cfg.WelcomeImageURL,
		RulesURL:        cfg.RulesURL,
		FinalPrize:      cfg.FinalPrize,
	})

	if cfg.DevMode {
		return serveDev(ctx, log, cfg, db, store, b)
	}
	if cfg.WebhookURL == "" {
		log.Info("starting in long polling mode")
		return poll(ctx, log, api, b)
	}
	return serveWebhook(ctx, log, cfg, api, b)
}

func serveWebhook(ctx context.Context, log *slog.Logger, cfg config.Config, api *maxbot.Api, b *bot.Bot) error {
	if cfg.WebhookSecret == "" {
		return errors.New("WEBHOOK_SECRET is required in webhook mode")
	}
	types := []string{
		string(model.UpdateBotStarted), string(model.UpdateMessageCreated), string(model.UpdateMessageCallback),
	}
	if _, err := api.Subscriptions.Subscribe(ctx, cfg.WebhookURL, cfg.WebhookSecret, types, ""); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("POST /webhook", api.GetHandler(b.Handle, cfg.WebhookSecret))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: cfg.BotAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("webhook server listening", "addr", cfg.BotAddr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func poll(ctx context.Context, log *slog.Logger, api *maxbot.Api, b *bot.Bot) error {
	var marker int64
	for ctx.Err() == nil {
		updates, next, err := api.Subscriptions.GetUpdates(ctx, marker)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Error("get updates", "err", err)
			time.Sleep(2 * time.Second)
			continue
		}
		marker = next
		for _, u := range updates {
			b.Handle(ctx, u)
		}
	}
	return nil
}
