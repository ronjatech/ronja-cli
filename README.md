# ronja CLI

Command-line access to Ronja, aimed first at AI coding agents and scripts and
second at humans at a terminal.

> **Reading this on `github.com/ronjatech/ronja-cli`?** That repository is a
> published mirror of the `cli/` directory of Ronja's main (private)
> repository, which is the source of truth. Changes made here are overwritten
> by the next release. Some sections below reference paths like `backend/…`
> that exist only in that repository — they are kept because this file is also
> the contributor guide.
>
> Customer documentation: **<https://ronja.tech/docs/guides/use-the-cli/>**

## What this is (and is not)

It is a **bootstrap, not an API wrapper**. Wrapping the API would mean every new
endpoint needs a new subcommand, and an agent would be limited to whatever we
had got around to wrapping. Instead the CLI does the one thing an agent cannot
do for itself — an interactive browser sign-in — and then hands over:

```
ronja login    # device flow through the browser
ronja context       # instance + identity + how to load the credential + the live API index
ronja env           # export lines for RONJA_URL / RONJA_TOKEN — meant for eval
ronja profile list  # which logins this machine holds
```

After `ronja context` an agent works over plain HTTP. `context` **fetches
`/llms.txt` from the instance and inlines it** rather than restating what the
API can do, so it cannot drift from the API it describes. Everything it points
at (`/llms.txt`, `/docs/api/endpoints.md`, `/docs/api/skills.md`,
`/docs/api/openapi.json`) is served unauthenticated.

Two commands make that HTTP half less unpleasant without describing any of it —
`ronja api` (a request runner) and `ronja query` (read-only SQL). Both are
below, and the doctrine that admits them is immediately below this.

The one exception is `ronja wf`, the workflow dev loop — see below. The rule it
keeps is **sync verbs yes, resource verbs no**: the CLI may own a
filesystem-to-resource sync loop, because that is stateful, multi-step and
drift-guarded in a way plain HTTP handles badly. It still owns no list, browse
or delete. If an agent needs something the API already does, the answer is a
better pointer in `context` or a better guide behind `/llms.txt` — not a new
command.

### Transport is not a wrapper

`ronja api` and `ronja query` (both below) look at first glance like the wrapper
this doctrine forbids. They are not, and the distinction is worth stating
because it is what keeps the next command from being one.

**What "no API wrapper" actually rules out is a subcommand per endpoint.** That
is the thing that has to be extended for every new route, that is permanently
behind the API, and that becomes a second definition of a shape which already
has one — so it drifts.

- **`ronja api` carries transport and auth, and knows no endpoints.** It takes
  a path and passes it through. There is nothing in it to drift, because
  nothing in it describes the API. This is `gh api`'s bargain, and the thing it
  replaces is worse: the documented alternative puts a live credential into a
  shell variable, an interpolation, and — when the shell echoes or the command
  is logged — a transcript.
- **`ronja query` earns a verb** on the same test `wf` passes: it is what plain
  HTTP does *badly*. Multi-line SQL inside a JSON string inside a shell string
  is a two-level escaping problem; the answer is a table, and "write it to a
  file" is a shape a generic runner cannot give; and a failed query comes back
  as **HTTP 200** with an `error` field, so a generic runner exits *zero* on a
  query that never ran.

The flags added to `ronja api` since — `--jq`, `-F`, `-o`, `--retry`,
`--wait-until` — pass the same test, and it is worth naming what the test
actually is. **None of them describes an endpoint.** `--jq` shapes output, `-F`
sets a content-type, `--retry` re-sends, `--wait-until` re-issues a request the
caller already wrote. Not one has to be extended when a route is added, which is
the property the doctrine is really protecting. Compare a hypothetical
`ronja workflow run --wait`: that one *would* have to know a route, a poll
endpoint and a status vocabulary, and would need a sibling for every other
asynchronous primitive.

Neither one moves the line on discovery. There is still no `list`, no `browse`,
no `delete`: finding out what exists stays on plain HTTP behind `/llms.txt`.

Customer-facing documentation lives at
[`docs-site/src/content/docs/guides/use-the-cli.md`](https://ronja.tech/docs/guides/use-the-cli/),
published at <https://ronja.tech/docs/guides/use-the-cli/>.
This file is for people working *on* the CLI.

## Install

On macOS, from our tap:

```bash
brew install ronjatech/tap/ronja
```

Anywhere Go is available:

```bash
go install github.com/ronjatech/ronja-cli/cmd/ronja@latest
```

Or take a binary from the
[releases](https://github.com/ronjatech/ronja-cli/releases) — `darwin` and
`linux`, `amd64` and `arm64`.

Both of the first two need `$(go env GOPATH)/bin` (or Homebrew's bin) on your
`PATH`:

```bash
export PATH="$PATH:$(go env GOPATH)/bin"   # add to ~/.zshrc to persist
ronja --help
```

**`brew` is macOS-only, and that is a Homebrew constraint rather than a
choice.** We ship a *cask*, because GoReleaser's formula support is deprecated
and a formula was always the wrong shape for a pre-built binary — and Homebrew
does not support casks on Linux. Linux and CI take `go install` or the tarball.

### Working on the CLI

From a checkout of the monorepo:

```bash
cd cli
go install ./cmd/ronja
```

**The `./cmd/ronja` path matters.** `go install` names the binary after the last
element of the *package* path, so installing the module root would produce
`ronja-cli` (the module is `github.com/ronjatech/ronja-cli`). The main package
lives at `cmd/ronja/` precisely so the binary comes out as `ronja` — keep it
there.

To build without installing:

```bash
go build -o ronja ./cmd/ronja && ./ronja login --url http://localhost:8080
```

With a version stamp (releases get this automatically — see below):

```bash
go install -ldflags "-X github.com/ronjatech/ronja-cli/internal/commands.Version=$(git describe --tags)" ./cmd/ronja
```

## Releasing

The published artifacts come from a **public mirror** of this directory at
[`github.com/ronjatech/ronja-cli`](https://github.com/ronjatech/ronja-cli).
The mirror exists for two reasons: the monorepo is private, so Homebrew cannot
download from it, and `go.mod` declares the module as
`github.com/ronjatech/ronja-cli` — so a public repo at exactly that path is
what makes `go install ...@latest` resolve.

**The monorepo is the source of truth. Never edit the mirror by hand** — the
next sync overwrites it.

**There is no CLI release ritual.** The CLI ships on the ordinary production
tag — `.github/workflows/cli_release.yml` triggers on the same `v*` tag as
`releaser_prod.yml`, as a separate workflow so that a CLI publishing failure
can never block a backend deploy. Merge a CLI change and it goes out with the
next deploy.

That means **the CLI carries the product version**: `ronja version 0.26.2` is
the CLI that shipped with Ronja 0.26.2. A separate `cli/v*` tag was rejected
deliberately — a second release ritual is a thing people forget, and the
failure is silent, with the published CLI quietly falling behind while
everyone assumes it is current.

Two consequences worth knowing:

- **Nothing is published when `cli/` did not change.** The workflow diffs
  `cli/` between this tag and the previous release and skips otherwise —
  without that, every backend deploy would ship a byte-identical CLI and
  Homebrew users would get a pointless upgrade a day.
- **A future `v2.0.0` needs a module path change.** Go requires a `/v2` suffix
  at major 2, so the module would become
  `github.com/ronjatech/ronja-cli/v2`. Nothing to do until then, but it is the
  one bill this versioning choice leaves unpaid.

The run syncs this directory to the mirror's root, tags it with the same
version (Go modules require a bare `vX.Y.Z` tag at a repository root), then
runs [GoReleaser](.goreleaser.yaml) — tests, four cross-compiled targets, a
GitHub Release on the mirror, and the Homebrew cask pushed to
`ronjatech/homebrew-tap`.

### Testing a change to this pipeline

**After it is on `main`:** **Actions → Release CLI → Run workflow** takes a
version and a `publish` checkbox. Unchecked, it mints the token, reaches the
mirror and runs the whole build while writing nothing. Checked, it publishes
for real at the version you name.

**Before it is on `main`** — `workflow_dispatch` does not appear until the
workflow reaches the default branch, but a **tag trigger runs the workflow
file as it exists at the tagged commit**. So a prerelease tag on your branch
gives a full end-to-end test:

```bash
git tag v0.26.2-rc.1 && git push origin v0.26.2-rc.1
```

That is safe against the backend by construction: `releaser_prod.yml` accepts
only `^v[0-9]+\.[0-9]+\.[0-9]+$`, so a prerelease fails its *first* job and
every later job depends on that one — nothing is built or deployed. It does
leave a red X on **Release Production**, which is noise worth warning people
about rather than a failure.

Prereleases are safe against customers too, and that is configured rather than
accidental:

- `homebrew_casks.skip_upload: auto` — the tap holds exactly ONE cask file, so
  publishing an `-rc` would overwrite it and hand every
  `brew install ronjatech/tap/ronja` user a release candidate until the next
  stable version replaced it.
- `release.prerelease: auto` — otherwise GitHub shows the rc as the repo's
  **Latest release**, which is what anyone landing on the releases page
  downloads.
- `go install ...@latest` ignores prereleases under Go's own semver rules.

Clean up afterwards by deleting the tag locally and on the remote, and the
draft release in the mirror.

**The mirror runs no CI and holds no secrets.** Publishing needs a credential
that can write to another repository, and the mirror is public — a release
token stored there would be a write-capable secret in a world-readable repo.
Everything therefore runs from this private repo, and the mirror is purely a
distribution artifact.

Validate a config change without releasing anything:

```bash
brew install goreleaser
cd cli && goreleaser check
goreleaser release --snapshot --clean --skip=publish   # builds into cli/dist/
```

### Credentials

One **org-owned GitHub App**, not a personal access token — a PAT is a person's
credential doing a machine's job: it expires (and releases then fail for a
reason that looks nothing like an expired token), and it dies with the account
that made it. The App mints an installation token scoped to the two target
repositories that expires in an hour.

Set the App up once (org **Settings → Developer settings → GitHub Apps**):

- Repository permissions: **Contents: Read and write**. Nothing else — in
  particular *not* Workflows, since the mirror has no workflows to push.
- Install it on `ronjatech/ronja-cli` and `ronjatech/homebrew-tap` only.

Then add both to this repo's **`production` environment** (Settings →
Environments → production) — not to the repository-level secrets:

| Name | Kind | Value |
| --- | --- | --- |
| `RONJA_CLI_DEPLOY_APP_ID` | Environment **variable** | the App's numeric ID (not sensitive) |
| `RONJA_CLI_DEPLOY_APP_PRIVATE_KEY` | Environment **secret** | the generated `.pem`, pasted whole |

The job declares `environment: production` to reach them, matching
`releaser_prod.yml`. Scoping them there keeps the private key — which can mint
write tokens for two public repos — reachable only from a job that opts in,
rather than from every workflow in this repository.

⚠️ The two halves of that must move together. An environment secret read by a
job with no `environment:` line resolves to the **empty string** rather than
failing, so the mismatch surfaces as an unexplained authentication error at
token-mint time rather than as a missing secret.

It is a **separate Go module** from `backend/`. That is deliberate: the backend
module pulls in pgx, Kafka and the AWS SDK, none of which belongs in a CLI
binary. The cost is that request/response shapes are hand-mirrored in
`internal/api` rather than imported — keep them in step with
`backend/api/v2/authentication/api_cli_auth.go`.

## Profiles

A **profile** is one instance, one organization, and the token that reaches it.

The organization is not decoration. A Ronja access token is bound to exactly one
— token auth carries the tenant recorded on the token row, and the product's
only tenant-switch mechanism (`POST /api/v2/tenant/set`) sets a browser
**cookie**, which a token can neither see nor use. So belonging to two
organizations means holding two tokens **on the same URL**, and a credential
store keyed by URL alone cannot do that without silently evicting one of them.

```
ronja profile list                  # name, instance, organization, which is current
ronja profile use                   # pick from a list, at a terminal
ronja profile use acme-retail       # or name it
ronja profile rename app-2 northwind
ronja query --profile local "SELECT 1"
```

`profile use` with no name opens an arrow-key picker starting on the current
profile, so enter alone changes nothing. Anywhere without a terminal — a script,
a pipe, an agent, or `--json` — the name is REQUIRED and a bare call is an error
rather than a prompt: the rule from `stdin.go` applies here too, and a picker
drawn into a pipe would block forever on input nobody is typing.

Profiles are created by signing in — there is no `profile add`, because a
profile without a working credential reports "signed in" and then 401s.

**Which organization you get is decided in the browser**, at the moment you
approve: the token binds to whichever organization your web session is on, and
the approval screen names it. To add a second one, switch organization in the
web app and run `ronja login` again. The new token is stored as its own
profile beside the first, never on top of it.

The name is derived from the organization (`Acme Retail` → `acme-retail`),
except on a loopback instance or when that name is already taken by the same
organization on a different URL — where the host label says more. On loopback
that label carries the PORT (`local-8082`), because several local backends at
once is the normal way to work on Ronja — one per worktree — and they are
identical in every other respect a name could draw on. `--profile <name>` overrides, and `profile rename` fixes anything you
dislike. A name is a **local handle only**; the profile's identity is (URL,
organization), which is what every lookup matches on and what a re-login updates.

### Selecting one

```
--profile / $RONJA_PROFILE     by name
--url / $RONJA_URL             by instance
(neither)                      the current profile, or cloud.ronja.tech when
                               nothing is stored yet
$RONJA_TOKEN                   always wins, and never touches disk
```

The default instance is the hosted one (`config.DefaultURL`), not a local
backend: almost nobody running this has one, and pointing at it fails as
"connection refused" — a message that says nothing about what to do instead.
Working on Ronja itself means `--url localhost:8080` once, after which that
login is a profile like any other.

Two rules are worth knowing because they are refusals rather than preferences:

- **A named URL is a filter, never a fallback.** `--url https://staging…` with no
  profile there means *not signed in to staging* — it does not fall back to the
  current profile, because that would send a production token to a host you
  named explicitly.
- **An ambiguous URL is refused, not guessed.** Two profiles on one URL differ by
  organization, so the CLI names both and asks for `--profile` rather than
  picking. (If one of them is already current, that counts as having chosen.)

A conflict between a typed flag and an exported variable resolves in favour of
the flag. A conflict *within* one layer — both flags, or both variables — is an
error.

## How login works

`ronja login` runs an RFC 8628 device-authorization flow:

1. `POST /authentication/cli/device` (unauthenticated) returns a secret
   `deviceCode` for the CLI and a short `userCode` for the human.
2. The human approves at `/cli-auth?code=<userCode>` in a browser, where they
   have a real session. That request is what actually authorizes anything.
3. `POST /authentication/cli/token` (unauthenticated) polls with the
   `deviceCode`. The first successful poll mints a Personal Access Token for
   the approving user and returns it exactly once.

The token is minted at collection rather than at approval, so no bearer
credential is ever stored server-side. Backend detail is in migration
`000481_cli_auth_requests.up.sql` and `backend/resource/rcliauth/`.

The PAT is **full access but time-bounded — 90 days**. Full access is right
because a PAT never exceeds its bound user's live role (and `/api/v2/authentication`
requires the `admin` scope, so a *scoped* token could not even call `/me`); the
expiry is right because this credential is reachable from a link a human can be
phished into clicking. The server returns `expiresAt`, the CLI stores it, and
`login` / `whoami` / `context` all report it — a credential's shelf
life should be visible, not discovered as a 401.

Two properties of the poll loop are deliberate and easy to undo by accident:

- **Transient failures are retried, not fatal.** A device flow lives ~10 minutes
  while a human walks to a browser, and a rolling deploy, a 429 from the global
  limiter or a dropped connection are all ordinary in that window. Transport
  errors, 429 and 5xx retry (bounded, with the flow deadline as the backstop);
  the RFC outcomes (`access_denied`, `expired_token`, `invalid_grant`) and any
  other 4xx stay terminal, because those are answers rather than blips.
- **The credential is persisted BEFORE it is verified.** The server burns the
  single-use latch before minting and hands the plaintext over exactly once, so
  discarding it on a failed `/me` would leave a live PAT nobody holds. Login
  writes first and warns if verification then fails. (`--with-token` keeps
  verify-first on purpose: a pasted token is re-pastable.)

### The credential never leaves the instance it belongs to

Every client built by `api.New` carries a `CheckRedirect` that refuses any hop
to a different **origin** — scheme, host *and* port — and says so rather than
following it.

This is not something net/http gives you. It strips `Authorization` on a
redirect only when the **hostname** changes, and on nothing else: a 302 to
`http://<same-host>:9999`, or a downgrade from `https` to `http` on the same
name, forwards the 90-day PAT to whatever is listening there. The instance does
not have to be hostile for that to happen — a misconfigured proxy is enough.

Two details worth keeping if this is ever touched: an explicitly written default
port (`https://host:443`) is folded onto the implicit form, so a caller who typed
one is not refused for reaching the other; and the guard implements its own
10-hop limit, because supplying a `CheckRedirect` **replaces** net/http's default
policy, which was the only thing stopping a same-origin redirect loop.

## Requests (`ronja api`)

```
ronja api [flags] <path>
```

An authenticated request runner. The path is joined to the resolved instance
and the credential is attached; nothing else is interpreted.

```bash
ronja api /api/v2/authentication/me
ronja api -X POST /api/v2/feature -d '{"name":"Sales","scope":"private"}'
ronja api -X POST /api/v2/feature -d @feature.json
cat feature.json | ronja api -X POST /api/v2/feature -d @-

# upload a file (multipart — the encoding -d cannot produce)
ronja api -X POST /api/v2/file/upload/report.pdf -F file=@./report.pdf

# read one field out of the answer
id=$(ronja api -X POST /api/v2/workflow -d @wf.json --jq -r '.id')

# wait for asynchronous work
ronja api "/api/v2/workflow/run/$runID" --wait-until '.status != "running"'
```

### Sending

| Flag | Does |
|---|---|
| `-X`, `--method` | HTTP method. Defaults to GET, or **POST when a body is given** — curl's rule, for curl's reason. Upper-cased for you. |
| `-d`, `--data` | Request body. `@file` reads a file, `@-` reads stdin, anything else is the literal string. A `@file` or `@-` body is capped at **16 MiB** and refused past it, naming the limit — the body is buffered whole, so an unbounded read is an unbounded allocation chosen by whatever path was typed. |
| `-F`, `--form` | A `multipart/form-data` part: `name=value` for a literal, `name=@path` for a file (`@-` reads stdin). Repeatable. Mutually exclusive with `-d`, and at most one thing in the whole command may read stdin. |
| `-H`, `--header` | `"Name: value"`, repeatable. Repeats of one name are sent as repeats, not collapsed. A `Host:` header is honoured (copied onto `req.Host`) — net/http takes the Host line from there and silently **drops** a `Host` key in the header map, so setting it without that step would do nothing at all. |
| `--timeout` | How long to wait for **one request**, response body included — the deadline rides on the body's `Close`, not on the request returning. Default 2m; `0` waits as long as it takes; negative is refused. Same semantics and the same `httpFor` plumbing as `ronja query --timeout` (below). |
| `--retry` | Retry a **429 or 5xx** this many times, honouring `Retry-After` (both the seconds and the HTTP-date form), else exponential backoff capped at 30s. Default 0. A 4xx is never retried — that is the server saying the *request* was wrong. Transport failures are not retried either: a connection that dropped mid-request may well have delivered it. |

`-F` exists because the upload endpoints are multipart-only, so without it the
one thing the CLI is for — never putting a credential in a shell variable — had
a hole in it exactly where files were concerned. It is not a doctrine breach: a
content-type is not an endpoint, and nothing here knows what any upload route
wants.

The multipart body is assembled **in memory**, not streamed from disk. A
streamed body is read once and cannot be replayed, and `--retry` and
`--wait-until` both re-issue the request — one whose body had evaporated would
retry as a zero-byte upload, which the server would accept.

Two refusals, both guarding a failure that is otherwise **silent**:

- **At most one thing may read stdin.** A pipe drains once, so `-d @-` beside a
  `-F x=@-`, or two `-F`s both naming `@-`, means the first gets the content and
  the rest get EOF — and an empty part is accepted, not rejected. The caller
  finds out much later that they uploaded nothing.
- **A caller-supplied `Content-Type` is reconciled, not honoured blindly.** The
  boundary is generated with the body, so a `-H` naming a different one produces
  a request the server parses as **zero parts and reports success**. A supplied
  `multipart/*` keeps its subtype and parameters with the real boundary
  substituted (`multipart/related` is legitimate); a supplied non-multipart type
  is refused, because the body demonstrably is multipart. See `formContentType`.

### Reading the answer

| Flag | Does |
|---|---|
| `-q`, `--jq` | Filter the JSON response through a jq expression (real jq, via `gojq` — see `internal/jqf`). Compiled **before** the request goes out, so a typo costs nothing. |
| `-r`, `--raw` | With `--jq`, print string results unquoted — the form that makes `$(...)` substitution work. Only strings are affected; a number or object still renders as JSON, as jq does. Refused without `--jq`. |
| `-o`, `--out` | Write the response body to a file, atomically and **0600**. stdout stays empty (the count goes to stderr). Streams unless a buffering flag is also set, so a large download is not held in memory. |
| `--fail-on-error` | Exit non-zero when a **2xx** carries a non-empty top-level `error`. Off by default. |

`--fail-on-error` closes the trap `ronja query` has always handled and this
command could not: some endpoints report failure *in the envelope*, so a query
that never ran answers HTTP 200 and a plain runner exits zero — and a pipeline
built on `&&` continues over a failure it never saw. It is opt-in because
reading inside the body means holding all of it, and the default path streams.
The field it reads is the API-wide error shape (the same one
`api.parseError` pulls out of a non-2xx), not endpoint knowledge, so it cannot
drift. A body that is not JSON — a CSV, a zip — has no envelope and is never
turned into a failure.

`--out` and `--jq` compose: the file gets the response **verbatim**, stdout gets
the filtered view. That is what each flag plainly means, so combining them makes
neither one lie.

### Waiting (`--wait-until`)

| Flag | Does |
|---|---|
| `--wait-until` | Re-send the request until this jq condition holds. **GET and HEAD only.** |
| `--wait-interval` | Pause between attempts. Default 3s, and **exact** — it does not back off. A caller who names a cadence has said what they want. Must be positive. |
| `--wait-timeout` | Bound on the loop as a whole. Default 5m; `0` waits indefinitely. |

This is the shape the doctrine explicitly makes room for: a stateful multi-step
loop HTTP does badly. Ronja is full of asynchronous state machines — workflow
runs, table builds, connector syncs, feature exports, managed-database
provisioning — and a verb per primitive would be a wrapper and would drift.
This knows only how to re-send a request the caller already wrote, so it covers
every one of them, including the ones added after it.

Three rules worth knowing:

- **A condition that produces no output is NOT satisfied.** "All of none" is
  vacuously true in logic and catastrophically wrong here: `.items[] |
  select(.done)` matching nothing would end the wait the instant an endpoint
  answered with an unexpected shape. See `jqf.Holds`.
- **jq truthiness surprises people**: `[]`, `0` and `""` are all **true**. Write
  an explicit comparison (`.errors | length > 0`), not a bare field.
- **An HTTP error ends the wait**, it does not retry it. A 404 on the polled
  path is a typo, not "not ready yet", and looping on it for five minutes would
  report a wrong path as a slow one. (429/5xx are handled by `--retry` when it
  is set.)

The response the condition **matched** is what gets emitted — nothing
re-fetches — so the printed answer is guaranteed to be the one that ended the
wait.

Behaviours that are contracts rather than conveniences:

- **The path must start with `/`.** A full URL is refused with a message saying
  the base URL is added automatically — that is the mistake the command's shape
  invites.
- **The path is passed through verbatim, query string included.** Nothing is
  escaped for you, so a caller percent-encodes anything that needs it; a raw
  space in a `?q=` produces a malformed request line and an opaque 400.
- **The response body goes to stdout verbatim, always** — including on an HTTP
  error, because an error body is the most useful thing the server said. That
  raw body is the machine-readable output, so `--json` is accepted and does
  nothing.
- **On HTTP >= 400**: the body still prints, exactly one line
  `HTTP <status> <METHOD> <path>` goes to stderr, and the exit code is
  non-zero. The one line is why `errAlreadyReported` exists in `root.go` — a
  described error would print a second, differently worded one underneath it.
- **Content-Type defaults to `application/json`** when there is a body, and
  `-H` overrides it. `Authorization` is overridable too, which is how someone
  tests a different credential without logging out.
- **The token is attached, never printed** — not in output, not in an error. A
  `url.Error` carries the method and URL and no headers; keep it that way.
- **The response is streamed as it arrives**, so a call that runs out of time
  has already written part of it to stdout. Raise `--timeout` for an endpoint
  that does real work rather than reading a truncated payload as the answer.
- **A redirect off the instance is refused, not followed.** See below.
- Reading stdin when stdin is a **terminal** is an error, not a wait. See
  `stdin.go`.

## Reading data (`ronja query`)

```
ronja query [flags] [sql]
```

Runs read-only SQL through `POST /api/v2/duckdb/query` and gives back CSV.
Tables are referenced as `{{ ref('<table id>') }}`, not by name.

```bash
ronja query "SELECT * FROM {{ ref('table-abc') }} LIMIT 10"
ronja query --file report.sql
cat report.sql | ronja query
ronja query --file report.sql --out rows.csv
```

The SQL comes from **exactly one** of: the positional argument, `--file <path>`
(`-` means stdin), or a pipe. Two sources is an error; no source with a
terminal on stdin is an error, never a prompt; blank SQL is an error before any
round trip.

| Flag | Does |
|---|---|
| `--file` | Read the SQL from a file, or from stdin with `-`. |
| `--out` | Write the CSV to a file, mode **0600** (tenant data, same posture as the credential store) and **atomically** — temp file plus rename, not `os.WriteFile`, whose mode applies only on *create* (a pre-existing 0644 file would stay 0644), which follows a symlink, and whose short write leaves a truncated file indistinguishable from a complete result. stdout then stays empty, and the narration ("N rows written to …") goes to stderr. |
| `--json` | Print the whole response envelope as one JSON object on stdout instead of the CSV. Combined with `--out`, the file gets the CSV and stdout gets the envelope. |
| `--jq` | Filter the **envelope** through a jq expression — `rowCount`, `truncated`, `error`, not the rows (those are CSV, and jq does not read CSV). Refused together with `--json`: two contradictory descriptions of stdout, and either precedence would silently discard a flag someone typed on purpose. |
| `-r`, `--raw` | With `--jq`, print string results unquoted. Refused without it. |
| `--max-rows` | `maxRows` on the request. Omitted means the server's default cap; the server clamps anything above its ceiling. |
| `--timeout` | How long to wait for the query. Default 2m; `0` waits as long as it takes; negative is refused. |

`--jq` filters the *decoded* envelope re-encoded, not the raw response — by that
point it is already a `QueryResult`. That is not a lossy round trip: the struct
carries no `omitempty` and mirrors the wire shape field for field, which is the
property `--json` already depends on.

**`--timeout` genuinely moves the ceiling, and that took a fix in the client.**
`http.Client.Timeout` is a ceiling a per-request context can only *shorten*, so
a `--timeout 10m` would otherwise have been silently capped at the client's 120s
and the query killed with a message about nothing — the same trap the split
read/slow timeouts fixed one level down. `Client.httpFor` therefore hands a
request whose deadline exceeds the ceiling a shallow copy of the client with
`Timeout` cleared (the Transport is shared, so the connection pool is not
duplicated), and `do` skips `context.WithTimeout` entirely for a non-positive
timeout — passing `0` there would produce an already-expired context, which is
the exact opposite of "no deadline". This matters because a large query is
routed to Batch compute server-side and is minutes by design.

The CLI always sends `format: "csv_meta"` — it is not a flag. CSV is the one
shape that is both legible at a terminal and consumable by every data tool, and
the "meta" half is what makes truncation and a failure distinguishable from an
empty result.

**The trap this command exists to close: a failed query is HTTP 200 with a
non-empty `error` field.** Nothing in the transport layer flags it, so
`ronja api -X POST /api/v2/duckdb/query …` exits *zero* on a query that never
ran, and a pipeline built on that continues over a failure it never saw.
`ronja query` treats a non-empty `error` as a failure: message on stderr,
non-zero exit. (With `--json` the envelope is still emitted first — it is the
answer to what was asked, and a caller left with only an exit code has to run
the query again to find out why.)

**Truncation is a note, not a failure.** `truncated: true` prints a notice on
**stderr** naming the row count and pointing at `--max-rows` / `--out` /
aggregating in SQL, and exits zero — the rows you got are real, there are
simply more of them. It is on stderr because stdout is a CSV somebody is
parsing.

## Managed databases (`ronja db`)

`ronja database`, aliased `db`, is the two managed-Postgres loops plain HTTP
does badly. Both are **USR_ADMIN**, as the underlying API is.

```bash
ronja db sql <database-id> "SELECT * FROM leads LIMIT 10"
ronja db sql <database-id> --file report.sql --out rows.csv
ronja db sql <database-id> "INSERT INTO leads (email) VALUES (\$1)" --params '["a@b.c"]'

ronja db migrate status --database <database-id>
ronja db migrate push   --database <database-id>
```

**Why these two and no others.** `db sql` clears the same bar `ronja query`
does — quoting, result shape, and an error that must not be silent. `db migrate`
is a stateful loop with a drift guard. Creating, listing and deleting a database
are single calls that `ronja api` runs perfectly well, so there is deliberately
no `db list` / `db create` / `db delete`.

**`db sql` is DML only.** It runs as the database's *write* role, which has no
CREATE privilege, so DDL is refused by Postgres itself rather than filtered
here. Schema changes go through `db migrate`, which runs as the owner and
ledgers what it did.

**Unlike `ronja query`, a failed statement is a real HTTP error.**
`POST /api/v2/database/:id/sql` answers **400** carrying the Postgres message —
deliberately not the duckdb endpoint's 200-with-an-`error`-field. So there is no
envelope to inspect and no trap to document: an ordinary non-zero exit covers
it, for `ronja api` and `curl` as much as for this command.

**`db migrate` keeps no local baseline and computes no hashes.** The migrations
folder is `migrations/*.sql`, applied in filename order, each file's stem being
its ledger name. `status` is `push` with `dryRun: true` — the server compares
the set against the database's own ledger and reports `pending` / `skipped` /
`drifted`. That is not merely less code: the ledger hash covers the submitted
bytes *verbatim*, and a client computing its own would have to agree about
whitespace forever — the first time it did not, every applied migration would
report as drifted. Drift is the one signal that must never cry wolf, so only the
thing that stored the hash is entitled to raise it.

`status` exits non-zero on drift, so it works as a CI gate without parsing.

**The binding is local-only** — `.ronja/database.json`, keyed by
`(url, tenantID)` through `wfdir.InstanceKey`, written only after a successful
`push`. It is deliberately *not* a committed `ronja.json`: `wfdir.FindRoot`
walks parents and `LoadManifest` refuses a non-`workflow` kind, so a committed
`kind: "database"` manifest at a repo root would break every `wf` command
beneath it, and a workflow manifest above a `migrations/` folder would break
`db migrate`. A shareable binding needs the manifest layer split properly; it is
not worth coupling to this.

## Workflow folders (`ronja wf`)

`ronja workflow`, aliased `wf`, develops one workflow from a local folder:
clone → edit → validate → push → test → publish. The code lives in the
customer's own repo and their own editor; Ronja holds the draft.

```bash
ronja wf clone wf_abc                  # or: ronja wf init --from script.py --feature col_abc
$EDITOR main.py
ronja wf validate                      # server-side check, saves nothing
ronja wf push                          # validates, then syncs the folder into YOUR draft
ronja wf test --param month=2026-07
ronja wf publish                       # commit, or submit for review, and say which happened
```

Two files describe the folder:

- **`ronja.json`** — the manifest, committed to the customer's git. Title,
  entrypoint, declared `parameters`, and an `instances` list — one entry per
  (instance, organization) the folder has been pushed to, so one folder can
  target a local backend and production, or two organizations on one instance,
  without any binding overwriting another. No matching entry means "never pushed
  there"; the first push creates the workflow and records the binding.

  ```json
  "instances": [
    {"url": "https://app.ronja.tech", "tenantID": "ten-…",
     "workflowID": "wf-…", "featureID": "col-…"}
  ]
  ```

  The organization is part of the key because workflow ids are scoped to one.
  Keyed by URL alone, a person in two organizations on one instance has a single
  entry for both, and a push made while signed in to the second sends the
  first's workflow id under the second's token. It is deliberately **not** keyed
  by profile name: this file is committed and shared, and your `prod` is not
  mine. A `tenantID` here is not a secret — it is an opaque identifier that
  appears in ordinary API paths and is useless without a credential.

  A signed-out `wf status` has no organization to match on; it falls back to
  matching on URL alone when that is unambiguous, and says so rather than
  guessing when it is not.

  `parameters` is the workflow's declared parameter set — what `wf test --param`
  supplies and the script reads with `tools.getVariable(name)`. Each entry takes
  `name`, `label`, `type` (`string` / `number` / `date` / `select`), and
  optionally `description`, `defaultValue`, `required`, `options`,
  `optionsQuery`. Push syncs the declaration; `status` reports it and flags a
  difference against the row.

  The key has **three states**, and the difference is load-bearing:

  | in `ronja.json` | means |
  |---|---|
  | absent | the folder does not manage parameters; push never touches them |
  | `[]` | managed, declaring none — push CLEARS the row's |
  | `[...]` | managed; push makes the row match |

  Absent is what every folder created before parameters existed looks like, so
  it has to mean "leave them alone" — reading it as "declares none" would make
  the first push after upgrading wipe the workflow's declaration. `init` and
  `clone` both write the key, so folders they create manage parameters from the
  start.
- **`.ronja/state.json`** — the sync baseline, never committed (`clone` and
  `init` write `.ronja/.gitignore` containing `*`). One entry per (instance,
  organization) binding, exactly like the manifest — the row the baseline came
  from and a sha256 per path — so a folder bound to localhost, staging and
  production keeps three, and a command acting on one instance touches only its
  own. It is local because drafts are
  per-user — a colleague cloning the repo must not inherit somebody else's
  baseline.

The verbs:

| Verb | Does | Flags worth knowing |
|---|---|---|
| `init` | writes the two files, optionally copying in a script you already have. Creates nothing server-side | `--from`, `--feature` (required), `--title` |
| `clone` | copies a workflow's files down into a new folder. Prefers your open draft over live when you have one. Creates nothing server-side | — |
| `status` | local changes, remote lifecycle/draft, and drift since the last sync. Read-only, and works signed out for the local half | `--json` |
| `validate` | posts the folder to `POST /workflow/validate`. Persists nothing; works before the workflow exists | `--json` |
| `push` | validates, ensures a draft (`checkout`, or `POST /workflow` on a first push), syncs files | `--no-validate`, `--force` |
| `test` | runs the draft and polls to completion | `--param k=v`, `--write-live`, `--stale-ok`, `--timeout`, `--logs` |
| `publish` | publishes a parentless draft, commits an attached one, or submits it for review | `--no-request-review` |
| `discard` | deletes your draft; the live workflow and your local files are untouched. A workflow that has never been published *is* its draft, so removing it needs `--delete-workflow` (a soft delete — it sits in the trash for 30 days) | `--yes`, `--delete-workflow` |

Four behaviours are deliberate and easy to undo by accident:

- **Push validates first.** The file-save path derives bindings from every file
  at once and reports the failure against whichever file you happened to be
  writing, so a stale untouched file makes an unrelated `PUT` fail naming an id
  and no file. One extra round trip buys an error that names the right file.
  `--no-validate` opts out.
- **Push refuses on drift.** The web builder edits the same per-user draft. A
  remote file that differs from the baseline stops the push with a per-file
  summary; so does a title or entrypoint someone renamed there, when the
  manifest disagrees with it. `--force` overwrites. A folder whose baseline
  predates metadata tracking is guarded on files only, not retroactively
  refused.
- **`--write-live` on test, and no y/N prompt.** A draft checked out from a live
  workflow inherits its output tables, and there is no test sandbox, so running
  it replaces a production table. A prompt would get muscle-memoried; a flag
  nobody types by accident does not. A workflow that binds no output tables yet
  — before you push a `{{ write }}` marker — runs without it; the save path
  re-derives the bindings on every push, so a converted script that writes a
  table needs the flag from its first push, published or not. An approval-gated
  workflow is refused outright rather than worked around — disabling the gate
  on the draft would ride into the parent on publish.
- **Exit zero means the thing finished.** `validate` exits non-zero on error
  findings, `test` only on a successful run. Ctrl-C during `test` stops the
  waiting, not the run — it is caught explicitly so the person is told that.

There is deliberately no `wf list` and no `wf delete`: discovery stays on HTTP.
`discard` is a sync verb rather than a resource one — it operates on the loop's
own artifact, your draft.

The HTTP loop this wraps is documented for agents in the `workflow-dev` API
guide (`backend/lib/api/openapi/guides/workflow-dev.md`, served at
`/docs/api/guides/workflow-dev.md`). Keep the two in step.

## Data-app folders (`ronja app`)

`ronja app`, aliased `dataapp`, is the same loop for a data app: clone → edit →
push → publish. It shares the whole folder model with `wf` (same `ronja.json`,
same `.ronja/` baseline, same per-instance bindings, same drift guard), and
`ronja.json`'s `kind` is what keeps the two apart — running `wf push` in a
data-app folder is refused rather than allowed to overwrite TSX with Python.

```bash
ronja app clone data_app-abc           # or: ronja app init --from Revenue.tsx --feature col_abc
$EDITOR App.tsx
ronja app push                         # syncs the folder into YOUR draft, then compiles it
ronja app validate                     # recompile the draft on its own
ronja app publish                      # commit, or submit for review, and say which happened
```

**`App.tsx` has to mount itself.** It must end with:

```tsx
createRoot(document.getElementById("app")).render(<App />);
```

A single-file app that only defines and exports a component is **refused**: the
compiler runs a mount check on the path every write, `validate` and `publish`
funnel through, so `push` prints `Compiles: NO` naming the missing call rather
than letting you ship a white screen.

Do not lean on that check. It is a deliberately LOOSE scan — it passes as soon as
*any* file in the folder contains *any* mount-shaped token (`createRoot`,
`hydrateRoot`, `ReactDOM`, `.render(`, `getElementById`, `querySelector`,
`document.body`, `innerHTML`), because a false positive would refuse to publish a
working app while a false negative costs one visible white screen. So a
multi-file app with a stray `querySelector` in some helper and no real mount call
compiles green and ships a blank page. And nothing static can promise that
`Compiles: yes` renders anyway — a component that mounts and returns `null`, or
throws on first render, publishes green too. `status` prints the app's URL; open
it.

### Four things that are not like a workflow

These are not arbitrary; each falls out of how data apps work server-side.

Creation is no longer one of them. `POST /dataapp` used to mint an empty LIVE
row, because the lifecycle trigger had no state for a parentless draft to be in;
migration 000495 added the branch workflows have always had. A first push now
creates an unpublished draft only you can see, publish promotes that same row in
place (the id never changes), and an abandoned first push leaves nothing behind.

- **The entrypoint is fixed at `App.tsx`.** `rdataapp.Patch` carries no
  entrypoint field: it is stamped at create and never moves. The manifest
  records it, push never syncs it, and a manifest naming a different one is
  refused rather than silently written elsewhere.
- **What the app may read lives in `ronja.json`.** A workflow's bindings are
  derived from `{{ ref }}` / `{{ secret }}` markers in its code. A data app's
  are explicit grants, and `POST :id/validate` does not scan source for them —
  so an app pushed without them compiles and can then query nothing. The
  `access` block is the folder's declaration:

  ```json
  "access": {
    "allowedTableIDs": ["table-abc"],
    "allowedSecretIDs": [], "allowedAgentIDs": [], "allowedWorkflowIDs": [],
    "allowedCodexIDs": [], "allowedMetricIDs": [], "capabilities": []
  }
  ```

  It has the same **three states** `parameters` does — absent means "not
  managed, push never touches them", present means "push makes the row match" —
  for the same reason: reading absent as "grants nothing" would revoke a
  working app's access on the first push after upgrading. Because these are
  privileges rather than settings, `push` and `status` list every id being
  granted or revoked instead of reporting that a difference exists.
- **Every file write recompiles the whole bundle.** A push of N files is N
  esbuild compiles and N bundle uploads, so it takes a few seconds per file and
  says which file it is on. It also means intermediate states that do not
  compile are NORMAL — `App.tsx` importing a module one request away — so a
  compile failure mid-push is reported and not fatal. The file is saved either
  way; the compile check at the end is the verdict, and it is what decides
  whether `publish` will work.

  ⚠️ `Compiles: yes` means the bundle BUILDS, not that it type-checks. esbuild
  strips TypeScript types rather than verifying them, so a wrong prop type or a
  component you never defined compiles clean and fails in the browser. What the
  check does catch is what stops a bundle existing at all: syntax errors, and
  imports that resolve to nothing (a typo'd path, a file you have not pushed).
- **There is no `app test`, and `app validate` means something different.**
  A data app has nothing to run headlessly; `status` prints its `/apps/<id>` URL
  instead. And `wf validate` can check a candidate before the workflow exists,
  because `POST /workflow/validate` persists nothing — data apps have no such
  endpoint, so `app validate` compiles the draft that is already on the server,
  and warns when your folder holds changes you have not pushed.

### The verbs

| Verb | Does | Flags worth knowing |
|---|---|---|
| `init` | writes the two files, optionally copying in a component you already have. Creates nothing server-side | `--from`, `--feature` (required), `--title` |
| `clone` | copies an app's files down into a new folder, `access` included. Prefers your open draft over live. Creates nothing server-side | — |
| `status` | local changes, remote lifecycle/draft, whether the draft compiles, allowlist differences, and drift. Read-only, works signed out for the local half | `--json` |
| `push` | ensures a draft, syncs allowlists then files, then compiles | `--no-validate`, `--force` |
| `validate` | recompiles your draft and reports diagnostics | `--json` |
| `publish` | commits the draft, or submits it for review. Refuses a draft that does not compile | `--no-request-review` |
| `discard` | deletes your draft; the live app and your local files are untouched | `--yes`, `--delete-app` |

An app that has never been published *is* a draft, with no live version behind
it, so discarding it would delete the app. That is refused unless you pass
`--delete-app`. The case it exists for is an abandoned first push — most often
one that failed to compile — where the alternative would be an app nothing in
the CLI can reach. It is a soft delete: the app sits in the trash for 30 days
before it is purged. Local files are kept and the folder is unbound, so the next
push creates a fresh app in the same feature.

Push does things in an order that is the reverse of `wf` in one place and
different in another, both deliberately:

1. **Allowlists before files.** The compiler validates a secret reference
   against `allowedSecretIDs`, so a file that arrives before its grant fails to
   compile for a reason that is nowhere in the file.
2. **Entrypoint LAST**, where a workflow writes it first. A workflow leads with
   it because renaming one needs the new file to exist before the row can name
   it — which cannot happen here. What entrypoint-last buys instead is cost: a
   fresh app's intermediate compiles fail before the bundle upload rather than
   after it.

`node_modules/`, `dist/` and `build/` are never synced, and are reported as
skipped rather than silently dropped. A data app holds at most 100 files and
5 MiB per file; both are checked locally so an oversized folder is refused
before the first request rather than half-way through a sync.

## Design rules

These are load-bearing for the agent use case — please keep them true:

- **Sync verbs yes, resource verbs no.** A stateful filesystem-to-resource loop
  earns a command; wrapping a call does not.
- **Transport may be a command; endpoints may not.** `ronja api` is allowed
  precisely because it describes nothing and so cannot drift. A subcommand that
  names a route, a resource kind or a field is the wrapper this rules out. See
  "Transport is not a wrapper" above.
- **Never prompt without a TTY.** A missing input must be an error, not a hang.
- **`--json` on everything**, and only ever on stdout. Human narration goes to
  stderr so `--json` output stays a single parseable object.
- **Non-zero exit on failure**, with the reason on stderr.
- **`RONJA_URL` / `RONJA_TOKEN` outrank the credential file**, so a CI job never
  inherits whoever last logged in on that machine.

## Layout

```
cmd/ronja/           main package — its NAME is what makes the binary `ronja`
internal/commands/   cobra command tree (root, auth, context, env, api, query,
                     stdin, workflow_*)
internal/api/        HTTP client + the endpoint shapes it mirrors, plus raw.go
                     (transport only, mirrors nothing) and query.go
internal/config/     the CLI config file (os.UserConfigDir()/ronja/config.json),
                     one profile per (instance, organization), mode 0600
internal/wfdir/      the workflow folder: ronja.json, .ronja/state.json,
                     local enumeration and the sha256 drift baseline
```

The workflow commands are one file per verb (`workflow_push.go`,
`workflow_testcmd.go`, …) with the shared folder plumbing in `workflow.go`;
their endpoint mirrors live in `internal/api/workflow.go`, `workflow_write.go`
and `workflow_run.go`, hand-mirrored against
`backend/api/v2/workflow/handler.go`.

`internal/api/raw.go` is the odd one out: it deliberately mirrors nothing, and
its response body carries the request's context cancellation on `Close` so a
streamed response is not truncated when `DoRaw` returns. `RawResponse` carries
`Status`, `Header` and `Body` and nothing else — the header map was added back
only when `--retry` needed `Retry-After`, and a retry that ignores the interval
a rate limiter just named is how a soft limit becomes a hard one.

`internal/jqf` is the jq layer both `--jq` and `--wait-until` sit on: compile
once, apply per response, render. It wraps `gojq` rather than a hand-rolled
path extractor, and the reason is the flag's name — a caller who writes the
expression jq documentation teaches must get jq's answer or a clear parse
error, never a subtly different one from a lookalike implementing the easy half
of the syntax. `jqf.Holds` is the poll condition's truth rule and the one piece
worth reading before touching it: zero outputs is **not** satisfied.

`internal/commands/api_output.go` holds everything `api` does with a response —
the send-with-retry loop, the wait loop, and the emit path. Note
`apiOutput.buffered()`: the streaming default is preserved except for the flags
that genuinely have to read *into* the document, and that method is where the
line stays drawn.

`internal/commands/outfile.go` is the atomic 0600 writer shared by both `--out`
flags. Its comment explains the three ways `os.WriteFile` fails at this job.
`internal/api/query.go` mirrors `POST /api/v2/duckdb/query` — keep it in step
with that handler, and note that its `QueryResult` fields carry no `omitempty`
because the struct is emitted verbatim by `ronja query --json`.

`internal/commands/stdin.go` holds the terminal guard both `api -d @-` and
`query` use. Its `isTerminal` is a package var so tests can reach the
"attached to a terminal" branch without allocating a pty — the branch that
turns a silent hang into an error, which is the worst failure mode a CLI can
hand an agent.

Alongside `config.json` you may see a short-lived `config.json.lock`. Every write
to the credential store takes it so two concurrent `ronja login` runs cannot
each read the same snapshot and drop the other's token. It covers **name
allocation** as well as the write: two fresh logins that derive the same
provisional name must not both take it, or one silently overwrites the other. It is removed when the write finishes, and a lock left behind by
a killed process is taken over automatically after a couple of minutes — but if
a command ever reports timing out on it and you are sure nothing else is
running, deleting the file is safe.

**There is no command whose job is to print the token, and there should not be
one.** The credential is *loaded*, not displayed:

```bash
eval "$(ronja env)"    # sets RONJA_URL + RONJA_TOKEN; eval consumes the output
```

Being able to READ a credential and having it ECHOED are different things, and
only the second puts it in a terminal scrollback, a CI log, or an agent
transcript. `ronja env` does print the token if you run it bare — that is
unavoidable for a command meant to be eval'd — so it warns on stderr when
stdout is a TTY, and every doc shows it only through `eval`.

**Shell state does not persist between separate agent tool calls.** Each call is
usually a fresh shell, so `eval "$(ronja env)"` in one and `$RONJA_TOKEN` in the
next reads an empty variable. The documented form keeps them together:

```bash
eval "$(ronja env)" && curl -H "Authorization: Bearer $RONJA_TOKEN" "$RONJA_URL/api/v2/..."
```

`ronja api` sidesteps that entirely for a one-off request — the credential is
loaded inside the process and never reaches a variable, an interpolation or a
transcript:

```bash
ronja api /api/v2/authentication/me
```

For non-shell callers the credential file is still the answer: `ronja context`
reports its path and the exact `profiles["<name>"].token` key — the name is
chosen at login, so it has to be read rather than assumed.

If you are tempted to add `ronja auth token` back for convenience, that is the
convenience being traded away on purpose.

`RONJA_CONFIG_DIR` relocates the credential file; the tests use it to stay off
the developer's real profile.

## Tests

```bash
go test ./...
```

Poll timing is a client field (`MinPollInterval` / `PollBackoff`) so tests drive
the device-flow state machine without waiting on real intervals.

CI runs them in `.github/workflows/cli_test.yml` (this module only — the backend
workflow is scoped to `backend/**` and would never have seen these). It also
asserts the installed binary is named `ronja`, which is the invariant the
`./cmd/ronja` package path exists to protect.
