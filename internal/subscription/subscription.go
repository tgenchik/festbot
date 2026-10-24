// Package subscription проверяет подписку пользователя на канал партнера через MAX Bot API.
//
// Важно: метод GET /chats/{id}/members работает, только если бот — администратор
// канала. Это нужно согласовать с каждым партнером до старта акции.
package subscription

import (
	"context"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
)

const batchSize = 100

type Checker struct{ chats maxbot.ChatsAPI }

func NewChecker(chats maxbot.ChatsAPI) *Checker { return &Checker{chats: chats} }

func (c *Checker) IsMember(ctx context.Context, channelID, userID int64) (bool, error) {
	got, err := c.Members(ctx, channelID, []int64{userID})
	return got[userID], err
}

// Members проверяет пачку пользователей (используется при перепроверке перед розыгрышем).
func (c *Checker) Members(ctx context.Context, channelID int64, userIDs []int64) (map[int64]bool, error) {
	res := make(map[int64]bool, len(userIDs))
	for start := 0; start < len(userIDs); start += batchSize {
		batch := userIDs[start:min(start+batchSize, len(userIDs))]
		list, err := c.chats.GetMembers(ctx, channelID, 0, 0, batch)
		if err != nil {
			return res, err
		}
		for _, m := range list.Members {
			res[m.UserID] = true
		}
	}
	return res, nil
}
