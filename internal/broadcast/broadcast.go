// Package broadcast — пуш-рассылки всем пользователям бота с общим лимитом скорости.
package broadcast

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/tgenchik/festbot/internal/messenger"
	"github.com/tgenchik/festbot/internal/outbox"
	"github.com/tgenchik/festbot/internal/tasks"
	"github.com/tgenchik/festbot/internal/users"
)

type Service struct {
	db    *pgxpool.Pool
	users *users.Repo
}

func NewService(db *pgxpool.Pool) *Service { return &Service{db: db, users: users.NewRepo(db)} }

// Create сохраняет рассылку и ставит задачу на ее разбор по получателям.
func (s *Service) Create(ctx context.Context, msg messenger.Message) (int64, error) {
	var id int64
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		raw, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO broadcasts (message) VALUES ($1) RETURNING id`, raw).Scan(&id); err != nil {
			return err
		}
		return outbox.Add(ctx, tx, tasks.TypeBroadcastStart, tasks.BroadcastRef{BroadcastID: id}, time.Time{})
	})
	return id, err
}

// Fanout разбивает рассылку на задачи «отправить одному пользователю».
// TaskID делает повторный запуск безопасным: уже поставленные задачи не задублируются.
func (s *Service) Fanout(ctx context.Context, id int64, client *asynq.Client) (int, error) {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT message FROM broadcasts WHERE id = $1`, id).Scan(&raw); err != nil {
		return 0, err
	}
	var msg messenger.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return 0, err
	}

	queued := 0
	var after int64
	for {
		ids, err := s.users.ReachableAfter(ctx, after, 1000)
		if err != nil {
			return queued, err
		}
		if len(ids) == 0 {
			break
		}
		for _, uid := range ids {
			payload, _ := json.Marshal(tasks.SendMessage{UserID: uid, Msg: msg})
			_, err := client.EnqueueContext(ctx, asynq.NewTask(tasks.TypeSendMessage, payload),
				asynq.Queue(tasks.QueueBulk),
				asynq.TaskID(fmt.Sprintf("bc-%d-%d", id, uid)),
				asynq.MaxRetry(3),
				asynq.Retention(24*time.Hour))
			if err != nil && err != asynq.ErrTaskIDConflict {
				return queued, err
			}
			queued++
		}
		after = ids[len(ids)-1]
	}
	_, err := s.db.Exec(ctx, `UPDATE broadcasts SET queued = $2 WHERE id = $1`, id, queued)
	return queued, err
}

// Limiter — общий для всех воркеров лимит сообщений в секунду (окно в Redis).
type Limiter struct {
	rdb    redis.UniversalClient
	perSec int64
}

func NewLimiter(rdb redis.UniversalClient, perSec int) *Limiter {
	return &Limiter{rdb: rdb, perSec: int64(perSec)}
}

func (l *Limiter) Wait(ctx context.Context) error {
	for {
		now := time.Now()
		key := fmt.Sprintf("rl:send:%d", now.Unix())
		n, err := l.rdb.Incr(ctx, key).Result()
		if err != nil {
			return err
		}
		if n == 1 {
			l.rdb.Expire(ctx, key, 2*time.Second)
		}
		if n <= l.perSec {
			return nil
		}
		next := now.Truncate(time.Second).Add(time.Second)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Until(next)):
		}
	}
}
