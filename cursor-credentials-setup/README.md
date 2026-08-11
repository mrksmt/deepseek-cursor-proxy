# cursor-credentials-setup

Хардкорный способ прописать креды DeepSeek в Cursor напрямую.

## Зачем

Обычно ключ DeepSeek и base URL вводятся через UI Cursor (Settings → Model → OpenAI API Key / Base URL). Иногда это не срабатывает — форма не принимает ввод, ключ не сохраняется, или нужно массово переключать несколько инстансов Cursor. Эти скрипты пишут значения напрямую в SQLite-хранилище Cursor (`state.vscdb`), минуя интерфейс.

> ⚠️ Это хардкорный путь: мы лезем внутрь внутреннего хранилища Cursor. **Перед каждым запуском создаётся резервная копия `state.vscdb`** (файл `.vscdb.bak-<timestamp>` рядом), так что всегда можно откатиться.

## Файлы

| Скрипт | Что делает |
|---|---|
| `set-url.sh` | Ставит `openAIBaseUrl` (адрес прокси) и включает `useOpenAIKey` |
| `set-key.sh` | Выключает `useOpenAIKey` и очищает сохранённый зашифрованный ключ, чтобы форма Cursor снова приняла ввод |

## Как пользоваться

1. Отредактируй `set-url.sh` — впиши свой base URL в переменную `NEW_BASE_URL` (в файле помечено `<<< СВОЙ URL >>>`).
2. Выполни:

```bash
# сперва прописать URL прокси
./cursor-credentials-setup/set-url.sh

# потом очистить старый ключ, чтобы форма приняла новый
./cursor-credentials-setup/set-key.sh
```

3. Полностью перезапусти Cursor (не просто перезагрузи окно — именно перезапусти процесс).
4. В настройках Cursor (Settings → Model) введи ключ DeepSeek в поле OpenAI API Key.

## Типичный адрес прокси

Прокси из этого репозитория запускается локально и наружу выводится через ngrok. После старта адрес виден в логах контейнера:

```bash
docker logs deepseek-cursor-proxy-ngrok-go 2>&1 | grep 'api_base_url'
```

Пример: `https://bribe-wilt-straining.ngrok-free.dev/v1`

## Откат

При каждом запуске рядом с `state.vscdb` появляется резервная копия `state.vscdb.bak-<timestamp>`. Чтобы вернуть состояние до запуска скрипта:

```bash
mv "~/Library/Application Support/Cursor/User/globalStorage/state.vscdb.bak-<timestamp>" \
   "~/Library/Application Support/Cursor/User/globalStorage/state.vscdb"
```
