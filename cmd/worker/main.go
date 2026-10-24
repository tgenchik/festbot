// Воркер: очереди asynq (рассылки, уведомления победителям, дедлайны призов),
// планировщик ежедневного розыгрыша и outbox relay.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/redis/go-redis/v9"

	"github.com/tgenchik/festbot/internal/broadcast"
	"github.com/tgenchik/festbot/internal/campaign"
	"github.com/tgenchik/festbot/internal/clock"
	"github.com/tgenchik/festbot/internal/config"
	"github.com/tgenchik/festbot/internal/devchat"
	"github.com/tgenchik/festbot/internal/draws"
	"github.com/tgenchik/festbot/internal/messenger"
	"github.com/tgenchik/festbot/internal/outbox"
	"github.com/tgenchik/festbot/internal/prizes"
	"github.com/tgenchik/festbot/internal/store"
	"github.com/tgenchik/festbot/internal/subscription"
	"github.com/tgenchik/festbot/internal/tasks"
	"github.com/tgenchik/festbot/internal/texts"
	"github.com/tgenchik/festbot/internal/users"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

type worker struct {
	log       *slog.Logger
	client    *asynq.Client
	msg       messenger.Sender
	limiter   *broadcast.Limiter
	users     *users.Repo
	campaign  *campaign.Repo
	draws     *draws.Service
	prizes    *prizes.Service
	broadcast *broadcast.Service
	clock     clock.Clock
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

	redisOpt := asynq.RedisClientOpt{Addr: cfg.RedisAddr, Password: cfg.RedisPassword}
	client := asynq.NewClient(redisOpt)
	defer client.Close()
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword})
	defer rdb.Close()

	// В режиме разработки сообщения уходят в локальный чат, подписка — галочка на странице.
	var (
		sender  messenger.Sender
		checker draws.MembershipChecker
	)
	if cfg.DevMode {
		store := devchat.NewStore(rdb)
		sender, checker = devchat.NewMessenger(store), devchat.NewChecker(store)
	} else {
		api, err := maxbot.NewApi(cfg.BotToken)
		if err != nil {
			return err
		}
		sender, checker = messenger.New(api), subscription.NewChecker(api.Chats)
	}

	camp := campaign.NewRepo(db)
	w := &worker{
		log:      log,
		client:   client,
		msg:      sender,
		limiter:  broadcast.NewLimiter(rdb, cfg.SendRatePerSec),
		users:    users.NewRepo(db),
		campaign: camp,
		draws: draws.NewService(db, camp, checker, draws.Config{
			MaxChances:          cfg.MaxChances,
			FinalWinners:        cfg.FinalWinners,
			FinalReserves:       cfg.FinalReserves,
			FinalPrize:          cfg.FinalPrize,
			ClaimTTL:            cfg.ClaimTTL,
			RecheckSubscription: cfg.RecheckSubscription,
		}),
		prizes:    prizes.NewService(db, cfg.ClaimTTL),
		broadcast: broadcast.NewService(db),
		clock:     clock.New(cfg.Location),
	}

	mux := asynq.NewServeMux()
	mux.HandleFunc(tasks.TypeSendMessage, w.sendMessage)
	mux.HandleFunc(tasks.TypeDailyDraw, w.dailyDraw)
	mux.HandleFunc(tasks.TypeDrawNotify, w.drawNotify)
	mux.HandleFunc(tasks.TypePrizeNotify, w.prizeNotify)
	mux.HandleFunc(tasks.TypePrizeExpire, w.prizeExpire)
	mux.HandleFunc(tasks.TypeBroadcastStart, w.broadcastStart)

	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: 20,
		Queues:      tasks.QueueWeights,
		Logger:      nil,
	})
	if err := srv.Start(mux); err != nil {
		return err
	}
	defer srv.Shutdown()

	// Если воркеров несколько, задача создастся в каждом, но второй розыгрыш
	// за тот же день не пройдет уникальный индекс в БД (ErrAlreadyDrawn).
	sched := asynq.NewScheduler(redisOpt, &asynq.SchedulerOpts{Location: cfg.Location})
	if _, err := sched.Register(cfg.DailyDrawCron, asynq.NewTask(tasks.TypeDailyDraw, nil),
		asynq.Queue(tasks.QueueDefault)); err != nil {
		return err
	}
	if err := sched.Start(); err != nil {
		return err
	}
	defer sched.Shutdown()

	go outbox.NewRelay(db, client, log).Run(ctx)

	log.Info("worker started")
	<-ctx.Done()
	return nil
}

// sendMessage — единственное место, откуда воркер шлет сообщения: с общим лимитом скорости.
func (w *worker) sendMessage(ctx context.Context, t *asynq.Task) error {
	var p tasks.SendMessage
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	if err := w.limiter.Wait(ctx); err != nil {
		return err
	}
	err := w.msg.Send(ctx, p.UserID, p.Msg)
	if messenger.IsUnreachable(err) {
		return w.users.MarkUnreachable(ctx, p.UserID)
	}
	return err
}

func (w *worker) enqueueSend(ctx context.Context, taskID string, userID int64, m messenger.Message) error {
	payload, err := json.Marshal(tasks.SendMessage{UserID: userID, Msg: m})
	if err != nil {
		return err
	}
	_, err = w.client.EnqueueContext(ctx, asynq.NewTask(tasks.TypeSendMessage, payload),
		asynq.TaskID(taskID), asynq.Queue(tasks.QueueCritical),
		asynq.MaxRetry(10), asynq.Retention(48*time.Hour))
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil
	}
	return err
}

// dailyDraw — по расписанию считает итоги вчерашнего дня. Результат ждет подтверждения админа.
func (w *worker) dailyDraw(ctx context.Context, _ *asynq.Task) error {
	day := w.clock.Yesterday()
	id, err := w.draws.RunDaily(ctx, day)
	switch {
	case errors.Is(err, campaign.ErrNoDay), errors.Is(err, draws.ErrAlreadyDrawn), errors.Is(err, draws.ErrNoEntries):
		w.log.Info("daily draw skipped", "day", day.Format(time.DateOnly), "reason", err)
		return nil
	case err != nil:
		return err
	}
	w.log.Info("daily draw ready for confirmation", "day", day.Format(time.DateOnly), "draw_id", id)
	return nil
}

func (w *worker) drawNotify(ctx context.Context, t *asynq.Task) error {
	var ref tasks.DrawRef
	if err := json.Unmarshal(t.Payload(), &ref); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	d, err := w.draws.Get(ctx, ref.DrawID)
	if err != nil {
		return err
	}
	if d.Status != "confirmed" {
		return nil
	}

	var day campaign.Day
	if d.Kind == "daily" {
		if day, err = w.campaign.ByDate(ctx, *d.Day); err != nil {
			return err
		}
	}
	for _, win := range d.Winners {
		if win.IsReserve {
			continue
		}
		var m messenger.Message
		if d.Kind == "daily" {
			m = texts.DailyWinner(day.PartnerName, win.PrizeTitle, day.PrizeInstruction)
		} else {
			claim, err := w.prizes.Get(ctx, d.ID, win.UserID)
			if err != nil {
				return err
			}
			m = texts.FinalWinner(d.ID, claim.PrizeTitle, claim.Deadline)
		}
		if err := w.enqueueSend(ctx, fmt.Sprintf("win-%d-%d", d.ID, win.UserID), win.UserID, m); err != nil {
			return err
		}
	}
	return w.draws.MarkNotified(ctx, d.ID)
}

// prizeNotify — приз перешел резервному победителю.
func (w *worker) prizeNotify(ctx context.Context, t *asynq.Task) error {
	var ref tasks.ClaimRef
	if err := json.Unmarshal(t.Payload(), &ref); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	claim, err := w.prizes.Get(ctx, ref.DrawID, ref.UserID)
	if err != nil {
		return err
	}
	return w.enqueueSend(ctx, fmt.Sprintf("win-%d-%d", ref.DrawID, ref.UserID), ref.UserID,
		texts.FinalWinner(ref.DrawID, claim.PrizeTitle, claim.Deadline))
}

func (w *worker) prizeExpire(ctx context.Context, t *asynq.Task) error {
	var ref tasks.ClaimRef
	if err := json.Unmarshal(t.Payload(), &ref); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	return w.prizes.Expire(ctx, ref.DrawID, ref.UserID)
}

func (w *worker) broadcastStart(ctx context.Context, t *asynq.Task) error {
	var ref tasks.BroadcastRef
	if err := json.Unmarshal(t.Payload(), &ref); err != nil {
		return fmt.Errorf("%w: %v", asynq.SkipRetry, err)
	}
	n, err := w.broadcast.Fanout(ctx, ref.BroadcastID, w.client)
	w.log.Info("broadcast queued", "id", ref.BroadcastID, "recipients", n)
	return err
}
