# Watch Progress API — Go + Supabase Auth + Supabase PostgreSQL + Render

A lightweight account + watch-progress backend for websites, Android, and Android TV.

## Architecture

```text
Website / Android / Android TV
          │
          │ email + password
          ▼
      Go API (Render)
          │
          ├── Supabase Auth
          │      └── one permanent user UUID
          │
          └── PostgreSQL (Supabase)
                 └── watch_progress.user_id
```

The important rule is: clients never choose `user_id` for watch progress. The Go API verifies the Supabase access token and gets the user ID from Supabase Auth.

That is what makes the same account work on website, mobile, and TV.

## 1. Supabase

The connected Supabase project is already active. Its project URL is:

```text
https://wawlivuobjvvkbkaampj.supabase.co
```

Run `schema.sql` in the Supabase SQL Editor.

The requested `watch_progress` table is server-owned. RLS is enabled as defense in depth; the Go API connects with the database credentials and performs the progress queries server-side.

For `DATABASE_URL`, use the Supabase Connect panel. For a long-running Render service, prefer the direct PostgreSQL connection if IPv6 connectivity is available. Otherwise use Supavisor session mode (port 5432). Avoid transaction mode (6543) for this stateful `database/sql` pool unless you specifically configure for transaction pooling.

## 2. Supabase Auth settings

In Supabase Dashboard → Authentication → Providers → Email, keep Email enabled.

You can enable or disable email confirmation depending on your desired signup flow. When email confirmation is enabled, sign-up can succeed without immediately returning a usable access token; the user must confirm the email and then sign in.

The browser never receives a database password. It receives only a normal Supabase access token.

## 3. Required environment variables

Local `.env` / shell:

```bash
export DATABASE_URL='postgresql://...'
export SUPABASE_URL='https://wawlivuobjvvkbkaampj.supabase.co'
export SUPABASE_PUBLISHABLE_KEY='sb_publishable_...'
export PORT=10000
```

`SUPABASE_PUBLISHABLE_KEY` is safe for client-side use in principle, but this implementation keeps it on the Go side and the browser talks only to your API.

Never commit `DATABASE_URL` or any secret/service-role key to GitHub.

## 4. Authentication API

### Sign up

```http
POST /api/auth/signup
Content-Type: application/json
```

```json
{
  "email": "user@example.com",
  "password": "strong-password"
}
```

### Sign in

```http
POST /api/auth/signin
Content-Type: application/json
```

```json
{
  "email": "user@example.com",
  "password": "strong-password"
}
```

The Go backend proxies these calls to Supabase Auth.

### Current user

```http
GET /api/auth/user
Authorization: Bearer ACCESS_TOKEN
```

The server asks Supabase Auth to validate the access token and returns the authenticated user's UUID/email.

## 5. Progress API

Save:

```http
POST /api/progress
Authorization: Bearer ACCESS_TOKEN
Content-Type: application/json
```

```json
{
  "video_id": 550,
  "media_type": "movies",
  "total_time_ms": 7200000,
  "playback_time_ms": 123000,
  "season_number": 0,
  "episode_number": 0,
  "device_host": "android-tv"
}
```

There is deliberately no `user_id` field and no `watched_id` field.

The server creates:

```text
movies + 550 -> m550
tv + 330     -> tv330
```

Load:

```http
GET /api/progress/{authenticated_user_id}/{watched_id}
Authorization: Bearer ACCESS_TOKEN
```

The server also checks that the path user ID equals the authenticated account, preventing one signed-in user from requesting another user's progress.

## 6. Same account on website, mobile, and TV

Suppose a user registers:

```text
user@gmail.com
```

Supabase Auth assigns one UUID, for example:

```text
f2c5...-....-....-....-........
```

Website sign-in → same UUID

Android sign-in → same UUID

Android TV sign-in → same UUID

A movie saved by the phone therefore belongs to that same UUID. The TV signs into the same account and receives the same progress.

Example:

```text
Phone:
Movie 550 -> 12:03

        ↓ same account

TV:
GET /api/progress/<same-user-id>/m550

        ↓

playback_time_ms = 723000
```

## 7. Local run

```bash
go mod tidy
go build ./...
go run .
```

Open:

```text
http://localhost:10000/
```

The included `index.html` contains:

- Sign Up
- Sign In
- Sign Out
- current-session check
- Save Progress
- Load Progress
- raw JSON response panel

## 8. GitHub

Create an empty repository named for example:

```text
watch-progress-api
```

Then:

```bash
git init
git branch -M main
git add .
git commit -m "Add Supabase auth and watch progress API"
git remote add origin https://github.com/YOUR_USERNAME/watch-progress-api.git
git push -u origin main
```

Do not commit `.env`.

Recommended `.gitignore`:

```gitignore
.env
.env.*
!.env.example
*.log
.DS_Store
```

## 9. Render

Use Render Blueprint with `render.yaml`.

The service is configured for:

- Go native runtime
- Singapore region
- `main` branch
- automatic deploy on commit
- `go build` build command
- compiled binary start command
- `/health` health check

Set these environment variables in Render:

```text
DATABASE_URL=your Supabase PostgreSQL connection string
SUPABASE_URL=https://wawlivuobjvvkbkaampj.supabase.co
SUPABASE_PUBLISHABLE_KEY=your sb_publishable key
```

`PORT` is set by the Blueprint, and the Go server also respects Render's injected value.

## 10. Deploy flow

```text
edit code
  ↓
git push origin main
  ↓
Render detects commit
  ↓
Go build
  ↓
Render deploys new version
  ↓
/health passes
  ↓
API live
```

## 11. curl tests

Set:

```bash
export API='https://YOUR-SERVICE.onrender.com'
```

### Health

```bash
curl -i "$API/health"
```

### Sign up

```bash
curl -i -X POST "$API/api/auth/signup" \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@example.com","password":"demo-password-123"}'
```

### Sign in

```bash
curl -s -X POST "$API/api/auth/signin" \
  -H 'Content-Type: application/json' \
  -d '{"email":"demo@example.com","password":"demo-password-123"}'
```

Copy `access_token` from the response:

```bash
export TOKEN='PASTE_ACCESS_TOKEN_HERE'
```

Get the current user:

```bash
curl -i "$API/api/auth/user" \
  -H "Authorization: Bearer $TOKEN"
```

Save movie progress:

```bash
curl -i -X POST "$API/api/progress" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "video_id":550,
    "media_type":"movies",
    "total_time_ms":7200000,
    "playback_time_ms":123000,
    "season_number":0,
    "episode_number":0,
    "device_host":"android-tv"
  }'
```

The response contains:

```json
"watched_id": "m550"
```

Get the current user UUID first, then:

```bash
export USER_ID='PASTE_UUID_HERE'
```

Load progress:

```bash
curl -i "$API/api/progress/$USER_ID/m550" \
  -H "Authorization: Bearer $TOKEN"
```

## 12. Security model

The current design intentionally separates public account credentials from database access:

```text
Browser / TV / Mobile
        │
        │ email/password
        ▼
      Go API
        │
        │ Supabase Auth
        ▼
     access token
        │
        ▼
 authenticated UUID
        │
        ▼
 PostgreSQL progress
```

Do not put `DATABASE_URL` or a Supabase service-role/secret key into HTML or an Android APK.

For a real public deployment, the next hardening steps are:

- restrict CORS to your actual website/app origins where practical;
- add request rate limiting;
- consider refresh-token/logout handling in native clients;
- add structured logs/metrics;
- decide whether progress writes should be throttled to every few seconds rather than every playback tick.
