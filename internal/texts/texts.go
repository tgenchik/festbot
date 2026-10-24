// Package texts — все экраны бота в одном месте (тексты из ТЗ).
// Редактор может править формулировки, не трогая логику.
package texts

import (
	"fmt"
	"strings"
	"time"

	m "github.com/tgenchik/festbot/internal/messenger"
)

// Payload-ы callback-кнопок. Формат «команда:параметр» разбирает клиент MAX.
const (
	CmdMenu    = "menu"
	CmdPromo   = "promo"
	CmdJoin    = "join"
	CmdCheck   = "check"
	CmdHowTo   = "howto"
	CmdChances = "chances"
	CmdRules   = "rules"
	CmdClaim   = "claim"   // claim:<draw_id>
	CmdConsent = "consent" // consent:<draw_id>.yes|no
)

func btn(text, payload string) m.Button { return m.Button{Text: text, Payload: payload} }

var menuKeyboard = [][]m.Button{
	{btn("Получить промокод", CmdPromo)},
	{btn("Участвовать в розыгрыше", CmdJoin)},
	{btn("Как участвовать", CmdHowTo), btn("Мои шансы", CmdChances)},
	{btn("Правила акции", CmdRules)},
}

var backKeyboard = [][]m.Button{{btn("В меню", CmdMenu)}}

func Welcome(imageURL string) m.Message {
	return m.Message{Text: "Добро пожаловать в Бот розыгрышей", ImageURL: imageURL, Keyboard: menuKeyboard}
}

func Menu() m.Message {
	return m.Message{Text: "Выбери, что сделать дальше 👇", Keyboard: menuKeyboard}
}

func NoCampaignToday() m.Message {
	return m.Message{Text: "Сегодня на сцене пауза — партнер дня еще не объявлен. Загляни позже!", Keyboard: backKeyboard}
}

func PromoIssued(partner, code, where string, validUntil *time.Time) m.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "Твой фестивальный бонус от %s готов 🎁\n\nПромокод: %s\n\n", partner, code)
	if validUntil != nil {
		fmt.Fprintf(&b, "Используй его до %s", validUntil.Format("02.01.2006"))
		if where != "" {
			fmt.Fprintf(&b, " %s", where)
		}
		b.WriteString(".\n\n")
	}
	b.WriteString("Теперь можешь принять участие в розыгрыше призов дня.")
	return m.Message{Text: b.String(), Keyboard: [][]m.Button{
		{btn("Участвовать в розыгрыше", CmdJoin)}, {btn("В меню", CmdMenu)},
	}}
}

func PromoSoldOut(partner string) m.Message {
	return m.Message{
		Text: fmt.Sprintf("Промокоды от %s уже закончились — их разобрали быстрее всех 🔥\n\n"+
			"Но ты все еще можешь подписаться на партнера и принять участие в розыгрыше призов дня.", partner),
		Keyboard: [][]m.Button{{btn("Участвовать в розыгрыше", CmdJoin)}, {btn("В меню", CmdMenu)}},
	}
}

func SubscribePrompt(partner, channelURL string) m.Message {
	return m.Message{
		Text: fmt.Sprintf("Чтобы попасть в розыгрыш призов от %s, подпишись на аккаунт партнера.", partner),
		Keyboard: [][]m.Button{
			{{Text: "Подписаться на " + partner, URL: channelURL}},
			{btn("Проверить подписку", CmdCheck)},
		},
	}
}

func NotSubscribed(partner, channelURL string) m.Message {
	return m.Message{
		Text: fmt.Sprintf("Пока не вижу подписку 👀\n\nПодпишись на аккаунт %s, вернись в бот и попробуй еще раз.", partner),
		Keyboard: [][]m.Button{
			{{Text: "Подписаться", URL: channelURL}},
			{btn("Проверить еще раз", CmdCheck)},
		},
	}
}

func Registered(partner string, chances, max int) m.Message {
	return m.Message{
		Text: fmt.Sprintf("Ты в лайнапе ✅\n\nЗаявка на розыгрыш призов от %s зарегистрирована.\n"+
			"Шансов на главный приз: %d из %d.\n\nВозвращайся завтра — на сцену выйдет новый партнер.",
			partner, chances, max),
		Keyboard: backKeyboard,
	}
}

func AlreadyRegistered() m.Message {
	return m.Message{
		Text:     "Ты уже участвуешь в сегодняшнем розыгрыше ✅\n\nРезультаты сообщим после подведения итогов.",
		Keyboard: backKeyboard,
	}
}

func HowTo() m.Message {
	return m.Message{
		Text: "Каждый день на сцене новый партнер.\n\n" +
			"1. Забери промокод партнера дня.\n" +
			"2. Подпишись на партнера и нажми «Участвовать в розыгрыше» — ты в розыгрыше призов дня.\n" +
			"3. Каждый день, когда ты что-то сделал в боте, дает +1 шанс на главный приз.",
		Keyboard: backKeyboard,
	}
}

func Chances(chances, max, daysLeft int, partners []string, finalPrize string) m.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "Твой фестивальный пропуск\n\nНакоплено шансов: %d из %d\n", chances, max)
	if len(partners) > 0 {
		fmt.Fprintf(&b, "Ты уже был у: %s\n", strings.Join(partners, ", "))
	}
	fmt.Fprintf(&b, "Осталось дней: %d\nГлавный приз: %s\n\n", daysLeft, finalPrize)
	b.WriteString("Чем больше дней розыгрыша ты посетишь, тем выше вероятность выиграть главный приз.")
	return m.Message{Text: b.String(), Keyboard: backKeyboard}
}

func Rules(url string) m.Message {
	msg := m.Message{Text: "Полные правила акции — по ссылке ниже.", Keyboard: backKeyboard}
	if url != "" {
		msg.Keyboard = [][]m.Button{{{Text: "Открыть правила", URL: url}}, {btn("В меню", CmdMenu)}}
	}
	return msg
}

func DailyWinner(partner, prize, instruction string) m.Message {
	return m.Message{Text: fmt.Sprintf("Ты в числе хедлайнеров дня 🔥\n\nТы выиграл %s от %s.\n\n%s",
		prize, partner, instruction)}
}

func FinalWinner(drawID int64, prize string, deadline time.Time) m.Message {
	return m.Message{
		Text: fmt.Sprintf("Твой фестиваль выходит на главную сцену 🔥\n\nТы выиграл: %s!\n\n"+
			"Чтобы подтвердить получение приза, нажми кнопку ниже и оставь контактные данные до %s.",
			prize, deadline.Format("02.01.2006 15:04")),
		Keyboard: [][]m.Button{{btn("Получить приз", fmt.Sprintf("%s:%d", CmdClaim, drawID))}},
	}
}

func AskFullName() m.Message {
	return m.Message{Text: "Напиши, пожалуйста, имя и фамилию одним сообщением — как в документе."}
}

func BadFullName() m.Message {
	return m.Message{Text: "Нужны имя и фамилия через пробел, например: Анна Иванова."}
}

func AskConsent(drawID int64) m.Message {
	return m.Message{
		Text: "Последний шаг: нужно согласие на обработку персональных данных и их передачу " +
			"партнеру или билетному оператору для вручения приза.",
		Keyboard: [][]m.Button{{
			btn("Согласен", fmt.Sprintf("%s:%d.yes", CmdConsent, drawID)),
			btn("Не согласен", fmt.Sprintf("%s:%d.no", CmdConsent, drawID)),
		}},
	}
}

func Claimed() m.Message {
	return m.Message{Text: "Готово ✅ Данные получены, скоро свяжемся по вручению приза.", Keyboard: backKeyboard}
}

func Declined() m.Message {
	return m.Message{Text: "Понимаем. Без согласия мы не сможем передать приз, поэтому он перейдет следующему участнику.",
		Keyboard: backKeyboard}
}

func ClaimClosed() m.Message {
	return m.Message{Text: "Срок получения этого приза уже истек или данные уже отправлены.", Keyboard: backKeyboard}
}
