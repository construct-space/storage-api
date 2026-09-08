# storage-api

R2-backed storage service for the Construct ecosystem. One service, one
domain, all uploads and downloads — same pattern as `delivery-api` for
transactional email.

## Why

Before this service every consumer (developer-api for tarballs,
marketplace-api for editorial images, accounts for avatars, etc.) was
either talking to R2 directly or hand-rolling presigning. That spread
credentials across services and made it hard to:

- rotate R2 keys
- enforce per-bucket size / mime / quota policy
- swap R2 → S3 → MinIO without rewriting clients
- audit who uploaded what

`storage-api` centralises all of that behind a thin HTTP surface.

## Layout

```
internal/
  config/      env-var loader + bucket allowlist
  r2/          *s3.Client wrapper for Cloudflare R2 (S3-compat)
  handlers/    presign, proxy upload, GET, delete, copy, list
  middleware/  CORS, logger, AdminAuth (X-Internal-Secret)
```

No database — R2 is the storage. Everything else is stateless.

## Routes

### Public (`/api/storage/*`)

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| POST | `/api/storage/presign` | `X-Internal-Secret` | Returns a presigned PUT URL — caller uploads directly to R2 |
| POST | `/api/storage/upload` | `X-Internal-Secret` | Multipart proxy upload (≤ 10 MB by default) |
| GET | `/api/storage/{bucket}/{key}` | none | 302 to a 5-min signed GET URL (or `?mode=stream` to proxy) |

### Internal (`/internal/storage/*`, gateway-secret only)

| Method | Path | Purpose |
|--------|------|---------|
| DELETE | `/internal/storage/{bucket}/{key}` | Delete one object (idempotent) |
| POST | `/internal/storage/copy` | Copy `src_bucket/src_key` → `dst_bucket/dst_key` |
| GET | `/internal/storage/{bucket}/list?prefix=` | List up to 1000 keys |

## Buckets

Set buckets via env:

- `R2_DEFAULT_BUCKET` (or `STORAGE_BUCKET`, then `R2_BUCKET`) is used when callers omit `bucket`.
- `ALLOWED_BUCKETS` is a CSV allowlist. If omitted, it defaults to the default bucket.

Requests to a bucket not in the allowlist return 400. Keeps a leaked
internal secret from being able to write to arbitrary buckets sharing
the R2 access key.

## Environment

```
PORT=8000
APP_URL=https://storage.lisaos.dev

R2_ACCOUNT_ID=<cloudflare account id>
R2_ACCESS_KEY_ID=<r2 access key>
R2_SECRET_ACCESS_KEY=<r2 secret>
R2_REGION=auto
R2_PUBLIC_BASE_URL=https://cdn.construct.space    # maps to the R2 bucket root
R2_DEFAULT_BUCKET=construct                       # bucket behind cdn.construct.space
# STORAGE_BUCKET=construct                        # accepted legacy alias

ALLOWED_BUCKETS=construct
ALLOWED_ORIGINS=https://my.lisaos.dev,https://oracle.lisaos.dev,tauri://localhost

MAX_UPLOAD_BYTES=10485760      # 10 MB; bigger files must use presign
PRESIGN_TTL_SECONDS=900        # 15 min

INTERNAL_SHARED_SECRET=<shared with my.c.s gateway, accounts, etc.>
```

## Typical client flow

### Big file (tarballs, video) — presigned PUT

```ts
// 1) ask storage-api for a URL
const r = await fetch('/api/storage/presign', {
  method: 'POST',
  headers: { 'X-Internal-Secret': SECRET, 'Content-Type': 'application/json' },
  body: JSON.stringify({ key: 'spaces/kanban/0.4.1.tgz', content_type: 'application/gzip' }),
}).then(r => r.json())

// 2) PUT bytes straight to R2
await fetch(r.url, { method: 'PUT', body: tarballBytes, headers: { 'Content-Type': 'application/gzip' } })

// 3) store r.public_url in your db
```

### Small file (icon, avatar) — proxy upload

```ts
const fd = new FormData()
fd.append('key', 'avatars/avatar.png')
fd.append('file', avatarBlob, 'avatar.png')

const r = await fetch('/api/storage/upload', {
  method: 'POST',
  headers: { 'X-Internal-Secret': SECRET },
  body: fd,
}).then(r => r.json())
// r.public_url is what to save
```

### Reading — let R2 / CDN serve

```html
<img src="https://cdn.construct.space/users/u_abc/avatars/avatar.png" />
```

Use the `public_url` returned by upload/presign when a CDN bucket root is
configured. For private reads through storage-api, `GET /api/storage/{bucket}/{key}`
still 302 redirects to a 5-min presigned URL.

## Run locally

```
R2_ACCOUNT_ID=...  \
R2_ACCESS_KEY_ID=...  \
R2_SECRET_ACCESS_KEY=...  \
R2_DEFAULT_BUCKET=construct  \
ALLOWED_BUCKETS=construct  \
INTERNAL_SHARED_SECRET=dev  \
go run .
```

Then:

```
curl http://localhost:8000/health
curl -X POST http://localhost:8000/api/storage/presign \
  -H 'X-Internal-Secret: dev' \
  -H 'Content-Type: application/json' \
  -d '{"key":"test.txt"}'
```
