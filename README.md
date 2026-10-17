# Lists

A small personal **Shopping / Todo** app: Go `net/http`, server-rendered `html/template`, HTMX fragments, plain CSS, and a remote Turso/libSQL database. Templates, CSS, and a pinned HTMX 2.0.4 release are embedded in the executable. No runtime asset directory or frontend build step is needed.

## Create a Turso database

Create a database and an authentication token with the Turso CLI:

```sh
turso db create personal-lists --tursodb
turso db show personal-lists --url
turso db tokens create personal-lists
```

Use the database URL and token in the `DATABASE_URL` value. The token is a query parameter; URL-encode reserved characters in it (`+` as `%2B`, `/` as `%2F`, `=` as `%3D`, `&` as `%26`, `?` as `%3F`, `#` as `%23`, and `%` as `%25`). For example:

```sh
DATABASE_URL='libsql://your-db.turso.io?authToken=YOUR_URL_ENCODED_TOKEN' ./todo-server
```

The remote client is `turso.tech/database/tursogo-serverless`, a pure-Go `database/sql` driver. The application decodes the URL and passes the token separately through the driver's documented `NewConnector(endpoint, token)` API. Do not commit the URL, print it in logs, or expose it in shell history or process diagnostics. Prefer a secret manager or a permissions-restricted environment file on a server. The executable does not log the URL or token.

`DATABASE_URL` is the only required setting. The optional `TODO_ADDR` controls the listen address and defaults to `127.0.0.1:8080`.

## Build and run

Requirements: the Go version declared in `go.mod` to build; no Node/npm, CGO, local SQLite library, or MySQL service is required.

```sh
go test ./...
go vet ./...
CGO_ENABLED=0 go build -o todo-server ./cmd/server

DATABASE_URL='libsql://your-db.turso.io?authToken=YOUR_URL_ENCODED_TOKEN' ./todo-server
```

The binary works from any current working directory. On Linux, tagged GitHub releases publish standalone `todo-server-linux-amd64` and `todo-server-linux-arm64` executables; there is no separate `web/` archive. `scripts/deploy.sh` installs the matching executable for the current user. An optional user systemd unit is in `scripts/todo.service`; it reads `DATABASE_URL` from `%h/.config/todo/environment`.

The app creates its SQLite-compatible schema idempotently at startup. Item text is trimmed at the ends and limited to **255 Unicode code points**. Both lists are ordered deterministically by item ID.

## Deployment and access

The server has no user authentication. Keep it private, bind it to loopback, or place it behind an authenticated reverse proxy. Do not expose the unauthenticated app directly to the public internet. When using a TLS-terminating reverse proxy, set the original `Host` and `X-Forwarded-Proto` headers (and overwrite any client-supplied forwarded-proto value) so same-origin mutation checks work correctly.

For a manual deployment, copy only the executable to the server and start it with `DATABASE_URL` set. No templates, static directory, local database file, or other runtime files need to be copied.
