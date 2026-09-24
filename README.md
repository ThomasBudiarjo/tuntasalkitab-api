# Tuntas Alkitab Android API

Go standard library + chi JSON API for the [Android app](https://github.com/ThomasBudiarjo/tuntasalkitab-apk). Uses the same `users` and `reading_progress` tables as [tuntasalkitab.com](https://github.com/ThomasBudiarjo/tuntasalkitab); there is no separate account database. Google ID token `sub` maps to `users.google_id`, not email. No anonymous API login.

## Run locally

Requires Go 1.27. Set a **web** OAuth client ID (the same one configured in the Android app) and a random secret of at least 32 bytes:

```sh
export GOOGLE_CLIENT_ID='your-web-oauth-client-id.apps.googleusercontent.com'
export API_TOKEN_SECRET="$(openssl rand -hex 32)"
export DATABASE_PATH=./bible-tracker.db
go run .
```

Defaults: SQLite at `./bible-tracker.db`, HTTP port `8494` (override with `PORT`). For Turso instead, set both `TURSO_DATABASE_URL` and `TURSO_AUTH_TOKEN` to the **same database used by the website**; neither variable alone is accepted. Do not commit secrets or database files. The Android build's `TUNTAS_API_BASE_URL` must point to the public HTTPS origin serving this API, with a trailing `/`. Its default currently points at the website, which does **not** serve this separate API; configure a separate API origin or route `/api/v1/*` to this service at the website's origin. Android Google Credential Manager still requires an Android OAuth client configured with the app's package and signing certificate.

## API (`/api/v1`)

| Method and path | Purpose |
| --- | --- |
| `POST /auth/google` `{ "idToken": "…" }` | Verify Google ID token (signature, issuer, audience, expiry); return `{accessToken, refreshToken, expiresAt, user}`. |
| `POST /auth/refresh` `{ "refreshToken": "…" }` | Return a new signed access/refresh pair; the old refresh token remains valid until its original expiry. |
| `GET /progress?since=0` | Bearer auth; complete progress snapshot. Nonzero `since` is rejected. |
| `POST /progress` `{ "items": [{"dayOfYear": 12, "completed": true, "updatedAt": 1738368000200}] }` | Bearer auth; batch up to 400 unique days 1–365, persist local intent and echo the stored status. |
| `GET /plan` | Public reading plan `{version, days}`; supports `ETag` / `If-None-Match`. |

`GET /health` responds 204 when the database can be reached. Progress responses include `{serverTime, items}`; all times are epoch milliseconds. The Android app stores local changes first, pushes pending rows, applies the echo, then pulls `since=0`. Pushes overwrite the stored status regardless of the incoming timestamp. `updatedAt` in responses is the **response time**, synthesized to satisfy the Android DTO; it is **not** a durable modification time or delta cursor. Full pulls are at most 365 days.

## Proposed Android-side reconciliation (not implemented yet)

Every toggle should update the app's local store immediately, whether online or offline, so the UI never waits for a network request and a failed request cannot lose user intent. When online, reconcile promptly; when offline, leave it pending and reconcile when connectivity returns. Before pushing pending rows (including on first sign-in), fetch a full server snapshot and compare the **sets of completed day numbers**, not just their counts. The UI can show `Server: N days read` and `Local: M days read` when the sets differ, then let the user choose which side wins. If the sets already match, sync quietly without showing a choice. Choosing server replaces local completion state; choosing local writes the local completion state to the server (including `completed: false` for server days the user unchecked locally). An absent server row means unchecked. Keep the chosen result locally after the write and confirm it with another full pull.

The app should persist the last **successfully reconciled set of completed days per Google user** as a local baseline. `lastSyncAt` alone is only a display/checkpoint timestamp, not a change history. Relative to that baseline, if both current sets only *add* days (no days removed), union those additions and sync automatically; this covers a new day's completed count growing on either side. If a day was removed, changes conflict, or no trustworthy baseline exists and sets differ, ask the user rather than guessing from a higher count. Save a new baseline only after the API write and follow-up pull succeed. This is entirely app-side bookkeeping; it adds no Turso columns or tables.

If writes from the website or another phone can race with reconciliation, app-only comparison is insufficient: a server edit between the pull and push can still be overwritten. For robust protection **without changing the database**, a future API revision can return a hash/ETag of the complete progress set and require the same value (`If-Match`) on a transactional conditional batch write; if it differs, return `409` so the app fetches again and re-evaluates rather than overwriting unseen changes. This requires coordinated Android **and API code** changes, but no new Turso schema. A hash of current state cannot reveal intermediate edits that return to the same state.

**Current Android/API code has not implemented this flow.** Android `SyncRepository` currently pushes pending local rows first and then pulls the server snapshot; it stores `lastSyncAt`, not a per-day reconciliation baseline. The API currently accepts unconditional progress pushes; it cannot decide which source the user prefers. Both sides need code changes before choice/automatic-addition/conditional-write behavior exists.

## Unchanged shared database

**No migration or schema change is performed on Turso.** The API uses only the website's existing `users` and `reading_progress` tables and columns. Only a new local SQLite file is initialized with the website's original schema. The reading plan is copied from the website's embedded `internal/reading/plan.json`, not put into a table. Do not deploy the closed, unmerged mobile API PR from the web repository alongside this implementation; it changes the shared schema.

**Sync tradeoff:** the website sets `completed_at=NULL` on an uncheck, so this schema cannot tell when that change occurred. Full snapshots include unchecked rows, but there is no reliable last-write-wins conflict resolution across offline devices and website edits. The last push received by the API wins; a later website toggle can overwrite it, and a delayed offline push can overwrite a newer website toggle. Concurrent local edits during a sync may also be lost when the Android client applies a fresh synthetic snapshot timestamp. Do not promise chronological ordering without storing modification times somewhere.

**Authentication tradeoff:** access tokens expire after 15 minutes. Refresh tokens are signed JWTs valid for up to 30 days from Google sign-in; refreshing does not extend that deadline. With no token table, used refresh tokens cannot be revoked or made single-use—replays remain valid until expiry. Rotate `API_TOKEN_SECRET` to invalidate *all* tokens. There is no server logout endpoint in the Android contract; sign-out only removes local credentials. HTTPS is required outside local development.

Run checks with `go test ./...` and `go vet ./...`. Tests use an isolated SQLite file and do not connect to Turso or Google; live Turso/auth integration requires deployment credentials and a real Google sign-in.
