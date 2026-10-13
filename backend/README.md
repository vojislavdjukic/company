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
