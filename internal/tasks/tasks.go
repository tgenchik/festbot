// Package tasks — типы фоновых задач (asynq поверх Redis) и их полезная нагрузка.
package tasks

import "github.com/tgenchik/festbot/internal/messenger"

const (
	TypeSendMessage    = "message:send"
	TypeDailyDraw      = "draw:daily"
	TypeDrawNotify     = "draw:notify"
	TypePrizeNotify    = "prize:notify"
	TypePrizeExpire    = "prize:expire"
	TypeBroadcastStart = "broadcast:start"
)

// Очереди с весами: уведомления победителям важнее массовых рассылок.
const (
	QueueCritical = "critical"
	QueueDefault  = "default"
	QueueBulk     = "bulk"
)

var QueueWeights = map[string]int{QueueCritical: 6, QueueDefault: 3, QueueBulk: 1}

func QueueFor(taskType string) string {
	switch taskType {
	case TypeDrawNotify, TypePrizeNotify, TypePrizeExpire:
		return QueueCritical
	case TypeBroadcastStart:
		return QueueBulk
	default:
		return QueueDefault
	}
}

type SendMessage struct {
	UserID int64             `json:"user_id"`
	Msg    messenger.Message `json:"msg"`
}

type DrawRef struct {
	DrawID int64 `json:"draw_id"`
}

type ClaimRef struct {
	DrawID int64 `json:"draw_id"`
	UserID int64 `json:"user_id"`
}

type BroadcastRef struct {
	BroadcastID int64 `json:"broadcast_id"`
}
