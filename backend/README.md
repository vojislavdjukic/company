# Backend

Go REST service organized into API, service, and PostgreSQL repository layers.

## Run

```sh
go run .
```

By default, the app connects to the local `thecompany` database as `company` (`company:company`). Set `DATABASE_URL` to override this connection. The server listens on `:4000` by default; set `PORT` to use another port.

## Endpoints

- `GET /` — service status
- `GET /healthz` — process liveness check
- `GET /readyz` — PostgreSQL readiness check
- `POST /codex/messages` — send a message to Codex and wait for its reply

The Codex endpoint accepts `{"message":"..."}` and returns the response and
conversation ID (`thread_id`). A thread is one conversation's stored history:
reuse its ID for each follow-up message so Codex can see earlier turns. Omit
the ID to start a separate conversation. The backend does not associate IDs
with users, so the calling app should store the ID alongside its own chat or
session record.

```sh
curl -X POST http://localhost:4000/codex/messages \
  -H 'Content-Type: application/json' \
  -d '{"message":"Summarize this project"}'
```

The backend starts one persistent `codex app-server` process on the first
request. Codex must be installed and authenticated for the backend's operating
system user. Set `CODEX_BIN` if the executable is not named `codex` or is not on
the backend's `PATH`. Requests are serialized through this process; each waits
up to two minutes for a completed reply.

## Layers

- `internal/api` handles HTTP routes, requests, and responses.
- `internal/service` holds application logic and depends on repository interfaces.
- `internal/repository` contains PostgreSQL connectivity and database access.

## Test

```sh
go test ./...
```

## Live reload

From the repository root, run Air:

```sh
air
```

Air builds the binary and temporary files under `backend/build/`.
