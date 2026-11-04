# Lists

A small personal Shopping / Todo app built with Go, HTMX, and Turso. Everything is bundled into one executable—no frontend build step or separate asset files.

- Add, edit, complete, and delete items.
- Set todo due dates in your local time and see a live countdown.
- Get optional browser reminders, even when the app isn't open.

## Setup

Create a Turso database and token:

```sh
turso db create personal-lists --tursodb
turso db show personal-lists --url
turso db tokens create personal-lists
```

Set your database connection in the server environment:

```sh
TODO_DB_URL=libsql://your-db.turso.io?authToken=YOUR_URL_ENCODED_TOKEN
```

URL-encode any reserved characters in the token. Keep this value in a private environment file or secret manager, not in Git or shell history.

## Install and run

With the Go version specified in `go.mod` or newer:

```sh
go install github.com/IOU4/todo/cmd/todo@latest
todo
```

Make sure Go's binary directory is on your `PATH` and `TODO_DB_URL` is set. Open **http://127.0.0.1:8080**. Set `TODO_ADDR` to change the listen address.

Alternatively, download a Linux executable from GitHub Releases or build from source:

```sh
CGO_ENABLED=0 go build -o todo ./cmd/todo
./todo
```

The database schema is created and upgraded automatically, preserving existing items.

## Notifications (optional)

Incomplete todos get a reminder at **23:00 UTC on the day before their UTC due date**. For example, a todo due June 10 gets its reminder June 9 at 23:00 UTC. Todos added after that cutoff are skipped. Changing a due date schedules a reminder only if the new cutoff hasn't passed.

1. Generate keys once:

   ```sh
   todo -generate-vapid-keys
   ```

2. Save both printed settings in your server environment alongside `TODO_DB_URL`, then add a contact email:

   ```sh
   TODO_VAPID_SUBJECT=mailto:you@example.com
   ```

3. Restart the server. Open **Todo → Enable reminders** and allow notifications. Enable each browser/device separately.

Keep the private key secret and reuse the same keys across deployments. Notifications stay disabled if no VAPID settings are provided.

Notifications require **HTTPS** (or localhost) and a running server. On iPhone/iPad with iOS/iPadOS 16.4+, add the app to your Home Screen and open it there first. Browser and OS settings can affect delivery; reminder text may appear on the lock screen.

The server checks every 30 seconds. Delivery records survive restarts, and missed or failed sends are retried until the deadline. Completed, deleted, undated, and shopping items are excluded.

## Deployment

**There is no login system. Keep the app private or use an authenticated reverse proxy.** When proxying HTTPS, preserve `Host` and overwrite `X-Forwarded-Proto` with the original protocol.

Only the executable and environment settings are needed on the server:

- `scripts/deploy.sh` installs the latest tagged Linux release.
- `scripts/todo.service` is an optional user systemd service. It reads settings from `~/.config/todo/environment`; protect that file with `chmod 600`.
- Disable Cloudflare Web Analytics and Rocket Loader for this app; its security policy blocks injected scripts.

## Tests

```sh
go test ./...
go vet ./...
```

Some tests use the `sqlite3` CLI or Node.js and skip if those tools aren't installed. Neither is needed to build or run the app.
