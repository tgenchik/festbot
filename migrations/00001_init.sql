-- +goose Up

-- Пользователи бота. id = user_id в MAX.
CREATE TABLE users (
    id            BIGINT PRIMARY KEY,
    first_name    TEXT        NOT NULL DEFAULT '',
    last_name     TEXT        NOT NULL DEFAULT '',
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    unreachable   BOOLEAN     NOT NULL DEFAULT false -- заблокировал бота, рассылки не шлём
);
CREATE INDEX users_started_at_idx ON users (started_at);

-- Партнеры и их каналы. channel_id нужен для проверки подписки:
-- бот должен быть администратором этого канала.
CREATE TABLE partners (
    id                BIGSERIAL PRIMARY KEY,
    name              TEXT   NOT NULL,
    channel_id        BIGINT NOT NULL,
    channel_url       TEXT   NOT NULL,
    promo_where       TEXT   NOT NULL DEFAULT '', -- «на сайте / в приложении / в точках продаж»
    promo_valid_until DATE
);

-- Расписание акции: какой партнер «на сцене» в какой день.
CREATE TABLE campaign_days (
    day               DATE PRIMARY KEY,
    partner_id        BIGINT NOT NULL UNIQUE REFERENCES partners (id),
    winners_count     INT    NOT NULL DEFAULT 1 CHECK (winners_count > 0),
    prize_title       TEXT   NOT NULL,
    prize_instruction TEXT   NOT NULL DEFAULT ''
);

-- Пул промокодов. Код свободен, пока user_id IS NULL.
CREATE TABLE promo_codes (
    id         BIGSERIAL PRIMARY KEY,
    partner_id BIGINT NOT NULL REFERENCES partners (id),
    code       TEXT   NOT NULL,
    user_id    BIGINT REFERENCES users (id),
    issued_at  TIMESTAMPTZ,
    UNIQUE (partner_id, code),
    UNIQUE (partner_id, user_id) -- один код на пользователя у партнера
);
CREATE INDEX promo_codes_free_idx ON promo_codes (partner_id, id) WHERE user_id IS NULL;

-- Заявки на ежедневный розыгрыш. PK = одна заявка на пользователя в день.
CREATE TABLE daily_entries (
    user_id       BIGINT      NOT NULL REFERENCES users (id),
    day           DATE        NOT NULL REFERENCES campaign_days (day),
    registered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day)
);
CREATE INDEX daily_entries_day_idx ON daily_entries (day);

-- Активные дни = шансы в финале. Промокод и заявка в один день дают одну строку.
CREATE TABLE active_days (
    user_id    BIGINT      NOT NULL REFERENCES users (id),
    day        DATE        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, day)
);

-- События для статистики (запросы промокода и т. п.).
CREATE TABLE events (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT,
    type       TEXT        NOT NULL,
    day        DATE        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_type_day_idx ON events (type, day);

-- Розыгрыши. seed + снимок участников позволяют воспроизвести результат.
CREATE TABLE draws (
    id                 BIGSERIAL PRIMARY KEY,
    kind               TEXT  NOT NULL CHECK (kind IN ('daily', 'final')),
    day                DATE REFERENCES campaign_days (day),
    status             TEXT  NOT NULL DEFAULT 'draft'
                       CHECK (status IN ('draft', 'confirmed', 'notified', 'cancelled')),
    seed               BYTEA NOT NULL,
    participants_count INT   NOT NULL,
    tickets_count      INT   NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at       TIMESTAMPTZ,
    confirmed_by       TEXT,
    notified_at        TIMESTAMPTZ,
    CHECK ((kind = 'daily') = (day IS NOT NULL))
);
-- Не больше одного действующего розыгрыша на день и одного финала.
CREATE UNIQUE INDEX draws_one_daily ON draws (day) WHERE kind = 'daily' AND status <> 'cancelled';
CREATE UNIQUE INDEX draws_one_final ON draws (kind) WHERE kind = 'final' AND status <> 'cancelled';

CREATE TABLE draw_participants (
    draw_id BIGINT NOT NULL REFERENCES draws (id),
    user_id BIGINT NOT NULL,
    tickets INT    NOT NULL,
    PRIMARY KEY (draw_id, user_id)
);

CREATE TABLE draw_winners (
    draw_id     BIGINT  NOT NULL REFERENCES draws (id),
    user_id     BIGINT  NOT NULL,
    position    INT     NOT NULL,
    is_reserve  BOOLEAN NOT NULL DEFAULT false,
    prize_title TEXT    NOT NULL,
    promoted_at TIMESTAMPTZ, -- когда резервист получил приз выбывшего
    PRIMARY KEY (draw_id, user_id),
    UNIQUE (draw_id, position)
);

-- Получение приза победителем: анкета и согласие.
CREATE TABLE prize_claims (
    draw_id     BIGINT      NOT NULL REFERENCES draws (id),
    user_id     BIGINT      NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending', 'awaiting_name', 'awaiting_consent',
                                  'claimed', 'declined', 'expired')),
    full_name   TEXT,
    consent_at  TIMESTAMPTZ,
    deadline_at TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (draw_id, user_id)
);
CREATE INDEX prize_claims_user_idx ON prize_claims (user_id, status);

-- Рассылки.
CREATE TABLE broadcasts (
    id         BIGSERIAL PRIMARY KEY,
    message    JSONB       NOT NULL, -- messenger.Message
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    queued     INT         NOT NULL DEFAULT 0
);

-- Transactional outbox: задачи, которые надо поставить в Redis-очередь.
-- Пишутся в той же транзакции, что и бизнес-данные.
CREATE TABLE outbox (
    id            BIGSERIAL PRIMARY KEY,
    task_type     TEXT        NOT NULL,
    payload       JSONB       NOT NULL,
    process_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    dispatched_at TIMESTAMPTZ
);
CREATE INDEX outbox_pending_idx ON outbox (id) WHERE dispatched_at IS NULL;

-- +goose Down
DROP TABLE outbox, broadcasts, prize_claims, draw_winners, draw_participants, draws,
    events, active_days, daily_entries, promo_codes, campaign_days, partners, users;
