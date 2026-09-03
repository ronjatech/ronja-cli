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

`context` also **notices which kind of folder you are standing in** — a
workflow, data-app or pipeline folder, or a loose `.py` — reports it as `kind`
under `--json`, and prints that loop's verbs. A capability nobody finds is a
capability that does not exist, and each of these loops already does the folder
sync, the declarations and the polling that people were otherwise hand-rolling
over raw HTTP.

Two commands make that HTTP half less unpleasant without describing any of it —
`ronja api` (a request runner) and `ronja query` (read-only SQL). Both are
below, and the doctrine that admits them is immediately below this.

The exceptions are the five sync loops — `ronja wf` (workflows), `ronja app`
(data apps), `ronja pipeline` (derived tables), `ronja automation` (automations)
and `ronja db` (managed-database migrations), each below. The rule they keep is
**sync verbs yes, resource verbs no**: the CLI may own a filesystem-to-resource
sync loop, because that is stateful, multi-step and drift-guarded in a way plain
HTTP handles badly. It still owns no list, browse or delete. If an agent needs something the API already does, the answer is a
better pointer in `context` or a better guide behind `/llms.txt` — not a new
command.

`ronja db promote` is the one verb in that set whose two ends are both remote,
and it passes the same test rather than getting an exception: it moves a ledger
**tail** from a database's dev copy onto the database, comparing the shared
prefix position by position, applying all-or-nothing, and reporting divergence
as an instruction. Stateful, multi-step, drift-guarded — and not a list, a
browse or a delete. What makes it a verb is the loop, not the endpoint.

One verb sits beside those loops rather than inside one: `ronja bind` reconciles
a folder's **declared dependencies** against one organization's rows (see
[Dependencies](#dependencies-ronja-bind)). It passes the same test — the
questions come from a file somebody committed, and the answers go back into it —
and it is deliberately unable to become a browser: it can only ever ask about
names this folder already declares, never "what tables exist".

And one GROUP sits above them: `ronja sync` (`status`, `check`) asks about a
whole tree of folders rather than the one you are standing in — a question a
repository has and a folder does not (see
[Whole-tree checks](#whole-tree-checks-ronja-sync)). It is read-only and
discovers nothing: it can only report on folders that already carry a committed
`ronja.json`, which is the same "no browsing" line the loops hold.

One command sits outside the doctrine entirely rather than under it: `ronja
update` is housekeeping of the binary itself, and touches no Ronja resource and
no Ronja credential — see [Keeping it current](#keeping-it-current), which also
covers the once-a-day notice that tells you a newer release exists.

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
the property the doctrine is really protecting. A *generic* run verb per
asynchronous primitive — one that takes any resource id — would be exactly the
wrapper this rules out: it would have to know a route, a poll endpoint and a
status vocabulary, and would need a sibling for every primitive that gained one.

`ronja wf run` is not that, and the line between them is worth being precise
about because it is where the next command will be argued. It takes **no
workflow id** — it runs the row *this folder* is bound to, the step after
`publish` in a loop that already owns the folder's state — and it refuses a
positional id outright, pointing at `ronja api` instead. That is the whole
distinction: **folder-bound, in-loop verification is a sync verb; running an
arbitrary id is transport**, and for that `ronja api ... --wait-until` remains
the answer.

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

The binaries are unsigned, so the cask carries a `postflight` hook that strips
the Gatekeeper quarantine bit. Without it macOS reports a freshly installed
`ronja` as "damaged" — which is not a message anyone connects to code signing.

### Keeping it current

Nobody knows they are running an old CLI, because nothing tells them. So once a
day, on a real invocation at a real terminal, `ronja` prints one line to
**stderr**:

```
ronja v0.29.0 is available (you have v0.28.4) — run: ronja update
```

The remedy is chosen by how the binary got onto the machine, because offering
the wrong one is worse than offering none: a Homebrew install is told
`brew upgrade ronja` (the file belongs to brew), and on Windows — where there is
no release asset and `ronja update` refuses — it is told
`go install github.com/ronjatech/ronja-cli/cmd/ronja@latest`.

**Never stdout, and never for a machine.** The line is suppressed outright when
any of these hold:

- the running build is not a release — `dev`, Go's `(devel)`, or the
  pseudo-version of a `go install …@<commit>`. There is nothing to compare
  against, and the remedy would not apply anyway.
- stdout or stderr is not a terminal. *Both*, so the line stays out of a piped
  `eval "$(ronja env)"` **and** out of `2>build.log`.
- `CI` is set to anything, or `RONJA_NO_UPDATE_CHECK` is set to anything
  (`RONJA_NO_UPDATE_CHECK=1` is the documented form). The check is one
  unauthenticated request to `api.github.com`, at most once a day, carrying no
  Ronja credential; suppressing it here switches that request off as well as the
  line, rather than making the request and discarding the answer.
- `--json` was passed.
- the command is `update`, `env`, `help`, `completion`, or one of cobra's `__`
  completion drivers — output somebody reads or pipes as a whole.
- the user was told within the last 24 h already, or the newest known release is
  not newer than the running version.
- the update-check file (`update-check.json`, below) cannot be read or written —
  including the `sudo` case, where the effective uid is 0 and the config
  directory belongs to somebody else.
- anything at all failed. Every path here returns quietly; the check cannot
  change an exit code, and it has nothing to say worth a second line.

**The one honest cost.** The lookup starts after cobra has parsed the flags,
runs concurrently with the command under a one-second deadline, and is joined
only once the command has finished writing. For anything that talks to the Ronja
server it is long finished by then. The worst case is a command that would have
finished in well under a second being held for the remainder of that second,
when github.com is slow or black-holed. A lookup that times out or is
interrupted — Ctrl-C during the command ends it too — rewinds the clock to 23 h
rather than 24, so a slow link retries hourly instead of going quiet forever,
and a habit of interrupting slow commands does not silence the notice for a day
at a time.

That rewind is also what decides how often the stall recurs, and the two
firewall shapes differ. One that silently DROPs packets to `api.github.com` hits
the timeout branch, so the clock goes back to 23 h and the second is spent again
an hour later — hourly, not daily. One that answers with an RST, like a host
that is simply not there, fails immediately: no stall at all, and the ordinary
24 h back-off.

Both cadences live in the update-check file, beside the credential file in the
config directory (`$RONJA_CONFIG_DIR`, else `~/Library/Application
Support/ronja` or `~/.config/ronja`): when the mirror was last asked, when the
user was last told, and the newest release known. It holds no secret, and losing
it costs one extra request. It is claimed *before* the fetch, which is what
makes an unreadable or root-owned file cost nothing rather than a stalled second
and a github.com request on every single command.

**`ronja update`** brings the binary forward, and nothing else ever runs it:

```bash
ronja update           # install the newest release
ronja update --check   # report what it would do, and change nothing
```

A binary that rewrites itself as a side effect of a command somebody asked for
is a binary whose bytes change under a CI job that pinned a version.

On a Homebrew install it swaps nothing and exits **0** with
`ronja is installed by Homebrew — run: brew upgrade ronja`: nothing is wrong,
and a non-zero exit would break `ronja update && …` on every Mac. Detection is
one thing — a `Caskroom` element in the fully resolved path. Deliberately not
`$HOMEBREW_PREFIX`, which on an Intel Mac is `/usr/local`, exactly where
somebody who unpacked the tarball puts the binary; they would be told to
`brew upgrade` a cask they do not have.

**Two ordinary refusals**, and neither is a malfunction. A build that is not a
release exits non-zero with `ronja is a development build (dev) — there is no
release to update to; build from source or install a release`: there is nothing
to compare a development build against, and the swap would replace somebody's
own build with a published one. When the mirror's newest release is not newer
than what is running, the command exits **0** with
`no release newer than ronja v0.28.4` — worded as a comparison rather than "you
are on the latest release", which would be a lie to someone running
`v0.29.0-rc.1` while the newest stable is `v0.28.4`.

**The report goes to stderr, and stdout stays empty** — both for the swap and
for `--check`. That is the tree's rule everywhere (`--json` on everything, and
only ever on stdout; human narration on stderr), and it is what keeps the one
parseable object parseable.

`--json` puts that single object on stdout instead. It always carries `current`,
`method` and `path`; `latest` and `upToDate` appear only when the swap is ours
to make, because a Homebrew install answers from the binary's own location and
asks the mirror nothing — there is no latest version to report, so it reports
`command`. The two paths that have resolved a release file for this platform —
`--check`, and a swap — also carry `asset`; a swap that completed adds
`updated: true`, which is the only field that says something was written.

The swap, in the order it happens, because the order **is** the design: sweep
any `.ronja-update-*` left behind by a SIGKILLed earlier run (only ones over an
hour old — a fresh one belongs to a run happening right now, and a run in
progress keeps its own probe fresh so a slow download is not swept out from
under itself); create the probe
file in the binary's own directory, which is both the writability test and, being
on the same filesystem, what makes the final rename atomic; fetch
`checksums.txt`; stream the archive into `os.TempDir()` through SHA-256, so
unverified bytes never land beside the binary; extract the entry named exactly
`ronja` onto the probe with the current binary's permission bits masked to
`0777` — a `0755` stays `0755`, but a setuid bit somebody once added is **not**
copied onto freshly downloaded code; then one `os.Rename` over the running
executable, which macOS and Linux allow because the mapped inode outlives the
name. Every failure before that rename leaves the installed binary untouched.

If the directory is not writable — `/usr/local/bin` is the usual one — you are
told before anything is downloaded, and told to re-run with `sudo` or to install
by the method you used originally. A read-only mount says so in those words
rather than "permission denied", because it is a different problem.

**The trust boundary.** The checksum defends against a corrupt or truncated
download, not against a compromised release: the archive and the manifest come
from the same origin. Both are fetched over https or not at all — an asset URL
the release listing named over plaintext is refused before the connection, and
so is a redirect that would drop to it. Beside that, the asset, checksum and
release-lookup URLs — and every redirect hop — are constrained to `github.com`,
`api.github.com` and `githubusercontent.com`, so a tampered release listing
cannot point the download at a host of its own. (The last is matched by domain
rather than by exact host: GitHub serves release assets from a subdomain of it —
`objects.` and `release-assets.` have each been the one — and pinning whichever
it is today would break every download the day it moves.) What none of that buys
is provenance: the checksum is not a signature — it proves the bytes match
what the release published, not who published them. See BL-6d3a.

**`releases/latest` is created-at order, not semver.** That is GitHub's own
rule, and it is the one `go install …@latest` and the Homebrew cask follow too,
so all three install paths agree — but a re-cut `v0.28.5` published after
`v0.29.0` would be offered as the newest. Documented rather than engineered
around.

**`update` is neither a sync verb nor a resource verb.** It is housekeeping of
the binary itself: the one command in the tree that touches no Ronja resource
and carries no Ronja credential. `internal/update` cannot even import
`internal/api` — a test pins that boundary — which is what keeps the bearer
token structurally unreachable from a request to github.com. So it is admitted
without widening "sync verbs yes, resource verbs no" rather than in spite of it,
because it is not a verb over anything on the server at all.

Binaries published before the first release carrying this have neither the check
nor the command, so those users upgrade once by hand (`brew upgrade ronja`,
`go install …@latest`, or a fresh tarball). Every version after that is
reachable by `ronja update`. See BL-4f18.

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

The release also carries `checksums.txt` (`checksum.name_template` in
`.goreleaser.yaml`), and that manifest is **what `ronja update` verifies** every
downloaded archive against before it replaces anything — so it is not an
optional nicety of the pipeline. See
[Keeping it current](#keeping-it-current).

The publish step passes `GORELEASER_CURRENT_TAG` together with `--skip=validate`
so that both triggers run **one** code path. On a tag push the environment
variable merely restates the tag already on HEAD; on a `workflow_dispatch` run
no such tag exists in the mirror checkout and GoReleaser's own validation would
fail a perfectly correct release. The version is validated in the workflow
instead.

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

⚠️ One detail in `internal/commands/picker.go` (raw mode via the already-present
`golang.org/x/term`) is easy to undo: `decodeKey` returns the number of bytes it
**consumed**, because one read is not one keypress. Key repeat delivers several
escape sequences in a single read, and a decoder that handles only the front of
the buffer drops every keypress but the first.

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

⚠️ An `$RONJA_TOKEN` credential resolves with **no organization**: `FromEnv`
leaves `Resolved.TenantID` empty on purpose. An exported token and a stored
profile are independent things — `eval "$(ronja env)"` followed by
`profile use other-org` is an ordinary sequence — so borrowing the current
profile's organization would have `wf push` send one organization's workflow id
under the other organization's token.

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

> ⚠️ **This was the ladder, and it is now closed.** A PAT holder could drive the
> whole handshake headlessly and mint fresh, independently-revocable *child*
> PATs — a credential that never really expires, which is precisely what the
> 90-day TTL exists to cut. `POST /api/v2/cli/device/:userCode/approve` is now
> `gt.HumanOnly`: a person approves in a browser, which is what `/cli-auth` was
> always for. Denying stays machine-callable, because refusing a request narrows
> nothing but the request.

The PAT is **full access but time-bounded — 90 days**. Full access is right
because a PAT never exceeds its bound user's live role (and `/api/v2/authentication`
requires the `admin` scope, so a *scoped* token could not even call `/me`); the
expiry is right because this credential is reachable from a link a human can be
phished into clicking. The server returns `expiresAt`, the CLI stores it, and
`login` / `whoami` / `context` all report it — a credential's shelf
life should be visible, not discovered as a 401.

### Full access still stops short of a few things — deliberately

Full access means "everything your role can do **through a machine**", which is
not quite everything your role can do. A small set of routes carry `gt.HumanOnly`
and refuse every machine caller — a PAT included — with a `403` whose body is
`{"code": "human_required", "error": "…"}` naming what to do instead.

Three things earn it: **hard delete** (a machine may never make something
unrecoverable, so `DELETE :id` works and `DELETE :id/purge` does not),
**changing who can get in** (invites, member removal, role assignment, auto-join
domains, workspace membership — though *revoking* stays scriptable, because
rotating a leaked credential at 3am is exactly when a human-only gate makes
security worse), and **irreversible at organisation scale** (deleting the org,
cascade execute, billing).

⚠️ **This makes the CLI strictly less capable than the app, on purpose, and
someone will file it as a bug.** The defence is the same one the whole PAT model
rests on: you approved this credential in a browser ninety days ago, for the
work you were doing then — not for *this*. Authorisation-at-mint is not
authorisation-at-action. Creating secrets and managed-database roles are NOT in
this class and stay fully scriptable; only Ronja's own auth is closed.

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

# wait for asynchronous work (the /head sibling follows a run that parks and
# resumes; wait for the statuses that mean FINISHED, not for "not running")
ronja api "/api/v2/workflow/run/$runID/head" \
  --wait-until '.status == "done" or .status == "error"'
```

### Sending

| Flag | Does |
|---|---|
| `-X`, `--method` | HTTP method. Defaults to GET, or **POST when a body is given** — curl's rule, for curl's reason. Upper-cased for you. |
| `-d`, `--data` | Request body. `@file` reads a file, `@-` reads stdin, anything else is the literal string. A `@file` is **streamed from disk** at whatever size the instance accepts; a `@-` body comes off a pipe, so it is held in memory and capped at **16 MiB**. A `@path` that is not a plain file — a FIFO, `/dev/fd/N` from process substitution, a character device — is **read into memory** on the same terms as stdin, and capped the same way: it has no size that says what reading it would yield and cannot be re-opened for a retry, so buffering it is what makes it replay-safe and bounded. Only a **directory** is refused, having no bytes to send at all. `--timeout` bounds the whole request, upload included, so raise it — or pass `--timeout 0` — for a large file. |
| `-F`, `--form` | A `multipart/form-data` part: `name=value` for a literal, `name=@path` for a file (`@-` reads stdin). Repeatable. Mutually exclusive with `-d`, and at most one thing in the whole command may read stdin. Same rule as `-d`: a file streams from disk at any size, a pipe — and any path that is not a plain file — is read into memory and capped at 16 MiB, only a directory is refused, and `--timeout` bounds the upload as well as the answer. Each file part is opened when the encoder reaches it and closed at its end, so an N-file form holds one descriptor, not N. |
| `-H`, `--header` | `"Name: value"`, repeatable. Repeats of one name are sent as repeats, not collapsed. A `Host:` header is honoured (copied onto `req.Host`) — net/http takes the Host line from there and silently **drops** a `Host` key in the header map, so setting it without that step would do nothing at all. |
| `--timeout` | How long to wait for **one request**, response body included — the deadline rides on the body's `Close`, not on the request returning. Default 2m; `0` waits as long as it takes; negative is refused. Same semantics and the same `httpFor` plumbing as `ronja query --timeout` (below). |
| `--retry` | Retry a **429 or 5xx** this many times, honouring `Retry-After` (both the seconds and the HTTP-date form), else exponential backoff capped at 30s. Default 0. A 4xx is never retried — that is the server saying the *request* was wrong. Transport failures are not retried either: a connection that dropped mid-request may well have delivered it. |

`-F` exists because the upload endpoints are multipart-only, so without it the
one thing the CLI is for — never putting a credential in a shell variable — had
a hole in it exactly where files were concerned. It is not a doctrine breach: a
content-type is not an endpoint, and nothing here knows what any upload route
wants.

The multipart body is a list of **segments**: the framing the CLI generates
(part headers, plain fields, a piped part, the closing boundary) as literal
bytes, and each file as a reference to its path. `Len` is the sum of the
literals and the stat'ed file sizes, so the request carries a real
`Content-Length`; `Open` concatenates them over freshly opened handles, so the
file bytes never enter memory. The generated boundary is fixed when the
segments are built, which is what makes every replay byte-identical — `--retry`
and `--wait-until` both re-issue the request, and one whose body had evaporated
would retry as a zero-byte upload the server accepts.

Two consequences worth knowing:

- **A file body must be a regular file.** What a FIFO, a character device
  (`-d @/dev/urandom`) or a socket stats as is not the size of what reading it
  would yield, so it would send nothing — or, with no size at all, go out
  chunked and unbounded. A directory has a size, but it is the entry's, and
  there are no bytes to send at all. Refused before the request; the
  non-directory cases name `@-` as the way to send one, a directory is told to
  point at a file inside it.
- **Stat-then-send is a window**, and the two directions fail differently. A
  file that **shrank** ends short of the `Content-Length` already written and
  fails at the transport, which the CLI translates into the file's name. A file
  that **grew** does not fail there: net/http writes exactly `Content-Length`
  bytes off a `LimitReader`, so the request is complete and valid on the wire
  and the instance may store that **prefix** as a finished upload. That one is
  caught by re-stat'ing every file after the response has been emitted — the
  server's answer is printed, then the command exits non-zero saying which file
  grew, from what size to what, and that the first N bytes may have been
  accepted as the whole thing.

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
| `-q`, `--jq` | Filter the JSON response through a jq expression (real jq, via `gojq` — see `internal/jqf`). Compiled **before** the request goes out, so a typo costs nothing. Prints **every** matched value or fails; two bounds can make it fail — a **work and memory budget** scaled to the response size, and **10 s** per application. |
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

**`--jq` prints every value the expression matched, or it prints nothing and
fails. There is no cap, so there is never a prefix.** That is the whole rule,
and it is what makes the output safe to capture: `ids=$(ronja api ... --jq
'.result[].id' -r)` either holds the complete list or the command exited
non-zero. Nothing is rendered until the whole stream has been collected, so a
filter that fails at value 15,000 prints nothing rather than 14,999 good lines
and then a failure.

⚠️ **A 10,000-value cap was tried and removed, and re-adding one would be a
regression.** It printed the first 10,000 with a note on stderr and exit 0, which
is silent data loss: `2>/dev/null` — what a script normally does — throws away
the only warning there was. Unlike `ronja query`'s `truncated:true`, which is
reported *in the data*, a jq stream has nowhere to put such a field, so there
was nothing honest to signal with. A `--jq-strict` flag to opt out only moved
the problem: the default still lost data, and turning it on made `--jq` over a
large listing print *nothing at all* for an answer that had been printable in
full one release earlier — and `--fail-on-error` implied it, so that flag
started destroying output it used to return. The flag is gone with the cap.

What bounds an enormous answer instead is **a work and memory budget scaled to
the size of the response**, plus **10 s** per application, `ronja query --jq`
included. `--timeout` bounds the *request* and `--wait-timeout` the poll loop,
and a jq expression can loop forever (`def f: f; f`) or manufacture data rather
than select it (`range`, `recurse`, `repeat`, a string repeated with `*`), so
the filter carries its own bounds rather than borrowing ones that do not exist.
Both fail loudly and name the fitting remedy: past ~256 KiB of response the
message says to narrow the **request**, below it that the **expression** is
generating data. Those budgets are also what bounds the *collected answer* — the
slice allocates like anything else, and the memory budget counts it — which is
why removing the cap cost nothing. Constants and calibration live in
`internal/jqf`.

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
  vacuously true in logic and catastrophically wrong here: `.result[] |
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
| `--jq` | Filter the **envelope** through a jq expression — `rowCount`, `truncated`, `zoneUsed`, `error`, not the rows (those are CSV, and jq does not read CSV). Refused together with `--json`: two contradictory descriptions of stdout, and either precedence would silently discard a flag someone typed on purpose. Same rule and bounds as `ronja api --jq`: every matched value or a failure, under the input-scaled work and memory budget and a 10 s deadline. |
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

`ronja database`, aliased `db`, is the managed-Postgres loops plain HTTP does
badly. All are **USR_ADMIN**, as the underlying API is.

```bash
ronja db sql <database-id> "SELECT * FROM leads LIMIT 10"
ronja db sql <database-id> --file report.sql --out rows.csv
ronja db sql <database-id> "INSERT INTO leads (email) VALUES (\$1)" --params '["a@b.c"]'

ronja db migrate status --database <database-id>
ronja db migrate push   --database <database-id>

# try it on the dev copy first, then move what worked onto production
ronja db migrate push   --database <database-id> --env dev
ronja db sql <database-id> "SELECT count(*) FROM leads" --env dev
ronja db promote <database-id> --dry-run
ronja db promote <database-id>            # asks first
ronja db promote <database-id> --yes      # for CI, agents, anything headless
```

**Why these three and no others.** `db sql` clears the same bar `ronja query`
does — quoting, result shape, and an error that must not be silent. `db migrate`
is a stateful loop with a drift guard. `db promote` moves a ledger **tail**
between two ledgers: the shared prefix is compared position by position, the
tail applies all-or-nothing, and divergence has to be reported as an instruction
rather than a hash mismatch — stateful, multi-step and drift-guarded, which is
the same test `migrate` passes, and it is not a list, a browse or a delete.
Creating, listing and deleting a database are single calls that `ronja api` runs
perfectly well, so there is deliberately no `db list` / `db create` /
`db delete`.

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

### The dev copy (`--env dev`) and `db promote`

A managed database can have a **dev copy**: a separate Postgres database with
the same schema, its own data, and its own migration ledger. `db sql` and
`db migrate` reach it with `--env dev`, and `db promote` moves the migrations
the copy has and production does not onto production.

**You never name the copy.** Every command takes the **production** id and the
server resolves the copy from it — the copy has no id the CLI ever learns, sends
or records. That asymmetry is the safety property, so the flag is a query
parameter on an admin-gated route rather than a second id passed around: it can
only ever narrow what a request touches, never widen it. `prod` is the default,
and asking for it explicitly puts nothing on the wire — a non-dev request is
byte-identical to the one this CLI sent before dev copies existed, which is also
what keeps it working against an older instance.

**Asking for the copy fails when there is none.** `--env dev` against a database
with no dev copy — or one that is provisioning, in error, or in the trash — is
refused by the server before a statement runs, naming what is wrong. It is
deliberately NOT the same rule a draft workflow gets: a draft with no copy falls
through to production so it stays runnable, while somebody who TYPED `--env dev`
asked for the copy, and production would be an answer to a different question.
On `db sql` that distinction is the difference between a refusal and a `DELETE`
that already ran.

Which copy answered is reported on **stderr** — never stdout, which is CSV
somebody is parsing or a `--json` envelope somebody is piping, and a dev result
read as production's is a wrong answer rather than an incomplete one. It is
printed whenever the answer could surprise: the dev copy answering (in every
mode, before the rows or the verdicts), and production answering a `--env dev`
request, which a current instance refuses but an instance predating dev copies
does silently. A plain production command says nothing extra.

**Minting a connection role against the copy is refused.** `POST :id/user` is a
production act, and there is nothing to mint anyway: every tier the parent hands
out is mirrored on the copy automatically, named `<parent> (<tier>) (dev)`. Use
those logins to connect to the copy directly.

**`db promote` sends no migrations.** What promotes is exactly the tail the
copy's ledger has past production's, computed server-side from the two ledgers —
there is no list to send and nothing to choose, for the same reason `db migrate`
computes no hashes. The tail applies through the *same* path `db migrate push`
uses, so promoting twice is safe and the second run reports everything as
skipped. `--dry-run` shows the plan.

**A real promote asks first.** It is the only `db` verb that changes
production's schema, so on a terminal it prompts `[y/N]` naming the database,
and `--yes` answers up front. Without a terminal — CI, an agent, anything
piping — `--yes` is *required*: the command refuses rather than prompting into a
pipe that will never answer, because a job that did not pass it did not mean to
change production. `--json` counts as "no terminal" for the same reason. Only
`--dry-run` is exempt, since it changes nothing.

Two things it is not:

- **It replays SQL, not data.** A migration that seeded rows from whatever was
  in the dev copy runs against production's rows instead. Any migration in the
  tail that writes rows comes back as an advisory `note`, printed and never
  fatal — only the person promoting can tell a deliberate backfill from a
  surprise.
- **It is not a sync.** Nothing comes back down; the copy is refreshed by
  refreshing it.

**Divergence** is the failure worth knowing about: the shared prefix of the two
ledgers disagrees, because production moved on or the copy was edited at a
position production already has. The server refuses, applies nothing, and says
so in words — a plain HTTP 400, so an ordinary non-zero exit covers it and the
command needs no second mechanism. The fix is to refresh the dev copy (which
rebases it on production), re-apply your migrations on top, and promote again.
`db promote` also exits non-zero on drift, so it works as a deployment gate
without parsing its output.

**A reused migration name is refused too**, naming it and telling you to rename
it on the copy. The ledger's names are not unique and the server resolves a name
to its recorded hash newest-first, so promoting a second migration under a name
production already has would make every later `db migrate status` report that
file as drifted — and drift is the one signal that must never cry wolf.

**The binding is local-only** — `.ronja/database.json`, keyed by
`(url, tenantID)` through `wfdir.InstanceKey`, written only after a successful
`push`. A `--env dev` push records it exactly as a production push does, because
the id it records is the production one either way. It is deliberately *not* a
committed `ronja.json`: `wfdir.FindRoot` walks parents and `LoadManifest`
refuses a non-`workflow` kind, so a committed
`kind: "database"` manifest at a repo root would break every `wf` command
beneath it, and a workflow manifest above a `migrations/` folder would break
`db migrate`. A shareable binding needs the manifest layer split properly; it is
not worth coupling to this.

## Workflow folders (`ronja wf`)

`ronja workflow`, aliased `wf`, develops one workflow from a local folder:
clone → edit → validate → push → test → publish → run. The code lives in the
customer's own repo and their own editor; Ronja holds the draft.

```bash
ronja wf clone workflow-abc            # or: ronja wf init --from script.py --feature collection-abc
$EDITOR main.py
ronja wf validate                      # server-side check, saves nothing
ronja wf push                          # validates, then syncs the folder into YOUR draft
ronja wf test --param month=2026-07
ronja wf publish                       # commit, or submit for review, and say which happened
ronja wf run --param month=2026-07     # run what is now live, and wait for it
```

A **durable** workflow — which is what a first push creates unless the folder
declares otherwise — adds one verb to that loop: `ronja wf test --resume`
continues a failed run instead of starting over. See "Durable workflows" below.

Two or three files describe the folder — see **Stacks** below for which, and
why the third one exists:

- **`ronja.json`** — the manifest, committed to the customer's git. Title,
  entrypoint, declared `parameters`, and either named **`stacks`** (the current
  shape) or a legacy `instances` list — one entry per (instance, organization)
  the folder has been pushed to, so one folder can target a local backend and
  production, or two organizations on one instance, without any binding
  overwriting another. No matching entry means "never pushed there"; the first
  push creates the workflow and records the binding.

  ```json
  "instances": [
    {"url": "https://app.ronja.tech", "tenantID": "ten-…",
     "workflowID": "wf-…", "featureID": "col-…"}
  ]
  ```

  **`init` says which organization it is binding to, and confirms `--feature` is
  reachable there.** Its report ends `Target: <organization> on <url> (not pushed
  yet)`, and `--json` carries `tenantID` and `organization` beside `url` — the
  binding is a pair, and reporting half of it left the other half to be
  discovered by `status` later. Before it writes anything it makes ONE `GET
  /api/v2/feature/:id` — plus the `GET /api/v2/authentication/me` a `$RONJA_TOKEN`
  credential already owes, since that credential carries no organization by
  construction and the report names one. A feature this credential cannot reach is
  refused with the directory left exactly as it was found, so a refusal never
  leaves a committed manifest naming a feature that was never going to work. Two
  things are a warning rather than a refusal: an instance that does not ANSWER (a
  5xx, a 429, no network) — `init` needs a credential, not a live instance — and a
  401/403 on the probe itself, which is an answer about the CREDENTIAL rather than
  about the feature (that route is `structure:read`, while the folder loops are
  `automation` and `data`, so a narrowly-scoped PAT can push this folder perfectly
  well and still be refused the question). Same check on `ronja bind --feature`,
  which is the other way a folder acquires an organization, and there too it runs
  before the folder is opened — opening one migrates a legacy manifest into the
  `stacks` shape, and a check made after that would rewrite a committed file and
  only then refuse.

  **`push` and `validate` name the organization when the feature is not reachable
  in it.** The server cannot tell "another organization" from "someone else's
  private feature" from "mistyped" — RLS makes them one query — so neither does
  the CLI: it says the organization has no such feature *you can reach*, lists
  the three possibilities, and points at `ronja bind --stack <name> --feature
  <id>`. It does not suggest `--no-validate`, which would fail identically one
  step later at create.

  **Nor does it suggest `--no-validate` when the instance did not answer.** A
  5xx, a 429 or a connection that never landed is not a verdict on the folder,
  and `wf push` runs validate before any write, so it says exactly that:
  *validate before pushing: the instance did not answer (…) — nothing was
  pushed, and this is not a verdict on your files. Try again in a moment.*
  Offering to skip the check on an instance that did not answer would push
  files nobody has looked at. ("Did not answer" rather than "is down" is the
  whole claim the CLI can make: it saw silence, and a 5xx from a healthy
  instance behind a broken dependency looks the same from here.) `ronja wf
  validate` says the same thing, minus the words about a push — it saves
  nothing, so what an outage costs there is the check, not the folder.

  The organization is part of the key because workflow ids are scoped to one.
  Keyed by URL alone, a person in two organizations on one instance has a single
  entry for both, and a push made while signed in to the second sends the
  first's workflow id under the second's token. It is deliberately **not** keyed
  by profile name: this file is committed and shared, and your `prod` is not
  mine. A `tenantID` here is not a secret — it is an opaque identifier that
  appears in ordinary API paths and is useless without a credential.

  A signed-out `wf status` has no organization to match on; it falls back to
  matching on URL alone when that is unambiguous, and returns
  `ErrAmbiguousInstance` — saying so rather than guessing — when it is not.

  **A `$RONJA_TOKEN` credential carries no organization**, deliberately: the
  environment token and the stored profile are independent, so a profile's
  organization says nothing about which one the token reaches. That is why the
  folder asks the server (`GET /authentication/me`) **before** looking its
  binding up, whenever the credential does not already name an organization and
  the folder already names an entry on this instance. Without it the lookup falls
  back to URL alone and adopts whichever organization's binding happens to be
  committed — and the push then sends that organization's resource ids under your
  token, which is the exact failure the per-organization key exists to prevent.
  The cost is one request, and only for a credential that genuinely does not know
  its own organization: an environment token always, and a **profile stored with
  an empty `tenantID`** — which is what login writes for a user who belonged to no
  organization at the time, and never rewrites afterwards. A profile that records
  one asks nothing, which is what `--profile` is for in CI.

  A command that ACTS on the bound row refuses when that lookup fails: without an
  answer it cannot tell which binding is yours. `status` reports the failure and
  still delivers its local half — see below.

  When the folder names **several** organizations on one instance and the
  credential names none, no command that acts on the bound row will guess: the
  refusal lists them and tells you to name one with `--profile`. There is
  deliberately no `--instance` or `--target` — two ways to name an organization
  is the same ambiguity one layer up. `status` is the exception and reports it
  instead, because degrading is the whole point of the command you run when you
  are already suspicious. The same applies when the organization lookup itself
  fails — a rotated token, a 5xx, no network: `status` prints the local half with
  the failure as its "not checked" reason, so a CI pre-flight under a dead
  credential still tells you what changed locally (and `pipeline status` still
  exits non-zero, because "I could not look" is not "nothing moved").

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

  `runtime` is the workflow's runtime version — `1` for the standard runtime,
  `2` for a **durable** workflow, `3` for a durable workflow whose container
  additionally holds no credential for Ronja's table storage (its code reads a
  table only through `tools.query`). **Absent means "let the instance choose",
  and a new workflow is created on `3`** — so a folder that wants `1` or `2` has
  to say so. Unlike `parameters` it is a plain int with no three-state pointer,
  because the third state has nothing to describe: the runtime moves **one way**,
  so a pointer would publish a distinction no code could act on. `wf init` writes
  the key whenever `--runtime` names one (`1` included), `wf clone` writes the
  runtime the cloned workflow is on (`1` included — the folder is committed, and
  re-pushing it into a NEW workflow elsewhere has to recreate the runtime the
  code was written for), and the push that CREATES a workflow records whatever
  runtime the instance stamped — but only a runtime this CLI knows, since
  recording a `4` it cannot open would brick the folder the push just created.
  The key is committed, and the folder is the only lasting statement of the
  runtime its code is written for. An absent key **declares nothing**, and every
  reader treats it that way: an unpinned folder bound to a workflow on any
  runtime pushes without complaint, and `wf status` reports no drift for it.
  `validate` — the standalone command and the
  pass inside `push` — rehearses against that same runtime, so a folder declaring
  none is checked against the one it is about to be created on rather than
  against no runtime rules at all. A push carries the declaration to a workflow
  on a lower runtime (`runtime  1 → 2 (Durable)` in the report; the patch lands
  on your draft, and `wf publish` commits the flip).
  Declaring a runtime BELOW the one the workflow already has is **refused** — the
  runtime cannot be lowered. A value this CLI does not know (`"runtime": 4`) is
  refused when the folder is opened, so it fails at the push rather than at the
  first unattended run.

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

  A CLI that does not know `reportingTimezone` **carries it through untouched**
  rather than dropping it — see "Unknown keys are preserved" below. Builds from
  before that change still strip it, and there are still some in the field, so a
  hand-added declaration that vanishes from a committed file means somebody on
  one of them rewrote it; `ronja --version` reports what you are running.

- **`.ronja/state.json`** — the sync baseline, never committed (`clone` and
  `init` write `.ronja/.gitignore` containing `*`). One entry per (instance,
  organization) binding, exactly like the manifest — the row the baseline came
  from and a sha256 per path — so a folder bound to localhost, staging and
  production keeps three, and a command acting on one instance touches only its
  own. It is local because drafts are
  per-user — a colleague cloning the repo must not inherit somebody else's
  baseline.

Three rules cover the manifest as a whole, in every folder kind — `wf`, `app`
and `pipeline` share one manifest format:

- **Unknown keys are preserved.** `ronja.json` is a committed file, and the
  people sharing it are not all on the same CLI build. A key this build has no
  field for — one a newer `ronja` writes — is carried through a rewrite with its
  value and its position intact, so `push`, `discard` and `clone` no longer
  quietly truncate a folder's declaration to whatever the running version
  happens to understand.

  Preservation is **per object, and the objects are the file itself and each
  `instances` entry**. Every key at the top level of `ronja.json` is kept, and
  every key on an entry. A key nested INSIDE one this build knows is **not**: an
  unrecognised key within an entry of `parameters`, or within the `access`
  block, is still dropped by a rewrite, because each of those is read into a
  value of its own and written back whole.

  What is *not* preserved is hand formatting. Values are re-encoded, so the file
  comes back with the two-space indentation the CLI has always written, and the
  characters JSON escapes (`&`, `<`, `>`) come back in their escaped spelling —
  so a hand-written URL with a query string is one diff line the first time a
  command rewrites the file. The value itself is unchanged, and every build
  escapes the same way, so the line does not come back a second time.

- **`formatVersion`** is the escape hatch for a change preservation cannot
  absorb — a key whose meaning moved, rather than a key that was added. An
  **absent** `formatVersion` means version 1, which is what every manifest
  written so far is, so the CLI never writes the key for it. A manifest
  declaring a version **higher** than the CLI understands is **refused**, naming
  the remedy (upgrade `ronja`) and leaving the file untouched — a refusal, not a
  best-effort read that would write the file back with the newer half missing.
  Write it as a bare number: `"2"`, `true` or anything else that is not a number
  is refused too, naming the file, because a manifest whose format cannot be
  established is better refused than half-read. Leaving the key out — or writing
  `null` — means version 1. **Version 2 is stacks**, below.

- **`stacks` and `ronja.lock.json`.** See the next section.

### Stacks (`--stack`)

A **stack** is one named environment this folder deploys to — `dev`, `staging`,
`prod`. It is what `--stack` selects, and it is the unit the committed files are
keyed by.

The word is `stack` and not `workspace` (a Ronja primitive) or `env` (which
collides with `ronja env` and with "environment" meaning the shell's). And it is
an **environment name, never a profile name**: `ronja.json` is committed and
shared, so `prod` has to mean the same thing to everybody, while the credential
that reaches it stays local. **Two selectors, no third** — `--stack` names an
environment, `--profile` names a credential.

A stack folder has three files, and the split is what a human decided / what a
deploy recorded / what belongs to one person:

```
myfeature/
  ronja.json          declaration + stack CONFIG   — committed, human-edited
  ronja.lock.json     per-stack STATE              — committed, machine-owned
  .ronja/state.json   per-USER state               — local only, never committed
  main.py
```

```json
// ronja.json
{ "formatVersion": 2, "kind": "workflow", "title": "Region Report",
  "entrypoint": "main.py",
  "stacks": {
    "dev":  {"url": "http://localhost:8098",  "tenantID": "development", "featureID": "col-…"},
    "prod": {"url": "https://app.ronja.tech", "tenantID": "ten-…",       "featureID": "col-…"} } }

// ronja.lock.json
{ "stacks": {
    "prod": {"workflowID": "wf-…", "headVersionID": "wf-…v3"},
    "dev":  {"tables": {"revenue.sql": {"tableID": "table-…", "liveSHA256": "…"}}} } }
```

**Why the second committed file.** Before this, `ronja.json` did two jobs: it
described the resource *and* recorded which server row it was on each instance.
So every CI push dirtied the file a reviewer reads, and if nothing committed the
result back the next deploy found no binding and **created a second set of
resources**. Splitting them puts the churn in a file whose churn is expected, and
— because the lock is committed rather than local — gives a fresh CI checkout a
drift baseline for the first time.

**What goes where.** The test is *who decided it*, not who wrote the bytes:

| | file | why |
|---|---|---|
| `url`, `tenantID`, `featureID` | `ronja.json` | a person chose where this deploys and which feature it lives in — even though the first push is what wrote it down |
| `workflowID` / `dataAppID` / a pipeline's `tables` | `ronja.lock.json` | a deploy created them |
| a pipeline's `liveSHA256` | `ronja.lock.json` | "the live table held these bytes when this folder last agreed with it" is true for everybody |
| a workflow's / app's `headVersionID` | `ronja.lock.json` | "this folder forked from that published version" is true for everybody |
| draft ids, draft fingerprints | `.ronja/state.json` | a draft is **yours**; a colleague inheriting one from git would be pointed at a row they cannot see |

**The head-version pointer, and why it is not a fingerprint.** A pipeline can
commit a *hash* of the live table's SQL, because the live table is one row
everybody shares. A workflow and a data app cannot: both sync their files into
**your own draft**, so every fingerprint either kind has describes one person's
row, and committing one would hand a colleague a baseline for a row they cannot
see. What is shared is the published lineage — which committed version this
folder's content was forked from — so that is what the lock records, read from
`GET /workflow/:id/versions` and `GET /dataapp/:id/versions` (element 0 is the
head; an empty list means the row has never been versioned and its own id is the
head).

It is written at the moments the claim is actually true, and nowhere else:

- **`clone --stack`.** Cloning the live row anchors on the current head. Cloning
  **your open draft** anchors on *that draft's* base version instead — the head
  may be newer, and a folder that recorded a publish it does not contain would
  later overwrite it without a word.
- **the push that creates the resource.** Nothing has been published, so the row
  is its own head.
- **a push that forked a fresh draft off live.** Whatever it then wrote descends
  from the version it forked from.
- **`publish`.** The commit made a new version; the folder that produced it is
  the folder that agrees with it.
- **a `--force` push**, which was told to proceed past a difference and must not
  re-report the same one for ever.

A push into a draft that was **already open** records nothing: that draft may
hold a web-builder edit forked from an older version, and stamping today's head
onto a folder whose content never saw it is exactly the silent overwrite this
guards against.

**Selecting one.** `--stack prod` is explicit. With no flag the folder matches on
`(url, tenantID)` exactly as it always has, so nothing about today's ergonomics
changes; `--stack` becomes *required* only when that match is not unique. Four
refusals guard the flag, and each one is a duplicate resource somebody would
otherwise have discovered later:

- `--stack prod` naming a stack that points somewhere this credential does not
  reach — acting on either half would be a guess.
- `--stack prd` where this organization is already called `prod` — a typo, and
  the message names the stack that was meant.
- naming a **second** stack for an (instance, organization) this folder already
  names — the local baseline is keyed by that pair, so the second stack would
  compare your drafts against the other environment's rows. One folder, one
  deployment per organization; use a second directory for a second one.
- a new stack name differing from an existing one only by **capitalisation**.
  They are two JSON keys and one word to everybody reading them, so `--stack
  Prod` against a folder naming `prod` would create a second set of resources
  rather than finding the first.

Neither of the last two is a *load*-time refusal, and that is deliberate: **the
CLI must never write a manifest it will then refuse to read.** Both pairs can
arrive with no writer involved — a git merge bringing two branches' stacks
together is a clean textual merge — and enforced at load one merge accident made
every command fail, `status` and an explicit `--stack prod` that resolves
perfectly well included, with hand-editing a committed file as the only recovery.
So they are enforced where a name is *accepted*, and a folder that is already in
that state stays usable: `--stack <name>` resolves, and a selection with no flag
reports the ambiguity and names both. **A folder must always be recoverable by
naming a stack.**

A declared `--stack` **pins the organization from the repo**, so a CI job under
`$RONJA_TOKEN` needs no stored profile to say which one it means. It does not
skip the `GET /me` that credential already pays: the stack says which
organization the folder *means* and `/me` says which one the token *reaches*,
and a stale declaration or a token exported for the wrong organization is
exactly where those differ. Asked, the disagreement is refused by name; skipped,
the push resolves the stack's `featureID` under a token that cannot see it and
reads the 404 as "create it".

**Migration, and how a folder stays on the old shape.** A folder written before
stacks keeps its `instances` list, ids and all, and **no push rewrites it** — a
legacy push produces a byte-identical manifest and never creates a
`ronja.lock.json`. That is deliberate: a stack manifest is `formatVersion` 2,
and a CLI old enough to predate the version gate reads one by silently dropping
the key it does not know, leaving a manifest that names no binding at all.

A folder moves when a **person names a stack**, which is the one thing an
`instances` entry does not have and the one thing that must never be invented —
a derived name would show up in a committed file and in a colleague's `--stack`
argument. So:

```bash
ronja wf push --stack dev      # names this organization's entry "dev", once
```

The entry moves across whole: `featureID` into `stacks.dev`, the ids into
`ronja.lock.json`, and the `instances` entry is dropped so the two shapes can
never both answer for one organization. A pipeline folder's live fingerprints
come across at the same moment, out of `.ronja/` and into the lock — left
behind they would be correct for whoever ran the migration and invisible to
everybody else, which is the opposite of what the lock file is for. The command says so on stderr, because
it changes the format of a committed file. Migration is **per organization**, so
a folder bound to two can be halfway through it: both shapes are read, and only
the named half is written in the new one.

`clone` and `init` take `--stack` too. Without it they write the legacy shape —
opt-in until the version gate is common in the field. `init --stack dev`
declares the stack and creates nothing, so it writes **no** `ronja.lock.json`
until the first push has an id to record; the folder is nonetheless *bound* to
that stack from the moment it is declared, and a later plain `push` creates the
row inside it rather than beside it.

**The lock is written first, and that is recoverable.** `SaveFolder` writes
`ronja.lock.json` before `ronja.json`, so a push that created a workflow and
then failed on the manifest leaves the id under a name no stack declares. Naming
it — `--stack dev` — adopts those ids rather than creating a second workflow, and
the same command declares the stack, so the folder is whole again and works with
no flag. The other order has no such recovery: a stack with no lock entry reads
as never-pushed, which is exactly the duplicate creation the lock file exists to
stop.

That recovery lives behind the flag, so a **flagless** command in the same state
is **refused** rather than recovered. The implicit selector reads the folder
through the manifest's `stacks`, which the stranded half is missing from, so it
would see nothing, take the create path, and make the duplicate — while
recovering here would mean guessing which of the lock's stacks was meant, and a
wrong guess pushes one environment's files into another's. The refusal names the
stacks the lock knows and asks for one with `--stack`.

The verbs:

| Verb | Does | Flags worth knowing |
|---|---|---|
| `init` | writes the two files, optionally copying in a script you already have. Creates nothing server-side | `--from`, `--feature` (required), `--title`, `--runtime` |
| `clone` | copies a workflow's files down into a new folder. Prefers your open draft over live when you have one. Creates nothing server-side | — |
| `status` | local changes, remote lifecycle/draft, and drift since the last sync. Read-only, and works signed out for the local half | `--json` |
| `validate` | posts the folder to `POST /workflow/validate`. Persists nothing; works before the workflow exists | `--json` |
| `push` | validates, ensures a draft (`checkout`, or `POST /workflow` on a first push), syncs files | `--no-validate`, `--force`, `--allow-dropped-bindings` |
| `test` | runs the draft and polls to completion | `--param k=v`, `--write-live`, `--stale-ok`, `--resume`, `--follow`, `--timeout`, `--logs` |
| `publish` | publishes a parentless draft, commits an attached one, or submits it for review | `--no-request-review`, `--overwrite-remote` |
| `run` | runs the LIVE workflow this folder is bound to and polls to completion. Takes no workflow id | `--param k=v`, `--follow`, `--timeout`, `--logs` |
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

  **A fresh checkout is not drift, and `--force` is not the way through it.**
  `.ronja/` is git-ignored, so a CI job and a colleague's `git clone` both start
  with no baseline at all — and that used to leave `--force`, which is also the
  flag that switches the per-file preconditions *off*: the documented CI path was
  the destructive one. A **stack** folder no longer needs it. The committed
  `headVersionID` says which published version this folder forked from, so a
  baseline-less push reads the current head and:

  - **it matches** — nothing has been published since, so the push proceeds and
    the file writes assert the live bytes they just read. Safer than the
    `--force` it replaces, not merely quieter. A note on stderr says which guard
    let it through.
  - **it moved** — somebody published. Refused, naming both versions. The
    refusal does **not** offer `--force`, because using it there would discard
    the publish the refusal just found; the remedy is to look at what they did
    and `clone` again to re-apply on top of it.
  - **it matches but somebody's EDIT DRAFT of the live row is already open** —
    refused too. The pointer speaks for the live row and says nothing about the
    bytes in a draft this checkout has never seen. `ronja wf discard` clears it.

  A resource that has **never been published** is not that third case, and the
  distinction is load-bearing. `init` → `push` → commit → CI is entirely
  ordinary — publishing is a separate manual step — and what it leaves on the
  server is a *parentless* draft: no live version behind it, no edit shadow over
  it, the row *is* the resource this folder created. That vouches like any other,
  anchored on its own id (an unversioned row's head), and the anchor starts
  moving the moment somebody publishes it. Treating it as somebody else's
  half-finished edit was worse than a refusal: the remedy the message named,
  `ronja wf discard`, *refuses* a parentless draft and routes to
  `--delete-workflow`, which deletes the resource.

  **`--force` CLEARS the pointer rather than advancing it**, when it was used to
  push past a version this folder had not seen. `pipeline push --force`
  re-records the live fingerprint of the row it just wrote; a forced `wf`/`app`
  push writes a *draft* and leaves live alone, so there is no row whose pointer
  it could honestly re-record. Advancing would let the next fresh checkout vouch
  for a publish nothing here contains and overwrite it with preconditions that
  pass — a refusal converted into a silent overwrite. Cleared, the folder simply
  goes back to asking for a baseline. `discard` leaves the pointer alone for the
  same reason: it throws the draft away and leaves your files exactly as they
  are.

  A **legacy `instances[]` folder** has nowhere committed to keep a pointer, so
  it behaves exactly as it did — including refusing, and including naming
  `--force`. It also makes no extra request: the version read is paid only by a
  folder that can record the answer. A **data app cloned from your own draft of
  an app that has published versions** is the other folder with no pointer:
  `data_apps.base_version_id` holds the live app's own id rather than a version
  (unlike `workflows.base_version_id`), so which version that draft forked from
  is not recorded anywhere the CLI can read. The clone says so and records
  nothing, rather than an anchor that could never match.
- **`--write-live` on test, and no y/N prompt.** A draft checked out from a live
  workflow inherits its output tables, and there is no test sandbox, so running
  it replaces a production table. A prompt would get muscle-memoried; a flag
  nobody types by accident does not. A workflow that binds no output tables yet
  — before you push a `{{ write }}` marker — runs without it; the save path
  re-derives the bindings on every push, so a converted script that writes a
  table needs the flag from its first push, published or not. An approval-gated
  workflow is refused outright rather than worked around — disabling the gate
  on the draft would ride into the parent on publish. That refusal is about the
  **legacy pre-run gate**, which is deprecated and can no longer be turned on:
  it holds the whole run and only a Ronja chat session can satisfy it. Ask Ronja
  to convert the workflow to a **mid-run approval** (`tools.requireApproval` in a
  durable workflow), which parks the run where the decision belongs and works
  from `wf test`, automations and data apps alike; the configured approvers and
  delivery carry over unchanged.
- **Exit zero means the thing did not fail.** `validate` exits non-zero on error
  findings; `test` and `run` exit non-zero on a failed run, on a timeout, and on
  a status they do not recognise — but zero on a run that finished AND on a
  durable run that parked (`waiting`) or was handed to a successor (`resuming`),
  neither of which is a failure. That verdict is one function (`runVerdict`), so
  the two commands cannot come to disagree about what a park is worth. Ctrl-C
  during either stops the waiting, not the run — it is caught explicitly so the
  person is told that.

### Dependencies (`ronja bind`)

A **dependency** is a name this folder's *code* uses for a row it does not own,
and it is what makes a folder deployable to more than one organization.

The problem it solves is that **a resource id belongs to exactly one
organization**. A folder whose SQL says `{{ ref('table-abc') }}` can only ever be
pushed where `table-abc` was minted; cloned into another organization and pushed,
it builds against an id nobody there has, and the refusal it earns names an id
that means nothing to the reader. So the code says `{{ ref('orders') }}` instead,
`ronja.json` declares `orders` as a dependency of kind `table`, and each stack
says which of *its* organization's rows answers to that name:

```json
{
  "formatVersion": 3,
  "kind": "workflow",
  "dependencies": {
    "orders":    {"kind": "table"},
    "hubspot":   {"kind": "secret"}
  },
  "stacks": {
    "dev":  {"url": "http://localhost:8098", "tenantID": "development", "featureID": "col-…",
             "bind": {"orders": "table-…", "hubspot": "secret-…"}},
    "prod": {"url": "https://app.ronja.tech", "tenantID": "ten-…", "featureID": "col-…",
             "bind": {"orders": "table-…", "hubspot": "secret-…"}}
  }
}
```

**The alias namespace is the FOLDER's own**, and that is the property that pays
for the whole layer. Two tables called `orders` in one organization stop being a
problem to manage and become a problem to *bind once*: whatever the rows are
called, this folder calls one of them `orders` and says so in one line. Nothing
about the name reaches the server — resolution happens in the CLI, before the
request — so the server's authorization surface never grows a second grammar to
check. A `bind` value is always an id.

**Both halves are config.** `dependencies` is what the code needs, `bind` is
which row answers it here, and neither is ever written by a push — a `bind` map
lives in `ronja.json` beside its stack and never in `ronja.lock.json`. See
`wfdir.Dependency` and `Stack.Bind` for the reasoning, which is the same
who-decided-it test everything else in those files passes.

**A data app's `access` block takes an alias too**, and it has to, because a data
app's dependencies are not all in its content: the `allowed*IDs` lists are raw
ids nothing derives from source, so a folder carrying its code in alias form and
its allowlist in id form would still be pushable to exactly one organization.
Each list resolves against the kind its rows are — `allowedTableIDs` a `table`,
`allowedSecretIDs` a `secret`, and so on — and an entry that is neither a
declared alias of that kind nor id-shaped is left exactly as written for the
server to refuse. See "What the app may read" under `ronja app` for the block
itself.

⚠️ **`allowedMetricIDs` takes a `table` alias**, and there is deliberately no
`metric` dependency kind. A metric is a `kind='metric'` row in `models_v2`
carrying a `table-` prefixed id, so it is already inside the id namespace
`markers.IsResourceID` and `Manifest.CheckBind` test against, and `GET
/api/v2/search` — which is how `ronja bind` answers a declaration — reports one
as kind `table`. A kind of its own would match no hit and report "nothing is
called that" for a row sitting right there. What the two share is the alias
namespace and not the grant: the server still checks each list against the row
it names, so listing a plain table as a metric is refused there exactly as it is
when written as an id. `commands.accessDependencies` carries the full reasoning.

**`formatVersion` 3 is refused BY ITS ABSENCE, at push time.** A manifest that
declares `dependencies` while stating a lower version is one this build cannot
write — `MarshalJSON` stamps 3 on any manifest that carries one — so it only ever
arrives hand-authored, which is what copying the JSON out of a guide into an
existing file produces. It is refused because it defeats the gate that exists for
it: the file MEANS v3 (the ids are nowhere in the source) while DECLARING that a
v1 CLI may read it, so that CLI passes the version check, resolves nothing, and
sends the alias as a literal id — a warning for a secret, and a green push that
deployed nothing. At ACCEPTANCE rather than at load, on `checkStacks`'s
discipline: a folder somebody else wrote must stay openable by `status`, and the
message quotes the one line to add. `Manifest.CheckDeclarations` also holds the
UNKNOWN-KIND refusal for the same reason — adding a kind adds no key, so the
version gate cannot catch a folder written by a newer CLI, and refusing that at
load would take `status` away from the person best placed to see why.

**Two ways of using an alias wrongly are refused**, and both are otherwise
completely silent, because the codec keys on `(kind, name)` and a MISS leaves the
argument exactly as found:

- **A declared alias under the WRONG marker family**, any kind. `orders` is
  declared a `table`, the code writes `{{ secret('orders', 'token') }}`, and
  every other check passes — `CheckBind` sees a declared alias bound to a table
  id, `literalBindTargets` sees no literal id, and the unused-alias warning stays
  quiet because the table-kind key IS used by the ref beside it. The message
  names both kinds.
- **An unresolvable argument, `secret` only.** Deliberately asymmetric: the
  secret legs are the ONLY ones the server answers with a WARNING rather than an
  error (`rworkflow`'s `secretIDs` / `querySecretIDs` are `SeverityWarning`;
  `ref`, `write`, `agent`, `codex`, `mailbox` and `workflow` are all
  `SeverityError`). Every other family hard-rejects the save, and that refusal is
  older and better-aimed than anything invented client-side, so those are left
  alone.

Both are GATED on the folder declaring dependencies at all, which keeps the layer
inert for every folder in the field, and both scan source as TEXT — so a
commented-out marker trips them. Accepted rather than mirroring
`stripLineComments` and `stripTripleQuotedBlocks`, whose drift would be silent in
both directions; deleting a dead line is a real fix, and the messages say so.

**`wf validate` and `app validate` fail on an alias refusal** — non-zero, `ok:
false`, and the refusals in the `--json` payload under `aliasRefusals`. They are
the commands CI gates on, and a green verdict on a folder `push` refuses is the
one output that costs somebody the deploy it was run to protect. Warnings stay
warnings. `app validate` grew an `ok` beside `compiles` for this: a draft can
compile perfectly while the folder cannot be pushed, and collapsing the two would
make `compiles: false` a claim about a compiler that was happy.

**Writing a bound id literally is refused**, in an `access` list as much as in a
source file, and the damage differs enough to be worth knowing. A source that
writes a bind target's id reads as *drifted for ever* — the remote read
de-aliases it back to the alias and the local file keeps the id. An allowlist
that writes one drifts not at all: an id compares equal to itself, so nothing
warns, and the folder simply grants the first organization's row wherever it is
pushed. The first anyone knows of it is an app in the second organization that
can read nothing, which is why it is a refusal and not a note.

**A pipeline folder resolves a SIBLING's stem before it looks at a
declaration**, and the file you can see wins over a declaration you have to
scroll for: `{{ ref('orders') }}` in a folder holding `orders.sql` names that
file, whether or not the folder declares `orders`. Three rules fall out of it,
and each one is permanent drift or a wrong table if it is missing:

- **A declaration colliding with a stem is refused**, at the point the name is
  invented rather than shadowed at the point it is used, because shadowing is
  visible only to whoever already suspects it. The comparison **folds case**:
  `ronja bind` answers a declaration with `strings.EqualFold` against a hit's
  title (it has to — the endpoint's own exact test is `ILIKE`), so `Orders.sql`
  plus a declared `orders` binds the declaration to the very row the file
  created, and one word then names one row twice while `ronja.json` asserts they
  are different.
- **A file that spells ONE sibling two ways is refused.** Both spellings are
  legal on their own — the stem, and the literal id every folder written before
  this slice uses — but `toWire` collapses them to one id, so the row stores a
  single spelling for the pair and the de-alias (one answer per id, necessarily)
  rewrites both occurrences to it. The literal one reads back as a word the file
  does not contain: drift on every `status`, reproduced by every push, and
  nothing the author can edit fixes it. `literalBindTargets` cannot cover this —
  it is about a declared alias's *bind target*, and a stem is not one.
- **Two files of one stem make that stem ambiguous**, and a ref naming it is
  refused rather than resolved to whichever sorted first. It is a legal layout
  (two tables of one name is something the schema permits), so picking silently
  would build the wrong table with no diff to look at.

**Filling in the second half is what `ronja bind` is for.**

```bash
ronja bind --stack prod
```

For every declared name with no answer on the selected stack it searches the
organization (`GET /api/v2/search`) for a resource of that kind called *exactly*
that, and offers what it found. Then it writes the accepted answers into
`ronja.json` and exits **non-zero while any declared name is still unbound**, so
CI can run it as a check rather than only as an authoring aid.

It is **kind-agnostic** — `dependencies` is a key all three folder kinds carry —
which is why it sits at the root of the command tree rather than inside `wf`,
`app` and `pipeline` as three commands that would have to stay identical.

Five rules, and each one is a wrong deploy that does not fail:

- **Only an exact name match is proposed.** The search endpoint scores 3 exact,
  2 prefix, 1 substring, and only the exact bucket is acted on. A near match is a
  guess, and a wrong bind deploys a workflow against somebody else's table while
  reporting success. Near misses are *reported*, not applied.
- **The title is checked as well as the score.** The endpoint merges a tag leg
  that stamps a matching **tag**'s score onto whatever it is attached to, so a
  table *tagged* `orders` arrives as an exact hit whatever it is called. A tag is
  a label somebody put on a row, not the row's name.
- **Ambiguity is never resolved, `--yes` included.** `--yes` says "I trust the
  unambiguous ones"; it is not an answer to "which of these three tables called
  `orders`". Zero matches and three matches are reported as different things,
  because "no exact match for `orders`" and "three tables are called `orders`"
  send the reader somewhere completely different.
- **A TRUNCATED result set proposes nothing.** `bind` asks for exactly the
  endpoint's hard cap (`maxTotalCap`, 40) and there is no paging, so a request
  that comes back FULL is a prefix of the answer rather than the answer — and the
  tag leg is what makes that reachable rather than theoretical, since a widely
  used tag fills the page with exact-scored hits and the second row genuinely
  called `orders` never arrives. A full page is reported as `ambiguous`, with the
  one candidate it did see named, so the invariant above holds structurally
  instead of resting on the page happening to be complete.
- **A codex cannot be proposed at all.** The search endpoint does not fan out to
  codexes, so an empty result there means "nobody looked", not "nothing is called
  that". Reported as exactly that, and set by hand.

Two smaller edges worth knowing. An alias shorter than two characters is below
the endpoint's minimum term and is reported as *not looked up* rather than as not
found — the wire answer for the two is identical. And a **scoped token with no
`secrets:read` grant** gets no `secret` hits at all, so a `secret` dependency
reports no match under a credential that simply cannot see secrets; the message
says so. A `mailbox` dependency carries the same class of hint for a different
gate — the endpoint's mailbox leg answers tenant admins only — so a non-admin's
empty result is about who they are rather than about what exists.

**`--feature` declares the stack in the same command.** Promoting a folder to an
organization it has never deployed to needs a stack to bind against, and without
that the two halves of this slice deadlock politely: `push --stack prod` refuses
because `orders` is unbound, and `bind --stack prod` refuses because `prod` is
undeclared. Both point at the manifest, so a hand edit breaks the tie — and the
promotion loop then reads as "edit JSON, then run the command that exists to stop
you editing JSON".

```bash
ronja bind --stack prod --feature col-…
```

The `url` and `tenantID` come from the credential; `--feature` supplies the one
thing nothing can work out for you, which is where this organization's new
resources get created. That is not a blurring of the config/state split: a person
typing `--feature` on a command line is the same human decision `init --feature`
already is, which is exactly why `Stack.FeatureID` is config even though a push
is what usually writes it down.

Four rules on the flag, and the reasoning is the same each time — an environment
is something a person names:

- **Without `--feature`, an undeclared stack is still refused**, and the refusal
  now names the flag. A stack declared with no feature is a half-answer in a
  committed file that reads as a decision somebody made — the hazard `adoptStack`
  declines for the same reason.
- **`--feature` needs `--stack`**, and the gate is on the *flag*, not on the
  resolved stack. A folder matches a stack implicitly perfectly well, so gating
  on the resolved name would let a bare `--feature` reach into whichever
  environment this credential happened to match.
- **It will not repoint a stack that already names a different feature.** That
  changes where every future resource is created, and belongs in the file under
  review rather than on a command about aliases.
- **The same feature is a no-op, not a refusal**, so a CI job can run the same
  line twice.

The organization is resolved from the server when the credential does not name
one — which a `$RONJA_TOKEN` credential never does, and that is the credential
the promotion loop runs under. A stack must name an organization (`checkStacks`
refuses one that does not), so declaring one asks `GET /me` rather than refusing
for want of an answer one request away.

**The promotion loop this exists for.** One folder, in one repository, deploying
the same code to two organizations:

```bash
# The folder already works against dev.
ronja bind --stack dev            # fills in dev's answers, once
ronja wf push --stack dev

# Promote it: declare prod and let it answer the same names with its own rows.
ronja bind --stack prod --feature col-…
git add ronja.json && git commit   # the answers are reviewed like any other config
ronja wf push --stack prod
```

The code does not change between the two pushes, and no id from either
organization appears in a file anybody edits by hand. What a reviewer sees in the
diff is one stack and one `bind` map: the list of decisions, stated once.

### `wf run` — the step after publish

`run` is the only verb that touches the **live** workflow, and it is a sync verb
rather than a wrapper because it takes **no workflow id**: it runs the row this
folder's binding names, resolved exactly as `status` and `publish` resolve it. A
positional id is refused with that argument and a pointer at `ronja api`. See
"Transport is not a wrapper" above.

Everything that can refuse does so before the POST, and one of those refusals is
not obvious enough to lose by accident:

- **An open draft refuses the run.** The sync baseline describes the row this
  folder last synced with, which for a folder with a draft is the *draft* — so
  after publish → edit → push, a folder that is perfectly clean against its own
  baseline would green-light a run of the live version the author stopped looking
  at two commands ago. The staleness check cannot see that; only "you have a
  draft, `wf test` runs it, publish or discard first" can.
- **Then staleness, against live.** With no draft in play the baseline *is* the
  live row — `publish` and `discard` both refresh it from live — so the ordinary
  local-changes comparison answers "live is not this folder". A baseline it
  cannot compare refuses rather than guesses: "I cannot tell" has to stop a live
  run, because the claim this command makes is that it ran the code in front of
  you. That is **two refusals with two messages**, not one: *no baseline at all*
  is a copy taken from git and the remedy is a fresh `clone`, while *a baseline
  naming another row* is the review path — `publish` on a shared workflow
  submits the draft and returns without re-anchoring, so once an admin commits
  it the folder names a draft that is gone and holds files that are usually
  exactly what went live. Telling that author to re-clone a perfectly good
  folder is what the split exists to stop.
- **Then the legacy approval gate**, refused with the same wording `test` uses
  and, like it, naming the conversion to a mid-run approval rather than the
  off-switch.

Two server refusals are rendered rather than passed through as a status code. A
**403** is the shared-feature admin gate (`requireWorkflowAccess(write=true)`) —
it names the gate and the two ways forward, ask an admin or let the automation
trigger it — and a **409** is the `skip` concurrency policy: it leads with the
fact that *nothing ran*, and names the blocking run from the response's own
`blockingRunId` detail, never out of the message (see `api.AsRunInFlight`, and
`gt.NewRunInFlightConflict` on the server for why the prose cannot be trusted to
carry it). Everything else — the credit kill-stop above all — arrives in the
server's own words.

A **timed-out run POST is not a refusal.** The server commits the `workflow_runs`
row and only then dispatches asynchronously, so a deadline of *ours* says nothing
about whether the run started — and reported as a failure, the operator's natural
retry fires a second live run into replace-mode output tables. So a timeout reads
the run history back and adopts the newest run stamped at or after the moment we
asked (`adoptTimedOutRun`), then follows it exactly as if the POST had answered.
The comparison is against the *server's* clock, so a server running behind ours
matches nothing and falls through to a message that says the run **may still be
running** and how to check — never that it did not start, which is the sentence
that makes somebody run it again.

A waiting run exits **zero**, exactly as it does for `test`, so the report says
`WAITING:` and "NOT a completed verification" out loud: this is the run somebody
is about to record a green tick against. `WAITING` and `CONTINUED` are the
product's words for the two durable-wait states (`docs-site/TERMINOLOGY.md`,
which bans "parked" and "resuming" for them); the engineering prose in these
files still says "park", because that is internal vocabulary and not what the
reader sees.

There is deliberately **no `--write-live`**. That flag exists on `test` because a
draft's output tables are the live workflow's, and testing would replace a
production table by surprise. The live workflow writing its own bound output
tables is not a surprise — it is what publishing it meant — and a flag everybody
types every time guards nothing.

  `push` exits non-zero for one thing besides a refusal: a push that **landed
  with a dropped binding**. A `{{ secret }}` marker naming a secret you cannot
  reach is the server's soft tier — the workflow saves, the binding is filtered
  out, and every run that touches the marker fails. The finding stays a
  *warning*, which is the right tier for a person about to create the secret;
  what was wrong was the exit code, because it is the only thing CI reads, and a
  deploy that shipped an unrunnable workflow used to report success. The push is
  not undone — the files are on the server and the report says so — and the
  dropped bindings are listed under `droppedBindings` in `--json`.
  `--allow-dropped-bindings` accepts them and exits zero, for the "I will bind it
  later" flow; it is named in the refusal. It is a flag rather than a TTY or
  `--json` test on purpose: the exit code has to mean the same thing wherever it
  is read. `--no-validate` skips the pass that reports them, and so skips the
  verdict.

### Durable workflows (`--runtime 2`, `--runtime 3`)

A durable workflow journals the result of every `@tools.step` under a key
DERIVED from the function and its arguments, so a failed run can be **resumed**
instead of re-run: the journaled steps are replayed and only the work that never
finished executes again.

`--runtime 3` is durable **and** withholds every Ronja table credential from the
container: its code reads a table only through
`tools.query("SELECT ... FROM {{ ref('tbl::...') }}")`, never by reading a
parquet file itself. Everything below about journaling, `--resume` and the
one-way upgrade applies to it unchanged — it is a superset of runtime 2, and
`wf init --runtime 3` scaffolds the same durable `main.py`, with a header stating
that one rule.

**Runtime 3 is what a new workflow gets.** A folder that declares no `runtime`
creates one on 3, and the create summary names the runtime it was given. Pass
`--runtime 1` or `--runtime 2` — or edit `ronja.json` before the first push — for
anything else; after the create it is a one-way upgrade, so the choice is made
once.

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

- **The runtime only ever goes up: 1 → 2, 1 → 3, 2 → 3.** The manifest's
  `runtime` rides the `POST /workflow` body of the push that creates the
  workflow, and afterwards a push **upgrades** a workflow on a lower runtime to
  match — the patch lands on
  your draft, so the flip is reviewed and published with the code change that
  needs it (`ronja wf publish`). The upgrade is reported as
  `runtime  1 → 2 (Durable)`, and `ronja wf status` shows it as pending drift
  beforehand. The other direction does not exist: a durable workflow's journal is
  keyed by the v2 derivation, so lowering it would orphan every journaled step
  and silently re-run the pipeline. A folder declaring a LOWER runtime than the
  workflow already has is refused before the push writes anything — fix `ronja.json`,
  or clone the workflow again. A live workflow is also refused while one of its
  runs is in flight; runs already started keep the runtime they began with,
  resumes included.
  Upgrading does not rewrite your Python. Convert the code in the same push:
  keyless `tools.step`, `tools.now()` / `tools.uuid()` / `tools.random()`,
  `tools.http` for outbound calls, and an entrypoint ending in a bare name.
  `ronja wf push` validates against the declared runtime, so the Durable
  advisories come back in the same command.
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

The resume hint is withheld when **nothing in the workflow's code ran**. "Fix
the code and push first if the failure was a bug" is advice about the author's
files, and a run that died getting S3 credentials or launching its container
never reached them. The discriminator is `processingStartedAt`: the Python
harness stamps it immediately before it `exec`s the workflow — the entrypoint is
compiled inside that `exec`, and the workflow's own imports run inside it too —
so an unstamped run never put a line of the author's code in front of the
interpreter. That ping is best-effort, so steps, a journal, captured logs or a
Python traceback in the error each outvote a missing stamp; the asymmetry is
deliberate, since wrongly saying "nothing ran" would hide a real bug from its
author. When it is withheld the report says so, and says the run is worth
repeating:

```
  Nothing in your code ran — the run failed while Ronja was setting it up
  (get S3 credentials: assume role: …).
  Run `ronja wf test` again; if it repeats, report it.
```

It says where the run died and nothing about whose fault that is: not every
pre-execution failure is Ronja's. A package list `ronja.json` declares is
checked before the container starts, so an `invalid pip packages: …` failure is
the author's — and that one gets an extra line naming the file to open.

That notice is printed for **every** failed run whose code never started —
`wf run` against live as much as `wf test` against a draft, on either runtime —
because what it reports is a fact about the run and not about what a resume
could skip. The command it tells you to repeat is the one you ran. The resume
hint keeps its own narrower conditions: a draft run, durable or with a journal,
whose code did in fact run.

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

#### Following a durable run

`--follow` on `test` and `run` waits through those pauses instead, until the run
finishes or fails. It exists because verifying a real pipeline from the command
line was otherwise impossible: the default behaviour stops at the first park and
says so, correctly, and leaves the person hand-rolling a polling loop against
the database to find out what happened next.

**A woken run never finishes under its own id.** The row that parked is left at
`resuming` — the wake latch's single-winner state — and a SUCCESSOR run carries
the work, so anything that polls the id it started watches a lineage that has
already finished sit at `resuming` until it gives up. That is why the follow
polls `GET /api/v2/workflow/run/:runID/head`, the run currently carrying the
named run's lineage, with the ORIGINAL id every time: the head is re-resolved
server-side on every call, so each hop is picked up without the CLI walking the
lineage. Walking it here would mean a second copy of the ordering rules that
predicate encodes — including its clock-skew correction — in another language,
which is exactly the copy the head route exists to prevent.

The narration is transition-keyed, because a park can last days and a line per
poll would bury the events that matter: a step prints when it moves, a status
prints when it changes, and a hop prints as `Resumed as run …`. So each park
prints — including the second one, since a wake that has to be retried rolls the
run from `resuming` back to `waiting` under the same id — while the long
"what a park is and how to wait longer" advice attached to the first one prints
once per follow. A replayed step reads `replayed` there for the reason it does
in the report.

Three things about how it ends:

- **Ctrl-C and `--timeout` stop the watching, not the run.** Same as without the
  flag. A park routinely outlasts the 15m default, so `--follow` on the default
  timeout says so on stderr up front — by the deadline, "raise `--timeout`" costs
  another whole run. `--timeout 0` waits for as long as it takes. A follow that
  times out mid-park still prints the park's own report, hints included: nothing
  failed, and the run continues. The `/head` route is named by the ERROR it
  gives up with — the right thing to read later, since the run you started is
  the row guaranteed to answer `resuming` forever once it has woken; the
  report's own check-later hint still names the plain run route.
- **`--json` emits the HEAD run.** Under `--follow` the object's `id` is the run
  that finished, which is not the run that started; `resumeOfRunID` names the
  lineage root. A script that recorded the started id and then read `id` back out
  of the JSON is reading two different runs — deliberately, since the head is the
  one that carries the answer.
- **An instance without the route falls back, it never refuses.** By the time a
  follow gets here the run has already started, so an error whose remedy is "run
  it again" would invite the second live run `adoptTimedOutRun` exists to
  prevent. So the CLI says so loudly on stderr and reads the plain run route for
  that poll instead. The fallback is decided per POLL and is never latched:
  Ronja runs many instances behind one load balancer, so mid-rollout a single
  follow is answered by pods that disagree about whether the route exists.
  Every poll therefore tries `/head` first — a rollout that completes mid-follow
  upgrades the follow live — and once any poll HAS been answered by the route, a
  later 404 costs nothing: the plain read stands in for that one poll, the
  failure budget is untouched and the wait carries on through a pause as before.
  Only on a fleet where nothing has answered the route does the follow behave as
  the documented non-follow poll and stop at the first pause. The fallback keys
  on a **404** alone: an instance that has the route answers a missing run with a
  400, and one that does not answers the router's plain-text breadcrumb, which is
  not JSON to read. A 404 is also what an access refusal looks like — and there
  the plain route refuses too, so nothing falls back and the poll fails exactly
  as it does without the flag.

Whether a workflow is durable is read off the **row** (`runtimeVersion`), not
the manifest: the row is what the run funnel reads, and it is right about a
workflow this folder did not create. The manifest is the fallback for one case
only — an instance predating durable workflows sends no `runtimeVersion`, and a
0 means "the instance did not say", not "runtime 1".

A durable `wf init` (`--runtime 2` or `--runtime 3`) also writes a `main.py` in
the shapes a resume depends on (decorated steps, keys derived rather than
written, `tools.now()`, handles across step boundaries). A durable workflow scaffolded from v1 code journals
nothing and fails silently: the run works, and the resume that was the point of
it does not.

### Two editors, one workflow

The drift guard above is a local comparison against a file listing read a few
requests earlier, so it cannot see a change that lands *while* the push is
running — and two agents driving `wf` at once is now an ordinary thing to have
happen. Two server-side compare-and-swap layers close that window. Neither is a
lock, and neither makes an overwrite impossible: they make it **explicit**.

**`ronja app` has the first of them, not the second.** `PUT`/`DELETE
/dataapp/:id/files/*path` now take the same optional `baseSha256`, so everything
in the first bullet below is true of `app push` word for word. What a data app
still has no equivalent of is `--overwrite-remote`: `POST /dataapp/:id/commit`
carries no version confirmation, so `app publish` has nothing to deliberately
commit over and simply reports the server's refusal.

One thing about the data-app precondition has no workflow counterpart. Editing a
LIVE app **auto-forks a draft** server-side, so the row a write lands on need not
be the id you addressed. The assertion is checked against the row it **lands
on** — which means a `PUT` to a live app while you have a draft open is compared
against that draft, not against live. That is the point rather than a caveat: the
draft holds edits `GET :id/files` on the live id does not show you, and
overwriting them in silence is exactly what this stops. In the ordinary case a
just-forked draft is byte-identical to live, so nothing changes.

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

  One case arms them from somewhere **else**: a push from a fresh checkout that
  the committed `headVersionID` vouched for (see "Stacks" above). There is no
  baseline to build them from, so they assert the file listing the push has just
  read — legitimate there and nowhere else, because the anchor has established
  that those bytes are the live row's and unchanged since this folder forked from
  it. That is what makes the anchored CI push *stronger* than the `--force` it
  replaces, rather than merely quieter: `--force` sends no preconditions at all.

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

The HTTP loop this wraps is documented for agents in the `dataapp-dev` API guide
(`backend/lib/api/openapi/guides/dataapp-dev.md`, served at
`/docs/api/guides/dataapp-dev.md`). Keep the two in step.

```bash
ronja app clone data_app-abc           # or: ronja app init --from Revenue.tsx --feature collection-abc
$EDITOR App.tsx
ronja app push                         # syncs the folder into YOUR draft, then compiles it
ronja app validate                     # recompile the draft on its own
ronja app test                         # render the draft headless and report what the browser saw
ronja app publish                      # commit, or submit for review, say which happened, and who can now use it
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
compiles green and ships a blank page.

**A name nothing imports or declares is refused too.** A hook you call without
adding it to the `react` import line, a misspelt component, a `<Missing />` tag,
a `import type { Foo }` used as a value — each used to bundle green and throw
`ReferenceError` in the browser, and now comes back from `push` as
`Compiles: NO` naming the file, the line and the identifier, plus the import to
add when it is a React export. The escapes are the ones a type-checker would
demand as well: reach a script-provided global as `window.X`, or introduce it
with `declare const X: SomeType`.

And nothing static can promise that `Compiles: yes` renders anyway — the
compiler does not check types, so a mistyped prop, a hook called after an early
return, a wrong table id, or a component that mounts and returns `null`
publishes green too. `status` prints the app's URL; open it.

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
  derived from the markers in its code (`{{ ref }}`, `{{ secret }}`, `{{ workflow }}` and the rest). A data app's
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

  Every `allowed*IDs` entry may be a **declared alias** instead of an id, which
  is what makes a data-app folder as portable as the other two kinds — see
  [Dependencies](#dependencies-ronja-bind) for the resolution rules and for why
  `allowedMetricIDs` takes a `table` alias.

  Only four names ever belong in `capabilities` — `ai`, `query_external`,
  `write_external`, `upload_file` — one per SDK call that is not derived from
  an allowlist (completeAI, queryExternal, executeExternal, uploadFile).
  Everything else — reading tables, running an allowed agent or workflow,
  evaluating an allowed metric, codex search, HTTP fetch through an allowed
  secret — is granted by the `allowed*IDs` lists themselves, and the org
  roster (`users()`) needs neither. `ronja app init --help` names the same
  four, so the vocabulary lives in the command.

  **`push` refuses any other name, locally, before its first request.** The
  server does not validate this field: an unknown capability is dropped when the
  app's token is minted, so `"upload_fil"` used to push green, publish green and
  show no drift, then fail in a viewer's browser with nothing pointing back at
  the manifest. The refusal names the offending entry and the four valid names —
  and says so if you are simply older than the server, since the vocabulary is
  closed but not frozen (`declarableCapabilities` in
  `internal/commands/dataapp.go`; the backend pins the correspondence in
  `TestDataAppCaps_DeclareOnlySetIsExactlyFour`).

  It has the same **three states** `parameters` does — absent means "not
  managed, push never touches them", present means "push makes the row match" —
  for the same reason: reading absent as "grants nothing" would revoke a
  working app's access on the first push after upgrading. Because these are
  privileges rather than settings, `push` and `status` list every id being
  granted or revoked instead of reporting that a difference exists.

  ⚠️ "Declared, never derived" is true **over HTTP and in `ronja app`** — it is
  not true of data apps in general, and asserting either half unqualified is
  wrong. The in-app AGENT path does derive: `autoRegisterDataAppRefs` scans a
  file the agent just wrote for `{{ ref('table-…') }}` markers and for id-shaped
  `secret-` / `workflow-` / `agent-` literals, and registers what it finds.
  Neither path is the gate, though. The **actual** enforcement is server-side:
  `ValidateDataAppScope` at checkout and `ValidateAllowlistRefsLive` at publish,
  plus admin-only commit for a shared app. `ronja.json` declares what the app
  should be allowed; the server decides what it is.
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
- **A compiler that does not ANSWER is a third outcome**, and `push` and
  `validate` both name it rather than picking one of the other two. The files
  are already saved when the closing validate runs, so a 5xx (or a connection
  that never landed) is not a failed push: it reports `Compiles: not known — the
  compiler did not answer (…)`, says the files are on the draft, and exits
  non-zero, because a verdict that was promised and not delivered is not a green
  CI step.

  There are therefore **three** `compiles` outcomes on the wire, and
  `compileCheck` is what separates them:

  | `compiles` | `compileCheck` | What happened |
  |---|---|---|
  | `true` / `false` | absent | The compiler answered. `false` is its own refusal, and `compileError` carries it. |
  | `null` | absent | **Nobody asked** — `--no-validate`. |
  | `null` | present | Asked, no verdict. `answered` is `false`; `timedOut` says whose end stopped. |

  Inside `compileCheck`, `timedOut` is the field that names the two ways a check
  ends with no verdict, because they are different events with different
  remedies and `status` cannot tell them apart (it is `0` for both):

  ```json
  {"compiles": null, "compileCheck": {"answered": false, "status": 500}}
  ```
  The **instance** failed to deliver a verdict. Worth reporting.

  ```json
  {"compiles": null, "compileCheck": {"answered": false, "timedOut": true, "status": 0}}
  ```
  The **CLI** stopped listening. The compile was still running when the deadline
  fired and may well have finished a moment later, so the remedy is to look
  again or allow more time — not to go hunting for a broken instance. `timedOut`
  is omitted when false.

  `push --json` **always** emits `compiles`, never omits it, so key presence is
  not the "was it checked" test — `compileCheck` is.

  ⚠️ `validate --json` emits `compiles: null` too, on the same two non-answers.
  It was a plain boolean before, so a consumer that decodes it into a
  non-nullable type now breaks, and one that reads it loosely sees `null` as
  falsey — i.e. as "does not compile", which is exactly the wrong conclusion.
  **`null` means NOT KNOWN, never `false`.** Check `compiles === null` before
  treating it as a verdict.
- **`app status` says `compiles: no clean build since the last change`, never
  `NO`.** The row carries one column, `validated_at`: every file write clears it
  in the same transaction and only a successful compile re-stamps it. So NULL is
  equally true of a draft that failed to compile, one pushed with
  `--no-validate`, and one whose compile check never answered — no compile
  status is persisted anywhere. `ronja app validate` is what finds out which.
  (`--json` is unchanged: `validated` is the same boolean it always was.)
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
| `test` | renders the draft in a headless browser and writes `report.json` + the frames into `.ronja/test/`; a report, never a verdict | `--route`, `--viewport`, `--timeout`, `--steps`, `--out-dir`, `--fail-on-errors`, `--json` |
| `publish` | commits the draft, or submits it for review. Refuses a draft that does not compile | `--no-request-review` |
| `discard` | deletes your draft; the live app and your local files are untouched | `--yes`, `--delete-app` |

`status`, `push` and `publish` print a `URL:` line the same way `wf` does and
for the same reasons — see the note in the workflow section, including why an
absent link is silent. Two details are specific to apps: `push` reports the
**draft** it wrote to while `status` and `publish` report the **app** itself,
and `status` only knows the link when it actually reached the server, so a
signed-out or unbound status has no `URL:` line.

A draft is its own address. The server renders exactly the row a link names —
it never swaps your open draft in for the live app — so after a push to a draft
of a published app, `push` prints the draft's `URL:` AND a `Live URL:` line: the
live link keeps showing the published version, to you as well as to everyone
else, until `ronja app publish`. `app test` renders the draft by naming it.

**`publish` says who can use it, on every path.** Both endings — `Published.`
and `Submitted for review` — print one server-composed sentence under the
outcome line:

```
  Published.
  committed to "NCR register"
  Everyone in your organization can open "NCR register" (its feature "Quality").
  Anyone who opens it can read tables "ncr", "ncr_events" and WRITE via secret
  "ncr_db write role". Every viewer gets the same powers — the app cannot tell
  viewers apart.
```

A private feature gets the other half of the answer: `Only you (and admins) can
open "…" — its feature "…" is private. Share the feature to give it an audience.`
For a draft
awaiting an admin it speaks in the future tense (`Once approved, …`), because
nothing went live.

The sentence is **not composed here.** It comes from the server
(`rdataapp.DescribeAudience`) on the commit and request-review responses, and is
printed verbatim; `--json` carries the whole `audience` object
(`reach` / `featureName` / `reads` / `calls` / `writes` / `sentence`). Composing
a local one would be a second statement of "who can do what", and the two would
drift from what the app's own publish response and Ronja's chat both say. It is
omitted — silently, with no stub line — when the server does not supply it.

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

**`--out-dir` defaults to `.ronja/test/` inside the folder**, and the resolved
path is printed on every run. The old default was the folder ROOT, which made
the next command impossible: a PNG beside `App.tsx` is binary content in a
folder of source, so `app push` refused the whole folder. `.ronja/` is the one
place the push walk structurally cannot see — it is the folder's own
bookkeeping, excluded by name and not even reported as skipped — and the `*`
.gitignore living there keeps the artefacts out of git as well (`app test`
writes that file if the folder does not have one yet; if it cannot, it warns —
twice, once before the render and once beside the paths it wrote — and carries
on, because refusing would throw away an observation over the way it is stored).
A `--out-dir` you name is used exactly as given, the folder itself included.

The default path is one the CLI chose, not one you typed, so it is **refused
when a symlink leads out of the folder** — at `.ronja`, at `.ronja/test`, or
anywhere above them. A folder is an ordinary git checkout, and a committed link
there would let whoever wrote that repository choose where the artefacts (and
the deletes a re-run makes) land on your machine. The same check guards
`state.json` and the `.gitignore` itself, so it holds for `push` and `clone`
too. It is resolved **before** the render, so nothing has been spent when it
fires; a `--out-dir` you name is your own business and is used as given.

Nothing is ignored by NAME: a folder that still holds artefacts from an older
CLI is refused by `app push` like any other binary, and the refusal adds a line
saying they look like `ronja app test` output. Excluding those names from the
walk instead would silently drop a `report.json` a user wrote themselves.

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

Non-zero, `--fail-on-errors` aside, means the render did NOT happen: a transport
failure, an HTTP error, a bad flag, an out-dir that cannot be written to, or a
busy harness that stayed busy. Every local refusal — the flags, the steps file,
the out-dir — is settled *before* the request, so it costs no round trip and
discards no render. The one thing that can still fail afterwards is the write
itself: a directory that cannot be created, a stale frame that cannot be
removed, or a `report.json` that cannot be saved exits non-zero and names the
file. A frame that cannot be *fetched* is only a warning — the answer is already
written, and losing an illustration is not losing the observation.

**429 is not a verdict** — the shared render pool, or this credential's
per-minute budget, was spent. The CLI waits the server's `Retry-After` (default
15s, capped at 60s) and asks **once** more; busy again prints a message that says
nothing about your app was observed and exits non-zero. A client that kept
retrying would add load to the thing it was just told is at capacity.

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
- **The baseline holds four fingerprints per file, not one**, because a
  pipeline file is synced with several different things. `files[path].sha256` is
  the last **complete** push (written *and* built) — what "changed since your
  last sync" means. `tables[path].draftSHA256` is what this checkout last **wrote
  into your draft**, in the **disk** form — the file's own bytes, aliases and
  sibling stems unresolved. `tables[path].draftWireSHA256` is that same write in
  the **wire** form, the exact bytes that went over the `PUT` with ids
  substituted for every name; that is what the row actually stores, so it is the
  only one of the four that may be sent to the server as a `baseCodeSha256`
  precondition. `tables[path].liveSHA256` is the **live** table's SQL as of
  the last moment this folder agreed with it (a clone, the fork of a draft, a
  publish). The rule they exist for: **a hash is only ever compared against the
  row it was taken from.** Folded into one value, the guard below compared the
  live table against bytes that only ever existed in a draft, and refused an
  ordinary `push` → edit → `push`; folded the other way, the *disk* hash sent as
  a precondition would have 409'd every push from any folder that names a
  sibling by its stem.

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

**A ref you cannot READ is named, not guessed at.** The server refuses a write
whose `{{ ref }}` ids are outside your reach with a bare 403, deliberately
withholding *which* id (naming it would answer "does this table exist?" for
anyone who can guess one). Two different problems arrive that way — an
unreachable ref, and a create you are not admin enough to make — and the CLI used
to report both as the second. It now asks the only question it is entitled to
ask: it reads each ref, and if any is unreachable the refusal quotes the marker
and names the id, matching what `ronja wf validate` says for the same mistake.
Those reads happen **only after a write has already been refused**, so the happy
path costs nothing. When every ref reads fine the admin explanation stands,
unchanged. The commonest cause is a folder cloned from another organization: the
ids in its SQL belong to that organization, and only re-pointing them fixes it.

### The verbs

| Verb | Does | Flags worth knowing |
|---|---|---|
| `init` | writes `ronja.json` + `.ronja/`. Creates nothing server-side — each table comes into existence on the first push of its file | `--feature` (a push refuses without one), `--title` |
| `clone` | writes one `.sql` per **derived** table in the feature, plus manifest and baseline. Prefers your own open draft over live. Creates nothing server-side; the directory must be empty or absent | — |
| `status` | local changes, per-table build health, open drafts and their verdicts, and drift since the last sync. Read-only — not even a checkout. **Exits non-zero on drift *and* on "not checked"** (see below) | `--json` |
| `push [paths...]` | per changed file: create → resume/check out your draft → write SQL + derived inputs → sync → build → verdict and confidence report | `--force` |
| `publish [paths...]` | per staged draft: commit onto the table, or submit it for review | `--no-request-review`, `--overwrite-remote` |
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

   ⚠️ **This guard is no longer the whole protection, and it stays anyway.** The
   table surface now carries both server-side compare-and-swap layers workflows
   have: `baseCodeSha256` on the `PUT` and a base-version CAS on the commit. This
   guard is still a check at **one instant** — the moment those two rows were
   read — which is exactly what the `PUT` precondition closes. What it keeps
   earning is the other three things a precondition cannot do: it runs *before* a
   write, a sync and a build the server would only refuse at the end of; it
   covers the **live** row, which no precondition on a draft write speaks about
   at all; and it explains the difference in terms of the **folder** — this file,
   that table — where the server can only name two digests. An empty fingerprint disarms its
   own leg deliberately (a colleague who cloned the repo from git has no
   `.ronja/`, and refusing that is refusing the normal way people join); a missing
   *live* one is adopted from the row the guard just read, so a folder can leave
   that state rather than stay unguarded for ever.

   The draft fingerprint advances on the **write**, not on the build: a failed
   build leaves the draft holding the bytes of the failed attempt, and a guard
   that had not recorded them would read your own last attempt as somebody else's
   edit and demand `--force` to fix your SQL. The *content* baseline advances only
   on a complete push, so the fixed file is still something a bare `push` sends.
5. **The `PUT` carries `baseCodeSha256`**, the server's layer-1 precondition:
   the wire-form fingerprint of what this checkout last wrote into *this* draft,
   so a chat or web-builder edit landing between the guard's read and this write
   is refused rather than silently overwritten. It is sent only when there is one
   to send — a draft this checkout has never written to, and every `--force`
   push, write unconditionally, since `--force` is the author saying they mean to
   overwrite and a precondition would refuse the one thing the flag exists to
   permit. It must be the **wire** fingerprint: the row stores the ids, not the
   stems, which is why the baseline records both.
6. **Then `/sync` → poll to a verdict.** Every command polls through one
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

**A conflict is machine-readable here too.** A file whose write was refused by
the server's code precondition — somebody edited that draft between this push's
read and its write — carries `conflict: true` beside its `refused` outcome, for
the reason `wf push` carries the same field: an agent driving the loop has to
tell "somebody got there first, re-read and try again" from "you may not do this
at all", and both are a `refused` with prose in `error`. Do not branch on the
`error` text; it is the server's, and it is reworded whenever that reads better.

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
  naming the intervening versions. Committing would revert them, so the commit is
  **refused** with a 409; `publish --overwrite-remote` is the way through.
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

The per-file `--json` `outcome` vocabulary is `published`,
`submitted_for_review`, `conflict` and `refused`. `conflict` is a refusal and is
split out for the reason `wf publish` splits it: an agent driving the loop has to
tell "somebody committed first, re-apply and try again" from "you may not do this
at all".

A commit onto a table that has been published to since your draft forked is
**refused** by the server's base-version CAS, and nothing is written — the check
runs inside the commit's own transaction, under the parent's row lock, so your
draft is intact. `--overwrite-remote` is the deliberate override, and it works
exactly as `wf publish`'s does: it re-reads the current head *at the moment you
answer* and **confirms that version id explicitly** rather than sending a bare
force. A version that lands between the read and the write earns a fresh refusal
instead of a retry — an override authorizes overwriting the version it was
shown, not whatever happens to be there by the time the request arrives. The
version it wrote over is reported as `overwroteVersionID` in `--json`, on that
path only: it is the record that somebody else's work was discarded, and by
which version.

Before each commit it warns, never refuses, on two kinds of staleness:
`baseStale` as above — which now says out loud that the commit will be refused,
and still earns its round trip because it names the *intervening versions* where
the server's 409 can only name the head — and **stale inputs**, an input table
whose row was updated after your draft's was. That second one is not paranoia (`GetDependents` excludes
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

## Automation folders (`ronja automation`)

`ronja automation` keeps a feature's automations as one `.json` file each. The
filename is the automation's name; everything that is not a `.json` file is
ignored, so the folder can sit inside an ordinary repository.

```bash
ronja automation init --feature collection-abc123 --cron "0 2 * * *" --workflow nightly
ronja automation status        # what a push would change, and what moved
ronja automation push          # make each automation match its file
```

Three verbs, and the missing ones are missing for a structural reason rather
than a staging one. `scheduled_jobs` has **no draft and no version history**, so
there is nothing to check out, nothing to publish and nothing to discard — a
push is the whole write. There is no `clone` either: it is the verb a second
person needs, not the one that proves the loop.

### The file is a curated schema, not the row

It mirrors the vocabulary the create and update **bodies** read, and the reasons
are each a silent failure:

- **The row and the input disagree about two field names.** A read returns
  `emailAllowedFromAddrs` and `watchedTableIDs`; the bodies read
  `emailAllowedFromAddresses` and `watchedTableIds`. Sending the row's spelling
  is accepted, ignored, and answered `200`, so both are refused **by name** with
  the write vocabulary in the message.
- **The row carries state a file must not own** — `nextRunAt`, `disabledReason`,
  the minted email address and webhook tokens, the run-health columns. None of
  them is a decision anybody makes in a pull request, and all of them are
  refused with their own reason rather than as "unknown field".
- **`private` stays out entirely.** It is create-only server-side, and a shared
  folder pushing `private: true` would hide the row from the next colleague who
  pushes — whose `status` then reports it gone and whose push creates a duplicate.

```json
{
  "triggerKind": "cron",
  "cronExpr": "0 2 * * *",
  "timezone": "Europe/Stockholm",
  "action": { "kind": "workflow", "config": { "workflowID": "nightly" } }
}
```

### `references` belong to an inline `agent` action, and nothing else

Only an inline `agent` action runs under the automation's own reference set, so a
non-empty `references` beside a `workflow` or a `saved_agent` action is refused
before anything is sent:

```json
{
  "cronExpr": "0 3 * * *",
  "references": [{ "kind": "note", "resourceID": "policy" }]
}
```

A `saved_agent` action would earn a `400` — the Agent runs under its own
references, so declare them on the Agent itself. A `workflow` action is the one
that matters: the server **accepts it and stores none**, so the push reports
success, the row reads back empty, and the folder then reports drift no edit can
close while every later push re-sends the same references for ever. A workflow
runs under the resources its own script markers bind, so there is nothing for a
reference set to add. An *empty* `references` is allowed on every kind — it
matches a row that has none, so nothing drifts.

**Leaving `action` out does not leave the rule out.** An absent `action` is
unmanaged, not "no action": the push sends none and the automation keeps the kind
it has. So a file declaring `references` and no `action`, against an automation
that already runs a workflow or a saved Agent, reaches the same dead end by a
second door — the server takes the references precisely because the body carried
no action, and the read still cannot see them. That one is refused against the
ROW, so it needs the row to have been read: `push` refuses the file and `status`
reports it as a problem, and neither writes anything. The refusal above still
fires first whenever the file names the kind itself.

### `action.kind` is one-way out of `agent`

Switching an automation **to** a `workflow` or a `saved_agent` action works, and
so does switching between those two: each has its own server-side setter, reached
from whichever kind the row holds. Switching **back** to an inline `agent` action
does not. `PUT :jobID` writes a child action row for those two kinds and for
nothing else — there is no agent setter to call for the third — so a body
carrying `"kind": "agent"` rewrites no action at all and the automation keeps the
kind it had.

That is the same dead end the two `references` rules above describe, by a
different field: the push answers `200`, the row reads back `workflow`, `status`
reports `action.kind` as changed on every run, and no edit to the file ever
closes it. So a file declaring an `agent` action against a row that runs a
workflow or a saved Agent is refused, against the ROW — `push` refuses it and
`status` reports it as a problem — and the message names the web app.

Only that one direction is refused. `agent` → `agent` is not a change at all, and
the two moves *out* of `agent` are exactly the ones that work.

### Every field is three-state, and `enabled` is why

Absent means **this folder does not manage the field**: a push never sends it and
`status` never reports drift on it. Present means managed, *including* an
explicit `""` or `[]` — which is how you clear a description or remove the last
watched table.

The model was built for `enabled`, and `automation init` deliberately writes it
absent. A folder declaring `enabled: true` — the line somebody adds months
earlier to stop `status` nagging about an unmanaged field — is what reverses an
incident response on the next CI merge. Generalising the rule to every field
closes the sibling trap: a file that simply omits `model` would otherwise clear
the one somebody set in the web app.

**It holds inside `action.config` too, and that one takes work.** The server
replaces an action's config *wholesale* — the action row is deleted and rewritten
from whatever the body carried — so a push that sends `action` at all decides the
value of every field inside it. A file managing only `workflowID` therefore has
its `parameterValues` read off the row and sent back unchanged; without that,
changing the workflow would clear the parameter values somebody set in the web
app, and clear them silently, since an unmanaged field is not in `changed`
either. `action.kind` is the one key that is always required when `action` is
present: an action sent without one is stored as an inline agent action whose
config is ignored, and answered `200`.

### Aliases resolve FIELD-WISE

An automation's references are a **structured field**, not a marker, and the kind
this loop most needs (`note`) has no marker family on either side. So resolution
walks a closed, finite list of id-bearing fields —
`action.config.workflowID`, `action.config.agentID`, `references[].resourceID`,
`mailboxID`, `watchedTableIds` — and a refusal names the one it could not
resolve:

```
references[2].resourceID: alias "policy" has no bind in stack "prod"
```

A `{{ … }}` marker **anywhere** in one of these files is refused rather than
sent. Nothing on the server reads a marker out of an automation, so one sent
literally would be stored as a resource id, resolve to nothing, and fail at the
first unattended run — which is the failure this loop exists to prevent. Three
reference kinds (`dataapp`, `mcpserver`, `feature`) have no alias at all, so
naming one would mean committing a raw id; those are refused too, and the message
says to manage that automation in the web app until the alias layer covers it.

### There is no local baseline

This is the one folder kind without `.ronja/`, and it is a simplification rather
than a gap. A read returns the whole live configuration, so "does the row already
say what this file says" is answered directly — on any machine, including a fresh
CI checkout. `init` therefore writes no `.ronja/` at all.

What the committed `ronja.lock.json` carries instead is the one thing the server
cannot answer: which row each path **is**, and the row's `updatedAt` at the moment
this folder last agreed with it. `scheduled_jobs.name` is neither unique nor
required, so a lost id is not a lookup that falls back to a search — it is a push
that creates a **second** automation beside the live one, firing alongside it.

### The refusals

Each is a silent wrong answer without it.

| refusal | why |
|---|---|
| the row moved since the recorded `updatedAt` | there is no version and no compare-and-set here, so this anchor is the only drift guard there is. `--force` overwrites |
| a file turns an automation back **on** that somebody paused after that anchor | a human pause is clearable server-side, so without this the automation is live again with nobody in the loop |
| a change to `triggerKind`, `referenceBased` or `eventName` | the update body carries **none of the three**, so sending the change answers `200` for a write that never happened |
| an `action` with no `kind` | the server reads a missing kind as the back-compat inline `agent` shape, so the action it stores is not the one the file describes |
| an `agent` action declared against a row that runs a `workflow` or a `saved_agent` | the update route writes a child action row for those two kinds only, so the switch back answers `200`, changes nothing, and leaves `action.kind` reporting drift for ever |
| a file this folder is bound to is gone | those automations are LIVE, and a bad rebase must not stop one. `--prune` is the explicit opt-in, and it obeys the drift anchor too: a row somebody has been editing is the last one to delete because a file went missing |
| a mailbox trigger or reference without the admin role | binding a mailbox needs admin **and** an `admin:write` token, while this loop's scope is `automation` |
| the feature holds more automations than one page carries | a listing that saw a prefix cannot say which rows are missing, and a row it cannot see reads as gone. Against a Ronja older than this CLI the same refusal fires for a different reason — that instance ignores `featureID`, so the count is the organization's — and the message says which of the two happened, because "split the feature" is unactionable advice for a feature holding three |
| a `rateLimitPerMinute` or `rateLimitPerDay` of `0` | an update reads 0 as "use the organization's default" and stores NULL, so the file and the row can never agree; a create refuses it outright as not a positive integer. Leaving the key out is the spelling that means what a 0 looks like it means |
| a bound folder that names no feature | `featureID` is optional in the manifest and absent from the lock, and a listing with an empty filter returns nothing rather than failing — which reads as every automation having been deleted, and with `--prune` deletes every orphan with no row to guard it |

Two of those are worth a second sentence.

**The re-enable refusal is not a second line of defence, and its value is the
message.** `updatedAt` moves on any write, so a row somebody paused has moved and
the ordinary drift refusal already catches it. What this earns is that the reader
is told *"a person disabled this at 02:00, and here is the reason they gave"*
rather than *"the row changed"*. Its one exclusive case — a folder that never
recorded an anchor — is a case where it is disarmed anyway.

⚠️ The anchor is compared as an **instant**, never as a string.
`time.RFC3339Nano` trims trailing zeros, so a fraction-less stamp sorts *after* a
fractional one and `Z` sorts above `.`. A string comparison here would be wrong
intermittently, which is the worst way for a guard nobody re-runs to be wrong.

**The trigger-kind refusal names the web app and never "delete and push again".**
`DELETE` is a 30-day soft delete that auto-pauses the row and leaves it in a trash
this CLI cannot empty — purging is a human-only route — so each attempt would
leave one more paused row behind, and a restored one would then meet the
re-enable refusal.

### The role bar is the server's, and it is deliberately not pre-checked

Creating or changing an automation in a **shared** feature is `USR_ADMIN`; in a
private feature it is the owner. Only the MAILBOX authority is checked before the
push, and the asymmetry is not an oversight:

- **A mailbox is per-file.** Some files in a folder declare one and some do not,
  so the server's `403` arrives on the fourth of nine with three already written
  — a half-applied folder, which is the shape every refusal here exists to stop.
  The role bar is per-FOLDER: one folder binds one feature, so a caller who is
  refused is refused on every file and nothing is half-applied.
- **A local role check would be WRONG in a real configuration.** Create in a
  workspace-scoped feature goes through `requireAdminOrWorkspaceAdmin`, which
  admits a plain **User** who is a member of an all-users-admin workspace —
  membership this CLI cannot see. Refusing that push locally would refuse one
  that works, and a false refusal in a sync loop is worse than the server's
  honest `403`. The mailbox gate has no such lane: it is flatly admin.

### Two things push does that nothing else can

**A half-applied update is reported at the moment it happens.** The row update and
the reference / action writes are separate server-side calls, so a failure at the
second leaves the new schedule with the old references — indistinguishable, a day
later, from somebody editing references in the web app. On the failure path only,
push re-reads the row and says which fields landed and which did not.

**A successful write is verified against the row it returned.** A field the server
accepts and does not store answers `200` and changes nothing — the exact class of
bug the two spelling mismatches on this surface produce — and without the check
the folder would report it as pushed for ever.

**And a failure that is the INSTANCE stops the loop.** Every refusal above is a
property of one file, which is why a failed file is data and the push moves on.
A transport error, a `429` or a `5xx` is not: the next file meets it too, so
forty automations against an instance mid-deploy would be forty failed writes at
full speed and a half-applied folder — and against a rate limit they would deepen
the backlog they are waiting on. The loop stops at that file and reports the rest
as **not attempted**, which is a different thing to debug from "these failed".
The lock is saved after every file, so the retry is the same push again.

### Everything else behaves like the other loops

Stacks, `--stack`, `dependencies` + `bind`, the alias collision rules and the
`--json` shapes are the same as everywhere else in this file. One asymmetry worth
knowing: because the binding is keyed by **path**, renaming a file is an orphan
plus a create rather than a rename — the same property `ronja pipeline` has.

## Whole-tree checks (`ronja sync`)

A repository holds many folders. The five loops each answer "is this folder in
step with the organization?" for themselves, and answering it forty times by
hand is how a broken edge sits in a repo for a month. `ronja sync` asks two
questions of a whole tree at once:

```bash
ronja sync status                      # is the committed content what the org holds?
ronja sync check                       # does every reference that content makes resolve?
ronja sync status --dir apps --stack prod --json
```

Both walk **down** from `--dir` for every `ronja.json` — workflow, data-app,
pipeline and automation folders alike. `status` computes each folder's report
with the same function `ronja wf status` / `ronja app status` /
`ronja pipeline status` / `ronja automation status` run, so
the two surfaces cannot drift about what a folder's state IS — but the VERDICT it
folds that report into is the tree's own, and it reads signals the per-folder
commands deliberately leave out of theirs (see below). There is no repo-level
file to declare: a read-only command does not need one, and a committed file
format is the most expensive thing to get wrong.

Read-only, and enforced: `TestSyncStatusWritesNothing` and
`TestSyncCheckWritesNothing` snapshot a fixture tree before and after and assert
it is byte-for-byte identical. That is not ceremony — `wfdir.SaveState` also
writes a `.gitignore`, so any refactor that let a status path reach a save would
create files in a customer's repository as a side effect of a command called
`status`. ⚠️ It also rules out `openFolder`, which calls `adoptStack` and
rewrites `ronja.json`: the tree commands open through `openFolderForStatusAt`.

⚠️ **Both tests used to be vacuous, and it was mutation-proved.** Inserting
`_ = f.adoptStack()` into the tree path left them green. `adoptStack` returns
immediately on `f.Stack == ""`, so a run with **no `--stack`** can never trip it
— and with `--stack`, a legacy `instances[]` folder is never even opened, because
`resolveStackForFolder` refuses it as `unnamed_stack` first. The one shape that
survives both gates is `seedLockAdoptedFolder`: a manifest declaring **no**
stack, a committed lock that **does** record one, and `--stack <that name>`.
`selectNamed` adopts a lock-recorded stack by name and hands back `Bound` with
the credential's own key, so `adoptStack` fires and rewrites `ronja.json`. Both
tests now run on it, and both fail on the mutation. If you touch this, re-prove
it: insert the write, watch them fail, remove it.

### The scopes a token needs

Neither command writes, but they read different surfaces, and a scope the token
lacks comes back as `lookup_failed` → **exit 2**, not as a failure. That
distinction is right (nothing was checked) and it is also exactly how an operator
ends up widening a CI token until the red goes away, so the minimum is stated
rather than discovered:

| what the tree holds | `sync status` | `sync check` |
|---|---|---|
| workflow folders | `automation:read` | `automation:read` (it is `POST /workflow/validate`) |
| data-app folders | `analytics:read` | — (the declared `access` ids are read by kind, below) |
| pipeline folders | `data:read` | `data:read` |
| any `{{ agent }}` reference | — | `agents:read` |
| any `{{ secret }}` reference, or an app granting one | — | `secrets:read` |
| any `{{ ref }}` / table grant | — | `data:read` |
| any `{{ workflow }}` reference | — | `automation:read` |

`sync check` needs strictly more because it follows what the content points AT,
not just what the folder is bound to. Route groups: `/workflow` is
`ScopeAutomation`, `/feature/model` is `ScopeData`, `/dataapp` is
`ScopeAnalytics`, `/agent` is `ScopeAgents`, `/secret` is `ScopeSecrets`.

### The walk

Dot-directories are pruned (`.git` above all), and so are `node_modules`,
`dist` and `build` — **unconditionally**, not per `Kind.SkipDirs`, because at
walk time the kind is not yet known: knowing it means having already read the
manifest the walk is looking for.

A `ronja.json` **nested inside another folder's root** is reported and not
checked. Both readings are defensible and they disagree — a command run inside
the inner folder acts on the inner one (folder lookup walks *up*), while the
outer folder's push would carry the inner folder's files — so `sync` names the
situation rather than guessing, and never double-reports the same files under
two bindings.

`ronja db` folders are not discovered, and that is not an oversight: a managed
database folder has no committed manifest by design (see `internal/dbdir`).

### One `--stack`, many folders

One stack name is typed; the folders each declare their own, and nothing makes
them agree. A folder that cannot resolve the requested stack is **skipped, not
fatal** — a repository legitimately holds folders belonging to several
organizations — and every skip is `not_checked`, which is **never green**. The
failure that rules out is a `--stack` typo reporting a whole repository healthy
because not one folder recognised the name.

⚠️ `stack_unverified` is the one row that is not a clean skip: `sync check` still
runs its no-network dependency leg there and only refuses the remote ones. See
the `sync check` section for why the two halves differ.

| the folder… | reason |
|---|---|
| declares the stack, same organization | checked |
| declares it pointing at another instance or organization | `stack_elsewhere` |
| names stacks, and none of them is this one | `stack_absent` |
| names no stacks at all (legacy `instances[]`) | `unnamed_stack` |
| declares it for an organization, and **ours could not be established** | `stack_unverified` |
| has a manifest that will not load | `unreadable` |
| sits inside another folder's root | `nested_root` |

### Exit codes

This is the one place the CLI **extends** rather than inherits the two-valued
exit contract, and the extension is the point: a job acts differently on "push
this" and on "I could not look".

| exit | `verdict` | meaning |
|---|---|---|
| 0 | `clean` | every folder was checked, and every one is clean |
| 1 | `drifted` (`status`) / `broken` (`check`) | checked, and something is out of step — changed on the server, or never deployed at all / a reference does not resolve |
| 2 | `unknown` | could not tell — a skipped folder, an unreadable manifest, a dead credential, or an **empty walk** |

**`unknown` wins over the middle answer.** The strongest true statement about a
tree where one folder drifted and another could not be read is that not
everything was verified; reporting it as drift claims the unverifiable half was
fine. There is exactly one ordering — `verdictRank` — and the two commands spell
the middle answer differently only because "drifted" is a claim about content
moving on the server and an unresolvable reference is not that. They never
appear in one report: a tree command runs one computation over every folder.

An **empty walk is exit 2**, never 0 — a renamed or moved directory must not
sail through the gate reporting success.

`--json` carries the same word in a stable `verdict` field, because exit codes
do not survive a wrapper script.

### The tree verdict reads BOTH halves, which the per-folder ones do not

⚠️ **This was the widest of the false greens.** The tree verdict read exactly one
leg — the server's copy against the local baseline — so a folder whose files had
been EDITED and never pushed came back `clean`, exit 0. The report already
computed the other half (`Local`); nothing read it.

Every per-folder `status` is right to leave it out of its own drift verdict —
*"Drift is remote-vs-BASELINE, never remote-vs-local… a file the author edited
locally is exactly what a push is for"* (`pipeline_status.go`). That is the
semantic of an interactive one-folder command. A repository gate asks a different
question — *would a push from this tree change the organization?* — and a local
edit is a yes. **The tree verdict changed; the three per-folder commands did
not.**

So `pushDelta` reads every signal the reports already compute:

| signal | source |
|---|---|
| files edited, added or deleted here since the last sync | `Local` |
| a `.sql` file with no table behind it | `pipelineStatusReport.WillCreate` |
| committed `.sql` differing from what was last **deployed** | `LockTable.LiveSHA256`, via `undeployedAgainstLock` — the only local leg that survives a fresh clone |
| declared parameters, reporting timezone, runtime generation | `remoteReport.Parameters` / `.ReportingTimezone` / `.Runtime` |
| a data app's access grants | `appRemoteReport.AccessChanges` |
| the server's own copy moving | `remoteReport.Drift` |

The three lists stay **apart** in the message, because they are different facts
and a reader acts differently on each: *you hold work that was never deployed*
(push it), *the organization moved under you* (look before you push), *your
declarations no longer match the row*. The metadata half earns its place on the
runtime case alone — a runtime-1 folder against a runtime-2 row is a push the
server **refuses**, and reporting that repository as healthy is worse than
reporting drift.

⚠️ **`Diff.Added` is the one signal that needs a baseline**, and reading it
without one is how this fix could have introduced a false *positive* to replace
the false negative. `DiffHashes` against an absent baseline reports every local
file as added (a missing entry is `Added`; `Modified` and `Deleted` can only
arise from an entry that exists), and a folder freshly cloned from git has no
`.ronja/` at all — so reading `Added` there would report a perfectly in-step
repository as holding undeployed work, in CI, on the run that matters most. The
workflow and data-app legs reach it only after the `baseline == nil` check has
already answered `unknown`; the pipeline leg does not read it at all and uses
`WillCreate`, which answers the same question without a baseline. `localChanges`
is where that line is drawn.

⚠️ Note what `Local` is **not**: it is computed against `.ronja/state.json`, not
against `ronja.lock.json`. On a fresh clone there is no local baseline, so this
leg contributes nothing there — which is exactly the hole `undeployedAgainstLock`
fills for a pipeline folder, by comparing the committed `.sql` against the
committed fingerprint instead. That comparison is only sound because the recorded
fingerprint is of the **disk** form, which `pipeline_push.go` states where it is
written: *"`content` is the DISK form, and the baseline has to be disk form… the
two are the same fingerprint by construction."* It is computed in the tree
command from the lock the folder already carries, NOT added to
`pipelineStatusReport` — a new key there would be a scripting-visible change to a
shipped command for a verdict only this one computes.

### What a FRESH CLONE can and cannot be told, per kind

This is the paragraph to read before putting `sync status` in a CI gate, because
the answer is **not the same for all four kinds** and the difference is
permanent by design.

A checkout straight from git has no `.ronja/` — it is git-ignored, correctly, and
is per-user state. So on that checkout every `Local` signal is silent and the
only comparisons left are the ones backed by the **committed** `ronja.lock.json`.
What the lock carries differs by kind, and `lock.go` explains why:

| kind | committed anchor | what it can answer on a fresh clone |
|---|---|---|
| pipeline, **naming a stack** | `LockTable.LiveSHA256`, a per-file **fingerprint** of the live table's SQL | both: has the server moved, **and** does the committed `.sql` differ from what was last deployed |
| pipeline, legacy `instances[]` | nothing — its live fingerprints stay in `.ronja/` | neither; `undeployedAgainstLock` returns nothing for it, deliberately |
| workflow | `LockStack.HeadVersionID`, a **pointer** to the published version this folder was taken from | only: has somebody published since |
| data app | the same `HeadVersionID` pointer | only: has somebody published since |
| automation | `LockAutomation.AutomationID` **and** the row's `updatedAt` | **everything.** There is no per-user half at all — see below |

So **`ronja sync status` cannot tell, on a fresh clone, whether a workflow's or a
data app's committed files have been deployed.** It reports the folder `unknown`
rather than clean (there is no baseline, and the no-baseline branch answers
first), so it never *claims* they are in step — but it cannot turn that into the
`drifted` a pipeline folder gets.

**That is a consequence of a deliberate decision, and the decision is right.**
Those two kinds sync their files into the **caller's own draft**, so any per-file
fingerprint they have is a fingerprint of one person's row — and `lock.go` is
explicit: *"committing it would hand a colleague a baseline for a row they cannot
see."* A pipeline's live hash is a different number about a different row, the
LIVE table, which is the same for everybody, which is precisely why that one
could be committed. The asymmetry is not a gap waiting for a fix; closing it
would mean committing per-user state.

An **automation** folder sits outside that table's logic entirely, because it has
no local baseline to be missing: a read returns the whole live configuration, so
the comparison is committed-file-against-server and is the same on every machine.
A fresh clone of one is vouched for exactly as strongly as the author's own
checkout, which is the property the other three kinds cannot have.

The practical reading for a CI gate: on a fresh clone, a **pipeline** or
**automation** folder is fully vouched for, and a **workflow** or **data-app**
folder is vouched for against the published version only. Run `ronja wf push` /
`ronja app push` to find out whether their files differ — those loops read the
row's files directly and do not depend on a baseline.

### Two things `sync status` does not claim

A **workflow or data-app** folder that is bound and has no baseline for the
selected stack is `unknown`, not clean — the fresh-`git clone` case, where
`.ronja/` is correctly absent and its files could not be compared against
anything. (A **pipeline** folder that names a stack is the exception, and it is
the point of the section above: the committed lock gives it both legs, so a fresh
clone of one gets a real answer rather than `unknown`. A pipeline table with no
lock entry is still `unknown`, and `ronja pipeline status` deliberately exits
*zero* there — right for one folder run by its author, wrong for a tree gate.)
And `sync status` compares the folder's **content and its declarations**: a change made in Ronja to something the folder does not describe
at all is not drift here.

`ronja wf status` and `ronja app status` still exit 0 unconditionally, exactly
as they always have. The verdict helper computes their answer for `sync` without
touching their exit codes; changing those is a scripting-visible break and is a
separate decision.

#### And a folder that has never been deployed is not clean either

⚠️ **This was a false green, on all three kinds, and it is the one worth
remembering.** Each loop has exactly one `NotCheckedReason` that is a
*conclusion* rather than a failure to look — `reasonNothingBound` /
`reasonNoWorkflowYet` / `reasonNoAppYet`: the folder is bound to this very
organization, it names a feature, and the organization simply holds nothing it
has pushed. The tree verdict mapped all three to **clean**, so a folder holding a
whole table's SQL or a whole workflow that had **never been deployed** reported a
healthy repository and exited **zero**. A CI gate built on `sync status` passed
while nothing was deployed.

The reason is honest only when there is nothing to deploy, so the answer now
turns on what that folder's own loop would push — `wfdir.Enumerate` under the
kind's own rules, so a pipeline counts its `.sql` files and a stray `README.md`
beside them does not:

| syncable files | verdict |
|---|---|
| none | `clean` — an empty folder genuinely has nothing to deploy, which is what keeps `init` followed by `sync status` sane |
| one or more | **`drifted`**, exit 1 — *not* `unknown`. Nothing is ambiguous: we know the files are here and we know nothing is over there |

The message says so in those terms ("has never been deployed here … a first push
would create …") rather than reusing the drift wording, which would claim a
comparison that never happened.

This lives in `neverDeployedVerdict` and is reached from all three verdict
functions. Note what it is **not** about: an *unbound* folder was already
non-green (each loop's `if !f.Bound` branch sets a different reason, which falls
through to `unknown`). Bound-with-nothing-deployed is a different state, and it
was the green one.

### `sync check` — edge verification

`sync check` answers the other question: does every reference the committed
content makes still point at something? A table renamed in one feature breaks a
marker in a folder nobody opens for a month, and nothing else notices.

It is **marker-derived**, not column-derived: the enumeration comes from what the
source actually writes, resolved through the folder's own alias codec, so a
reference that no longer resolves is found by the same rule a push would resolve
it by.

| verdict | meaning | decided |
|---|---|---|
| `ok` | resolved and readable | server |
| `unresolved` | nothing here could turn it into an id at all — an alias this stack binds nothing to, a missing sibling stem, an ambiguous name | **locally, definite** |
| `unreachable` | a well-formed id the server will not show us | server |
| `not_checked` | with a reason | — |

⚠️ **`unreachable` cannot be split, and the two statuses do not line up with the
two meanings.** A row the caller may not see answers **404** deliberately so that
existence does not leak (the workflow handler's own comment calls it the
"no-enumeration 404"), while a deleted, trashed or cross-tenant row is removed by
RLS or by the store's live pin before the predicate runs and surfaces as
`table.ErrNoRows` — which is `rjerr.Input`, and therefore **400**. So the id's
SHAPE is checked locally first (`markers.IsResourceID`, which takes a kind), and
**400 and 404 on a well-formed id are then treated identically**. A verifier
keyed on 404 alone reads a deleted table as "my request was malformed" and says
nothing.

**`unreachable` scores 1, not 2**, and that is the one judgement call. It looks
like a "could not tell" — we genuinely cannot say whether the row was deleted or
is merely invisible — but the exit codes do not draw that line. They draw the
line at whether anything was **asked**: an unreachable edge was asked about and
the instance gave a definite negative, so a run through it will fail.
`not_checked` is the other thing entirely, and a job must conclude nothing from
it.

**Per-kind legs.**

- **Workflow — the server does it.** `POST /api/v2/workflow/validate` takes
  candidate files pre-creation and reports per-marker findings against the
  caller's own reach. `ronja wf validate` already runs it, so the workflow half
  shipped some time ago; `validateWorkflowFolder` is that command's body, hoisted
  so both call one implementation. Its `resolved` bindings become the `ok`
  edges — **including codex ids, which the server resolves perfectly well**. An
  `unresolved_*` finding is `unreachable` rather than `unresolved`, and the split
  is exact: `validateFilesOf` sends the RESOLVED source, so a name that could not
  be resolved was already refused locally by the alias pre-flight.
  ⚠️ `secret_dropped` is a **warning** server-side and a **finding** here,
  deliberately — and this is shipped precedent, not a new opinion. `ronja wf
  push` already exits non-zero on exactly this code for exactly this reason (see
  its own note above): the warning *tier* is right for a person about to create
  the secret, and what was wrong was the exit code, because that is the only
  thing CI reads. The save path only warns because refusing a save over a
  credential somebody is about to connect would be obstructive, which is a policy
  about writing rather than an answer about resolving. The alias layer takes the
  same stricter line locally (`misusedAliases`).
  **So `sync check` is stricter than `ronja wf validate` on this one code, and
  only this one.** `wf validate` exits on `result.OK()`, which counts errors and
  not warnings, so it reports a dropped secret and exits **zero**; `wf push` and
  `sync check` both exit non-zero. The two commands looking at one folder and
  disagreeing is deliberate: `validate` answers "would this save", and a save
  genuinely would succeed.
  And, like push, the strictness comes with the hatch: **`--allow-dropped-bindings`**
  takes those edges out of the SCORE and leaves them in the REPORT — still
  listed, still `unreachable`, still marked `droppedBinding` in `--json`, with
  the human line relabelled `accepted` the way push relabels its own. A flag that
  hid the evidence would be the failure mode. It is named in the refusal, because
  a hatch nobody can find is not a hatch, and a repository mid-credential-setup
  otherwise has no route to a green tree except not running the command.
- **Pipeline.** `{{ ref }}` resolved locally — a **sibling's stem wins** over a
  declared alias, exactly as `pipelineCodec` resolves it — then `GetTable` per
  distinct id. A ref naming a sibling is `ok` without a request: on a first push
  that table does not exist yet, and asking would report a folder that deploys
  perfectly as broken.
- **Data app.** `ValidateDataApp` is unusable here — it needs an existing DRAFT
  id, so reaching it means creating or forking one, which is write-adjacent. So
  the declared `access` block is verified id by id, and the source is compared
  against it **in both directions**.

**Both directions, and why the second one matters.** The agent path auto-repairs
an app's allowlist (`autoRegisterDataAppRefs` derives the grants from the source
it is writing), but the HTTP route the CLI pushes through —
`PUT /dataapp/:id/files/*path` — calls `UpsertFileChecked` and **nothing else**.
So **a CLI-pushed app's allowlist is never auto-repaired**: an app that queries a
table it never declared pushes clean, compiles, publishes, and then fails at run
time with `rejected_access`. That is `used_but_undeclared`, a finding that fails
the run. The other direction — a grant nothing uses — is a warning, because it is
wider access than the app needs and not a broken edge. The used-side scan takes
**quoted string literals only**, and only in files the bundle actually executes
(`.tsx` / `.ts` / `.jsx` / `.js` / `.mjs` / `.cjs`).

⚠️ **Both halves of that narrowing are about the same hazard, and it is worse
than a red build.** `wfdir.DataAppKind` declares no `SyncExt`, so an app folder
syncs README, design notes and test fixtures alike — and a quoted `table-…` in
any of them used to fail the tree, with a finding whose advertised remedy was to
add that id to the app's real `access` allowlist. A false positive whose fix
WIDENS privileges is worse than the miss it prevents. So the scan is scoped, and
the message no longer leads with "declare it": it names the file, asks what the
line is doing, and offers deleting the reference as the equal half of the answer.
What that gives up is stated in `appSourceExts`: an id reaching the app through a
non-source file (`import cfg from './config.json'`) is not seen, which is a miss
rather than a regression.

The prefixes the scan matches are **derived from `markers.IDPrefixes()`**, not
spelled beside it — they used to be, with nothing pinning the two together, so
adding a kind to `markers` would have silently stopped this check covering it
with no failing test.

**All folders — declared dependencies.** Every alias in `dependencies` must have
a `bind` for the selected stack. This is the only leg decidable with **no
network at all**, it is reported signed out, and it is the failure the whole
alias layer exists to fix. `checkAliases` computes it; there is no second copy.

It runs **first, for every kind**, and before the credential check. The workflow
leg used to run it after and return `no_credential` without it, so the one kind a
CI job under a rotated token hits first reported nothing at all — while this
paragraph, the code's own comment and the customer guide all promised otherwise.

It is also the reason `stack_unverified` is the one not-checked reason
`sync check` does **not** skip outright. That reason means `--stack X` was given,
the folder declares X, and our own organization could not be established — so
every REMOTE leg is unsafe (an id lookup would ask *our* organization about
*another* one's rows and call every one `unreachable`), while the dependency leg
compares the committed file against itself and is answerable regardless. The
local leg runs, the remote legs do not, and the folder is `not_checked` — never
green either way.

⚠️ **It needs a resolved entry to be a finding at all**, and that is the no-flag
half of the one-`--stack`-many-folders rule above. With **no `--stack`**, a folder none of whose stacks names
the organization this credential reaches falls through to an *unbound*
`Selection`, whose `Bind` is nil — so `checkAliases` reports every declared alias
as bound to nothing and the folder comes back `broken`. That is a confident,
definite verdict about a folder we cannot speak for: we do not know which
organization it targets, so "this alias is unbound" is not a statement anything
here is entitled to make. (It also reaches `describeBindSite`'s unnamed-entry
wording and names an `"instances"` entry a stacks-only manifest does not
contain.) So `sync check` reports `not_checked{not_bound_here}` there, naming the
stacks the folder *does* declare. The case that must stay `broken` — a folder
that **does** resolve an entry here and has an alias with no bind — is unchanged,
which is why the guard is on `Selection.Bound` rather than on the alias report:
being bound here is exactly "there is a target to be unbound *for*".

**Two kinds cannot be verified by id at all, and say so rather than passing.**
Codex's route group carries **no `AccessScope`**, and `requireScope` fail-closes
on a route that declares none ("this route is not accessible to scoped tokens"),
so no scoped token can ever read one. Mailbox has **no GET-by-id route**, only a
list. Both are `not_checked` with a reason naming the limitation — and the codex
limit is about the **by-id read**, not about codex references in general, which
is why a workflow's codex markers still come back decided.

**Cost.** Verification is deduped by `(kind, id)` across the **whole tree** — ids
repeat heavily across a feature's folders, and one shared dimension table read by
nine files is nine edges and one row. **Serial, no concurrency**, for the reason
`syncStatusTree` gives: parallelism stacked on the root threading and the shared
stack resolution is where the subtle bugs would be, and dedupe already fixes the
pathological case.

**What it does not claim.** A data app's *runtime* queries are not statically
enumerable, so what is verified is what the app **declares** plus the ids written
literally in its source. A workflow's references are answered by the instance's
validate endpoint, which **refuses without a `featureID`** — the normal state
before a folder's first push, and therefore `not_checked{no_feature}` rather than
a failure or a silent skip.

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
- **Folder machinery is never source.** `ronja.json`, `ronja.lock.json` and
  `.ronja/` are excluded by `StructuralExclusion` *and* by `isFolderMachinery`
  (excluded, and not reported as skipped) — and both lists have to name the same
  set. A workflow or data app has no `SyncExt`, so anything the structural rules
  allow is pushed: miss the lock file in one list and the folder's own state is
  `PUT` into the customer's workflow, a later `clone` writes the server's stale
  copy over the real lock, and the folder reports `modified ronja.lock.json` for
  ever.

## Layout

```
cmd/ronja/           main package — its NAME is what makes the binary `ronja`
internal/commands/   cobra command tree (root, auth, context, env, api, query,
                     stdin, bind, sync_*, workflow_*)
internal/api/        HTTP client + the endpoint shapes it mirrors, plus raw.go
                     (transport only, mirrors nothing), query.go and search.go
internal/config/     the CLI config file (os.UserConfigDir()/ronja/config.json),
                     one profile per (instance, organization), mode 0600
internal/wfdir/      the synced folder: ronja.json, ronja.lock.json,
                     .ronja/state.json, local enumeration and the sha256 drift
                     baseline — one Kind per loop (workflow / data app /
                     pipeline), and stacks in stack.go / lock.go
internal/tablerefs/  the {{ ref('…') }} grammar: canonicalization to id form,
                     and the input list derived from it
```

The workflow commands are one file per verb (`workflow_push.go`,
`workflow_testcmd.go`, `workflow_runcmd.go`, …) with the shared folder plumbing
in `workflow.go` — `test` carries the `…cmd` suffix because `workflow_test.go`
is a test file to the Go toolchain, and `run` matches it rather than colliding by
name with the api package's `workflow_run.go`.
Their endpoint mirrors live in `internal/api/workflow.go`, `workflow_write.go`
and `workflow_run.go`, hand-mirrored against
`backend/api/v2/workflow/handler.go`. The data-app and pipeline commands follow
the same shape (`dataapp_*.go`, `pipeline_*.go`, with the shared folder plumbing
in `dataapp.go` / `pipeline.go`); the pipeline mirrors are
`internal/api/table.go` and `table_write.go`, against
`backend/api/v2/feature/api_model.go` and the draft-review route in
`backend/api/v2/governance/`.

`internal/commands/bind.go` is the one command that belongs to no loop: it is
kind-agnostic, because `dependencies` is a manifest key all three folder kinds
carry, so it is registered at the root and works in whichever folder it is run
from. Its mirror is `internal/api/search.go`, against
`backend/api/v2/search/handler.go` and `backend/lib/search/hit.go` — four fields
of eleven, plus the two server constants whose absence would be misread
(`minQueryLen`, which answers a short term with an empty result and no error, and
the fact that the endpoint has no `codex` leg at all).

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

**`cli/internal/jqf` is now the only copy.** It used to be hand-synced with a
`backend/lib/jqf` twin, kept honest by a golden fixture CI diffed across the two
modules; the backend copy went away when the agent's `callAPI` tool stopped
taking a jq filter at all. That removal is worth knowing before anyone proposes
putting one back: `match`/`gsub` with the `g` flag can peg a core inside a
*single* uninterruptible gojq builtin, which no step, allocation or context
budget can interrupt — tolerable on a laptop the caller owns, not on a shared
multi-tenant pod with the expression chosen by a model. `testdata/jq_golden.json`
survives as this package's own regression suite rather than a drift guard.

`Run`/`RunBytes` have one policy — collect every output or return an error —
and every command applies a filter through `runFilter` (or `runCondition` for
`--wait-until`), which is where the 10 s deadline lives. `cmd.Context()` carries
no deadline of its own.

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
