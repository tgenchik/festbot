// Package bot — обработка входящих событий MAX: экраны, кнопки, анкета победителя.
package bot

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/max-messenger/max-bot-api-client-go/v2/model"

	"github.com/tgenchik/festbot/internal/campaign"
	"github.com/tgenchik/festbot/internal/clock"
	"github.com/tgenchik/festbot/internal/entries"
	"github.com/tgenchik/festbot/internal/messenger"
	"github.com/tgenchik/festbot/internal/prizes"
	"github.com/tgenchik/festbot/internal/promo"
	"github.com/tgenchik/festbot/internal/subscription"
	"github.com/tgenchik/festbot/internal/texts"
	"github.com/tgenchik/festbot/internal/users"
)

type Deps struct {
	Log          *slog.Logger
	Messenger    *messenger.Client
	Clock        clock.Clock
	Users        *users.Repo
	Campaign     *campaign.Repo
	Promo        *promo.Service
	Entries      *entries.Service
	Subscription *subscription.Checker
	Prizes       *prizes.Service

	WelcomeImageURL string
	RulesURL        string
	FinalPrize      string
}

type Bot struct{ Deps }

func New(d Deps) *Bot { return &Bot{d} }

// Handle — точка входа для каждого апдейта (вебхук или long polling).
func (b *Bot) Handle(ctx context.Context, u model.Update) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var err error
	switch u.UpdateType {
	case model.UpdateBotStarted:
		err = b.start(ctx, u.UserID, u.GetUser())
	case model.UpdateMessageCreated:
		err = b.onText(ctx, u)
	case model.UpdateMessageCallback:
		err = b.onCallback(ctx, u)
	}
	if err != nil {
		b.Log.Error("handle update", "type", u.UpdateType, "user", u.UserID, "err", err)
	}
}

func (b *Bot) start(ctx context.Context, userID int64, user model.User) error {
	if err := b.Users.Touch(ctx, userID, user.FirstName, user.LastName); err != nil {
		return err
	}
	return b.Messenger.Send(ctx, userID, texts.Welcome(b.WelcomeImageURL))
}

func (b *Bot) onText(ctx context.Context, u model.Update) error {
	msg := u.GetMessage()
	userID := msg.Sender.UserID
	text := strings.TrimSpace(msg.Body.Text)

	if text == "/start" {
		return b.start(ctx, userID, model.User{FirstName: msg.Sender.FirstName, LastName: msg.Sender.LastName})
	}
	if err := b.Users.Touch(ctx, userID, msg.Sender.FirstName, msg.Sender.LastName); err != nil {
		return err
	}

	// Пользователь заполняет анкету победителя — текст считаем именем и фамилией.
	drawID, ok, err := b.Prizes.AwaitingName(ctx, userID)
	if err != nil {
		return err
	}
	if ok {
		switch err := b.Prizes.SaveName(ctx, drawID, userID, text); {
		case errors.Is(err, prizes.ErrBadName):
			return b.Messenger.Send(ctx, userID, texts.BadFullName())
		case errors.Is(err, prizes.ErrClosed):
			return b.Messenger.Send(ctx, userID, texts.ClaimClosed())
		case err != nil:
			return err
		}
		return b.Messenger.Send(ctx, userID, texts.AskConsent(drawID))
	}

	return b.Messenger.Send(ctx, userID, texts.Menu())
}

func (b *Bot) onCallback(ctx context.Context, u model.Update) error {
	cb := u.GetCallback()
	userID := cb.User.UserID
	p := u.GetCallbackPayload()
	if err := b.Users.Touch(ctx, userID, cb.User.FirstName, cb.User.LastName); err != nil {
		return err
	}

	replace := func(m messenger.Message) error { return b.Messenger.Replace(ctx, cb.CallbackID, m) }

	switch p.Payload {
	case texts.CmdMenu:
		return replace(texts.Menu())
	case texts.CmdHowTo:
		return replace(texts.HowTo())
	case texts.CmdRules:
		return replace(texts.Rules(b.RulesURL))
	case texts.CmdChances:
		return b.chances(ctx, userID, replace)
	case texts.CmdPromo:
		return b.promo(ctx, userID, cb.CallbackID, replace)
	case texts.CmdJoin:
		return b.join(ctx, userID, replace)
	case texts.CmdCheck:
		return b.check(ctx, userID, replace)
	case texts.CmdClaim:
		return b.claim(ctx, userID, p.Param, replace)
	case texts.CmdConsent:
		return b.consent(ctx, userID, p.Param, replace)
	}
	return replace(texts.Menu())
}

// promo — раздел 2 ТЗ. Код отправляется отдельным сообщением, чтобы остался в истории чата.
func (b *Bot) promo(ctx context.Context, userID int64, callbackID string, replace func(messenger.Message) error) error {
	day, err := b.today(ctx)
	if errors.Is(err, campaign.ErrNoDay) {
		return replace(texts.NoCampaignToday())
	}
	if err != nil {
		return err
	}
	res, err := b.Promo.Issue(ctx, userID, day.PartnerID, day.Date)
	if errors.Is(err, promo.ErrSoldOut) {
		return replace(texts.PromoSoldOut(day.PartnerName))
	}
	if err != nil {
		return err
	}
	if err := b.Messenger.Toast(ctx, callbackID, "Промокод отправлен 🎁"); err != nil {
		b.Log.Warn("toast", "err", err)
	}
	return b.Messenger.Send(ctx, userID, texts.PromoIssued(day.PartnerName, res.Code, day.PromoWhere, day.PromoValidUntil))
}

// join — раздел 3.2: если уже участвует, сразу говорим об этом; иначе просим подписаться.
func (b *Bot) join(ctx context.Context, userID int64, replace func(messenger.Message) error) error {
	day, err := b.today(ctx)
	if errors.Is(err, campaign.ErrNoDay) {
		return replace(texts.NoCampaignToday())
	}
	if err != nil {
		return err
	}
	registered, err := b.Entries.IsRegistered(ctx, userID, day.Date)
	if err != nil {
		return err
	}
	if registered {
		return replace(texts.AlreadyRegistered())
	}
	return replace(texts.SubscribePrompt(day.PartnerName, day.ChannelURL))
}

// check — разделы 3.3–3.5: проверка подписки и регистрация заявки.
func (b *Bot) check(ctx context.Context, userID int64, replace func(messenger.Message) error) error {
	day, err := b.today(ctx)
	if errors.Is(err, campaign.ErrNoDay) {
		return replace(texts.NoCampaignToday())
	}
	if err != nil {
		return err
	}
	ok, err := b.Subscription.IsMember(ctx, day.ChannelID, userID)
	if err != nil {
		return err
	}
	if !ok {
		return replace(texts.NotSubscribed(day.PartnerName, day.ChannelURL))
	}
	res, err := b.Entries.Register(ctx, userID, day.Date)
	if err != nil {
		return err
	}
	if res.Already {
		return replace(texts.AlreadyRegistered())
	}
	sum, err := b.Entries.Summary(ctx, userID)
	if err != nil {
		return err
	}
	return replace(texts.Registered(day.PartnerName, res.Chances, sum.MaxChances))
}

func (b *Bot) chances(ctx context.Context, userID int64, replace func(messenger.Message) error) error {
	sum, err := b.Entries.Summary(ctx, userID)
	if err != nil {
		return err
	}
	_, last, _, err := b.Campaign.Bounds(ctx)
	if err != nil {
		return err
	}
	daysLeft := 0
	if today := b.Clock.Today(); !last.Before(today) {
		daysLeft = int(last.Sub(today).Hours()/24) + 1
	}
	return replace(texts.Chances(sum.Chances, sum.MaxChances, daysLeft, sum.Partners, b.FinalPrize))
}

func (b *Bot) claim(ctx context.Context, userID int64, param string, replace func(messenger.Message) error) error {
	drawID, err := strconv.ParseInt(param, 10, 64)
	if err != nil {
		return replace(texts.Menu())
	}
	switch err := b.Prizes.Start(ctx, drawID, userID); {
	case errors.Is(err, prizes.ErrClosed):
		return replace(texts.ClaimClosed())
	case err != nil:
		return err
	}
	return replace(texts.AskFullName())
}

func (b *Bot) consent(ctx context.Context, userID int64, param string, replace func(messenger.Message) error) error {
	idStr, answer, _ := strings.Cut(param, ".")
	drawID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return replace(texts.Menu())
	}
	agree := answer == "yes"
	switch err := b.Prizes.Consent(ctx, drawID, userID, agree); {
	case errors.Is(err, prizes.ErrClosed):
		return replace(texts.ClaimClosed())
	case err != nil:
		return err
	}
	if agree {
		return replace(texts.Claimed())
	}
	return replace(texts.Declined())
}

func (b *Bot) today(ctx context.Context) (campaign.Day, error) {
	return b.Campaign.ByDate(ctx, b.Clock.Today())
}
