# MikroTik Remote Hook

Удалённое управление правилами MikroTik через веб-панель.

## Как это работает

1. **Веб-панель** (Go, Docker) — управление параметрами через toggle-переключатели
2. **Скрипт RouterOS** — по таймеру читает состояние с сервера и включает/выключает правила по comment-тегам

Правила на MikroTik помечаются комментариями вида `hook:<param-name>`. Скрипт находит их и ставит `disabled=yes/no` в зависимости от состояния параметра на сервере.

## Запуск сервера

```bash
# Docker Compose
AUTH_TOKEN=your-secret-token docker compose up --build -d

# Пересборка после изменений
docker compose up --build

# Или напрямую (без Docker)
go build -o hook-server ./server/
AUTH_TOKEN=your-secret-token ./hook-server
```

Веб-панель доступна на `http://localhost:8080`.

### Переменные окружения

| Переменная   | Default   | Описание                           |
|-------------|-----------|-------------------------------------|
| `LISTEN_ADDR` | `:8080`  | Адрес и порт для прослушивания     |
| `DATA_DIR`    | `./data` | Директория для хранения состояния   |
| `AUTH_TOKEN`  | —        | Bearer token для защиты API         |

## Настройка MikroTik

### 1. Пометьте правила комментариями

У каждого правила в RouterOS есть поле `comment`. Впишите туда `hook:<имя>`, где `<имя>` — это имя параметра из веб-панели. Скрипт найдёт все правила с таким комментарием и будет включать/выключать их:

```
/ip/firewall/filter add chain=forward action=drop comment="hook:block-social" disabled=yes
/ip/kid-control add name=kids comment="hook:kid-control" disabled=yes
```

### 2. Скачайте и установите скрипт

Скрипт доступен для скачивания прямо с сервера (авторизация не требуется):

```
# Скачайте скрипт с сервера на роутер
/tool/fetch url="http://your-server:8080/mikrotik/remote-hook.rsc" dst-path=remote-hook.rsc

# Отредактируйте url и token в начале скрипта, затем создайте скрипт
/system/script add name=remote-hook source=[/file/get remote-hook.rsc contents]

# Создайте расписание (каждую минуту)
/system/scheduler add name=remote-hook interval=1m on-event="/system/script/run remote-hook"
```

> **Примечание:** используется синтаксис RouterOS 7. Для ROS 6 замените `/` между разделами на пробелы (например `/ip/firewall/filter` → `/ip firewall filter`).

### Поддерживаемые разделы

- `/ip/firewall/filter`
- `/ip/firewall/nat`
- `/ip/firewall/mangle`
- `/ip/kid-control`

Для добавления новых разделов — отредактируйте массив `sections` в скрипте.

## API

Запросы к `/api/*` требуют заголовок `Authorization: Bearer <token>` (если `AUTH_TOKEN` задан). Остальные URL (UI, скачивание скрипта) доступны без авторизации.

| Метод    | URL             | Auth | Описание                     |
|----------|-----------------|------|------------------------------|
| `GET`    | `/api/state`    | да   | Получить состояние параметров |
| `POST`   | `/api/state`    | да   | Изменить параметр (`{name, enabled}`) |
| `POST`   | `/api/params`   | да   | Добавить параметр (`{name, description}`) |
| `DELETE`  | `/api/params?name=xxx` | да | Удалить параметр      |
| `GET`    | `/mikrotik/remote-hook.rsc` | нет | Скачать скрипт для MikroTik |
| `GET`    | `/`             | нет  | Веб-панель управления        |
