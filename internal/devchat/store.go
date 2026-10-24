// Package devchat — режим разработки без MAX.
//
// Вместо мессенджера бот общается с локальной веб-страницей: входящие действия
// (старт, текст, нажатие кнопки) превращаются в обычные model.Update и идут через
// тот же bot.Handle, что и в проде. Исходящие сообщения бота и воркера складываются
// в Redis, поэтому уведомления победителям из воркера тоже видны в чате.
// Подписка на каналы партнеров имитируется галочкой.
package devchat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tgenchik/festbot/internal/messenger"
)

const keepMessages = 200

// ChatMessage — одно сообщение в ленте чата.
type ChatMessage struct {
	ID       int64                `json:"id"`
	From     string               `json:"from"` // bot | user | toast
	Text     string               `json:"text"`
	ImageURL string               `json:"image_url,omitempty"`
	Keyboard [][]messenger.Button `json:"keyboard,omitempty"`
	At       time.Time            `json:"at"`
}

type Store struct{ rdb redis.UniversalClient }

func NewStore(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

func chatKey(userID int64) string    { return fmt.Sprintf("dev:chat:%d", userID) }
func versionKey(userID int64) string { return fmt.Sprintf("dev:ver:%d", userID) }

const subscribedKey = "dev:subscribed"

func (s *Store) Append(ctx context.Context, userID int64, m ChatMessage) (int64, error) {
	id, err := s.rdb.Incr(ctx, "dev:seq").Result()
	if err != nil {
		return 0, err
	}
	m.ID, m.At = id, time.Now()
	raw, err := json.Marshal(m)
	if err != nil {
		return 0, err
	}
	pipe := s.rdb.TxPipeline()
	pipe.RPush(ctx, chatKey(userID), raw)
	pipe.LTrim(ctx, chatKey(userID), -keepMessages, -1)
	pipe.Incr(ctx, versionKey(userID))
	_, err = pipe.Exec(ctx)
	return id, err
}

// Replace подменяет сообщение с кнопками новым экраном (аналог ответа на callback в MAX).
func (s *Store) Replace(ctx context.Context, userID, msgID int64, m messenger.Message) error {
	list, err := s.rdb.LRange(ctx, chatKey(userID), 0, -1).Result()
	if err != nil {
		return err
	}
	for i, raw := range list {
		var cm ChatMessage
		if json.Unmarshal([]byte(raw), &cm) != nil || cm.ID != msgID {
			continue
		}
		cm.Text, cm.ImageURL, cm.Keyboard, cm.At = m.Text, m.ImageURL, m.Keyboard, time.Now()
		updated, _ := json.Marshal(cm)
		pipe := s.rdb.TxPipeline()
		pipe.LSet(ctx, chatKey(userID), int64(i), updated)
		pipe.Incr(ctx, versionKey(userID))
		_, err = pipe.Exec(ctx)
		return err
	}
	// Сообщение уже вытеснено из ленты — показываем экран новым сообщением.
	_, err = s.Append(ctx, userID, ChatMessage{From: "bot", Text: m.Text, ImageURL: m.ImageURL, Keyboard: m.Keyboard})
	return err
}

// Chat возвращает версию ленты и сами сообщения. Версия растет при любом изменении,
// поэтому странице достаточно перерисовываться, только когда она поменялась.
func (s *Store) Chat(ctx context.Context, userID int64) (int64, []ChatMessage, error) {
	ver, err := s.rdb.Get(ctx, versionKey(userID)).Int64()
	if err != nil && err != redis.Nil {
		return 0, nil, err
	}
	list, err := s.rdb.LRange(ctx, chatKey(userID), 0, -1).Result()
	if err != nil {
		return 0, nil, err
	}
	msgs := make([]ChatMessage, 0, len(list))
	for _, raw := range list {
		var cm ChatMessage
		if json.Unmarshal([]byte(raw), &cm) == nil {
			msgs = append(msgs, cm)
		}
	}
	return ver, msgs, nil
}

func (s *Store) ClearChat(ctx context.Context, userID int64) error {
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, chatKey(userID))
	pipe.Incr(ctx, versionKey(userID))
	_, err := pipe.Exec(ctx)
	return err
}

// ClearAll стирает все чаты и подписки режима разработки.
func (s *Store) ClearAll(ctx context.Context) error {
	iter := s.rdb.Scan(ctx, 0, "dev:*", 500).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil || len(keys) == 0 {
		return err
	}
	return s.rdb.Del(ctx, keys...).Err()
}

func (s *Store) SetSubscribed(ctx context.Context, userID int64, on bool) error {
	if on {
		return s.rdb.SAdd(ctx, subscribedKey, userID).Err()
	}
	return s.rdb.SRem(ctx, subscribedKey, userID).Err()
}

func (s *Store) IsSubscribed(ctx context.Context, userID int64) (bool, error) {
	return s.rdb.SIsMember(ctx, subscribedKey, userID).Result()
}
