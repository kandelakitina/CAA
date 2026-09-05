# Neva Approvals — instructions for Codex

## Project purpose

This repository contains a private web application for placing, versioning and
approving committee documents. It is a standalone application: do not introduce
Bitrix24 or Nuxt. The stack is Go, Gin, server-rendered HTML/HTMX-style
interactions, PostgreSQL and an S3-compatible object store.

The application is deployed from this Git repository to Timeweb Cloud App
Platform. Preserve compatibility with that platform.

## How to work with the owner

- Communicate with the owner in Russian unless asked otherwise.
- The owner is learning web development. Explain important Go, database and web
  concepts briefly and plainly when they affect a decision.
- Inspect the current repository before assuming this document exactly matches
  the implementation.
- Before editing, run `git status --short` and preserve unrelated user changes.
- For any non-trivial request, first state a short plan. Then implement, format,
  test and summarize the result.
- Do not commit, push, deploy, delete production data or change cloud resources
  unless the owner explicitly asks.
- Never print, commit or copy passwords, database credentials, session secrets,
  S3 access keys or other secrets. `.env` files must remain untracked.

## Current architecture

- Language/framework: Go with Gin.
- UI: Go HTML templates and lightweight server-rendered interactions. Avoid
  adding a JavaScript SPA framework without explicit approval.
- Database: PostgreSQL accessed through `pgx`.
- File storage: Timeweb S3-compatible private bucket.
- Authentication: email/password, database-backed sessions and role checks.
- Passwords are hashed; the minimum length for newly created, reset and changed
  passwords is 8 characters.
- The HTTP server must listen on all interfaces using `PORT`, with `8080` as the
  fallback. The health endpoint is `GET /health`.
- Database migrations currently run from the application on startup. Keep them
  idempotent and safe for an existing production database.

## Timeweb Cloud environment

Production components are in Timeweb Cloud and connected through a private
network:

- App Platform application private IP: `192.168.0.4`.
- PostgreSQL private IP: `192.168.0.5`, port `5432`.
- Database name: `default_db`.
- S3 endpoint: `https://s3.twcstorage.ru`.
- S3 region: `ru-1`.
- Private bucket: `neva-approvals-documents-2636`.
- Build command: `go build -o app .`.
- Start command: `./app`.
- Health-check path: `/health`.

Configuration is supplied only through environment variables. Existing names
include `PORT`, `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`,
`PGSSLMODE`, `SESSION_SECRET`, `INITIAL_ADMIN_EMAIL`,
`INITIAL_ADMIN_PASSWORD`, `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`,
`S3_ACCESS_KEY_ID` and `S3_SECRET_ACCESS_KEY`. Confirm names against the code
before changing configuration. Never put real values into repository files.

## Implemented product behavior

- Login and logout with protected pages.
- Initial administrator creation from environment variables on an empty
  database.
- Forced password change after first login or administrator reset.
- Roles: administrator, secretary, committee member, approver and observer.
- Administrator can create users, reset their passwords and revoke/delete user
  access.
- User deletion is intentionally a soft deletion (`active = false`) so names,
  decisions and the audit trail remain available. Active sessions are revoked.
- An administrator cannot delete their own account.
- Deletion is blocked when the user still has a pending response in an active
  approval round.
- Document registry and upload of `.doc`, `.docx` and `.pdf` files, currently
  limited to 25 MB.
- Explicit document versions: every upload is a new immutable version rather
  than an overwrite. Previous versions can be listed and downloaded.
- Approval rounds are attached to a specific document version.
- Participants submit their own approval response. Secretary/administrator can
  complete or cancel a round.
- Uploading a new version is blocked while that document has an active approval
  round.

## Domain and data-integrity rules

- Auditability is essential because this is a corporate approval system.
- Do not hard-delete users, documents, versions, approval rounds, participants
  or decisions unless the owner explicitly approves a retention design.
- Never mutate an already approved document version or silently replace its S3
  object.
- A decision must remain tied to the exact version reviewed by the participant.
- Perform related database changes in a transaction.
- Enforce authorization on the server. Hiding a button in a template is not an
  authorization control.
- Files must remain private. Downloads should pass through an authorized route
  or use a short-lived signed URL only after permission checks.
- Validate uploads by extension, size and available content metadata. Do not
  trust the browser-provided filename or MIME type alone.
- Preserve historical display data when deactivating users.

## Coding conventions

- Prefer the standard library and current dependencies. Ask before adding a new
  production dependency.
- Keep handlers small where practical; put shared business rules into explicit
  functions rather than duplicating them across handlers.
- Return user-facing errors in Russian and log operational detail without
  secrets.
- Use parameterized SQL only.
- Use `html/template` contextual escaping; avoid emitting trusted HTML from
  user-controlled fields.
- Use secure cookie settings appropriate to HTTPS production. Authentication or
  state-changing form work must consider CSRF protection, session fixation,
  brute-force resistance and authorization tests.
- Keep `.env.example` limited to placeholders.

## Verification before handoff

After changing Go or templates, run the checks available in the environment:

```bash
gofmt -w *.go
go test ./...
go vet ./...
go build -o /tmp/neva-approvals-app .
```

Also inspect `git diff --check` and `git diff`. For changes to routes or forms,
test both the permitted role and a forbidden role. For migrations, reason about
re-running them against an existing database. If a tool or external service is
unavailable, state exactly which check could not be completed; do not claim it
passed.

## Deployment handoff

Timeweb deploys after the owner pushes to the connected branch. A successful
local build is not itself authorization to push. When the owner requests a
deployment, provide a concise summary, the exact commit contents, any new
environment variables or migrations, and a short production smoke-test list.

