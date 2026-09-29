# deepseek-cursor-proxy — Go-порт

Прокси-сервер, совместимый с OpenAI API, который решает проблемы интеграции DeepSeek V4 reasoning-моделей с Cursor IDE.

## Мотивация

Этот репозиторий — **полный порт оригинального Python-сервиса** [yxlao/deepseek-cursor-proxy](https://github.com/yxlao/deepseek-cursor-proxy) на Go.

В основе лежит та же идея: DeepSeek V4 в режиме thinking требует, чтобы `reasoning_content` передавался обратно в API при каждом tool-call. Cursor этого не делает, из-за чего запросы падают с ошибкой. Прокси кэширует `reasoning_content` и автоматически подставляет его в запросы к DeepSeek.

### Что исправлено по сравнению с Python-версией

Оригинальный Python-сервис [yxlao/deepseek-cursor-proxy](https://github.com/yxlao/deepseek-cursor-proxy) решал правильную задачу, но имел архитектурные ограничения:

- **Конкурентность.** Python (GIL) + однопоточный `asyncio` — когда несколько агентов Cursor одновременно шлют запросы, они обрабатываются последовательно. В Go-версии каждый запрос обрабатывается в отдельной горутине, состояние защищено `sync.RWMutex`, установлен лимит на concurrent requests.

- **Инфраструктура.** Оригинал требует установки Python-зависимостей через pip и запуска ngrok отдельным процессом. Go-версия собирается в один бинарник без зависимостей и запускается через `docker compose` — ngrok встроен в entrypoint.

- **Наблюдаемость.** В Python-версии не было инструментов для диагностики. Go-версия отправляет полный OpenTelemetry-трейсинг в Jaeger (OTLP gRPC) — каждый этап обработки запроса виден на дашборде.

- **Скорость.** Натуральный код на Go быстрее Python на задачах сериализации JSON и SQLite-операциях, которые составляют горячий путь каждого запроса.

- **Безопасность.** В оригинальном репозитории ngrok authtoken был захардкожен в конфиге и попал в git. В Go-версии токен передаётся только через переменную окружения.

### Что даёт Go-версия

| Характеристика | Python | Go |
|---|---|---|
| Язык | Python 3.12 | Go 1.26 |
| Сборка | pip install | `go build` — один бинарник |
| Запуск | Python + ngrok отдельно | Docker / docker compose |
| Конкурентность | GIL + asyncio | Нативные горутины |
| Reasoning cache | SQLite (dict fallback) | SQLite через bun/ORM |
| Трейсинг | — | OpenTelemetry (Jaeger) |
| ngrok | Отдельный процесс | Встроен в entrypoint |

### OpenTelemetry-трейсинг

Go-версия отправляет полный трейсинг в Jaeger (OTLP gRPC):

- Все HTTP-запросы к прокси с длительностью каждого этапа
- Операции с reasoning cache (lookup, put, batch insert — видно, не тормозит ли SQLite)
- Upstream round-trip (сколько времени ждёт DeepSeek API)
- Recovery-ситуации (когда cached reasoning недоступен и прокси восстанавливает историю)

Это позволяет находить узкие места: например, выяснить, что recovery упирается в несколько последовательных lookup'ов в SQLite, или что upstream отвечает медленно на определённых моделях.

Всё это доступно в локальном Jaeger UI (`http://localhost:16686`).

## Быстрый старт

### Требования

- [Docker](https://docker.com) + Docker Compose
- [ngrok](https://ngrok.com) аккаунт и authtoken

### Запуск

```bash
# 1. Клонировать репозиторий
git clone git@github.com:mrksmt/deepseek-cursor-proxy.git
cd deepseek-cursor-proxy

# 2. Создать .env с ngrok токеном
echo "NGROK_AUTHTOKEN=ваш_токен" > .env

# 3. Запустить
docker compose up --build -d

# 4. Проверить
curl http://127.0.0.1:9000/healthz
curl https://f7pdmcx6-9000.euw.devtunnels.ms/health
# → {"ok":true}
```

После запуска прокси будет доступен через ngrok. URL будет в логах:

```bash
docker logs deepseek-cursor-proxy | grep "ngrok tunnel"
# → ngrok tunnel established: https://что-то.ngrok-free.dev
```

### Настройка Cursor

Cursor **не подключается к локальным адресам** (`127.0.0.1`, `localhost`), поэтому ngrok обязателен.

В настройках Cursor (Settings → Models → OpenAI API Key) укажите:

- **Override OpenAI Base URL:** `https://что-то.ngrok-free.dev/v1` (из логов контейнера)
- **API Key:** любой (прокси пробрасывает Authorization в DeepSeek)

### OpenTelemetry + Jaeger

Если у вас уже запущен Jaeger на `localhost:4317` (OTLP gRPC), трейсинг включится автоматически.

Запустить Jaeger через Docker:

```bash
docker run -d --name jaeger \
  -p 16686:16686 \
  -p 4317:4317 \
  jaegertracing/all-in-one:latest
```

Откройте [http://localhost:16686](http://localhost:16686), выберите сервис `deepseek-cursor-proxy-go`.

## Конфигурация

Прокси настраивается через переменные окружения (префикс `DEEPSEEK_`) или YAML-файл.

Основные параметры:

| Переменная | По умолчанию | Описание |
|---|---|---|
| `DEEPSEEK_HOST` | `127.0.0.1` | Адрес для привязки |
| `DEEPSEEK_PORT` | `9000` | Порт |
| `DEEPSEEK_MODEL` | `deepseek-flash` | Модель по умолчанию (V4.1-Flash) |
| `DEEPSEEK_BASE_URL` | `https://api.deepseek.com` | Адрес DeepSeek API |
| `DEEPSEEK_NGROK` | `true` | Включить ngrok-туннель |
| `DEEPSEEK_REASONING_EFFORT` | `medium` | Уровень reasoning: low, medium, high, max |
| `DEEPSEEK_DISPLAY_REASONING` | `true` | Показывать reasoning в ответе |
| `DEEPSEEK_COLLAPSIBLE_REASONING` | `true` | Сворачивать reasoning в `<details>` |
| `DEEPSEEK_REQUEST_TIMEOUT` | `300` | Таймаут запроса к DeepSeek (сек) |
| `DEEPSEEK_OTEL_ENDPOINT` | `host.docker.internal:14317` | OTLP gRPC эндпоинт Jaeger (локальный коллектор слушает 14317 → 4317) |
| `DEEPSEEK_MAX_MESSAGES` | `0` (выключено) | Обрезка истории: держать не больше N сообщений |
| `DEEPSEEK_MAX_PROMPT_TOKENS` | `0` (выключено) | Грубый потолок оценочного размера промпта |
| `NGROK_AUTHTOKEN` | — | Токен ngrok (обязателен) |

Полный пример в [`config.example.yaml`](config.example.yaml).

## Разработка

```bash
# Сборка без Docker
CGO_ENABLED=0 go build -o deepseek-cursor-proxy ./cmd/deepseek-cursor-proxy

# Быстрая проверка сборки
go build ./...
go vet ./...
```

## Управление большим контекстом (context window management)

При длительной работе агента Cursor (множество исправлений, десятки раундов tool-call'ов) история сообщений разрастается. Упирается это не в DeepSeek (у `deepseek-flash` окно ~1M токенов), а в **сам Cursor**: его окно (200k–300k) заполняется накопленной историей задолго до лимита DeepSeek, после чего Cursor молча включает само-суммаризацию и агент «забывает» контекст.

Реализована **proactive truncation** — обрезка истории в прокси до отправки наверх:

- **`max_messages`** — основной предохранитель. Когда число сообщений превышает лимит, самые старые раунды отбрасываются. **По умолчанию 200.**
- **`max_prompt_tokens`** — грубый вторичный потолок по оценочному размеру промпта (~4 символа на токен). По умолчанию выключен (`0`).

### Переопределение лимита именем модели

Cursor не даёт других каналов настройки на чат, поэтому `max_messages` можно задать **именем модели**. Суффиксы именованные и **порядок не важен** — по той же схеме, что использует OpenRouter (`model:nitro:exacto`):

```
deepseek-flash:mm200                    # лимит истории
deepseek-flash:low:mm120                # effort + лимит
deepseek-flash:low:nothink:mm300        # effort + thinking off + лимит
deepseek-flash:mm300:nothink:low        # то же самое, порядок любой
```

| суффикс | смысл |
|---|---|
| `low` / `high` / `max` | reasoning effort |
| `nothink` | thinking выключен |
| `mmN` (`mm120`, `mm200`, `mm0`) | лимит истории для запроса |

Голое число (`:120`) капом **не считается** — слишком двусмысленно, такое имя уходит наверх как есть. `mm0` — валидное переопределение «без лимита».

Имя разбирается только если **все** суффиксы распознаны; иначе оно отправляется наверх без изменений. Это защищает реальные имена моделей с двоеточиями от молчаливого переписывания.

Правила обрезки:

- **system-сообщения сохраняются всегда** — они несут идентичность агента и контракты инструментов.
- **Режем только по границам раундов.** Раунд начинается с `user`-сообщения, поэтому `assistant`/`tool`-ответы никогда не осиротеют (осиротевший `tool` без предшествующего `assistant tool_call` отвергается апстримом). Следствие: `max_messages` — **мягкий** лимит, фактическое число может оказаться ниже; недоопускание безвредно, разрыв раунда — нет.
- **Последний раунд всегда сохраняется** — даже если он один превышает лимит, запрос пройдёт как есть, а не станет пустым.

Оба параметра принимают `0` = выключено. Результат обрезки виден в Jaeger в событии `prepare.result` (`truncated_count`, `truncated_estimated_tokens`, `messages.upstream_count`), а действующий лимит — в атрибуте `truncate.max_messages`.

Оставшиеся идеи (не реализованы):

- **Aggressive cache pruning** — более интеллектуальная чистка reasoning cache для длинных сессий (старые записи вряд ли пригодятся для поиска).

Pull requests приветствуются.

---

*Основано на [yxlao/deepseek-cursor-proxy](https://github.com/yxlao/deepseek-cursor-proxy) — спасибо автору за оригинальную идею.*
