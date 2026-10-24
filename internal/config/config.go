// Package config читает настройки сервиса из переменных окружения.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
	_ "time/tzdata" // база часовых поясов внутри бинарника (для минимальных образов)
)

type Config struct {
	// DevMode — разработка без MAX: вместо мессенджера чат в браузере на BOT_ADDR,
	// подписка на каналы переключается галочкой. Токен бота не нужен.
	DevMode bool

	BotToken      string
	WebhookURL    string // пусто — бот работает через long polling (удобно локально)
	WebhookSecret string
	BotAddr       string

	AdminAddr     string
	AdminUser     string
	AdminPassword string

	DatabaseURL   string
	RedisAddr     string
	RedisPassword string

	Location *time.Location // часовой пояс, по которому считается «день» акции

	MaxChances          int           // потолок шансов в финале (по ТЗ 10)
	FinalWinners        int           // основные победители финала
	FinalReserves       int           // резервные победители финала
	FinalPrize          string        // название главного приза
	ClaimTTL            time.Duration // срок, за который победитель должен оставить данные
	DailyDrawCron       string        // когда проводить розыгрыш за прошедший день
	RecheckSubscription bool          // перепроверять подписку участников перед розыгрышем
	SendRatePerSec      int           // глобальный лимит исходящих сообщений

	WelcomeImageURL string
	RulesURL        string
}

func Load() (Config, error) {
	loc, err := time.LoadLocation(env("TZ_CAMPAIGN", "Europe/Moscow"))
	if err != nil {
		return Config{}, fmt.Errorf("TZ_CAMPAIGN: %w", err)
	}
	ttl, err := time.ParseDuration(env("CLAIM_TTL", "72h"))
	if err != nil {
		return Config{}, fmt.Errorf("CLAIM_TTL: %w", err)
	}

	c := Config{
		DevMode: env("DEV_MODE", "false") == "true",

		BotToken:      os.Getenv("BOT_TOKEN"),
		WebhookURL:    os.Getenv("WEBHOOK_URL"),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
		BotAddr:       env("BOT_ADDR", ":8080"),

		AdminAddr:     env("ADMIN_ADDR", ":8081"),
		AdminUser:     env("ADMIN_USER", "admin"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),

		DatabaseURL:   env("DATABASE_URL", "postgres://festbot:festbot@localhost:5432/festbot?sslmode=disable"),
		RedisAddr:     env("REDIS_ADDR", "localhost:6379"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),

		Location: loc,

		MaxChances:          envInt("MAX_CHANCES", 10),
		FinalWinners:        envInt("FINAL_WINNERS", 5),
		FinalReserves:       envInt("FINAL_RESERVES", 5),
		FinalPrize:          env("FINAL_PRIZE", "2 билета на главный концерт"),
		ClaimTTL:            ttl,
		DailyDrawCron:       env("DAILY_DRAW_CRON", "10 0 * * *"), // в часовом поясе кампании
		RecheckSubscription: env("RECHECK_SUBSCRIPTION", "true") == "true",
		SendRatePerSec:      envInt("SEND_RATE_PER_SEC", 25),

		WelcomeImageURL: os.Getenv("WELCOME_IMAGE_URL"),
		RulesURL:        os.Getenv("RULES_URL"),
	}
	return c, nil
}

// RequireBot проверяет настройки, без которых не стартуют бот и воркер.
func (c Config) RequireBot() error {
	if c.BotToken == "" && !c.DevMode {
		return fmt.Errorf("BOT_TOKEN is required (или DEV_MODE=true для локальной разработки)")
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
