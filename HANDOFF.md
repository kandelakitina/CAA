# Handoff: Neva Approvals

## Repository

- Workspace: `/home/boticelli/Documents/CorpDocsReview`.
- Read and follow `AGENTS.md`.
- Communicate with the owner in Russian.
- Do not push or deploy without explicit approval: every push to the connected
  branch automatically deploys to the only Timeweb Cloud production environment.

## Source of truth

- The agreed target workflow is documented in `README.md`, section
  `Целевая бизнес-логика`. Read it instead of reconstructing the business rules
  from this handoff.
- The README explicitly distinguishes the target workflow from currently
  implemented behavior.
- Existing architecture and production constraints are documented in
  `AGENTS.md`.

## Current implementation

- Go/Gin, server-rendered templates, PostgreSQL/pgx and private S3-compatible
  storage.
- Existing security work includes CSRF protection, login rate limiting and
  authorization tests.
- The application has an immutable audit journal at `/admin/audit`.
- Uploads have content validation for PDF, DOC and DOCX formats with tests.
- The current application still uses the older single-document approval model;
  the target multi-file question workflow in README is not yet implemented.

## Recommended next session

Start phase 1 of the target workflow: design and implement the data model for
questions, mutually exclusive roles, four internal services, multi-file bundles
and independent file versions.

Before editing:

1. Inspect the actual schema and handlers again; do not assume README equals the
   current code.
2. Produce a migration plan that preserves existing production documents,
   versions, approvals and audit history.
3. Prefer additive, idempotent migrations and a staged compatibility transition
   over destructive renames or table replacement.
4. Keep exact-version traceability for every visa and Committee vote.
5. Do not implement notifications yet; README marks delivery channels and
   scheduling as a later design stage.

The owner has no separate test server. Local Docker setup was abandoned.
Verification is limited to isolated tests, static checks and build unless the
owner explicitly arranges another environment. Never connect to or mutate
production as part of ordinary development.

## Verification

After changes, follow the commands in `AGENTS.md`: `gofmt`, `go test ./...`,
`go vet ./...`, build to `/tmp`, `git diff --check`, and inspect the full diff.
Go may only be available through `nix develop` in the current environment;
preserve any pre-existing `flake.lock` changes.

## Deferred topics

- Email notifications and reminders need a separate requirements session.
- Production cleanup of obsolete documents and users was discussed but
  explicitly deferred; do not delete production data.
- Protocol generation is an auxiliary convenience, not the legal archive;
  signed paper scans remain outside the application.

## Suggested skills

- No task-specific skill is required for the next implementation phase.
- Use `handoff` only when preparing another compact session transfer.
