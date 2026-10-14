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
a three-letter `agent_name`. Send that name with follow-up messages to continue
the same Codex thread. Omit it to start a new thread. Names are stored in
PostgreSQL in a `codex_agent_threads` table and are global because this API does
not currently authenticate callers.

```sh
curl -X POST http://localhost:4000/codex/messages \
  -H 'Content-Type: application/json' \
  -d '{"message":"Summarize this project"}'
```

Example follow-up:

```sh
curl -X POST http://localhost:4000/codex/messages \
  -H 'Content-Type: application/json' \
  -d '{"message":"What are the main packages?","agent_name":"kzr"}'
```

The backend starts one persistent `codex app-server` process on the first
request and connects to it over WebSocket. Codex must be installed and
authenticated for the backend's operating system user. Set `CODEX_BIN` if the
executable is not named `codex` or is not on the backend's `PATH`. Set
`CODEX_APP_SERVER_LISTEN` to change the listener address; the default is
`ws://127.0.0.1:8765`. Keep it bound to localhost unless you configure Codex's
WebSocket authentication. Requests are serialized through this connection;
each waits up to two minutes for a completed reply. Codex currently describes
the app-server WebSocket transport as experimental/unsupported.

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
