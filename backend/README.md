# Backend

Go REST service organized into API, service, and PostgreSQL repository layers.

## Run

```sh
go run .
```

By default, the app connects to the local `thecompany` database as `company` (`company:company`). Set `DATABASE_URL` to override this connection. Set `JWT_SECRET` to a stable, random secret of at least 32 characters. The server listens on `:4000` by default; set `PORT` to use another port.

## Endpoints

- `GET /` — service status
- `GET /healthz` — process liveness check
- `GET /readyz` — PostgreSQL readiness check
- `POST /auth/register` — create an account and return a bearer token
- `POST /auth/login` — verify credentials and return a bearer token
- `GET /auth/me` — return the account associated with a bearer token
- `POST /codex/messages` — send a message to Codex and wait for its reply

Registration and login accept `{"email":"person@example.com","password":"..."}`.
Passwords must contain at least 8 characters. Passwords are stored as bcrypt
hashes; successful registration and login return a seven-day HS256 JWT with
`sub`, `email`, `role`, `iat`, `exp`, and `iss` claims. New users receive the
`user` role. Include the token as `Authorization: Bearer <token>` when calling
`/auth/me` or `/codex/messages`. User records and roles are stored in
PostgreSQL. Codex agent names are scoped to the authenticated user. To promote
an account, update its `app_users.role` to `admin`; newly issued tokens will
then carry that role.

```sh
curl -X POST http://localhost:4000/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"person@example.com","password":"a-long-password"}'
```

The Codex endpoint accepts `{"message":"..."}` and returns the response and
a three-letter `agent_name`. Send that name with follow-up messages to continue
the same Codex thread. Omit it to start a new thread. Names are stored in
PostgreSQL in a `codex_agent_threads` table and scoped to the authenticated
account.

```sh
curl -X POST http://localhost:4000/codex/messages \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <token>' \
  -d '{"message":"Summarize this project"}'
```

Example follow-up:

```sh
curl -X POST http://localhost:4000/codex/messages \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <token>' \
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
go test ./tests/unit
```

All tests are under `tests/`: unit tests are in `tests/unit`, and the live
registration/login/Codex flow is in `tests/integration`. The integration test
defaults to `postgres://company:company@localhost:5432/thecompany?sslmode=disable`
(or uses `DATABASE_URL` if set). It creates a fresh schema and drops it
afterward. Set `INTEGRATION_TEST_DATABASE_URL` to override the database URL.
Codex must be installed and authenticated for the current OS user:

```sh
go test -v ./tests/integration
```

## Live reload

From the repository root, run Air:

```sh
air
```

Air builds the binary and temporary files under `backend/build/`.
