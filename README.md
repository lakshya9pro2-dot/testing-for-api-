# Watch Progress API — Go + Supabase + Render

Production-oriented watch-progress backend using Go `net/http`, PostgreSQL on Supabase, and Render native Go hosting.

## Architecture

`HTML tester / Android / TV app -> Render Go API -> Supabase PostgreSQL`

The client never controls `watched_id`. The server derives it from `media_type` and `video_id`:

- `movies` + `550` -> `m550`
- `tv` + `330` -> `tv330`

## 1. Supabase

### Create or use a project

You can use an existing Supabase project or create a new one. Open the SQL Editor and run `schema.sql`.

The schema enables RLS on the table because it is in the public schema. The Go API connects with the PostgreSQL connection string, so the public browser never receives a Supabase secret.

### Connection string

Open Supabase Dashboard -> Connect and copy a PostgreSQL connection string.

For a persistent Render service:

1. Prefer the **direct connection** on port `5432` if the Render network can reach your Supabase database over IPv6.
2. If you need IPv4, use Supavisor **session mode** on port `5432`.
3. Do not use the Supavisor **transaction-mode** connection on port `6543` for this long-running service unless you deliberately configure your driver for transaction pooling. Transaction pooling is aimed at short-lived/serverless traffic and does not support prepared statements.

The app also keeps its own small `database/sql` pool (`5` max open, `2` idle), which is appropriate for a small Render service.

### Important: existing table name

If your Supabase database already has a different `public.watch_progress` table, do not blindly overwrite it. In the supplied setup, the existing table was preserved as `watch_progress_legacy` before the requested schema was installed. Review that legacy table before deleting it.

## 2. Local project

Install Go 1.25+ and Git. Then create the project:

```bash
mkdir watch-progress-api
cd watch-progress-api
git init
git branch -M main
```

Copy these files into the directory:

- `main.go`
- `go.mod`
- `schema.sql`
- `.env.example`
- `Dockerfile`
- `render.yaml`
- `index.html`
- `README.md`

Download dependencies and verify the build:

```bash
go mod tidy
go build ./...
```

For local testing, create `.env` from `.env.example` and set your real `DATABASE_URL`. Do not commit `.env`.

A simple local run is:

```bash
export DATABASE_URL='postgresql://...'
export PORT=10000
go run .
```

Then open `http://localhost:10000/`.

## 3. GitHub

Create an empty GitHub repository, then run:

```bash
git add main.go go.mod go.sum schema.sql .env.example Dockerfile render.yaml index.html README.md
git commit -m "Build watch progress API"
git remote add origin https://github.com/YOUR_USERNAME/watch-progress-api.git
git push -u origin main
```

Never commit a real `.env` or a real Supabase database password.

## 4. Render auto-deploy

The repository contains `render.yaml`. It defines:

- Go native runtime
- free web service
- Singapore region
- `main` branch
- automatic deploy on commit
- `go build ...` build command
- `./watch-progress-api` start command
- `/health` health check
- `DATABASE_URL` as a secret/sync-false environment variable
- `PORT=10000`

Render's native Go runtime is the preferred deployment method here. It avoids maintaining a container image and lets Render provide the Go build environment. The Dockerfile is retained as an optional fallback for environments where you explicitly choose Docker.

### Connect the Blueprint

In Render:

1. Open the Render dashboard.
2. Choose **New -> Blueprint**.
3. Select the GitHub repository containing this `render.yaml`.
4. Select the `main` branch.
5. Review the Blueprint before applying it.
6. Render creates the web service from `render.yaml`.
7. Because `DATABASE_URL` uses `sync: false`, provide the real Supabase connection string when Render asks for the secret. It is not stored in Git.
8. Apply the Blueprint.

After that, a push to `main` should automatically start a new Render deploy.

### What to verify in Render

Open the web service and check:

- Runtime: Go
- Branch: `main`
- Auto Deploy: enabled
- Build command: `go build -trimpath -ldflags='-s -w' -o watch-progress-api .`
- Start command: `./watch-progress-api`
- Health check path: `/health`
- `DATABASE_URL` exists as a secret environment variable
- Deploy history shows a successful build and running instance

Your public URL will look like:

```text
https://watch-progress-api.onrender.com
```

Use the exact URL Render gives your service.

## 5. Frontend tester

The Go service serves `index.html` at `/`, so the easiest option is simply:

```text
https://YOUR-SERVICE.onrender.com/
```

You can also open `index.html` directly from disk. Enter the live Render URL in **Backend Base URL**. CORS is enabled for testing, including local-file usage.

For production, replace `Access-Control-Allow-Origin: *` with your real frontend origin and add authentication/rate limiting before exposing user progress data publicly.

## 6. API

### Health

```http
GET /health
```

Response:

```json
{"status":"ok"}
```

### Save progress

```http
POST /api/progress
Content-Type: application/json
```

Example:

```json
{
  "user_id": "user123",
  "video_id": 550,
  "media_type": "movies",
  "total_time_ms": 7200000,
  "playback_time_ms": 123000,
  "season_number": 0,
  "episode_number": 0,
  "device_host": "android-tv",
  "watched_id": "anything"
}
```

The server ignores the supplied `watched_id` and returns `m550`.

For TV `video_id=330`, the returned ID is `tv330`.

### Load progress

```http
GET /api/progress/user123/m550
```

or:

```http
GET /api/progress/user123/tv330
```

## 7. curl tests against Render

Set your deployed URL:

```bash
export API='https://YOUR-SERVICE.onrender.com'
```

Health check:

```bash
curl -i "$API/health"
```

Save a movie:

```bash
curl -i -X POST "$API/api/progress" \
  -H 'Content-Type: application/json' \
  -d '{
    "user_id":"demo-user",
    "video_id":550,
    "media_type":"movies",
    "total_time_ms":7200000,
    "playback_time_ms":123000,
    "season_number":0,
    "episode_number":0,
    "device_host":"android-tv",
    "watched_id":"client-value-is-ignored"
  }'
```

Expected important field:

```json
"watched_id": "m550"
```

Load it:

```bash
curl -i "$API/api/progress/demo-user/m550"
```

Save a TV item:

```bash
curl -i -X POST "$API/api/progress" \
  -H 'Content-Type: application/json' \
  -d '{
    "user_id":"demo-user",
    "video_id":330,
    "media_type":"tv",
    "total_time_ms":1800000,
    "playback_time_ms":420000,
    "season_number":2,
    "episode_number":4,
    "device_host":"android-tv"
  }'
```

Load it:

```bash
curl -i "$API/api/progress/demo-user/tv330"
```

Invalid media type test:

```bash
curl -i -X POST "$API/api/progress" \
  -H 'Content-Type: application/json' \
  -d '{
    "user_id":"demo-user",
    "video_id":550,
    "media_type":"movie",
    "total_time_ms":1000,
    "playback_time_ms":500
  }'
```

That request should return HTTP `400`.

## 8. Upsert behavior

The unique key is `(user_id, watched_id)`. Saving `demo-user + m550` again updates the existing row instead of creating a duplicate. `updated_at` is refreshed by the database trigger.

## 9. Production hardening

This project is production-deployable as a small internal/service API, but if it will serve real users, add authentication before treating `user_id` as trusted identity. At minimum:

- authenticate the caller and derive `user_id` server-side;
- replace wildcard CORS with the exact frontend origin(s);
- add rate limiting;
- add request metrics/log aggregation;
- consider a larger pool only after measuring connection usage;
- keep database credentials only in Render environment variables;
- keep RLS enabled on the Supabase table;
- add a proper authorization policy if clients ever access Supabase directly.

The current Go API uses the PostgreSQL connection directly and does not expose the Supabase database credentials to the browser.
