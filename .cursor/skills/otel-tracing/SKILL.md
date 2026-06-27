---
name: otel-tracing
description: >-
  Добавляет и поддерживает OpenTelemetry-трейсинг в deepseek-cursor-proxy:
  спаны через otel_ctx, echo-opentelemetry, bunotel, span.SetStatus(codes.Error)
  на листьях дерева спанов. Используй при добавлении otel, span, трейсинга,
  Jaeger, OTLP или отладке observability в этом репозитории.
---

# OpenTelemetry в deepseek-cursor-proxy

## Инфраструктура

| Компонент | Файл | Назначение |
|-----------|------|------------|
| Инициализация OTLP | `internal/trace/otel.go` | `InitOTel`, экспорт в Jaeger (gRPC `:4317`) |
| Tracer в context | `internal/otel_ctx/context.go` | `WithTracer`, `Tracer(ctx)` |
| HTTP middleware | `internal/server/server.go` | `echo_otel.NewMiddleware` + `injectTracerMiddleware` |
| SQL query hooks | `internal/store/store.go` | `bunotel.NewQueryHook` |
| Конфиг | `DEEPSEEK_OTEL_ENDPOINT`, `DEEPSEEK_OTEL_SERVICE_NAME` | env в `docker-compose.yml` |

Tracer попадает в `context.Context` через middleware; downstream-код **всегда** берёт его через `otel_ctx.Tracer(ctx)`, не через глобальный `otel.Tracer()`.

## Добавление спана в функцию

1. Принимай и пробрасывай `ctx context.Context` первым аргументом.
2. Создай спан в начале функции, закрой через `defer`:

```go
ctx, span := otel_ctx.Tracer(ctx).Start(ctx, "package.FunctionName")
defer span.End()
```

3. Именование: `package.method` — например `store.Put`, `readAndParseBody`, `transform.normalizeMessages`.
4. Передавай обновлённый `ctx` во все дочерние вызовы.

## События и атрибуты

- **События** (`span.AddEvent`) — для этапов внутри функции (декодирование body, stats стрима).
- **Атрибуты** (`span.SetAttributes`, `attribute.*`) — для итоговых метаданных спана.
- Импорты: `go.opentelemetry.io/otel/attribute`, `go.opentelemetry.io/otel/trace` (как `otel_trace` при `WithAttributes`).

## Размещение span.SetStatus(codes.Error)

`span.SetStatus(codes.Error)` ставь как можно глубже в дереве спанов — на листьях или ближайших к листьям узлах, чьи дочерние спаны уже не могут быть отмаркированы (например, вызов внешней библиотеки, которая не ставит статус на своих спанах).

### Правило

- **НЕ** ставь `span.SetStatus(codes.Error)` на корневом спане, если есть дочерний спан, который покрывает тот же участок кода.
- Ставь `span.SetStatus(codes.Error)` на своём спане функции, которая возвращает ошибку, **если у неё есть свой спан**.
- Если у функции нет своего спана — ошибка маркируется на родительском спане (это нормально).
- **Исключение**: паника, пойманная в defer хэндлера — ставится на корневом спане, так как она не принадлежит ни одному дочернему.

### Пример дерева (handleChatCompletions)

```
handleChatCompletions       ← НЕТ SetStatus на ошибках
├── readAndParseBody        ← ДА, свой спан
├── prepareUpstream         ← ДА, свой спан
├── buildUpstreamRequest    ← ДА, свой спан
├── doUpstream              ← ДА, свой спан
│   └── upstreamRoundTrip   ← ДА, circuit breaker
├── handleUpstreamError     ← ДА, свой спан
└── proxyResponse
    ├── proxyRegularResponse
    └── proxyStreamingResponse
```

### Шаблон обработки ошибки

```go
if err != nil {
    span.SetStatus(codes.Error, err.Error()) // или короткое описание
    return nil, err
}
```

Для паники в HTTP-хэндлере — `span.RecordError` + `span.SetStatus` на корневом спане (см. `handleChatCompletions`).

## Чеклист для нового кода

- [ ] `ctx` пробрасывается по цепочке вызовов
- [ ] Спан создан через `otel_ctx.Tracer(ctx).Start`
- [ ] `defer span.End()` сразу после Start
- [ ] Ошибки отмечены `SetStatus(codes.Error)` на **своём** спане, не на родительском
- [ ] Значимые этапы — через `AddEvent` / `SetAttributes`

## Чего не делать

- Не создавай отдельный TracerProvider в пакетах — используй tracer из ctx.
- Не дублируй `SetStatus` на родителе и ребёнке для одной и той же ошибки.
- Не оборачивай в спаны код, который уже инструментирован библиотекой (HTTP через echo_otel, SQL через bunotel).
