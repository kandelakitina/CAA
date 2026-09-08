# Handoff: Neva Approvals

## Repository

- Current workspace: `C:\Users\boticelli-win\Downloads\CAA\CAA`.
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
- Uploads have content validation for PDF, DOC, DOCX, XLS and XLSX with tests.
- Questions and independently versioned multi-file bundles are implemented,
  including secretary confirmation of uploads.
- Four-service internal reviews, withdrawals, revision rounds and explicit
  carry-forward of positive visas are implemented.
- Committee voting includes frozen rosters/files/decision text, quorum,
  attachments, withdrawals, deadline extensions and repeat rounds.
- Numbered protocols support manual agenda order, HTML and editable Word export.
- Question cancellation is implemented locally: secretary only, mandatory reason,
  atomic active-round cancellation and audit, retained history. Approved and
  already cancelled questions cannot be cancelled. Committee readers retain
  access only if the cancelled question had reached Committee voting.
- The legacy single-document workflow remains for compatibility.
- Draft editing is implemented at GET/POST `/questions/:id/edit`, secretary only.
  It is allowed only in draft with no internal or Committee round history,
  including cancelled rounds. Updates lock the question, check the submitted
  `updated_at` token and write old/new field values to audit in one transaction.
  Validation errors retain submitted fields. No schema/config changes for editing.

## Recommended next session

Next: design post-review editing as a separate revision workflow requiring
explicit rules for visa carry-forward. Draft editing is complete and deliberately
does not cover drafts returned from cancelled rounds. Then implement file
exclusion without deleting history, with mandatory-bundle validation and stage
restrictions.

The cancellation change adds three columns with `ADD COLUMN IF NOT EXISTS`:
`questions.cancellation_reason`, `cancelled_at`, `cancelled_by_name`. Existing
rows get empty text / NULL; existing business records are not rewritten.
No new environment variables or production dependencies are required.

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
The current Windows environment has Go 1.27.0. Its module cache may require
sandbox approval. Build to the OS temporary directory on Windows.

## Deferred topics

- Email notifications and reminders need a separate requirements session.
- Production cleanup of obsolete documents and users was discussed but
  explicitly deferred; do not delete production data.
- Protocol generation is an auxiliary convenience, not the legal archive;
  signed paper scans remain outside the application.

## Suggested skills

- No task-specific skill is required for the next implementation phase.
- Use `handoff` only when preparing another compact session transfer.
