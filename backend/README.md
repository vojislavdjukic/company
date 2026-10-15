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
- `POST /codex/agents` — create a named agent, its Podman container, and an initial thread
- `POST /codex/messages` — send a message to Codex and wait for its reply
- `GET /codex/agents` — list the authenticated user's agent containers (admins see all)
- `DELETE /codex/agents/{agent_name}` — stop and permanently remove one of your agents

Registration and login accept `{"email":"person@example.com","password":"..."}`.
Passwords must contain at least 8 characters. Passwords are stored as bcrypt
hashes; successful registration and login return a seven-day HS256 JWT with
`sub` (user ID), `role`, `iat`, `exp`, and `iss` claims. Email is not included
in the token; `/auth/me` loads it from PostgreSQL. New users receive the
`user` role. Include the token as `Authorization: Bearer <token>` when calling
`/auth/me` or `/codex/*`. User records and roles are stored in
PostgreSQL. Codex agent names are scoped to the authenticated user. To promote
an account, update its `app_users.role` to `admin`; newly issued tokens will
then carry that role.

```sh
curl -X POST http://localhost:4000/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"person@example.com","password":"a-long-password"}'
```

Create an agent before sending messages when you want its name and container
up front. The response includes the generated three-letter name, Codex thread
ID, and Podman container name. The thread ID is managed by the server; use the
agent name in message requests.

```sh
curl -X POST http://localhost:4000/codex/agents \
  -H 'Authorization: Bearer <token>'
```

The Codex message endpoint accepts `{"message":"..."}` and returns the
response and a three-letter `agent_name`. Send that name with follow-up messages
to continue the same Codex thread. Omit it to create another agent and thread.
Every agent runs in its own Podman container. Names are stored in PostgreSQL in
a `codex_agent_threads` table and scoped to the authenticated account.

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

Delete an agent and its container, Codex login copy, thread history, and name
mapping with the same bearer token:

```sh
curl -X DELETE http://localhost:4000/codex/agents/kzr \
  -H 'Authorization: Bearer <token>'
```

The endpoint returns `204 No Content` after removal. An agent name belonging
to another account is treated as not found.

List app-managed Codex containers with:

```sh
curl http://localhost:4000/codex/agents \
  -H 'Authorization: Bearer <token>'
```

The response contains each visible container's ID, name, agent name, owner ID,
status, and creation time. Regular users only receive their own containers;
admins receive all app-managed Codex containers.

The backend starts one Podman container for each `(user_id, agent_name)` when
the agent is created or first receives a message. Each container runs `codex app-server` and exposes its
WebSocket only on the backend machine's loopback interface. Containers use
names beginning with `thecomp` (for example, `thecomp-u12-kzr`). Each agent has
its own persistent Codex home under `~/.local/share/thecomp/agents`; the local
login at `~/.codex/auth.json` is copied into that home the first time the agent
starts. The backend stops opened containers during shutdown while preserving
their homes and thread history for the next start.

Build the local image from the backend directory:

```sh
podman build -t localhost/thecomp-codex:latest -f codex-container/Containerfile .
```

The backend OS user must be logged in to Codex before starting the service.
Set `CODEX_PODMAN_IMAGE` to choose another built image, `PODMAN_BIN` to choose
the Podman executable, `CODEX_AGENT_DATA_DIR` to change persistent per-agent
storage, and `CODEX_AUTH_FILE` to choose the local login file. The container
image must provide the `codex` command. Requests to the same agent are
serialized; different agents use separate app-server processes. Each request
waits up to two minutes for a completed reply.

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
The live Codex flow needs the Podman image and a Codex login for the current OS user:

```sh
go test -v ./tests/integration
```

## Live reload

From the repository root, install the Vue dependencies once and run the
development launcher:

```sh
npm --prefix frontend install
./dev.sh
```

Vite serves the frontend at `http://127.0.0.1:5173` with hot module
replacement. It proxies API calls to the Go server on port 4000. The launcher
supervises both Vite and Air and cleans up both when stopped; Air builds the
backend under `backend/build/` and stops its Go child on exit.

Build static frontend assets before building the production backend:

```sh
npm --prefix frontend run build
cd backend && go build ./...
```

The Go binary embeds `backend/web/dist` and serves the frontend and API from
the same process.
