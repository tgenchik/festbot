// Package messenger — тонкая обертка над MAX Bot API.
// Сообщение описывается простой сериализуемой структурой, чтобы его можно было
// положить в очередь задач и отправить из воркера.
package messenger

import (
	"context"
	"errors"
	"strings"

	maxbot "github.com/max-messenger/max-bot-api-client-go/v2"
	"github.com/max-messenger/max-bot-api-client-go/v2/model"
)

type Button struct {
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"` // callback-кнопка
	URL     string `json:"url,omitempty"`     // кнопка-ссылка
}

type Message struct {
	Text     string     `json:"text"`
	ImageURL string     `json:"image_url,omitempty"`
	Keyboard [][]Button `json:"keyboard,omitempty"`
}

// Sender — то, чем бот и воркер отправляют сообщения. Реализации: Client (MAX)
// и devchat.Messenger (локальный чат в браузере для разработки).
type Sender interface {
	Send(ctx context.Context, userID int64, m Message) error
	Replace(ctx context.Context, callbackID string, m Message) error
	Toast(ctx context.Context, callbackID, text string) error
}

type Client struct{ api *maxbot.Api }

func New(api *maxbot.Api) *Client { return &Client{api: api} }

func (m Message) build() *maxbot.Message {
	msg := maxbot.NewMessage().SetText(m.Text)
	if m.ImageURL != "" {
		msg.AddImageUrl(m.ImageURL)
	}
	if len(m.Keyboard) > 0 {
		kb := model.NewKeyboard()
		for _, row := range m.Keyboard {
			r := kb.AddRow()
			for _, b := range row {
				if b.URL != "" {
					r.AddLink(b.Text, b.URL)
				} else {
					r.AddCallBack(b.Text, b.Payload)
				}
			}
		}
		msg.AddKeyboard(kb)
	}
	return msg
}

// Send отправляет новое сообщение пользователю в личный диалог с ботом.
func (c *Client) Send(ctx context.Context, userID int64, m Message) error {
	_, err := c.api.Messages.Send(ctx, m.build().SetUser(userID))
	return err
}

// Replace отвечает на нажатие кнопки, заменяя сообщение с кнопками новым экраном.
func (c *Client) Replace(ctx context.Context, callbackID string, m Message) error {
	body := m.build().MessageBody()
	_, err := c.api.Messages.AnswerOnCallback(ctx, callbackID, model.CallbackAnswer{Message: &body})
	return err
}

// Toast показывает короткое всплывающее уведомление в ответ на нажатие кнопки.
func (c *Client) Toast(ctx context.Context, callbackID, text string) error {
	_, err := c.api.Messages.AnswerOnCallback(ctx, callbackID, model.CallbackAnswer{Notification: &text})
	return err
}

// IsUnreachable — пользователь заблокировал бота или удалил диалог.
// Коды ошибок MAX нужно сверить на реальном API: здесь проверка по смыслу кода.
func IsUnreachable(err error) bool {
	var apiErr *maxbot.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	code := strings.ToLower(apiErr.Code)
	return strings.Contains(code, "denied") || strings.Contains(code, "blocked") ||
		strings.Contains(code, "forbidden")
}
