package devchat

import (
	"context"
	"fmt"

	"github.com/tgenchik/festbot/internal/messenger"
)

// Messenger реализует messenger.Sender поверх Store.
type Messenger struct{ store *Store }

func NewMessenger(s *Store) *Messenger { return &Messenger{store: s} }

var _ messenger.Sender = (*Messenger)(nil)

func (m *Messenger) Send(ctx context.Context, userID int64, msg messenger.Message) error {
	_, err := m.store.Append(ctx, userID, ChatMessage{
		From: "bot", Text: msg.Text, ImageURL: msg.ImageURL, Keyboard: msg.Keyboard,
	})
	return err
}

func (m *Messenger) Replace(ctx context.Context, callbackID string, msg messenger.Message) error {
	userID, msgID, err := parseCallbackID(callbackID)
	if err != nil {
		return err
	}
	return m.store.Replace(ctx, userID, msgID, msg)
}

func (m *Messenger) Toast(ctx context.Context, callbackID, text string) error {
	userID, _, err := parseCallbackID(callbackID)
	if err != nil {
		return err
	}
	_, err = m.store.Append(ctx, userID, ChatMessage{From: "toast", Text: text})
	return err
}

// В режиме разработки callback_id = "<user_id>:<message_id>": так Replace знает,
// какое сообщение заменить.
func callbackID(userID, msgID int64) string { return fmt.Sprintf("%d:%d", userID, msgID) }

func parseCallbackID(id string) (userID, msgID int64, err error) {
	_, err = fmt.Sscanf(id, "%d:%d", &userID, &msgID)
	if err != nil {
		err = fmt.Errorf("devchat: bad callback id %q: %w", id, err)
	}
	return
}

// Checker имитирует проверку подписки: «подписан на канал партнера» — галочка
// на странице, одна на все каналы.
type Checker struct{ store *Store }

func NewChecker(s *Store) *Checker { return &Checker{store: s} }

func (c *Checker) IsMember(ctx context.Context, _ int64, userID int64) (bool, error) {
	return c.store.IsSubscribed(ctx, userID)
}

func (c *Checker) Members(ctx context.Context, _ int64, userIDs []int64) (map[int64]bool, error) {
	res := make(map[int64]bool, len(userIDs))
	for _, id := range userIDs {
		ok, err := c.store.IsSubscribed(ctx, id)
		if err != nil {
			return res, err
		}
		res[id] = ok
	}
	return res, nil
}
