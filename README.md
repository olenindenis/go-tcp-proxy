# tcp-proxy

Універсальний L4 TCP proxy на Go 1.27. Прозоро пересилає байти між клієнтом і upstream
(MySQL, PostgreSQL, Redis, будь-який TCP-сервіс) без розбору протоколу, тому працює й з TLS end-to-end.
Один процес обслуговує кілька маршрутів (`route` = listen-адреса → upstream).

## Можливості

    - **Обмеження з'єднань** - `max_connections` на маршрут і `max_connections_per_ip`. Понад ліміт з'єднання
  одразу скидається через RST (без черги в backlog ядра).
- **Таймаути** - `dial` (підключення до upstream), `idle`, `max_session`, `half_close`, TCP keepalive.
- **Захист від завислих сесій**
  - `idle` рахується на сесію: сесія закривається лише коли трафіку немає в *обох* напрямках
    (довгий запит, поки клієнт мовчить, не обривається);
  - write deadline - захист від пира, що перестав читати;
  - `half_close` - обмежує, скільки другий напрям може жити після EOF першого;
  - TCP keepalive - виявлення мертвих пирів без FIN;
  - `max_session` - абсолютний ліміт життя сесії.
- **Graceful shutdown** - на SIGINT/SIGTERM `/readyz` віддає 503, listeners закриваються, активні сесії
  доробляють до `shutdown_timeout`, після чого закриваються примусово. Повторний сигнал - негайний вихід.
- **Метрики Prometheus** на `/metrics`, а також `/healthz` і `/readyz`.

## Структура

```
cmd/tcp-proxy/         точка входу
internal/config/       YAML + env override, валідація
internal/proxy/        server (route), manager, limiter, session
internal/metrics/      Prometheus collectors
internal/admin/        HTTP: /metrics, /healthz, /readyz
configs/               приклад конфігурації
build/package/         Dockerfile
deployments/           docker-compose (proxy + MySQL + Redis)
test/e2e/              e2e-тести проти реальних сервісів
```

## Запуск

```sh
make build
./bin/tcp-proxy -config configs/proxy.yaml
# або один маршрут без файлу:
./bin/tcp-proxy -listen :3307 -upstream db.internal:3306
```

Демо-стек: `make up` → MySQL на `:3307`, Redis на `:6380`, admin на `:9090`; `make down` для зупинки.

## Конфігурація

Див. [configs/proxy.yaml](configs/proxy.yaml). Секція `defaults` успадковується всіма маршрутами;
будь-яке поле можна перевизначити в маршруті.

Змінні середовища (мають пріоритет над файлом):

| Змінна | Що перевизначає |
|---|---|
| `PROXY_CONFIG` | шлях до YAML (якщо не задано `-config`) |
| `PROXY_ADMIN_LISTEN`, `PROXY_LOG_LEVEL`, `PROXY_SHUTDOWN_TIMEOUT` | глобальні параметри |
| `PROXY_DEFAULTS_LIMITS_MAX_CONNECTIONS`, `..._MAX_CONNECTIONS_PER_IP` | ліміти за замовчуванням |
| `PROXY_DEFAULTS_TIMEOUTS_{DIAL,IDLE,MAX_SESSION,HALF_CLOSE}` | таймаути за замовчуванням |
| `PROXY_DEFAULTS_TCP_{KEEPALIVE,NODELAY}` | TCP-опції за замовчуванням |
| `PROXY_ROUTE_<NAME>_{LISTEN,UPSTREAM}` | адреси маршруту (`my-db` → `MY_DB`) |

> Для баз даних з пулами з'єднань виставляйте `idle` не меншим за idle-час пулу / `wait_timeout` сервера,
> інакше proxy закриватиме з'єднання, які пул вважає живими.

## Метрики

Префікс `tcp_proxy_`, у всіх є label `route`.

| Метрика | Тип | Додаткові labels |
|---|---|---|
| `connections_active` | gauge | |
| `connections_max` | gauge | |
| `connections_accepted_total` | counter | |
| `connections_rejected_total` | counter | `reason`: `global`, `per_ip`, `shutting_down` |
| `upstream_dial_errors_total` | counter | |
| `upstream_dial_duration_seconds` | histogram | |
| `sessions_closed_total` | counter | `reason`: `eof`, `idle_timeout`, `half_close_timeout`, `max_lifetime`, `shutdown`, `client_error`, `upstream_error`, `dial_error` |
| `session_duration_seconds` | histogram | |
| `bytes_transferred_total` | counter | `direction`: `client_to_upstream`, `upstream_to_client` |

## Тести

```sh
make test       # unit + інтеграційні (go test -race ./...)
make up && make test-e2e && make down
make lint       # golangci-lint (версія закріплена в Makefile, бінарник ставиться в ./bin)
make lint-fix   # автовиправлення
```

Конфігурація лінтерів - [.golangci.yml](.golangci.yml).

## Обмеження

- Upstream бачить IP proxy, а не клієнта (PROXY protocol не підтримується).
- Один upstream на маршрут, без балансування та health-check upstream.
