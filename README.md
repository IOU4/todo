# Lists

A small personal **Shopping / Todo** app: Go `net/http`, server-rendered `html/template`, HTMX fragments, plain CSS, and a remote Turso/libSQL database. Templates, CSS, and a pinned HTMX 2.0.4 release are embedded in the executable. No runtime asset directory or frontend build step is needed.

## Create a Turso database

Create a database and an authentication token with the Turso CLI:

```sh
turso db create personal-lists --tursodb
turso db show personal-lists --url
turso db tokens create personal-lists
```

Set the database URL and token in the `TODO_DB_URL` environment variable. The token is a query parameter; URL-encode reserved characters in it (`+` as `%2B`, `/` as `%2F`, `=` as `%3D`, `&` as `%26`, `?` as `%3F`, `#` as `%23`, and `%` as `%25`). For example:

```sh
TODO_DB_URL='libsql://your-db.turso.io?authToken=YOUR_URL_ENCODED_TOKEN' ./todo
```

The remote client is `turso.tech/database/tursogo-serverless`, a pure-Go `database/sql` driver. The application decodes the URL and passes the token separately through the driver's documented `NewConnector(endpoint, token)` API. Do not commit the URL, print it in logs, or expose it in shell history or process diagnostics. Prefer a secret manager or a permissions-restricted environment file on a server. The executable does not log the URL or token.

`TODO_DB_URL` is the only required setting. The optional `TODO_ADDR` controls the listen address and defaults to `127.0.0.1:8080`.

## Build and run

Requirements: the Go version declared in `go.mod` to build; no Node/npm, CGO, local SQLite library, or MySQL service is required.

```sh
go test ./...
go vet ./...
CGO_ENABLED=0 go build -o todo ./cmd/todo

TODO_DB_URL='libsql://your-db.turso.io?authToken=YOUR_URL_ENCODED_TOKEN' ./todo
```

The binary works from any current working directory. Install the latest tagged version directly with Go (requires the Go version declared in `go.mod` or newer):

```sh
go install github.com/IOU4/todo/cmd/todo@latest
```

This installs `todo` into `$(go env GOBIN)` or, if unset, `$(go env GOPATH)/bin`; ensure that directory is on your `PATH`.
Then run it with `TODO_DB_URL` set as above. On Linux, tagged GitHub releases also publish standalone `todo-linux-amd64` and `todo-linux-arm64` executables.
`scripts/deploy.sh` installs the matching executable for the current user.
An optional user systemd unit is in `scripts/todo.service`; it reads `TODO_DB_URL` from `%h/.config/todo/environment`. Put a line like `TODO_DB_URL=libsql://your-db.turso.io?authToken=YOUR_URL_ENCODED_TOKEN` in that file, and restrict its permissions (for example, `chmod 600 ~/.config/todo/environment`).

The app creates and upgrades its SQLite-compatible schema idempotently at startup, preserving existing items. Item text is trimmed at the ends and limited to **255 Unicode code points**. Both lists are ordered deterministically by item ID.

Todo items have an optional **due date and time**, which can be added, edited, or cleared. The picker uses your browser's local time; dates are stored in UTC. Incomplete items show a live countdown such as `5d 2h 38m`, or **Overdue** once the deadline passes. Hover over the countdown to see the exact due date. Without JavaScript, the picker uses UTC and the countdown updates when the page is reloaded. Shopping items are unchanged.

## Deployment and access

The server has no user authentication. Keep it private, bind it to loopback, or place it behind an authenticated reverse proxy. Do not expose the unauthenticated app directly to the public internet. When using a TLS-terminating reverse proxy, set the original `Host` and `X-Forwarded-Proto` headers (and overwrite any client-supplied forwarded-proto value) so same-origin mutation checks work correctly.

For a manual deployment, copy only the `todo` executable to the server and start it with `TODO_DB_URL` set. No templates, static directory, local database file, or other runtime files need to be copied. Static asset URLs include a content hash, so new deployments don't reuse stale browser or CDN JavaScript/CSS. The deployment script downloads the latest **tagged release**, not the latest commit on the default branch.

Cloudflare's injected analytics scripts are not needed by the app and are blocked by its Content Security Policy. Disable Web Analytics and, if enabled, Rocket Loader for this hostname rather than enabling unsafe inline scripts or styles.
