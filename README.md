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

The exceptions are the four sync loops — `ronja wf` (workflows), `ronja app`
(data apps), `ronja pipeline` (derived tables) and `ronja db` (managed-database
migrations), each below. The rule they keep is **sync verbs yes, resource verbs
no**: the CLI may own a
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

Homebrew puts it somewhere already on your `PATH`. After `go install` it lands
in the Go bin directory, which often is not:

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

### When a release fails part-way

The mirror is tagged before GoReleaser publishes, because a GitHub Release
attaches to a tag ref and `go install ...@vX.Y.Z` resolves the module by
fetching the repo at that tag. So a failure *after* the tag push leaves the
version claimed, and the "Refuse to republish" guard then blocks a re-run at
that same version.

**That guard is deliberate — releases are immutable.** Re-running into a
half-published version is how you get a tag whose assets do not match its
source, and a `brew` user with a checksum that does not verify. The intended
recovery is to fix the cause and cut the next patch version.

If the failure was transient and nothing was published, the version can be
reclaimed by deleting the tag, the release, and the sync commit:

```bash
gh release delete vX.Y.Z --repo ronjatech/ronja-cli --cleanup-tag --yes
```

Check the tap too (`ronjatech/homebrew-tap`) — if the cask was already
pushed, the version is not reclaimable and you want the next patch instead.

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
| `CLI_DEPLOY_APP_ID` | Environment **variable** | the App's numeric ID (not sensitive) |
| `CLI_DEPLOY_APP_PRIVATE_KEY` | Environment **secret** | the generated `.pem`, pasted whole |

The job declares `environment: production` to reach them, matching
`releaser_prod.yml`. Scoping them there keeps the private key — which can mint
write tokens for two public repos — reachable only from a job that opts in,
rather than from every workflow in this repository.

⚠️ The names must **not** start with `RONJA_`. `releaser_dev.yml` /
`releaser_prod.yml` sweep every `RONJA_*` var and secret visible on the
`production` environment into the deployed workload's container env — that
prefix means *application config*. Named `RONJA_CLI_DEPLOY_APP_PRIVATE_KEY`
this key was both shipped into the running production workload and, being a
multi-line PEM, broke the sweep's line-based `$GITHUB_ENV` write and failed
the entire backend deploy with `Invalid format '***'`. The sweep now uses the
heredoc form so a multi-line value can no longer break it, but the naming rule
stands on its own: a CI-only credential never takes the `RONJA_` prefix.

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

Approval is gated at `USR_USERRO` **plus** an assertion that the caller is a
*user* principal (`sherlock.CurrentActor(ctx).Kind == ActorKindUser`) with a
non-empty tenant. Gating it at `USR_ADMIN` — the bar for the raw `POST /token`
mint — would have made the CLI admin-only; a PAT is safe at the lower bar
because it carries its bound user's live role and so can never exceed them. A
plain `api_token` is refused outright, because `Who()` is then the token id
rather than a row in `users`.

> ⚠️ **Approving is therefore not session-only.** A PAT holder can drive the
> whole handshake headlessly and mint fresh, independently-revocable *child*
> PATs. This is accepted — but note revoking the parent does **not** revoke the
> children.

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
id=$(ronja api -X POST /api/v2/workflow -d @wf.json --jq '.id' -r)

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

⚠️ **`--jq` takes a value, so `--jq -r '.id'` binds `-r` as the expression** and
leaves `.id` as a stray positional — which cobra reports as *"accepts 1 arg(s),
received 2"*, a message about argument counts for a mistake that is entirely
about a flag. Both `ronja api` and `ronja query` refuse it by name and show the
working form (`--jq '.id' -r`). The check reads the command's own flag set
rather than a list, so a flag added later is covered, and a legitimate
dash-leading expression (`-1`) still runs.

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
| `--jq` | Filter the **envelope** through a jq expression — `rowCount`, `truncated`, `zoneUsed`, `error`, not the rows (those are CSV, and jq does not read CSV). Refused together with `--json`: two contradictory descriptions of stdout, and either precedence would silently discard a flag someone typed on purpose. |
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

**The reporting timezone is stated on every successful run.** The envelope
carries `zoneUsed` (the IANA timezone the query's DuckDB session actually ran
at), and the CLI prints it on **stderr** as `Reporting timezone: <zone>` —
before the truncation note, so the calendar is stated before any caveat about
the rows. It is narration, not a warning, and it exits zero.

It is said out loud every time rather than only when something looks wrong,
because the asymmetry is invisible from either side: the CLI sends no client
timezone, so it reads at **UTC**, while the same query in the app reads at the
signed-in user's timezone. Any rollup bucketed by day, week or month therefore
falls on different boundaries on the two surfaces — the numbers differ, both are
correct, and nothing else in the output says why. Check `zoneUsed` first when
CLI numbers disagree with the app. (With `--json` the field is in the envelope
too; `--jq .zoneUsed` reads it.)

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
ronja wf clone workflow-abc            # or: ronja wf init --from script.py --feature collection-abc
$EDITOR main.py
ronja wf validate                      # server-side check, saves nothing
ronja wf push                          # validates, then syncs the folder into YOUR draft
ronja wf test --param month=2026-07
ronja wf publish                       # commit, or submit for review, and say which happened
```

A **durable** workflow (`ronja wf init --runtime 2`) adds one verb to that loop:
`ronja wf test --resume` continues a failed run instead of starting over. See
"Durable workflows" below.

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

  `runtime` is the workflow's runtime version — `2` for a **durable** workflow,
  absent for the default. Unlike `parameters` it is a plain int with no
  three-state pointer, because the third state has nothing to describe: the
  runtime is *stamped at create* and the server has no patch for it, so
  "unmanaged" and "declares the default" both mean "send nothing, get 1". The
  key is written only for `--runtime 2`, which keeps a v1 folder's `ronja.json`
  byte-identical to what the CLI wrote before durable workflows existed.

  `reportingTimezone` is the **calendar the workflow's runs execute on** — the
  IANA zone its DuckDB session is set to, so it is what `date_trunc`,
  `CAST(ts AS DATE)` and `AT TIME ZONE` bucket against inside `{{ ref() }}`
  queries, i.e. which day or month a timestamp near midnight lands in. Every run
  reads it, so a scheduled run and one you start by hand produce the same
  numbers; without a declaration the calendar follows whoever triggered the run,
  which for a workflow that writes a table means the stored table changes shape
  depending on who ran it last. It is stored as a fixed value, so changing the
  organization default later does not move an existing workflow.

  It has the same **three states** `parameters` does, with one wrinkle worth
  knowing before hand-editing it:

  | in `ronja.json` | means |
  |---|---|
  | absent | the folder does not manage the calendar; push never touches it |
  | `""` | managed, declaring the RESET — push sets the row to an explicit `UTC` |
  | `"Europe/Stockholm"` | managed; push makes the row match |

  `""` is the reset, **not** "declares none" — the row ends up holding the
  literal `UTC`, which is a real declaration. There is no way to put a row back
  to declaring nothing: an empty value on the server means the row has never
  declared a calendar — because it predates the field, or because it was created
  while the organization had no default — and it falls back to the caller's zone
  at run time. Because `""` and `UTC` are the same request,
  `status` compares the two on their *effective* value and reports no difference
  between them.

  `clone` writes the key when the workflow declares a calendar and leaves it out
  when the row declares none — writing it for that row would make the very first
  push stamp UTC onto a workflow nobody asked to change. **`init` deliberately
  does not write it at all**, unlike `parameters`: the only value it could write
  without a round trip is `""`, which means "declare UTC", so every fresh folder
  would silently override its organization's default. Absent means "inherit it",
  and the create then takes the organization default.

  `push` reports the change in words (`updated  the reporting timezone to "…"`,
  in the same list as the pushed and deleted files) and the drift guard covers
  it like any other metadata field: a calendar changed in the web builder since
  your last sync stops the push rather than being overwritten. To change a
  workflow's calendar without the CLI, ask Ronja in chat or
  `PUT /api/v2/workflow/<id>`.

  **Against an older instance the key is READ BACK, not assumed.** An instance
  that predates the workflow calendar accepts `reportingTimezone` in the request
  body, ignores it, and answers success — unknown JSON keys are dropped, not
  refused. So after changing the calendar `push` re-reads the workflow: if the
  row does not hold what was sent, it warns (`this server does not support
  reportingTimezone — the key was ignored`, plus `reportingTimezoneIgnored` in
  `--json`) and records **what the server holds**, never what it sent. That is
  what keeps `wf status` honest — it goes on reporting the calendar as unsynced,
  and a later push against an upgraded instance applies it. Everything else in
  such a push still lands normally.

  ⚠️ **An older `ronja` CLI silently STRIPS the key from `ronja.json`.** The
  manifest is decoded into a typed struct and written back whole, so a version
  of the CLI that does not know `reportingTimezone` drops it from any file it
  rewrites — the first `push` of a new folder (which records the binding) and
  `wf discard` both rewrite the manifest. A hand-added declaration can therefore
  disappear from a committed file with no message. Everyone working on one
  folder wants the same CLI version; `ronja version` reports it.
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
| `init` | writes the two files, optionally copying in a script you already have. Creates nothing server-side | `--from`, `--feature` (required), `--title`, `--runtime` |
| `clone` | copies a workflow's files down into a new folder. Prefers your open draft over live when you have one. Creates nothing server-side | — |
| `status` | local changes, remote lifecycle/draft, and drift since the last sync. Read-only, and works signed out for the local half | `--json` |
| `validate` | posts the folder to `POST /workflow/validate`. Persists nothing; works before the workflow exists | `--json` |
| `push` | validates, ensures a draft (`checkout`, or `POST /workflow` on a first push), syncs files | `--no-validate`, `--force` |
| `test` | runs the draft and polls to completion | `--param k=v`, `--write-live`, `--stale-ok`, `--resume`, `--timeout`, `--logs` |
| `publish` | publishes a parentless draft, commits an attached one, or submits it for review | `--no-request-review`, `--overwrite-remote` |
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
  refused. That check is LOCAL and happens once, so each *file* write carries a
  server-side precondition of its own — see "Two editors, one workflow" below.
  The metadata patch does **not**: `PUT /workflow/:id` takes no precondition, so
  the title/entrypoint/parameters half of this guard is the local drift check
  alone, and a rename landing between that check and the patch still wins
  silently.
- **`--write-live` on test, and no y/N prompt.** A draft checked out from a live
  workflow inherits its output tables, and there is no test sandbox, so running
  it replaces a production table. A prompt would get muscle-memoried; a flag
  nobody types by accident does not. A workflow that binds no output tables yet
  — before you push a `{{ write }}` marker — runs without it; the save path
  re-derives the bindings on every push, so a converted script that writes a
  table needs the flag from its first push, published or not. An approval-gated
  workflow is refused outright rather than worked around — disabling the gate
  on the draft would ride into the parent on publish.
- **Exit zero means the thing did not fail.** `validate` exits non-zero on error
  findings; `test` exits non-zero on a failed run, on a timeout, and on a status
  it does not recognise — but zero on a run that finished AND on a durable run
  that parked (`waiting`) or was handed to a successor (`resuming`), neither of
  which is a failure. Ctrl-C during `test` stops the waiting, not the run — it is
  caught explicitly so the person is told that.

### Durable workflows (`--runtime 2`)

A durable workflow journals the result of every `@tools.step` under a key
DERIVED from the function and its arguments, so a failed run can be **resumed**
instead of re-run: the journaled steps are replayed and only the work that never
finished executes again.

Resume itself is not durable-only. On the standard runtime a step is journaled
when the author gives it an explicit key (`tools.step("key", fn, ...)`), and
`--resume` replays exactly those. What runtime 2 changes is that nobody has to
write a key — so a loop journals per item, and forgetting one stops being
possible.

```bash
ronja wf init --feature collection-abc --runtime 2   # manifest key + a durable main.py
ronja wf push                                 # the create stamps the runtime
ronja wf test                                 # it fails somewhere in the middle
$EDITOR main.py && ronja wf push              # fix the bug
ronja wf test --resume                        # continue from where it stopped
```

Three things are worth knowing, and each is a place the obvious behaviour would
be wrong:

- **The runtime is stamped at create, and only there.** There is no patch path
  server-side, so the manifest's `runtime` rides the `POST /workflow` body of
  the push that creates the workflow and is never sent again. A folder that
  declares `runtime: 2` against a workflow that already exists is *not* an
  error — it simply changes nothing, because nothing can. Getting it wrong
  means a new folder, not a flag.
- **`--resume` finds its target on the SERVER.** It reads
  `GET /workflow/:id/runs` (newest first, explicitly ordered — the endpoint has
  no default ordering) and resumes the most recent **failed** run of the row
  `wf test` runs, which is your draft. Nothing is remembered locally: a run id
  in `.ronja/` would be wrong the moment the workflow was run from the web
  builder, an automation or a colleague's checkout, and missing exactly when it
  is most wanted — after the clone that follows a failure somebody else saw.
  With no failed run it refuses and says so; it never quietly starts a fresh one.
  It does **not** refuse a standard-runtime workflow — that workflow may have
  explicitly-keyed steps to replay — but it does say so before resuming, because
  a resume that finds nothing journaled simply re-runs everything.
- **A resume inherits the original run's parameters**, so `--param` with
  `--resume` is refused rather than dropped. That is the server's rule — changed
  parameters would invalidate every step result computed under the old ones —
  and a CLI that silently discarded the flag would hide the mistake the rule
  exists to catch. A resume runs the workflow's **current** files, which is what
  makes fix-then-resume the intended loop.

A failed durable run ends its report with the resume invocation and `N steps
journaled — a resume skips them`. That N is the response's `journalEntries` —
the size of the lineage's journal, which the server derives on read and sends
only for a failed run — and deliberately **not** `len(steps)`: steps are the
observable timeline (run spans), a different set, and printing one as the other
would be a confident wrong number in the one place somebody is deciding whether
to trust the skip. No `journalEntries` means no line, rather than "0 steps
journaled" — which reads as a fact about the journal when it is equally the
answer from an instance that does not send the field.

In a resumed run's step timeline, a step whose result came out of the journal
reads **`replayed`** rather than `skipped` — the status a continue_on_error step
that never ran also carries.

A durable run can also **park**: it hands work to an agent or waits on a timer,
the burst finishes cleanly with its outputs persisted, and the row sits at
`waiting` until something wakes it. `wf test` ends its poll there and exits
zero, and both halves are deliberate. Waiting a park out is not on offer — it
lasts until a colleague answers or a timer fires, which is days rather than
minutes — and reporting it as a failure would stop a pipeline over a workflow
doing exactly what it was written to do. So the report names the park, prints
what ran before it, and offers neither of the usual closing hints: `publish`
would invite taking code live on the strength of a run that has not finished,
and `--resume` would offer to continue a run that nothing has stopped. A poll
can also land on `resuming` — the single-winner latch between "a wake decided to
continue this run" and "the successor run exists". That is terminal for the row
being polled and is not a failure either, so it exits zero too, reported as work
that continued in a NEWER run: the id in the report is no longer the one to
follow.

Whether a workflow is durable is read off the **row** (`runtimeVersion`), not
the manifest: the row is what the run funnel reads, and it is right about a
workflow this folder did not create. The manifest is the fallback for one case
only — an instance predating durable workflows sends no `runtimeVersion`, and a
0 means "the instance did not say", not "runtime 1".

`wf init --runtime 2` also writes a `main.py` in the shapes a resume depends on
(decorated steps, keys derived rather than written, `tools.now()`, handles
across step boundaries). A durable workflow scaffolded from v1 code journals
nothing and fails silently: the run works, and the resume that was the point of
it does not.

### Two editors, one workflow

The drift guard above is a local comparison against a file listing read a few
requests earlier, so it cannot see a change that lands *while* the push is
running — and two agents driving `wf` at once is now an ordinary thing to have
happen. Two server-side compare-and-swap layers close that window. Neither is a
lock, and neither makes an overwrite impossible: they make it **explicit**.

**`ronja app` has neither of them.** Everything in this section is workflows
only: `PUT /dataapp/:id/files/*path` takes no `baseSha256`, and `app publish`
has no `--overwrite-remote`. A data-app folder is guarded by the local drift
check alone, so a change landing while a push is running still wins silently.

- **Every file write carries a precondition.** `push` sends `baseSha256` with
  each `PUT` and `DELETE` — the sha256 the local baseline recorded the server as
  holding — and the server checks it inside that write's own transaction. A path
  the baseline has never seen asserts the **empty string**, which means "this
  file must not exist yet". A mismatch is an HTTP **409**: the write is rolled
  back, the file keeps the other person's content, and the push stops naming the
  file. Nothing is retried and nothing escalates to `--force` for you — the
  whole value of the refusal is that a human decides what happens to the change
  it just protected.

  Two cases send **no** precondition, and they are really one case: the drift
  guard was already bypassed, so the baseline demonstrably does not describe the
  row, and a precondition built from it could only refuse a difference the push
  was told to proceed past. Those are `--force` — whose entire meaning is
  "overwrite the remote with what I have", so a 409 there would be the flag
  refusing itself — and a workflow this push just **created**, which has no
  baseline at all and whose create *seeds* a starter file at the entrypoint
  inside its own transaction, so "must not exist" would refuse a workflow the
  CLI made seconds earlier. Resuming a crashed first push is that same seed one
  command later, and is suppressed with it.

- **`publish` refuses to commit onto a parent that moved.** A draft records
  which committed version it was forked from. If somebody has published a new
  version since, the commit is refused with a 409 and **nothing is written** —
  your draft is completely intact, it simply was not applied. The refusal names
  the current version and, when the server can tell, the files that changed.

  The good way out is to look at what they did and re-apply your work on top of
  it: `ronja wf discard`, then clone again. `--overwrite-remote` is the other
  way, and the name is deliberately unpleasant. It reads the current head from
  `GET :id/versions` (element 0, or the workflow's own id when it has never been
  versioned), confirms that exact id back to the server, and commits over their
  version. **It never loops.** A third commit landing between reading the head
  and confirming it produces a fresh refusal rather than a second attempt: an
  override authorises discarding the version you were *shown*, not whatever
  happens to be there by the time the request arrives.

  The version id is read from the API and never scraped out of the 409's text.
  The HTTP error body is the flat app-wide `{"error": "…"}` shape with no
  structured payload, and a client that regexes an id out of prose starts
  overwriting the wrong version the day somebody rewords the message.

  A 409 is deliberately kept out of publish's needs-review fallback, which is
  keyed on a **400**. Filing a review request for a draft built on stale code
  would report a publish that never happened as a successful one.

Under `--json`, a publish that overwrote something carries
`overwroteVersionID`. It is absent on every ordinary publish, which is what
makes its presence meaningful: it is the record that somebody else's work was
discarded, and by which version.

**A conflict is machine-readable on both commands**, because an agent driving
the CLI has to tell "somebody committed first, re-apply and try again" from
"you may not do this at all" — and both are a non-zero exit with prose on
stderr.

- `publish --json` extends its `outcome` vocabulary with a third value,
  `conflict`, and emits the payload **even though the command fails** (`error`
  carries the server's message verbatim). The other two values,
  `published` and `submitted_for_review`, are unchanged and still ride a zero
  exit; `conflict` is the only one that does not. A caller that knows only the
  original two still behaves correctly — the exit code is non-zero and the
  outcome is simply not one it recognises.
- `push --json` carries `conflict: true` when a file precondition is what
  stopped it. The rest of the payload still describes what *did* land, which is
  the state the draft is now in.

Do not branch on the `error` text on either command. It is the server's prose,
it names versions and files precisely so a human can read it, and it is
reworded whenever that reads better.

**Status, push and publish print a link.** A successful `push` reports the
draft's page and a successful `publish` the live workflow's, in the same
key-value column as the ids above it; `status` reports the **workflow** the
folder is bound to, whoever has a draft open — and only when it actually
reached the server, so a signed-out or unbound status has no `URL:` line at
all:

```
  Draft:    workflow-def
  Target:   Acme on https://api.acme.ronja.tech
  URL:      https://acme.ronja.tech/workflows/workflow-def?org=tenant-acme
```

The link is whatever the **server** returned on the response the command
already holds — the CLI never assembles one. The instance URL a profile records
is the *API* origin, and a deployment puts the backend on `api.*` and the
frontend on `app.*`; in local dev the backend serves no frontend routes at all.
So a locally-built link points at a host that serves no pages, which is the bug
this replaced (the server had the same one from its own side, now fixed in
`backend/lib/deeplink`).

The `?org=` on the end is the server's doing too. No frontend route carries an
organization — the selected one is a cookie — so the link names the org the
resource lives in, and a reader who opens it while signed into a different one
is asked whether to switch rather than shown a resource-not-found page. It is a
hint and nothing more: it grants no access, and the app never acts on it without
a click. A response built on a context with no tenant degrades to a param-less
link (still a working link for a single-org reader), so a `URL:` line without
one is not a bug.

An instance with no frontend origin configured returns no `url`, and then there
is simply no `URL:` line — no warning and no failure, because there is nothing
the reader could act on. That is the ordinary state of a plain dev box. Under
`--json` nothing extra is printed and the payload is unchanged — including
`status`, whose `url` key has always meant the *instance*, not the page. A
script that wants the link reads it off the API response, which is where this
came from: `ronja api /api/v2/workflow/<id> --jq .url`.

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
ronja app clone data_app-abc           # or: ronja app init --from Revenue.tsx --feature collection-abc
$EDITOR App.tsx
ronja app push                         # syncs the folder into YOUR draft, then compiles it
ronja app validate                     # recompile the draft on its own
ronja app test                         # render the draft headless and report what the browser saw
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

**Forking a kit component is a local file copy.** Every app compiles against an
embedded component kit (the shadcn/ui primitives and Ronja's operator
components, imported as `@/components/ui/button` and friends), and an app file
at the same path *shadows* the kit's for the whole bundle — so customisation is
expressed by having your own `components/ui/button.tsx`, and no import anywhere
changes. `ronja app fork components/ui/button.tsx` writes exactly that file,
byte-identical to what the compiler currently resolves, reading it from the
instance's public `/docs/api/kit/<path>` docs route (the full list is
`/docs/api/kit.md`). It changes **nothing on the server** — no draft, no row —
so the loop is fork → edit → `ronja app push`. An existing file at that path is
never overwritten; that file already *is* the fork, and deleting it goes back to
the built-in component.

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
- **`app validate` means something different, and `app test` is a report rather
  than a run.** `wf validate` can check a candidate before the workflow exists,
  because `POST /workflow/validate` persists nothing — data apps have no such
  endpoint, so `app validate` compiles the draft that is already on the server
  and warns when your folder holds changes you have not pushed. And `wf test`
  RUNS a workflow, where `app test` renders the draft in a headless browser and
  reports what it saw: it is perception, not a gate, so it exits **zero** on an
  app full of runtime errors (see below). `status` still prints the app's URL as
  the server returned it — and no link at all when it never reached the server,
  or when the instance has no frontend origin (see the `URL:` note under the
  verbs below).

### The verbs

| Verb | Does | Flags worth knowing |
|---|---|---|
| `init` | writes the two files, optionally copying in a component you already have. Creates nothing server-side | `--from`, `--feature` (required), `--title` |
| `clone` | copies an app's files down into a new folder, `access` included. Prefers your open draft over live. Creates nothing server-side | — |
| `status` | local changes, remote lifecycle/draft, whether the draft compiles, allowlist differences, and drift. Read-only, works signed out for the local half | `--json` |
| `fork` | copies one built-in kit file into the folder at the same path, so it shadows the kit's. Local only — changes nothing server-side, and never overwrites an existing file | `--json` |
| `push` | ensures a draft, syncs allowlists then files, then compiles | `--no-validate`, `--force` |
| `validate` | recompiles your draft and reports diagnostics | `--json` |
| `test` | renders the draft in a headless browser and writes `report.json` + the frames; a report, never a verdict | `--route`, `--viewport`, `--timeout`, `--steps`, `--out-dir`, `--fail-on-errors`, `--json` |
| `publish` | commits the draft, or submits it for review. Refuses a draft that does not compile | `--no-request-review` |
| `discard` | deletes your draft; the live app and your local files are untouched | `--yes`, `--delete-app` |

`status`, `push` and `publish` print a `URL:` line the same way `wf` does and
for the same reasons — see the note in the workflow section, including why an
absent link is silent. Two details are specific to apps: `push` reports the
**draft** it wrote to while `status` and `publish` report the **app** itself,
and `status` only knows the link when it actually reached the server, so a
signed-out or unbound status has no `URL:` line.

⚠️ One thing is *not* the same as `wf`: the `--json` payloads of `app status`
and `app publish` **did** change. Both carry an `appURL` field, and both used to
derive it locally — so it was always present, and always pointing at the API
host. It is now the server's link, and therefore **absent** when the instance
has no frontend origin configured, or (for `status`) when the remote read never
happened. `ronja app publish --json | jq -r .appURL` goes from a wrong string to
`null` on such an instance. `app push` is unchanged: its link rides the human
report only, exactly like `wf`'s.

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

### `app test` — render it and look at it

```bash
ronja app test
ronja app test --route '#/orders' --viewport mobile --out-dir ./preview
ronja app test --steps steps.json          # admin only; the steps really execute
```

Renders what is ON THE SERVER (your own open draft when you have one, the same
substitution `/embed` makes) through `POST /dataapp/:id/preview`, and writes
into `--out-dir`:

| File | What |
|---|---|
| `report.json` | the server's answer **verbatim** — written from the raw bytes, so a field newer than this CLI's hand-mirror still lands in it |
| `screenshot-<n>.png` | every captured frame, in capture order |
| `screenshot.png` | the representative frame, under a second stable name |

Everything is 0600, atomically (`--out-dir` is created 0700 when missing).
`screenshot.png` is a copy of the numbered file already on disk, so the two
names are guaranteed to be the same pixels and the frame is fetched once. A
re-run REMOVES the previous run's frames first — and only those names, never
anything else in the directory — so a render that captured nothing cannot leave
an old picture beside a report describing a different one. `screenshot.png` is
written only when a frame is marked representative, which is not every render.

**The exit code is the whole difference from `wf test`.** A workflow run has an
outcome; a render does not, and the endpoint deliberately carries no pass/fail
field. So `app test` exits **zero whenever the harness ran** — an app with a
hundred runtime errors included, because the errors are what you asked for.
`--fail-on-errors` is how CI opts into the stricter reading, and it fires on
compile diagnostics as well as runtime errors: a bundle that did not build comes
back with an **empty** `errors` list, so a gate reading `errors` alone would pass
the most broken state a data app can be in. Console errors are excluded (an app
can log one on purpose).

Non-zero means the render did NOT happen: a transport failure, an HTTP error, a
bad flag, or a busy harness that stayed busy. **429 is not a verdict** — the
shared render pool, or this credential's per-minute budget, was spent. The CLI
waits the server's `Retry-After` (default 15s, capped at 60s) and asks **once**
more; busy again prints a message that says nothing about your app was observed
and exits non-zero. A client that kept retrying would add load to the thing it
was just told is at capacity.

⚠️ `--steps` requires an **administrator** — and, when you are signed in with a
narrowly scoped token, `admin:write` on top of the role — and executes for real,
with the same authority a viewer of the app has: a click runs the app's own code
and can dispatch workflows and Agents, upload files, and call external systems.
The plan is a JSON array in a file you name — nothing is ever synthesised — and
it is decoded **strictly**: a mistyped key decoded leniently becomes a click with
no selector, which the harness then reports as a fact about the *app*, and a file
holding two JSON values is refused rather than half-run.

`domText` and console message bodies reach `report.json` but are never printed to
the terminal: they are the app's own rendered content (tenant rows, whatever an
external system last wrote), and in a terminal they would be indistinguishable
from the CLI's own words.

## Pipeline folders (`ronja pipeline`)

`ronja pipeline`, aliased `pl`, develops a feature's **derived tables** from a
folder of `.sql` files: clone → edit → push → publish. It is the fourth folder
loop, and the first one that binds *many* resources — one `.sql` file is one
derived table.

```bash
ronja pipeline clone collection-abc    # or: ronja pipeline init --feature collection-abc
$EDITOR orders-by-region.sql
ronja pipeline status                  # local changes, table health, drift
ronja pipeline push                    # per file: draft → SQL → build → verdict + report
ronja pipeline publish                 # commit, or submit for review, and say which
```

**The name collides with something, and only one of them is this.** `api.Workflow`
carries a `pipeline` *kind* (a workflow whose job is a data pipeline).
`ronja pipeline` does not operate on those — it operates on derived tables.
Expect the question.

**What it earns.** Iterating on a derived table over raw HTTP is five requests
and two documented footguns per edit cycle: check out a draft, `PUT` the
*draft's* id, sync the *draft*, poll status **and** the error endpoint through a
truth table, commit. Multiply by the number of tables in the pipeline and add
the draft-id bookkeeping. The loop is stateful, multi-step and drift-guarded, so
it passes the same test `wf` does.

### The folder

`ronja.json` `kind: "pipeline"`, the same manifest and the same local-only
`.ronja/` baseline the other loops use, with three differences:

- **Only `.sql` files are synced, and everything else is silently ignored** — not
  reported as skipped. A README, a Makefile, a `fixtures/` directory, a dbt
  project you are porting: the point of the rule is that a pipeline folder can be
  an ordinary repo directory, and a note about `README.md` on every command is
  noise. The rule lives on the Kind (`wfdir.Kind.SyncExt`) and is read through
  one predicate (`wfdir.NotSyncable`) by both `Enumerate` and `CheckLocalPaths`,
  because a walk that keeps a path the baseline drops is a phantom deletion. It
  also means the walk never reads a non-`.sql` file, so a `venv/` in the folder
  costs a directory listing rather than a hash of every file in it. It filters the *skipped* list too: `.git/`, a `.venv/` and
  a dot-file were never candidates here, and reporting them on every command
  trains the reader past the line that matters — a `.sql` file that was skipped,
  which is still reported.
- **The binding is a map, not an id.** `ronja.json` records `tables`, relative
  path → the live `table-…` id that file builds, per (instance, organization)
  like every other binding:

  ```json
  "instances": [
    {"url": "https://app.ronja.tech", "tenantID": "ten-…", "featureID": "col-…",
     "tables": {"orders-by-region.sql": "table-…"}}
  ]
  ```

  `Binding.ResourceID()` / `WithResourceID()` **refuse** this kind rather than
  answering with a workflow's field, and the baseline records the table id
  alongside each hash — so a manifest repointed at a different table by hand or
  by a merge is *detected* rather than silently inheriting the old table's
  fingerprints: `push` refuses it, naming both rows, and `--force` accepts the
  new binding by discarding the baseline rather than comparing against it.
- **The baseline holds three fingerprints per file, not one**, because a
  pipeline file is synced with three different things. `files[path].sha256` is
  the last **complete** push (written *and* built) — what "changed since your
  last sync" means. `tables[path].draftSHA256` is what this checkout last **wrote
  into your draft**. `tables[path].liveSHA256` is the **live** table's SQL as of
  the last moment this folder agreed with it (a clone, the fork of a draft, a
  publish). The rule they exist for: **a hash is only ever compared against the
  row it was taken from.** Folded into one value, the guard below compared the
  live table against bytes that only ever existed in a draft, and refused an
  ordinary `push` → edit → `push`.

  A baseline entry whose file has left both the folder *and* the `tables` map is
  pruned on the next `push` (and disappears from `status` immediately), so
  removing the `ronja.json` entry is the whole of the cleanup for a table this
  folder no longer manages.

A table's display name is the file stem, stamped once at create. Renames are
unmanaged in v1: push never writes the name again, so a rename in the web app
survives.

That makes the name a **round trip**, which is why `clone` derives filenames
with `wfdir.FileSlug` rather than the `wfdir.Slug` the other loops use for their
clone *directories*: `FileSlug` keeps underscores **and case**. Folding either
away gave `orders-clean.sql` back for the `orders_clean.sql` git was already
tracking, and `orders.sql` for `Orders` — two files for one table, and a phantom
deletion the moment either was pushed. Everything else slugifies as before
(spaces and punctuation become one dash, runs collapse). Collisions still take a
`-2` suffix rather than dropping a file, and are resolved **case-insensitively**
so `Orders` and `orders` land as `Orders.sql` and `orders-2.sql` — a folder that
round-trips on a case-insensitive filesystem too. `Slug` itself is deliberately
unchanged.

### Refs are id-form, and that is enforced

Tables reference each other with `{{ ref('table-…') }}`, and the CLI derives each
table's declared `inputModels` from the id-shaped refs in the file on **every**
write. Adding an upstream is therefore editing the SQL and nothing else.

It never omits the field. Server-side derivation fires only when the effective
declared list is empty, and a checked-out draft inherits its parent's non-empty
`input_models` — so an omitted `inputModels` 400s the moment an edit adds a new
upstream, which is the commonest pipeline edit there is. Sending the exact list
also *removes* stale entries that would otherwise linger as phantom lineage
edges and trigger cascade rebuilds nobody asked for.

**Stored code comes back in two forms, and the second one is not legacy.** The
write API stores id-form verbatim, but the AI build path — `POST /:id/build`,
`/fix`, and the agent's `editDerivedTable` — persists **positional**
`{{ ref('0') }}`, an index into the row's `input_models`. Any table a colleague
has touched through chat holds positional refs. `internal/tablerefs` is a
byte-for-byte mirror of `esql.IndexToTableIDRefs` (with that function's own test
cases as fixtures), and **every remote code read is canonicalized to id-form
before it is hashed or written to disk** — without which every chat-edited table
would read as permanently drifted.

Two refusals fall out of that, and both are hard rather than warnings:

- **A local file carrying a positional ref is refused by `push`.** A positional
  ref is an index into a list the file on disk does not have. The only list the
  CLI can build is derived from the id-shaped refs and *sorted*, so sending the
  file would hand the server a ref numbered against one list and a declaration
  that is a different one — the SQL silently starts reading a different table,
  the build succeeds, and nothing says so. The refusal names each marker, and is
  scoped to the files that push is actually **sending**: a marker in a file this
  run is not pushing cannot mis-resolve anything, and refusing on it would let
  one stale file hold every other table in the folder hostage.
- **A remote row whose positional ref cannot be resolved is refused by `clone`**
  (collectively, before a byte is written) and counts as *drift* in `push` and
  `status` rather than as agreement — the stored SQL and the stored inputs
  disagree with each other, and pushing over that quietly is exactly the case the
  guard exists for.

### The verbs

| Verb | Does | Flags worth knowing |
|---|---|---|
| `init` | writes `ronja.json` + `.ronja/`. Creates nothing server-side — each table comes into existence on the first push of its file | `--feature` (a push refuses without one), `--title` |
| `clone` | writes one `.sql` per **derived** table in the feature, plus manifest and baseline. Prefers your own open draft over live. Creates nothing server-side; the directory must be empty or absent | — |
| `status` | local changes, per-table build health, open drafts and their verdicts, and drift since the last sync. Read-only — not even a checkout. **Exits non-zero on drift *and* on "not checked"** (see below) | `--json` |
| `push [paths...]` | per changed file: create → resume/check out your draft → write SQL + derived inputs → sync → build → verdict and confidence report | `--force` |
| `publish [paths...]` | per staged draft: commit onto the table, or submit it for review | `--no-request-review` |
| `discard [paths...]` | deletes your drafts; the live tables and your local files are untouched | `--yes` |

`push`, `publish` and `discard` all take file paths and act on the whole folder
when given none. Narrowing does not narrow the *graph*: dependency order is still
computed over every file in the folder, so a partial `publish` still lands
upstream first.

⚠️ **A table created by a push is LIVE from the moment it exists** — `POST
/feature/model` mints a real row, not a parentless draft the way `POST /workflow`
and (since migration 000495) `POST /dataapp` do. So a first push puts an empty
table in the feature that colleagues can see, and it stays empty until the draft
behind it is published. The binding is therefore written to `ronja.json` and
saved *immediately* after the create, before anything else is attempted: a push
that died in between would otherwise leave a table nobody can find and the next
push would create a second one beside it. Parentless table drafts are a
follow-up.

There is deliberately no `pipeline list`, no `pipeline delete` and **no `run`**:
committing cascades server-side, so publishing already rebuilds everything
downstream (see below).

`clone` says on stderr what it left behind, in two deliberately different
wordings: non-derived kinds are "not cloned (kinds: dynamic, integration)" —
they have no SQL a file could hold — while metrics are "not cloned (out of scope
for this loop)", because a metric runs the *identical* draft flow and claiming
otherwise would send someone looking for a capability the product already has.

### A push builds a draft, never the live table

That is the safety story, and it is the reason this loop reports more than
"saved":

1. **Local guards first** — file sizes, case-only collisions, positional refs.
   None needs the network and each names a file.
2. **Dependency order.** Files are pushed after everything *in this folder* they
   read from, so a downstream table is not materialized against yesterday's
   upstream. The graph is built over the whole folder even for a narrowed
   `push a.sql b.sql`, so a cycle is refused naming the loop rather than
   surfacing later as a build error about one table. When an upstream is dirty in
   the same push, the downstream's report says `validated against live
   <upstream>` — a draft builds against its inputs' **live** data, and
   draft-overlay chaining is a follow-up.
3. **Your draft is resolved before it is written.** `GET :id/draft` first,
   always: `POST :id/checkout` is not idempotent, so "check out and see" is not
   available the way it is for a workflow.
4. **A two-leg drift guard**, before the first byte: (a) the **live** table's
   canonical SQL against `liveSHA256` — a colleague committed while you were
   away; (b) when resuming, **your own draft's** canonical SQL against
   `draftSHA256` — the chat agent's `editDerivedTable` works in exactly that row,
   so without leg (b) a chat or web-builder edit to your own draft vanishes
   silently under the `PUT`. Either mismatch refuses, naming the table, unless
   `--force`.

   Each leg compares a row against the fingerprint **taken from that row**, which
   is what the three-fingerprint baseline above is for. A draft this checkout has
   never written to (opened in the web builder, or by chat) has no fingerprint of
   its own, so leg (b) compares it against the **live** row instead: a draft
   nobody has edited is byte-identical to the parent it forked from, so a
   difference is still somebody's unsynced work.

   ⚠️ **There are no per-write preconditions on this API.** A `PUT` to a draft
   overwrites it unconditionally and a commit overwrites its parent
   unconditionally — there is no `baseSha256` and no commit CAS the way there is
   for workflows. This guard is the whole protection and it is a check at one
   instant: the moment those two rows were read. An empty fingerprint disarms its
   own leg deliberately (a colleague who cloned the repo from git has no
   `.ronja/`, and refusing that is refusing the normal way people join); a missing
   *live* one is adopted from the row the guard just read, so a folder can leave
   that state rather than stay unguarded for ever.

   The draft fingerprint advances on the **write**, not on the build: a failed
   build leaves the draft holding the bytes of the failed attempt, and a guard
   that had not recorded them would read your own last attempt as somebody else's
   edit and demand `--force` to fix your SQL. The *content* baseline advances only
   on a complete push, so the fixed file is still something a bare `push` sends.
5. **Then `PUT` → `/sync` → poll to a verdict.** Every command polls through one
   helper, so the `ready`-with-an-error truth table exists exactly once. It reads
   `buildVerdict`, the computed field on the single-row GET added for this loop
   and for HTTP callers alike: `ok`, `ok_partial`, `failed_stale` (the run failed
   and the table is still serving its previous data), `failed`, `building`,
   `pending`, `invalidated`.

A failed build is **not** the end of the push. The file was written and the draft
was synced — what failed is the build, so the draft is kept to iterate on, the
live table is untouched, the remaining files are still attempted, and the command
exits non-zero. The per-file `--json` `outcome` vocabulary is `pushed`,
`build_failed`, `refused`, and a caller that read `build_failed` as "nothing
happened" would discard a draft holding their work.

**One failure message gets a diagnosis rather than a repeat.** The server's bare
`no data found` is the chain-bootstrap failure: a draft builds against its
inputs' **live** data, so a new downstream that reads a new upstream builds
against a row that exists, is empty, and stays empty until the upstream's draft
is published — and nothing in the message points at that. `push` therefore
checks the failing file's folder-local inputs and adds a `hint` (a stderr note,
and a field in `--json`): an input with a draft recorded in the baseline, or one
this push created, gets *"run `ronja pipeline publish <file>` first"*; otherwise
the note says the message can be transient right after an input is published
(issue #4607) and to push again. It never retries — a retry that happened to
succeed would hide the first cause — and it never replaces the server's own
`error`.

### The confidence report

After a successful build, `push` renders what committing that draft *would*
change, from `GET /api/v2/table/draft/:id/review` plus one sample read:

- **Schema delta** — columns added and dropped. The server compares names only,
  so a retype is reported as neither.
- **Lineage delta** (`Inputs: +table-a, -table-b`) — the table ids the draft's
  declared inputs gain and lose. Printed on its own line because it is the one
  change committing can make with *nothing in the SQL diff to look at*: a
  positional `{{ ref('0') }}` resolves by index into that list, so repointing it
  moves what the same text reads. Signed on one line rather than split across two
  labels, since the commonest lineage change is a swap. The payload's `nameDelta`
  and `descriptionDelta` are deliberately **not** mirrored — this loop never
  pushes either (a name is stamped once at create), so a field it cannot cause is
  a field nobody would keep in step.
- **Row counts**, draft against live. `-1` means "not available" and prints as
  `n/a`; printing it as a number would report a table that was never built as one
  that came back with minus one rows.
- **`baseStale`** — the live table has been published to since your draft forked,
  naming the intervening versions. Committing would revert them, and nothing
  server-side will stop you.
- **Up to five sample rows**, via `POST /api/v2/duckdb/query` on the *draft's*
  id. Read only after a successful sync (a checked-out-never-synced draft of a
  partitioned parent has no partitions of its own and answers "no data") — and
  only when the draft's row count is under `maxSampledRowCount` (1,000,000).
  Above it the read is skipped with a note naming the row count: five rows of
  garnish are not worth a scan of a table that size, and the user can query it
  directly. Skipped entirely under `--json`, where the field is `json:"-"` and
  the read would buy an LLM routing call — and possibly a Batch job — for output
  the run does not print.

All of it degrades to a stderr note and never fails the push — the build
succeeded and the work is safe; the report is how you decide whether to publish
it. The one degradation worth recognising is the **scope quirk**: the authoring
loop is `data` scope but `/table/draft/*` lives in the `admin` scope group, so a
token scoped to `data` alone runs the whole loop except this read and
`request-review`, which sits in the same group. A full-access PAT — what
`ronja login` stores — is unaffected.

### Publishing, and why there is no `run`

`publish` decides its route **up front** from the two server facts that decide it
server-side — the feature's scope and your role, read once for the run — rather
than by trying a commit and reading the rejection prose. Shared feature +
non-admin → `request-review`, outcome `submitted_for_review`; otherwise commit,
outcome `published`. `--no-request-review` turns the review route into an error,
which is what CI wants when a merge is expected to land directly. A commit
refused with a **400** on a *shared* feature falls back to a review request (the
race the up-front read cannot close); a 400 on a private one does not, because a
review request would neither fix nor describe it. A feature whose scope could not
be read is **unknown**, not private, and keeps the fallback — read as private, an
unreadable scope would also disarm it, and a shared-feature publish whose scope
read happened to fail would die on the commit instead of filing the review
request it needed. A draft the review payload already reports as submitted is not
submitted again; the outcome is still `submitted_for_review`, and the detail says
an admin has had it all along.

It refuses a draft whose verdict is `failed`/`failed_stale` — committing it would
put a table live with nothing behind it — one still `building`, and one still
`pending`, which is worded differently: a checked-out draft sits at `pending`
until something syncs it, so the commonest way to be there is a draft nobody has
ever built, and "wait for it to finish" is advice to wait for something nobody
started.

Drafts are published in **dependency order**, as they are pushed: a commit
cascades, so landing a downstream before its upstream commits a table computed
from data that is about to be replaced and rebuilt again seconds later.

Before each commit it warns, never refuses, on two kinds of staleness:
`baseStale` as above, and **stale inputs** — an input table whose row was updated
after your draft's was. That second one is not paranoia (`GetDependents` excludes
shadow rows, so a staged draft is never invalidated by an upstream publish, and
the confidence report silently decays between push and publish with nothing else
to say so) but it *is* a **heuristic**, and it is worded as one: neither
timestamp is a materialization time — the API exposes none — so it over-reports a
renamed upstream and under-reports one rebuilt without a row write.

**Committing cascades.** The server marks every table that reads the one you
published as `invalidated` and rebuilds it asynchronously — which is why this
loop needs no `run` verb, and why the HTTP guide's "nothing cascades over the
API" is true of `/sync` only. The report prints a **folder-local, direct** count
("2 tables in this folder read this one directly and will rebuild
automatically") — never transitive and never tenant-wide: `GetDependents` is not
exposed over HTTP, and the real cascade is both wider (it leaves the folder) and
conditional (a zero-partition parent defers it entirely, a mid-build dependent is
skipped, one with a dangling input is parked) in ways a confident N would paper
over. A folder-local, direct number is one the reader can verify by looking.

After a commit both fingerprints are refreshed from the live row — they now
describe the same thing, because the draft they were taken from no longer exists
— best-effort, since the server-side change already happened and failing the
command afterwards would report something that did happen as something that did
not. The draft pointer is cleared.

`discard`'s per-file `--json` `outcome` vocabulary is `discarded` and `no_draft`
(plus `refused`, on a file the run stopped at) — the same two words `wf discard`
uses. `no_draft` is a SUCCESS, not a miss: the draft being already gone is the
documented normal case, and a caller that read it as a failure would fail a
folder somebody had tidied up from the web UI.

`discard` re-points the content baseline at the **live** table, which is the only
row the file is synced with once its draft is gone. That is what makes the
command's own closing line true: with the baseline still describing the discarded
draft, the local file read as unchanged and the next `push` answered "Up to date"
with no draft on the server at all. It re-points it **equally when the server
reports no draft** — the documented normal case, somebody discarding it from the
web UI — since the draft is gone either way. One table's failure is per-file
data, like `push`'s, and the baseline is saved as each draft goes; a baseline
write that *fails* is that same stale state, so it stops and exits non-zero
rather than reporting a clean discard.

A `create` or a `commit` whose request **timed out** may still have landed — the
deadline was ours, the transaction was the server's — so neither is treated as a
plain failure. `push` reads the feature back and adopts the table the create left
behind (refusing rather than guessing when the name is not uniquely matched;
without this the next push creates a *second* live table beside the first, and
this loop has no delete verb). `publish` reads the draft back: gone means the
commit landed and cascaded, still open means it did not. The trigger is a timeout
and nothing else — an answered failure is one the server considered and refused.

### Everything else behaves like the other loops

`--json` on every verb, on stdout only, with narration on stderr; non-zero exit
on failure with the payload still emitted, because a push that stopped part-way
has really created tables and staged drafts and "which ones" is the first thing
anyone needs to know. The baseline is saved after **every** file, success or
failure, for the same reason.

A `.sql` file deleted from the folder is **not** a table this loop deletes. It is
reported on stderr and left alone — there is no delete verb here on purpose, and
quietly dropping a colleague's table because a merge removed a file is exactly
what a sync loop must not do. The note says how to stop it being reported for
ever: remove the file's entry from `tables` in `ronja.json`, and the baseline
entry is pruned with it.

**`status` exits non-zero on REMOTE drift AND on "not checked", so it works as a
CI gate without parsing** — the same contract `db migrate status` has, and for
the same reason: a gate that has to be parsed is a gate somebody eventually
forgets to parse. The exit code answers exactly one question — *has anything
moved on the server under this folder?* — and everything else is reported without
touching it.

Non-zero is: real remote drift (the live table's SQL, or your own draft's, is not
what your last sync recorded — the two legs `liveDrift` and `drift` cover); "I
could not check" (unreachable instance, expired token, a failed list); an
ambiguous or broken binding (a manifest naming a table the baseline was not taken
from, an unresolvable instance entry); and a bound row that could not be READ, so
drift cannot be ruled out for it. "I could not check" is a different answer from
"everything is fine", and a job conflating them would report a clean pipeline it
never looked at.

Zero is everything the SERVER has not moved, which deliberately includes three
states an earlier draft of this gate failed on. A **locally-changed file** is not
drift — local edits are the input to a push, not evidence that somebody else
moved something, and failing on them would make the gate fire on every ordinary
working tree. A **file with no table yet** (fresh `init`, nothing pushed) has
nothing on the server to have moved. And a **fresh clone with no baseline at
all** is the load-bearing one: `.ronja/` is never committed, so a colleague who
just cloned has no record to compare against — that is the normal way somebody
joins a pipeline, and it is the same state `push`'s drift guard deliberately
disarms for (`driftReason`'s empty-baseline leg). Status says there is nothing to
check yet and names `pipeline push` as the way to establish a baseline, rather
than failing a CI job for the shape of a checkout. `--json` still emits its full
payload first either way — the LOCAL half (changed files, unbound files) lives
there, and the exit code is a summary of the remote half, not a substitute for
the report.

`status` degrades signed-out the way `wf status` does — "not checked" is a
different answer from "fine" — and its remote half is two tiers: **one** list
call for the health leg (kind, status, shadow markers, `hasError` for every bound
row), then the per-table reads the list deliberately omits the code for —
concurrently and bounded. Where a draft is open that is the live row *and* the
draft by its own id, because the two drift legs are different questions: your
file against your draft (`drift`) and your draft's base against what the live
table has since become (`liveDrift`, present in `--json` only when a draft is
open). Reading one row could only ever answer one of them, which made push's
"run status to see the detail" untrue exactly when it mattered. A thirty-table
folder must not cost ninety serial requests, or people stop running it.

The HTTP loop this wraps is documented for agents in the `build-a-data-pipeline`
API guide (`backend/lib/api/openapi/guides/build-a-data-pipeline.md`, served at
`/docs/api/guides/build-a-data-pipeline.md`). Keep the two in step.

## Design rules

These are load-bearing for the agent use case — please keep them true:

- **Sync verbs yes, resource verbs no.** A stateful filesystem-to-resource loop
  earns a command; wrapping a call does not.
- **Transport may be a command; endpoints may not.** `ronja api` is allowed
  precisely because it describes nothing and so cannot drift. A subcommand that
  names a route, a resource kind or a field is the wrapper this rules out. See
  "Transport is not a wrapper" above.
- **A link a report prints comes from the server.** Which page a resource has,
  and which origin serves it, are the backend's to answer (`lib/deeplink`); the
  CLI prints the `url` off a response and prints nothing when there is none. It
  never templates a route and never falls back to the instance URL — that is
  the *API* origin, and a link built from it lands on a host serving no pages.
- **Never prompt without a TTY.** A missing input must be an error, not a hang.
- **`--json` on everything**, and only ever on stdout. Human narration goes to
  stderr so `--json` output stays a single parseable object.
- **Non-zero exit on failure**, with the reason on stderr.
- **`RONJA_URL` / `RONJA_TOKEN` outrank the credential file**, so a CI job never
  inherits whoever last logged in on that machine.
- **Enumeration and path-checking must share one folder `Kind`.** The local walk
  (`Enumerate`) and the local-path validator (`CheckLocalPaths`) both take the
  `wfdir.Kind` as a single threaded value, because the `Kind` decides which
  directories are never synced (`node_modules` / `dist` / `build` for a data
  app). Let those two disagree and the baseline records a file the walk will
  never see again — a phantom deletion that the *next* push applies server-side.
  This is why `LoadManifest(root, kind)` refuses the other kind outright: the two
  folder types are indistinguishable by shape, so nothing else would catch it.

## Layout

```
cmd/ronja/           main package — its NAME is what makes the binary `ronja`
internal/commands/   cobra command tree (root, auth, context, env, api, query,
                     stdin, workflow_*)
internal/api/        HTTP client + the endpoint shapes it mirrors, plus raw.go
                     (transport only, mirrors nothing) and query.go
internal/config/     the CLI config file (os.UserConfigDir()/ronja/config.json),
                     one profile per (instance, organization), mode 0600
internal/wfdir/      the synced folder: ronja.json, .ronja/state.json, local
                     enumeration and the sha256 drift baseline — one Kind per
                     loop (workflow / data app / pipeline)
internal/tablerefs/  the {{ ref('…') }} grammar: canonicalization to id form,
                     and the input list derived from it
```

The workflow commands are one file per verb (`workflow_push.go`,
`workflow_testcmd.go`, …) with the shared folder plumbing in `workflow.go`;
their endpoint mirrors live in `internal/api/workflow.go`, `workflow_write.go`
and `workflow_run.go`, hand-mirrored against
`backend/api/v2/workflow/handler.go`. The data-app and pipeline commands follow
the same shape (`dataapp_*.go`, `pipeline_*.go`, with the shared folder plumbing
in `dataapp.go` / `pipeline.go`); the pipeline mirrors are
`internal/api/table.go` and `table_write.go`, against
`backend/api/v2/feature/api_model.go` and the draft-review route in
`backend/api/v2/governance/`.

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
