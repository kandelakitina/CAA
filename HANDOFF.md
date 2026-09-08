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
- Post-review decision text revisions are implemented. GET
  `/questions/:id/decision-revisions/new` and POST `/questions/:id/decision-revisions`
  are secretary-only. No active internal/Committee round is allowed; approved,
  cancelled and never-reviewed questions are blocked. The secretary supplies a
  new text, reason, deadline and explicit `recheck_all` or `carry_positive` policy.
  Only positive visas for unchanged file versions from the latest internal round
  carry forward; rejected files require a new official version. A new internal
  round is created atomically and auto-completes when all visas are carried.
  History records both texts, author, reason, policy and round references.
  Committee start checks a question timestamp token to reject stale forms.
- File exclusion is implemented at POST `/questions/:id/files/:fileID/exclude`,
  secretary-only with CSRF, a mandatory reason and a stale-question check.
  Allowed between rounds in draft/revision-required/ready/rejected/no-quorum.
  Active rounds, approved/cancelled questions, already excluded files and pending
  versions are blocked. Removing an official file must leave a mandatory bundle;
  never-official rejected uploads can be excluded from incomplete drafts.
  All exclusions return the question to draft for full internal review. Decision
  revisions cannot carry visas from a round predating file exclusion.
  Excluded files/versions remain visible and downloadable; no new uploads or
  confirmation/rejection is allowed. There is no restore action yet.
- Full round history is implemented: the card lists all internal/Committee rounds
  and GET `/questions/:id/rounds/:kind/:roundID` opens a read-only detail page.
  Question authorization runs before any history read, and round queries require
  both question ID and round ID. Missing/foreign IDs return 404 without fallback.
  History includes superseded internal requirements and their old visas, precise
  file versions, attachments and withdrawals; superseded requirements do not
  affect final progress. Committee history uses frozen roster/bundle/text and
  shows withdrawn vote times. Even selected active rounds have no mutation controls.
  Current card loaders still default to the latest round. No new migrations.

## Recommended next session

The source-level workflow review is complete; see WORKFLOW_REVIEW_2026-09-08.md.
Fixed rework after positive internal review / unsuccessful Committee voting,
uploads after Committee cancellation, all-carried round completion, stale repeat
forms, and the combined attachment request limit. No migrations were added.
The owner authorized a local test environment. Portable PostgreSQL 17.6 and
SeaweedFS 4.46 are now available in ignored .local-test; see LOCAL_TEST.md.
Run scripts/local-test.ps1 -Action Test for the real database/S3 HTTP workflow.
The integration run passed migrations, auth/password changes, access/CSRF checks,
large attachments, concurrent round starts, revisions, pending uploads, Committee
rejection/no-quorum/cancellation, protocols and audit immutability. Next: optional
additional integration scenarios and a separately authorized Timeweb smoke test
for deployment/proxy/storage specifics. Notifications remain a
separate requirements/implementation phase. Post-review revisions change only
decision text; other metadata remains protected after the first review.

File exclusion migrations add `question_files.exclusion_reason`, `excluded_at`
and `excluded_by_name` with IF NOT EXISTS. Existing `status = 'excluded'` is used;
no versions, review records or S3 objects are deleted. Mandatory-bundle checks are
shared with internal-review start. No new dependencies or configuration.

Decision revision migrations add immutable `decision_text_revisions` and nullable
`internal_review_rounds.frozen_decision_text`. New initial, repeat and text-revision
rounds save their text. Older rounds are NOT backfilled from current text; the UI
explicitly labels the missing snapshot. Migrations use IF NOT EXISTS and a guarded
immutable trigger. No new environment variables or dependencies.

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

The owner has no separate remote test server. Docker was abandoned, but the new
portable Windows test setup works without Docker/WSL. It binds only to 127.0.0.1,
uses generated test credentials, and retains a separate schema for each run.
Never connect to or mutate production as part of ordinary development.

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
