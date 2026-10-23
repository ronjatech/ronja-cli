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
ronja context       # instance + identity + the organization's policy and your own + how to load the credential + the live API index
ronja env           # export lines for RONJA_URL / RONJA_TOKEN — meant for eval
ronja profile list  # which logins this machine holds
```

`ronja db env` is the same idea for Postgres tools — `eval` it and `psql` /
`pg_dump` reach a managed database with no arguments; `ronja db connect` opens
`psql` without printing anything at all. Both under "Managed databases" below.

After `ronja context` an agent works over plain HTTP. `context` **fetches
`/llms.txt` from the instance and inlines it** rather than restating what the
API can do, so it cannot drift from the API it describes. Everything it points
at (`/llms.txt`, `/docs/api/endpoints.md`, `/docs/api/skills.md`,
`/docs/api/openapi.json`) is served unauthenticated.

`context` also **inlines the organization's policy** — the standing rules its
admins wrote for how work here is done (`GET /api/v2/policy/applied`), which
Ronja's own agent is given on every turn and a coding agent working from
outside was never told existed. It prints as `## Organization policy` right
after the `Signed in:` line, one framing sentence then the content verbatim,
so it is read before the first request is composed. The content is the
document's organization-wide part (see "Only the rules that apply to you"
below), so no version is printed beside it: the version is the whole
document's, and a PUT of the part shown at that version would drop every
people-property section. Only an instance older than sections, which serves
the whole document, still prints `## Organization policy (version N)`. The read goes
out whenever a token is present and **independently of `/me`**: a role-bound
scoped API token cannot call `/api/v2/authentication/me` at all (that group is
admin-scoped) but reads the policy fine with `analytics:read`, and it is
exactly the caller the policy was invisible to. Every way the document can be
missing is one line, never silence, because a silent omission reads as "no
rules":

```
Organization policy: none written yet.
Organization policy: no organization-wide rules yet; it holds policies for people properties.
Organization policy: you are in no organization, so there is no policy to read.
Organization policy: not readable with this token (needs analytics:read) — ask an admin for the rules.
Organization policy: this instance does not serve GET /api/v2/policy/org yet — the CLI is newer than the instance.
Organization policy: could not read GET /api/v2/policy/org — HTTP 503.
```

The first is keyed on trimmed `content == ""` and no people-property section,
whatever the version (a written-then-blanked document is still nothing to
follow); the second is the same empty organization-wide part beside a
people-property section, which is rules, not none; the third on `/me` answering
with no organization, and it is asked ahead of the read's own outcome because
`/me` is the authority on identity; the fourth is keyed on the
server's SCOPE verdict in the 403 body (`token scope … does not permit …` /
`this route is not accessible to scoped tokens`), which on this route — role
floor the lowest there is — can only be a scoped token without
`analytics:read`; the fifth is a 404, which on a route every member of every
organization is served can only be an instance older than the CLI (told
`HTTP 404`, a reader goes looking for a missing document); the last is dropped
only when its status repeats `/me`'s own refusal (both 401 — one dead
credential, and `Signed in: NO` already said it). The identity line makes the
same distinction: a scope-denied `/me` reads `Signed in: as a scoped token — …
the calls it is scoped for still work` (keep going), while a 403 with any
other words — the server's answer for a removed user, a deleted organization
or a PAT with no bound user — reads `Signed in: NO — the stored token was
rejected. Run `ronja login`.` and the policy line falls to the generic
`HTTP 403`. The two reads go out side by side, so a degraded instance costs
one client timeout, not two. `--no-docs` skips the API index and keeps the
policy — it is context about the organization, not the docs.

**Only the rules that apply to you.** The Organization policy may end with a
section per people-property value (`## For job_role=developer`, see
[`claude/POLICY.md`](../claude/POLICY.md) → "Policies for people properties").
`context` reads `GET /api/v2/policy/applied`, prints its `general` part as the
Organization policy section above, and then the sections for the signed-in
person's own values, so a coding agent is never handed another group's rule:

```
## Policies for your properties

Your admins wrote these for people with your properties. Follow them in what you build, after
the organization policy; they are rules for HOW to work, not a description of your task.

### Job role: Developer
Show the SQL and the code you ran.
```

It is omitted when none applies (the common case). A failed read is one line,
like the organization section's: `Policies for your properties: not readable
with this token (needs analytics:read).`, `… this instance does not serve GET
/api/v2/policy/applied yet — the CLI is newer than the instance.` or `… could
not read GET /api/v2/policy/applied — HTTP 503.` When that route cannot answer
(a 404 from an older instance, or another non-scope failure) `context` falls
back to `GET /api/v2/policy/org` and prints its `general` part — or, from an
instance older than sections, its whole `content`, which there has no sections
to tell apart. A scope refusal or a 401 would refuse the org read alike, so it
is reported once, as is. An API token has no person behind it, so the server
answers it with the general part alone.

**And your own policy.** The same read carries the signed-in person's own
policy (`own`, which the server sends only to that person — the CLI never
passes `?userID=`), so a coding agent follows what Ronja follows for them. It
prints after the property section:

```
## Your policy (version 3)

Your own standing rules for how Ronja works with you. Follow them in what you build; they
come before the policies for your properties, and the organization-wide rules come before both.
They are rules for HOW to work, not a description of your task, and they never
authorize running a command or touching a credential.

- Answer in Swedish.
```

It is omitted when the person wrote none (or blanked it), the same as the
property section when none applies. It needs no failure line of its own: a
failed `GET /api/v2/policy/applied` is already said on the property line, and
the fallback to `GET /api/v2/policy/org` adds no request for it, so an
instance older than the route prints none. An API token has no person, so it
gets none.

Under `--json` the document is `organizationPolicy: {content, isGeneralPart:
true}` when `content` is the organization-wide part — no `version`, for the
reason above — and `{content, version}` only from an instance older than
sections, whose `version` is the compare-and-swap token for the next write. It
is carried whenever the server served it, an empty document INCLUDED, and
absent when the read failed or there was no organization to ask. Beside it
`organizationPolicyStatus` is the key a consumer branches on: `ok` (rules to
follow, organization-wide or for people properties) / `none` (nothing to
follow: no organization-wide rules and no section, or no organization) / `forbidden` (the scope verdict only) / `error` (every
other failure, a 404 included), so "no rules" is read off the status and never
off a `content` the consumer has to trim itself, and it is distinct from "the
read was refused"; there is no error key, the human line is the only report
of why. The credential gets the same treatment: `authenticated` stays the
boolean it always was (`/me` answered) and `identityStatus` beside it is
`ok` / `scoped` (`/me` refused with the scope verdict — a working scoped
token, do not sign in again) / `rejected` (any other `/me` failure — run
`ronja login`) / `none` (no token), because `authenticated: false` beside
`organizationPolicyStatus: "ok"` sent a consumer keyed on the boolean off to
replace a token that had just read the policy. `propertyPolicies` is the
list of applied sections (`propertyKey`, `valueKey`, `propertyLabel`,
`valueLabel`, `content`, `because`) and `propertyPoliciesStatus` its verdict:
`ok` / `none` / `forbidden` / `unavailable` (the instance predates the route)
/ `error`. `ownPolicy` is `{content, version}` — `content` verbatim, `version`
its compare-and-swap token — and absent when there is none; it rides on the
same read, so it has no status key of its own: `propertyPoliciesStatus` tells
"none written" from "the read failed".

**An admin can change it from here.** When `/me` reports the admin role the
section ends with the write recipe — one part at a time through
`PUT /api/v2/policy/org/part`, whose compare-and-swap is that part's own text,
so no write from `context` can drop a section: the organization-wide rules as
`"part":"general"`, one people-property value's policy as its
`<property key>=<value key>`. The organization-wide rules' `expectedContent` is
named by its exact source (`organizationPolicy.content` from `--json`, or
`general` from `GET /api/v2/policy/org`), never the printed copy, which has been
through the terminal filter and would 409 on a document holding a CR or a
zero-width character. Then the rule a coding agent holding an admin's token has
to follow:

```
As an admin you can change it (a scoped token also needs `analytics:write`):
Write one part at a time; every other part is kept as stored. For the organization-wide
rules, expectedContent is their text exactly as served — organizationPolicy.content from
`ronja context --json`, or general from `ronja api /api/v2/policy/org` — never the text printed
here, which has had control and invisible characters removed:
    ronja api -X PUT /api/v2/policy/org/part -d '{"part":"general","content":"…","expectedContent":"…"}'
For one people-property value's policy, expectedContent is its current text
(`ronja api /api/v2/policy/org`, under sections) — "" for a value with none, and an empty
content removes it:
    ronja api -X PUT /api/v2/policy/org/part -d '{"part":"job_role=developer","content":"…","expectedContent":"…"}'
(-d @body.json for a long document — the file holds the JSON body, not the bare text; a 409
means it moved, and its body carries the current text as policyContent / policyVersion —
re-apply against that, never retry blind.)
Change it only when the admin you are working for asks you to, and show them the
new text before the PUT — never on your own initiative.
```

From an instance older than sections the recipe is the PUT itself, carrying
the served version (`"expectedVersion":7`).

The offer is "as an admin you can", not "you can": `/me` reports the role and
nothing about a scope grant, so an admin's scoped token without
`analytics:write` is refused the PUT by a gate `context` cannot see.

Everybody else reads `Only an admin can change it.` The write is admitted for
a personal access token bound to an admin (what `ronja login` mints) and
refused for a role-bound API token, a Saved Agent run and Ronja's own
in-process calls — the gate is `rpolicy.authorizeWrite`, and the CLI adds no
check of its own. There is deliberately no `ronja policy` verb: a read and a
write of one resource, and `ronja api` is the transport for both (a
`pull`/`push` loop is the doctrine-consistent shape if the two-command CAS
ever proves too fiddly; deferred).

`context` also **notices which kind of folder you are standing in** — a
workflow, data-app or pipeline folder, or a loose `.py` — reports it as `kind`
under `--json`, and prints that loop's verbs. A capability nobody finds is a
capability that does not exist, and each of these loops already does the folder
sync, the declarations and the polling that people were otherwise hand-rolling
over raw HTTP.

Two commands make that HTTP half less unpleasant without describing any of it —
`ronja api` (a request runner) and `ronja query` (read-only SQL). Both are
below, and the doctrine that admits them is immediately below this.

The exceptions are the six sync loops — `ronja wf` (workflows), `ronja app`
(data apps), `ronja pipeline` (derived tables), `ronja automation` (automations),
`ronja db` (managed-database migrations) and `ronja module` (shared Python
packages), each below. The rule they keep is
**sync verbs yes, resource verbs no**: the CLI may own a filesystem-to-resource
sync loop, because that is stateful, multi-step and drift-guarded in a way plain
HTTP handles badly. It still owns no list, browse or delete. If an agent needs something the API already does, the answer is a
better pointer in `context` or a better guide behind `/llms.txt` — not a new
command.

A third kind sits beside the sync loops and the transport commands: **credential
handshakes**. A credential needs an interactive half — an echo-off prompt, a
browser approval, a provider's consent screen — and plain HTTP cannot do that
half without putting the credential in an argument list, a shell history or a
transcript. The CLI may own exactly that half. `ronja login`, `ronja secret
create`, `ronja secret connect` and `ronja token create` are handshakes. Each
one creates or stores one credential. None
of them prints a credential. None of them grows a list, show, rotate or delete:
whether a credential exists, and revoking it, stay on `ronja api` and in Ronja.
(`ronja env` and `ronja db env`/`connect` are the loaders: they move a credential
the CLI already holds into the place another program reads it, and only as
something to `eval` or exec.)

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

And one GROUP sits above them: `ronja sync` (`status`, `check`, `apply`,
`adopt`, `unmanage`) asks about a whole tree of folders rather than the one you
are standing in — a question a repository has and a folder does not (see
[Whole-tree checks](#whole-tree-checks-ronja-sync)). `status` and `check` are
read-only; `apply` deploys, by orchestrating the loops rather than writing
anything itself; `adopt` and `unmanage` move a tree's folders between keeping
their records in the lock and keeping them on the server (see
[`ronja sync adopt` and `ronja sync unmanage`](#ronja-sync-adopt-and-ronja-sync-unmanage)).
`unmanage` is not a delete verb: it releases the server's RECORD of which row a
file deploys to, after writing that record into the lock, and every row stays
exactly as it was. All five discover nothing: they can only act on folders that
already carry a committed `ronja.json`, which is the same "no browsing" line the
loops hold.

`ronja <kind> pull` is the loop run backwards, for an adopted folder: it writes
the live row the folder deploys into the folder, so a change made in Ronja can
be committed (see [Other writers: the notice, and `ronja <kind> pull`](#other-writers-the-notice-and-ronja-kind-pull)). It reads only the
rows the folder already manages, so it is a sync verb too.

`ronja import feature` is the tree's first step, as `clone` is a folder's: it
writes a feature's rows into folders — references rewritten into stems,
addresses and declared dependencies — and claims them for one stack (see
[Importing a feature](#importing-a-feature-ronja-import-feature)). It reads the
whole feature only to write it, never to show it, so it is a sync verb and not a
browser.

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

**Nor is there a `ronja mcp` verb.** An AI assistant that should *answer*
questions from an organization's data connects to the backend's MCP endpoint
(`POST /api/v2/mcp`, read-only tools) with the Personal Access Token
`ronja login` minted — loaded, never printed: the client config names
`${RONJA_TOKEN}` and the client is started after `eval "$(ronja env)"`. Recipe:
`backend/lib/api/openapi/guides/mcp.md` (served at `/docs/api/guides/mcp.md`);
design: `claude/MCP-ENDPOINT.md`.

Customer-facing documentation lives at
[`docs-site/src/content/docs/guides/use-the-cli.md`](https://ronja.tech/docs/guides/use-the-cli/)
(getting started) and
[`docs-site/src/content/docs/guides/work-from-your-editor.md`](https://ronja.tech/docs/guides/work-from-your-editor/)
(the folder loops), deliberately short: every flag, refusal and edge case lives
here and in each command's `--help`, not there.
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

The binaries are unsigned, so the cask carries a `postflight_steps` stanza that
strips the Gatekeeper quarantine bit. Without it macOS reports a freshly
installed `ronja` as "damaged" — which is not a message anyone connects to code
signing. The stanza is written by hand in `.goreleaser.yaml`'s `custom_block`,
because GoReleaser's own hook still emits the `postflight` stanza Homebrew 7.0
deprecated, and that made every `brew install` warn and blame our tap. It needs
Homebrew 6.0.13 or later. `brew install` normally auto-updates Homebrew first,
but not when `HOMEBREW_NO_AUTO_UPDATE` is set: an older Homebrew then reports
`Cask 'ronja' is unreadable`, and `brew update` fixes it.

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

Every request the CLI makes carries `User-Agent: ronja-cli/<version>` (the
version `ronja --version` prints; `dev` for an unstamped build), from one place
(`internal/useragent`, set at startup; the release fetch uses the same format).
An instance counts CLI versions by it before it changes a default an older CLI
cannot follow, and reads a bare `ronja-cli` — what every release before the
version was added sends — as older than all of them. A request `ronja api`
passes through carries `ronja-cli-api/<version>` instead (a `-H User-Agent`
still wins), so an instance never counts a hand-written request as one a CLI
command built. Keep both formats.

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

### A CLI that needs a newer server

Because the CLI ships on the same tag as the backend, a release is only safe
when every server route the new CLI depends on is already DEPLOYED — merged is
not enough: `cli_release.yml` is deliberately a separate workflow, so it
publishes the CLI even when that tag's backend deploy fails or is rolled back,
and a self-hosted or lagging instance may run older code for weeks.

The adopted-folder loops are the case in point. A folder with an `"id"` needs:

- the **managed-state routes** (`/api/v2/managed`, claims and anchors — the
  managed-state backend slice), which every adopted folder reads before it
  acts; and
- the **reference inventory and validate's `writeTargets`** (`GET
  /api/v2/feature/:id/references`, and `writeTargets` plus `resolved.workflowIDs`
  on `POST /api/v2/workflow/validate` — the reference-inventory backend slice),
  which `ronja import feature` and the write gate read.

So a CLI carrying the write gate must not be released before both backend slices
are live in production — merge them first, and let a deploy carry them out,
before the CLI change merges. On an instance older than that, an adopted folder
does not deploy: the write gate refuses it `no_write_targets`, saying that
the instance "does not report write targets" and "is older than this CLI", and
nothing is written. A classic folder (no `"id"`) is unaffected. The remedy the
refusal names — upgrade the instance, use a matching `ronja`, or `ronja sync
unmanage` — is the user's; the ordering is the maintainers'. `ronja import
feature` against such an instance refuses by name too: the inventory route
answers the router's plain-text 404, read as "older than this CLI", and nothing
is written.

`ronja <kind> pull` needs nothing more than an adopted folder already does: it
reads the rows' own single-row GETs and records its anchors through the
managed-state routes. The classic-loop notice reads `managedBy` off those GETs
(the managed-by backend slice); an instance that sends none simply prints no
notice, so it degrades to silence rather than to a refusal and needs no
ordering of its own.

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

One refusal carries the same code without being one of those routes: making or
refreshing a managed database's dev copy **with the production data** in it,
which terminates every connection to the production database while the copy is
taken. That is a property of the argument rather than the route — the same two
routes answer `202` for a **structure-only** copy — so the route is not flagged
and the handler refuses the narrower thing. See below.

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

## Secrets (`ronja secret create`, `ronja secret connect`)

```
ronja secret create --feature <feature-id> --name <name>
    (--host <host>... [--field <name>...] | --dialect <database>)
    [--description <text>] [--from-file <path> | --in-browser [--no-browser]] [--json]
```

Stores a credential in Ronja with one `POST /api/v2/secret`, taking the value
from a person in the one way plain HTTP cannot: without it ever being an
argument. With `--in-browser` the CLI takes no value at all — see below.

**Kind.**
- `--host` (repeatable) makes an **API key**: each one is a host the value may
  be sent to (`api.stripe.com`, `*.example.com`) — a bare hostname, since it is
  matched against the hostname of the URL a request goes to; a URL, a path, a
  port or a user is refused rather than stored to never match. It is `--host`
  and not `--url` because `--url` is the global flag naming the Ronja instance. `--field`
  (repeatable) names the values to store, default one called `apiKey`; every
  field is also listed as one a Workflow may read, since a field left off that
  list is one nothing can ever use.
- `--dialect` (`postgres`, `mysql`, `sqlserver`, `snowflake`, `bigquery`, …)
  makes a **database login**. The server applies the database's own list of
  fields and refuses a login missing one it needs to connect, naming it.
  `--host` and `--field` are refused beside it.

**Where the value comes from**, decided in this order, with no fallback — and
only once the CLI knows it is signed in, so a piped value is never read by a
command that was always going to refuse:
1. `--from-file <path>`, or stdin when it is not a terminal (both read whole,
   capped at 1 MiB — no credential is that large, so over the cap is refused as
   the wrong input on either). With one API-key field the content is the value,
   minus one trailing newline (`\n` or `\r\n`); content that is a JSON object
   carrying that field's name is stored whole as given, with a warning on stderr.
   Otherwise it is a JSON object of field → value, each field named once; an API
   key's object must carry exactly the `--field` names, each a string, and a
   database login's is sent as written (numbers stay numbers). Empty input is
   refused — `no value on stdin — pipe one in, or pass --in-browser to enter
   it yourself in Ronja` — rather than becoming a prompt nobody will
   answer, and never by quietly opening a browser.
2. A terminal: each API-key field is prompted for on stderr with echo off, and
   Ctrl-C at the prompt stops at once with nothing stored. A database login
   takes a JSON object, so on a terminal without `--from-file` it is refused.

```
pbpaste | ronja secret create --feature collection-abc --name Stripe --host api.stripe.com
ronja secret create --feature collection-abc --name Warehouse --dialect postgres --from-file db.json
```

**Refused before anything is sent:** a `--field apiKey=sk-…` (values are never
flags — they end up in your shell history), an empty value, a `--field` given
twice, a `--host` that is not a bare host, a JSON key given twice, `--dialect`
with `--host` or `--field`. There is no `--value` flag, and a test pins that.

**What it prints.** One line on stdout — the id, the name, the kind, the
feature, and each field's name and length:

```
secret-abc123  Stripe (api_key, feature collection-abc, 1 field: apiKey — 107 chars)
```

A length is the one thing about a value that is safe to show, and it is what
catches a truncated paste. `--json` prints the same as metadata (`id`, `name`,
`secretType`, `featureID`, `fields[{name, length}]`). A server refusal is
forwarded as the error, scrubbed first of every API-key value and of every
database-login field whose name marks it sensitive (the server's own
`password|key|secret|token|…` pattern) — the server does not echo a credential
today, and this is the CLI's half of that guarantee. A login's other fields are
left alone, since they are the very words a refusal is made of.

**`--in-browser` — the value is entered in Ronja, not here.** For a value that
must not pass through whatever runs the command: an AI agent has no terminal to
be prompted at, and its transcript is no place for a key. Only when asked —
there is no fallback into it. Nothing is read from stdin, a prompt or a file
(`--from-file` is refused beside it). **The guarantee, and its limit:** the
value never passes through the requesting program, its arguments, a shell
history or a transcript; whoever can edit the secret or the feature's
workflows — the same login included — can still use it and change where it
goes.
1. `POST /api/v2/secret/pending` creates a **pending** secret — the name, the
   hosts (or the dialect) and the fields, no value — and answers `{secret, url,
   reused, existing}`. Every requested API-key field is marked required in the
   field schema; all must be filled before saving. Database fields use the
   server's schema, including its optional fields. `url` is the secret's page,
   built and org-stamped by the server; the CLI opens it as given and never
   builds one. The server refuses,
   on this route only, an allowlist a pasted value could leave by: `*`, a
   wildcard over a suffix other people control (`*.com`, `*.co.uk`,
   `*.github.io`, `*.googleapis.com`) and anything that is not a plain ASCII
   hostname (an internationalised name goes in its `xn--` form). That refusal is
   a floor, not an exhaustive list: the person still reads the hosts. It also
   refuses a `pingConfig` (a health probe is a URL Ronja requests with the value
   attached, so a requester would be choosing where the value first goes; set
   one with `PUT /secret/:id` after the fill), and a control, bidi or zero-width
   character in the name or description. The CLI sends none of these. The route
   refuses an API token (its principal is not a person, so no one could fill
   the secret): `--in-browser` needs a `ronja login`.
2. `existing` — secrets you can already reach that point at the same host or
   database engine — is printed on stderr as a notice. It never stops the run.
3. The page is opened, or printed on stderr with `--no-browser` ("the form to
   enter it shows only for you, signed in"). Anyone who can read the secret can
   open the page; for the person the secret belongs to it is ONLY a short form
   (**Connect** <name>): one line naming the registrable domain each host
   belongs to ("Ronja will use this to connect to stripe.com."), plain-language
   warnings for a non-ASCII host, an entry matching no host and a wildcard, the
   fields, **Save**, and who asked (you, Ronja's agent in a chat, the access
   token by name, or an app or automation). **Secret settings** opens the full
   page at `?view=settings`, where the same form sits under **Enter the
   value**. The save is refused (409, nothing stored) if the secret changed
   after the page was rendered.
4. The CLI polls `GET /api/v2/secret/<id>` every 3 s until `status` is `ready`
   (pending → ready has no other cause). A 404 is terminal; transient failures
   get the login poll's window; Ctrl-C stops the wait and leaves the secret
   pending. After 15 minutes: `still waiting for the value after 15 minutes.
   Secret <id> stays pending; finish it at <url> or re-run with the same
   arguments`.

A re-run with the same name in the same feature, once the first run has
answered, gets the same pending secret back (`Using the pending secret you
started earlier`) when nothing differs — two runs in flight at once may each
create one; a
difference is the server's 409, naming the field — finish or delete that one,
or pick another name. A secret that is already ready is never reused. Against a
server without the route: `this Ronja server can't take a value in the browser
yet (the CLI is newer)`. The result line has the terminal path's shape, with
`value entered in Ronja` (or `<dialect> login entered in Ronja`) where the
lengths would be — they never passed through this process; `--json` adds
`enteredInRonja: true`.

A secret's existence, its value's lengths, and deleting it stay on `ronja api`
(`/api/v2/secret/query`, `/api/v2/secret/:id/field-lengths`,
`-X DELETE /api/v2/secret/:id`) and in Ronja.

### Connecting a service (`ronja secret connect`)

```
ronja secret connect <service> [--feature <id>] [--scope <scope>...] [--no-browser] [--json]
```

Connects one of Ronja's managed OAuth services (`gmail`, `google_sheets`,
`hubspot`, `fortnox`, …) by sending a person through the provider's own consent
screen — from a page in Ronja, never by opening the provider out of a terminal.
The CLI calls `POST /api/v2/oauth/<service>/initiate` with its login and opens
the `url` it answers: the secret's page with `?connect=<service>` (and a
`scope=` per `--scope`). For the secret's owner that page is only a **Connect**
<service> view saying what Ronja is asking to do and who asked; the provider's
sign-in opens, in the app's shared OAuth popup, only when they click
**Continue to** <provider>, and that click runs the same initiate from the
browser — so the `state` minted for the CLI's call simply expires unused. The
CLI waits on `GET /api/v2/secret/<id>`. The provider still redirects to Ronja's
registered callback, and the one-shot `state` is minted and consumed exactly as
in the app, so the CLI never sees a provider token. Against a server whose
initiate returns no `url`, the CLI opens the `authURL` itself, as it used to;
the callback page, opened in a plain tab, then first tries to close itself and
otherwise says "Connected. You can close this tab — if you started from a
terminal, return to it." A failed sign-in there says "If you started from a
terminal, return to it, press Ctrl-C and run the command again."

1. Initiate answers `{authURL, secretID, requestedScopes}` — `requestedScopes`
   is exactly what this run asks the provider for. Without `--scope`, a
   service with no scope choice asks for its scopes plus the connection's
   current grant, and one with a choice asks for its required scopes plus the
   stored scopes still in its catalog (its default set when there are none) —
   so a stored scope the catalog dropped is NOT in the request. With `--scope`
   it is exactly the chosen set (plus anything the service requires), which
   REPLACES the grant. There is one connection per provider and person, so
   connecting a second Google service widens the Google connection;
   `--feature` places a NEW connection, and when the connection already lives
   elsewhere the command says so on stderr and uses it.
2. **Already connected** — the secret is `ready` and its recorded grant COVERS
   `requestedScopes` (without `--scope`) or EQUALS it as a set (with `--scope`)
   — prints `already connected: …`, exits 0 and opens nothing. Coverage is
   enough without `--scope` because a consent over a grant that already covers
   the request could only REMOVE scopes the service does not use; `--scope`
   replaces the grant, so a broader grant that covers a narrowing request is
   not done. Coverage is the server's `ScopesCover` rule: `offline_access` asks
   for a refresh token and is never granted back, and a full Google scope
   covers its `.readonly` variant.
3. Otherwise the page `url` is opened (http/https only, as `login` does) and
   printed to stderr, or only printed with `--no-browser`; only the secret's
   owner can connect from it. Against an older server the `authURL` takes its
   place, with the old residual: it carries the `state`, and whoever completes
   it connects THEIR provider account to the caller's secret. Neither is ever
   on stdout or in `--json`.
4. **Completion** (a 3 s poll, up to 15 minutes from the page — its link does
   not expire, and Continue mints a fresh 10-minute `state` — or 10 minutes,
   the `state`'s TTL, from an `authURL`): from
   `pending` or `reauth_required`, the status reaching `ready`; from `ready`, a
   CHANGE in `grantedScopes`, the column only the callback writes. `updatedAt`
   is never read — a background token refresh bumps it. A 404 is terminal
   ("the secret was deleted while waiting"); a dropped connection, a 429 or a
   5xx is tolerated for 90 s like the login poll; Ctrl-C stops.
5. **Report**: `connected: <secretID> (<name>, scopes: …)` on stdout. If the
   recorded grant lacks some of `requestedScopes` (a granular consent screen
   with a box unticked), the error names them and the command exits 1. A
   timeout prints the grant recorded so far — a provider-side failure reads the
   same as an abandoned sign-in, because the callback reports errors only to
   the browser. Unticking exactly the new permissions on the consent screen of
   a `ready` connection leaves its recorded grant unchanged, so there is nothing
   to see and the command waits out its limit; its timeout then says "…, or the
   consent finished without granting anything new: the recorded grant is
   unchanged", the second reading offered only for a connect that started from
   `ready`. From the page the timeout adds that connecting there still works.

An older server answers without `requestedScopes`: the command then never
claims "already connected", always opens the sign-in, and skips the coverage
check with "this server can't confirm the scopes; check the secret in Ronja".
Re-consenting a `ready` connection to the SAME grant has no server-visible
change to wait on, so it is not offered; that stays on the Integrations page's
Reconnect.

**Refusals.** An unknown service is the server's 400, forwarded — it lists
every service this deployment can connect. `outlook_mailbox` is refused: it is
the mailbox-only Microsoft grant, connected by an admin as a mailbox. A scoped login without `secrets:write` gets "this login's
token cannot write secrets (needs secrets:write)". `--json` prints
`secretID`, `name`, `status`, `grantedScopes`, `missingScopes` (`null` when the
server could not say), `featureID` and `alreadyConnected` (true when nothing
had to be signed in to).

## Tokens (`ronja token create`)

```
ronja token create --name <name> --scope <scope>:<read|write>...
    [--expires <N>d|<YYYY-MM-DD>|never] [--no-browser] [--no-wait] [--json]
```

Gives an app its own Ronja token without the token ever reaching the terminal.
A token is minted only by a person signed in to Ronja in a browser: `POST
/api/v2/authentication/token` refuses every token, this CLI's login included,
because a login is full access and "no wider than itself" would bound nothing.
So the command does not mint. It hands the mint to the browser:

1. `GET /api/v2/authentication/token-link?name=…&scope=…&expires=…` — the
   server checks the caller is an **Admin** (403 with the Account page's own
   sentence, "Only an Admin can create a personal access token."), checks every
   scope against the one scope vocabulary, caps the name at 200 characters and
   checks the expiry's shape — `<N>d` between `1d` and `3650d`, or a date no
   earlier than today in UTC (400 naming the bad one, before any browser
   opens) — and answers the URL of **Account → Access tokens** with the create
   form pre-filled. The query is `new=1` first, then `expires`, `name`,
   `scope`, then `org`: `/account?new=1&expires=…&name=…&scope=…
   &org=<organization>#tokens`. The `org` is what makes a browser signed in to
   another organization ask before it shows the form. The expiry is passed
   through as given: the PAGE resolves `<N>d` to N days from its own today, in
   the browser's timezone, since the date a token expires on is a date there.
   The route mints nothing and writes nothing. The command then prints the
   organization on stderr — the profile's name for it, or the link's `org` id
   under an environment token.
2. Unless `--no-wait`, the ids of the caller's tokens (`GET
   /api/v2/authentication/token-mine`) are snapshotted **before** the link
   opens.
3. The link is opened as the server answered it, or printed to stderr with
   `--no-browser`. The form opens pre-filled under "Pre-filled from the Ronja
   CLI for <organization> — review before you create." and is never submitted
   for the person; an unknown scope in a hand-edited link, or an expiry that is
   not after the browser's today, shows in red and blocks **Create** (the
   expiry until a date is picked or never is chosen, so a token never quietly
   falls back to never expiring). The person clicks **Create token** and copies
   the token from the page, where it is shown once.
4. Unless `--no-wait`, the command reports the new token by diffing the
   caller's token list: `token-mine` is polled (3 s, up to 10 minutes) for an
   id that was **not** in the snapshot AND whose name is `--name` or whose
   grant is exactly the requested scopes — so the person may rename the token
   on the form, or change its scopes, but not both. A new token that matches
   neither (another terminal's `ronja login`, an MCP client signing in: full
   access, so never a scope match) is named on stderr as it appears and does
   not end the wait. On stdout:

   ```
   created: api_token-d3f…  Sales dashboard  data:read, structure:write  expires never
   ```

   An expiry prints as an RFC 3339 instant with its offset
   (`expires 2026-12-30T00:00:00+01:00`), never a bare date: the form sends
   midnight in the browser's zone, which is the day before in a zone west of
   it. The rows' `token` field is never decoded (the route blanks it anyway).

   `--json` prints one object with `status: "created"` and a `tokens` array.
   Each entry carries `id`, `name`, `scopeGrants` and `expiresAt` (an RFC 3339
   instant, or `null` for no expiry). All matching new tokens are included.
   With `--no-wait`, the object is `{"status":"pending","tokens":[]}`: creation
   has not been observed. Links and progress stay on stderr; neither form
   contains token values.

`--scope` is required, repeatable and comma-separable. There is no
`--full-access`: a token with full access is a choice made on the form, so
nothing an agent passes can pre-select it. `--expires` defaults to **never** —
an app built on the token must not stop working on a date nobody chose — and
takes `90d` (days from when the form is opened) or a date; `never` sends no
expiry at all.

**Refusals.** A 403 from a scoped login without `admin` (on either route):
"this login can't manage tokens (its token lacks admin scope). Create the
token in Ronja under Account → Access tokens." A 404 on `token-link` is an
older server: "this Ronja server is older than the CLI. Create the token in
Ronja under Account → Access tokens." A 404 on `token-mine` mid-wait is
terminal; a dropped connection, a 429 or a 5xx is tolerated for 90 s like the
other waits; Ctrl-C stops waiting (the form stays open); after 10 minutes it
says "no new token named "<name>" or with exactly the scopes asked for appeared
within 10 minutes; if you created it, it is in Account → Access tokens", and
lists the new tokens that did not match.

Listing and revoking tokens stay on `ronja api`
(`/api/v2/authentication/token-mine`, `-X DELETE
/api/v2/authentication/token-mine/<id>`) and in Ronja.

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

# organization settings: read every section, set currency and number format
# together (admin), list the locales reportingLocale accepts
ronja api /api/v2/organization/settings
ronja api -X PUT /api/v2/organization/settings/reporting -d '{"reportingCurrency":"SEK","reportingLocale":"sv-SE"}'
ronja api /api/v2/organization/settings/options --jq '.locales[].tag' -r

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
the exact opposite of "no deadline". This matters because a heavy query can
take minutes.

The CLI always sends `format: "csv_meta"` — it is not a flag. CSV is the one
shape that is both legible at a terminal and consumable by every data tool, and
the "meta" half is what makes truncation and a failure distinguishable from an
empty result.

The CSV is printed (or written to `--out`) exactly as the server's DuckDB
writer produced it — nothing re-encodes it client-side: a NULL is an empty
field (a one-column NULL row is `""`, so it survives any CSV reader it is
piped into), a boolean is `true`/`false`, a DECIMAL keeps its scale. The full
per-type list is in `claude/AGENT-TOOLS.md` → "queryRonja's CSV".

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

# reach it from outside Ronja, with your own Postgres tools
eval "$(ronja db env <database-id>)" && pg_dump -Fc > dump.pgc
ronja db connect <database-id>
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

`db env` and `db connect` are a different kind of thing again — see
"psql and pg_dump" below.

**`db sql` is DML only.** It runs as the database's *write* role, which has no
CREATE privilege, so DDL is refused by Postgres itself rather than filtered
here. Schema changes go through `db migrate`, which runs as the owner and
ledgers what it did.

**Unlike `ronja query`, a failed statement is a real HTTP error.**
`POST /api/v2/database/:id/sql` answers **400** carrying the Postgres message —
deliberately not the duckdb endpoint's 200-with-an-`error`-field. So there is no
envelope to inspect and no trap to document: an ordinary non-zero exit covers
it, for `ronja api` and `curl` as much as for this command.

**A write says what it changed, on stderr — and silence is not zero.** A
statement with no result set prints an empty CSV, so `N rows affected` is the
only thing that distinguishes a run that touched forty rows from one that
touched none. It counts the rows the statement's **outer** command affected, so
a write tucked inside a `WITH` clause is not in it. The line is **absent**
rather than zero whenever the server cannot honestly say — a statement that
returned rows, more than one statement in one call, a command whose tag carries
no count (`DO`, `CALL`, `SET`, `TRUNCATE`, `COMMENT`) — which is why
`RowsAffected` is a `*int` and `--json` emits the key with `null` rather than
omitting it: `null` is a value a script has to be able to tell apart from `0`,
and printing `0 rows affected` for an unknown count would be a confident lie
about a statement that may have written a great deal.

**`--out` narrates the BODY, not the count.** With a result set it is
`N rows written to <file>`; with none it is `no result set — <file> is empty`,
because `rowCount` is multiplexed and "40 rows written" over an empty file is a
lie about a path the caller is about to hand to a parser. The affected count has
its own line, above.

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

**Two limits, each counting what it is about.** The whole folder is always
sent, and one request may carry up to 1000 migrations (past that the server
refuses with "too many migrations in one request"). At most 40 of them may be
*not yet applied*, because those run in one transaction under the database's
lock; already-applied ones are only compared. Over 40, `push` is refused before
anything runs and the error names the 40th pending migration — push the folder
up to and including it, then the full folder. `status` prints the same as a
note (when nothing has drifted) and still exits 0: it warns about the next
push, the folder is not broken. Drift is refused first, naming every drifted
file.

### The dev copy (`--env dev`) and `db promote`

A managed database can have a **dev copy**: a separate Postgres database with
the same schema, its own data, and its own migration ledger. `db sql` and
`db migrate` reach it with `--env dev`, and `db promote` moves the migrations
the copy has and production does not onto production.

**Making one is not a CLI verb** (resource verbs never are — see the doctrine
above), but it is machine-callable, so a pipeline can stand up its own sandbox
through `ronja api` rather than depending on one made by hand:

```bash
ronja api -X POST /api/v2/database/<database-id>/twin -d '{"mode":"schema"}'
```

That answers `202` with the copy at `provisioning`; poll
`GET /api/v2/database/<database-id>` until its `twin` says `ready`. Do not
re-POST to poll — a repeat while it is provisioning returns the same row and
starts nothing, so it tells you no more than the GET does. Refreshing a
structure-only copy is the same shape at `:id/twin/refresh`, with no body — so
`-X POST` is required there, since `ronja api` defaults to `GET` when there is
nothing to send:

```bash
ronja api -X POST /api/v2/database/<database-id>/twin/refresh
```

A refresh while the copy is still `provisioning` is refused: the discard that
starts it would delete the record of a database that is still being built. Wait
for `ready`.

⚠️ **`{"mode":"data"}` is refused to a machine** with `403 human_required`,
create and refresh alike: a copy carrying production's rows is taken by
`CREATE DATABASE ... TEMPLATE`, which terminates every connection to production
while it runs. A person does that on the database's page in Data Studio, or by
asking Ronja in a chat. And a refresh may not CHANGE the kind — a stated `mode`
that differs from the copy's is a `400` for everyone, since downgrading would
empty a data sandbox and upgrading would disconnect production for a request
that never said so. Send no `mode` to rebuild the copy as it is.

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

### psql and pg_dump (`db env`, `db connect`)

These two point **ordinary Postgres tools** at a managed database, through
Ronja's Postgres-wire proxy. Neither wraps an endpoint. They are the same kind
of command `ronja env` is: they move the credential the CLI already holds into
the place a different program reads it from, without it ever being displayed.

```bash
eval "$(ronja db env <database-id>)"   # sets PGHOST/PGPORT/PGUSER/PGDATABASE/
psql                                   #      PGSSLMODE/PGSSLROOTCERT/PGPASSWORD
pg_dump -Fc > dump.pgc

ronja db connect <database-id>                              # interactive psql
ronja db connect <database-id> -- -c "select count(*) from orders"
```

**`db env` is meant to be evaluated, not read** — the same rule as `ronja env`,
for the same reason. `eval` consumes the command's stdout, so your access token
moves from the credential file into `PGPASSWORD` and nothing prints it. Running
it bare prints a live credential to your terminal, and it warns on stderr when
it notices it is about to. Shell state does not persist between separate agent
tool calls, so keep the load and the work in one command:

```bash
eval "$(ronja db env <database-id>)" && pg_dump -Fc > dump.pgc
```

**`db connect` prints nothing at all.** It execs `psql` with the token on the
child process's own environment, so the credential never reaches your terminal,
your shell history or `ps`. `psql` must be on your PATH; everything after `--`
is passed to it unchanged, and its exit status is this command's.

**Why both.** `libpq` has no environment variable for `keepalives_idle`. The
proxy sits behind a load balancer that idles a TCP flow out after a few minutes,
and only the *client's* keepalives reset that timer — so a long interactive
session needs `keepalives_idle=60` in the conninfo, which only a command that
builds the conninfo itself can supply. `db env` is the form that lets `pg_dump`
(never idle) work with no arguments; `db connect` is the one that survives a
coffee break.

**The session is read-only.** The proxy resolves the database's `read` role
server-side, so `SELECT` works and nothing else does — no DDL, no DML, no
`nextval` — whatever your own role is. Your token is the *password*, and it is
still your token: it carries your live role, dies when you leave, and can be
revoked like any other.

**`sslmode=verify-full` and `sslrootcert=system`, never `require`.** The token
travels as the password, and `require` accepts any certificate — so a downgrade
hands the credential to whoever answers the socket. Both commands set the strict
pair and no Ronja surface ever prints a weaker one. `sslrootcert=system` needs
libpq 16 or newer.

**Objects outside `public` need `-n public`.** The read role holds no `USAGE` on
other schemas, so a whole-database `pg_dump` of such a database fails on the
first object in one; restrict it to `public`.

**Two refusals, deliberately distinct.** *"This organization has no psql proxy
configured, or your role is not admin"* means there is no proxy block to work
from — the server does not distinguish the two causes, so neither does the
message. *"No read role Ronja can use"* means the proxy is there and the login
would authenticate before being refused; mint one with

```
ronja api -X POST /api/v2/database/<database-id>/user \
  -d '{"access":"read","featureID":"<feature-id>"}'
```

`featureID` is required and has no default — the feature owns the stored
credential, which makes it an access decision. Name an **organization** feature
when something inside Ronja will use the credential (a workflow, a data app, an
agent), since only that leg reaches a non-admin member; a **private** feature is
narrower and enough for a role you only ever use from `psql`, which the proxy
resolves either way. Private is not "yours alone", though — another admin can
still bind it. List features with their scope:

```
ronja api /api/v2/feature/query --jq '.result[] | [.id, .scope, .name] | @tsv' -r
```

And if the database already has a read credential, minting **replaces** it:
anything still holding the old one stops working.

**`--env dev` is not supported here** and says so rather than failing as an
unknown flag. A dev copy is a database in its own right at the proxy — its own
id, its own physical name, its own read role — and it is addressed by that id.
The prod→dev overlay exists for requests that *name* a production database, and
a proxy login never does: the username **is** the id.

**There is deliberately no `db dump`.** `pg_dump` after `db env` is the whole
feature, and a wrapper would have to re-expose every `pg_dump` flag — format,
compression, parallelism, table selection, `--no-owner` — forever, to be worth
using at all. What the CLI can add is the credential and the connection
parameters, which is exactly what these two do.

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

**`push` syncs every file in the folder** except dot-files and dot-directories,
`ronja.json`, `ronja.lock.json` and `.ronja/` (each dot-file and dot-directory,
a symlink, and a path the server would refuse are reported as skipped): a README, notes or a data file is stored
with the workflow like the code. A run loads only the entrypoint and
the other `.py` files whose file and folder names are letters, digits and
underscores (none starting with a digit), so `validate` and `push` (unless
`--no-validate`) name every other file: a note for a file that is not `.py` (kept, but the code cannot read
it) and a warning for a `.py` file whose name breaks that rule
(`lib/my-helpers.py`, `main.old.py`; rename it `lib/my_helpers.py`). Neither stops the push.

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

  `runtime` is the workflow's runtime version: `3`, a **durable** workflow whose
  container holds no credential for Ronja's table storage (its code reads a
  table only through `tools.query`), or `4`; which of them an instance creates
  new workflows on is the instance's to say. `1` (the original runtime) and `2`
  (durable, reading tables in the container) are **retired**: a folder cloned from such a workflow records its
  true `1` or `2` and keeps pushing files to its draft, but its code can be
  test-run or published only once the folder says `"runtime": 3` — the folder's
  spelling of the server's `runtimeVersion 3`. "Code" is more than the files:
  changing `parameters` or `entrypoint` in `ronja.json` counts, and so do the
  packages it installs and the tables, secrets, Agents and modules its files
  use. What still publishes on such a workflow is its title, its description and
  how it runs (reporting timezone, concurrency, dedicated compute, approval
  defaults). **Absent means "the instance's default", and the push that creates
  the workflow asks the instance which that is and pins it.** Unlike `parameters` it is a
  plain int with no three-state pointer, because the third state has nothing to
  describe: the runtime moves **one way**, so a pointer would publish a
  distinction no code could act on. `wf init` **always** writes the key, and the
  starter `main.py` with it, both read from the instance: `GET
  /api/v2/workflow/defaults?runtime=N` for the runtime `--runtime` names, else
  `GET /api/v2/workflow/defaults` for the one it creates new workflows on. The
  CLI carries no starter and no default of its own, so every failure fails
  `init` before it writes anything: a runtime the instance does not create on
  (its refusal names the ones it does), an instance too old to serve the route
  (update the instance, or use an older `ronja`), an expired credential, a
  `5xx`, a default runtime this release does not know (run `ronja update`),
  and a starter over 256 KiB. With `--from`, or over a `main.py` already in the
  folder, and no `--runtime`, the folder is still pinned to the instance's
  default, and the report (a `Note:`, or `warnings` under `--json` beside
  `scaffolded: false`) says the code was not written from its starter. Pinning
  is the point: the first push sends the pinned runtime on
  the create, so the workflow is created on the runtime its starter was written
  for even after the instance's default has moved. (`--runtime 1` and
  `--runtime 2` are refused: those runtimes are retired.) `wf clone` writes the
  runtime the cloned workflow is on (`1` included — the folder is committed, and
  is the only lasting statement of the runtime its code is written for). A
  folder that names none and has no workflow yet — written by hand, or by an
  older `wf init` — is pinned by the push that creates it: the push reads the
  instance's default, validates against it, sends it on the create and records
  it once the workflow exists (the report says `pinned to the instance's
  default`); a push refused before the create leaves `ronja.json` exactly as it
  was, and a failed read of the default stops the push before it sends anything.
  The key is committed, and the folder is the only lasting statement of the
  runtime its code is written for. An absent key **declares nothing**, and every
  reader treats it that way: an unpinned folder bound to a workflow on any
  runtime pushes without complaint, and `wf status` reports no drift for it.
  `validate` — the standalone command, the pass inside `push`, and `sync check`
  — rehearses against that same runtime, so a folder declaring none is checked
  against the one it is about to be created on (the instance's default, read
  once per run and never written) rather than against no runtime rules at all.
  A folder declaring none that is bound to a workflow `validate` cannot read is
  checked against no runtime rules, and its `could not read` note says so. A
  push raises a workflow on runtime 1 or 2 to
  a folder's `3` (`runtime  1 → 3 (Durable, tables via tools.query)` in the
  report; the raise lands on your draft as its own request, after the files,
  and `wf publish` commits the flip).
  Declaring a runtime BELOW the one the workflow already has is **refused** — the
  runtime cannot be lowered — and so, before anything is written, is a folder
  declaring `2` against a runtime-1 workflow (3 is the only upgrade) and a folder
  declaring `1` or `2` that would CREATE a workflow. A value this CLI does not
  know (`"runtime": 5` on this release) is refused when the folder is opened, so
  it fails at the push rather than at the first unattended run, with
  `run: ronja update` in the message. When the instance refuses a runtime
  instead — it does not take the one sent, or the workflow has moved past the
  folder's — the error ends with the runtimes this CLI knows and the way out
  either way: `ronja update` when the instance names a newer one, the
  `"runtime"` key when it names an older one. `sync apply` puts that hint in the
  folder's `notes`.

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
  `null` — means version 1. **Version 2 is stacks**, below; **3 is
  `dependencies`**; **4 is a JavaScript workflow** (`"language": "js"`); **5 is
  `moved`**, and `removed` in an automation folder (see "Addresses: `moved` and
  `removed`"); **6 is the folder `"id"`**; **7 is a dependency by address, as a
  write target, as an output or as a run edge**.

- **`stacks` and `ronja.lock.json`.** See the next section.

### Stacks (`--stack`)

A **stack** is one named environment this folder deploys to — `dev`, `staging`,
`prod`. It is what `--stack` selects, and it is the unit the committed files are
keyed by.

⚠️ The local `.ronja/state.json` is NOT keyed by stack but by instance and
organization (BL-4e71): two stacks of the same organization share one local
baseline, so deploy each from its own checkout — switching stacks in one
checkout is refused as a repointed binding (see "Deploying it to a second
feature of the same organization" under `ronja import feature`).

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
| a pipeline's `metaSHA256` and its `tableDocs` entries | `ronja.lock.json` | same test, about the row's **documentation**: which table a docs sidecar names, and what its prose looked like when everybody last agreed with it |
| a pipeline's `metrics` entries | `ronja.lock.json` | same test, about a metric's **definition**: which row a `metrics/<name>.json` file is, what that row's recipe hashed to when everybody last agreed with it, and what the file itself last declared. ⚠️ A metric's id lives here and **never** in `ronja.json` — that map is keyed by `.sql` path, and the SQL half of the loop walks it |
| a workflow's / app's `headVersionID` | `ronja.lock.json` | "this folder forked from that published version" is true for everybody |
| the applied **name** of a table, metric or automation (`name` on its entry), and the applied **title** of a workflow, app or module (`title` on the stack) | `ronja.lock.json` | "the live row was called this when everybody last agreed with it" — the baseline the name rule compares against (see **Names are declared attributes** below). Optional: a lock written before the field existed reads as "no answer" and falls back to the file's stem or filename, and an older CLI keeps the key through the lock's forward-compatibility sidecar |
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
reads the 404 as "create it". (That reading is right for the CREATE twins: they
404 a private Feature the caller cannot write into, which for a push is indeed
"there is nothing here for you". The RUN route answers the same status only for
a workflow the credential can no longer SEE, and `run` renders it rather than
passing it through.)

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
  silently. The **title** is additionally held to the lock's applied title, so
  a fresh checkout (no `.ronja/`) refuses a title renamed in the web app rather
  than writing it back — see **Names are declared attributes**.

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

**A folder on a retired runtime does not deploy to a new stack.** The first
push to a stack creates the workflow, and a workflow is created only on runtime
3 — so a committed folder declaring `"runtime": 1` or `2` keeps pushing to the
stacks that already hold its workflow, but its first push to a new one (another
organization, or a fresh environment) is refused at the create. Convert the
code and declare `"runtime": 3` first.

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

**A dotted alias is legal and is the natural name for a qualified handle.** The
charset admits `.`, so a folder may declare `"Sales.orders"` and write
`{{ ref('Sales.orders') }}`. It is resolved LOCALLY like any other alias — the
bound id is what reaches the wire — so the dot is a spelling choice for readers
of the folder, not a request for the server to resolve `feature.table`.

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

The same rule binds the `ronja pipeline` loop's own metric carrier, and for the
identical reason: a `metrics/<alias>.json` file's stem and its `recipe.source`
are both spelled `{"kind": "table"}`. See "Metrics — a definition, not SQL",
which states the consequence out loud — `ronja bind` cannot tell a metric from a
plain table of the same name, and the mistake surfaces at push, by name.

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

**Three ways of using an alias wrongly are refused**, and all three are
otherwise completely silent, because the codec keys on `(kind, name)` and a MISS
leaves the argument exactly as found:

- **A declared alias under the WRONG marker family**, any kind. `orders` is
  declared a `table`, the code writes `{{ secret('orders', 'token') }}`, and
  every other check passes — `CheckBind` sees a declared alias bound to a table
  id, `literalBindTargets` sees no literal id, and the unused-alias warning stays
  quiet because the table-kind key IS used by the ref beside it. The message
  names both kinds.
- **An unresolvable argument, `secret` only.** Deliberately asymmetric: the
  secret legs are the ONLY ones the server answers with a WARNING rather than an
  error (`rworkflow`'s `secretIDs` / `querySecretIDs` are `SeverityWarning`;
  `ref`, `write`, `agent`, `codex`, `mailbox`, `workflow` and `module` are all
  `SeverityError`). Every other family hard-rejects the save, and that refusal is
  older and better-aimed than anything invented client-side, so those are left
  alone.

- **A `{{ ref }}` naming a table nothing here resolves** — not a table id, not a
  positional index, not a sibling file's stem, not a declared dependency. This
  one is newer than the other two and exists because the SERVER changed: a
  table's name is now a handle into its feature, so Ronja resolves
  `{{ ref('Sales.orders') }}` — or a bare `{{ ref('orders') }}` inside the
  resource's own feature — and **stores the id**. Which is right for the server
  and fatal for a folder: the push succeeds, the remote copy holds an id, no
  alias is bound to that id so the de-alias leaves it alone, and the file on disk
  keeps the name. The two hashes never agree again and there is nothing to edit
  that fixes it — the same permanent drift as writing a bound id literally,
  reached from the opposite direction. Declaring the name under `dependencies`
  and binding it makes the round trip a function again: the name goes out as the
  id and the id comes back as the name.

The first two are GATED on the folder declaring dependencies at all, which keeps
the layer inert for every folder in the field. The third cannot be: the folder it
is about is precisely the one that declares nothing and writes a bare name — and
until names resolved server-side such a folder could not push at all, so refusing
it takes away nothing that ever worked.

All three scan source as TEXT — so a commented-out marker trips them. Accepted
rather than mirroring `stripLineComments` and `stripTripleQuotedBlocks`, whose
drift would be silent in both directions; deleting a dead line is a real fix, and
the messages say so. (The instance's runtime-3 starter, which `wf init` writes,
names its example ref as `{{ ref('table-…') }}` for exactly this reason: an
id-shaped placeholder, so `wf init` never writes a file `push` would then
refuse.)

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

It is **kind-agnostic** — `dependencies` is a key the workflow, data-app and
pipeline folder kinds all carry — which is why it sits at the root of the
command tree rather than inside `wf`, `app` and `pipeline` as three commands
that would have to stay identical.

⚠️ **A module folder is refused**, before anything is opened or looked up.
Module files are marker-free, so a declared alias has nothing to resolve *into*:
the block is not merely unbound, it is unfillable. Left to run, `bind` would
report "0 names bound" on one — true, and read as "nothing was missing" rather
than "this command does not apply here". Declare the dependency in the consumer
workflow, which does carry markers, and pass the resolved value into the
module's function as an argument. `ronja module status` says the same thing
about a `dependencies` block already committed to a module folder, which nothing
else refuses and nothing ever fills.

Seven rules, and each one is a wrong deploy that does not fail:

- **A write target is never proposed.** A dependency declared `"write": true`
  is a row the folder's code CHANGES, and the organization-wide exact-name
  match for it in a new stack is the very table another stack writes —
  production, when the new stack is a Dev copy in the same organization. It is
  reported unresolved with reason `write_target` ("write target — bound by
  hand, per stack") and left for a person to bind, stack by stack.
- **A run target is never proposed.** A dependency declared `"run": true` is a
  workflow or saved agent outside the stack's feature that the folder RUNS —
  with that row's own authority — and its exact-name match is the row another
  stack runs. Reported unresolved with reason `run_target` ("run target — bound
  by hand, per stack").

- **Only a name match is proposed — exact, or the same name ignoring case and
  separators.** The search endpoint scores 3 exact, 2 prefix, 1 substring, and
  an exact match is a hit in the exact bucket whose title equals the alias up
  to case. Only when there is none does a LOOSE match count: a title equal to
  the alias once case is ignored and every run of `_`, `-`, `.` and whitespace
  is one separator, with none at the edges (`looseBindName`, the one
  definition) — so `qa_import_crm` binds the secret "QA Import CRM". The
  endpoint escapes `_` and matches substrings, so `qa_import_crm` never finds
  that secret; when the alias has a separator and no exact match turned up,
  `bind` searches at most TWICE more: for the alias with its separators as
  spaces ("qa import crm"), whose full page proposes nothing, then for its
  longest word, which every loose spelling contains (`qa-import-crm` and
  "QA.Import.CRM" as well as "QA Import CRM"), only to rule out a second loose
  match — that word's page filling does not block a proposal, since a common
  word ("orders") fills one in any organization and says nothing about the
  spelling the spaced search saw whole. It keeps the hits that are the same
  name (and with a loose match already in hand too, since a second one is the
  ambiguity below). A
  loose match is judged on its title alone, the score being about whichever
  term was sent; a title spelled exactly like the alias still needs the exact
  score. An exact match always wins over loose ones, so a folder that bound
  before binds to the same row now, and the plan marks a loose match
  `(matched ignoring case and separators)` (`"loose": true` in `--json`).
  Anything nearer than that is a guess, and a wrong bind deploys a workflow
  against somebody else's table while reporting success.
- **The title is checked as well as the score.** The endpoint merges a tag leg
  that stamps a matching **tag**'s score onto whatever it is attached to, so a
  table *tagged* `orders` arrives as an exact hit whatever it is called. A tag is
  a label somebody put on a row, not the row's name.
- **Ambiguity is never resolved, `--yes` included.** `--yes` says "I trust the
  unambiguous ones"; it is not an answer to "which of these three tables called
  `orders`". Zero matches and three matches are reported as different things,
  because "no match for `orders`" and "three tables are called `orders`"
  send the reader somewhere completely different. Two loose matches are just
  as ambiguous as two exact ones.
- **A TRUNCATED result set proposes nothing.** `bind` asks for exactly the
  endpoint's hard cap (`maxTotalCap`, 40) and there is no paging, so a request
  that comes back FULL is a prefix of the answer rather than the answer — and the
  tag leg is what makes that reachable rather than theoretical, since a widely
  used tag fills the page with exact-scored hits and the second row genuinely
  called `orders` never arrives. A full page is reported as `ambiguous`, with the
  one candidate it did see named, so the invariant above holds structurally
  instead of resting on the page happening to be complete.
- **A codex or a module cannot be proposed at all.** The search endpoint fans
  out to neither, so an empty result there means "nobody looked", not "nothing is
  called that". Reported as exactly that, and set by hand.

A dependency declared **by address** or as an **output** is skipped altogether:
the first resolves through the other folder's claim and the second through its
name (see "Dependencies by address" and "Write-capable data sources and outputs"
below), and neither is ever bound by a search. A **write target** is not
searched for either (the first rule above), but it IS bound, by hand, so it is
reported unresolved until it is.

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

#### Select-parameter queries resolve aliases too

A workflow's `parameters[].optionsQuery` is SQL the server binds `{{ ref }}`
markers in, exactly as it does in a file. It goes through the same codec: an
alias there is sent as its id, the row's id comes back as the alias (so a
second push sends no parameter patch), and the pre-flight reads each
`optionsQuery` beside the folder's files — an alias used only there is not dead
config, and a bare table name there is refused like one in a file.

#### Dependencies by address

A folder can name a row ANOTHER FOLDER OF THE SAME REPOSITORY manages — a table
or metric a pipeline folder builds, a workflow, an app, a module — by where that
folder declares it, instead of by an id and a bind:

```json
"dependencies": {
  "orders":  {"kind": "table",    "from": "../../pipelines/sales", "address": "orders.sql"},
  "aov":     {"kind": "table",    "from": "../../pipelines/sales", "address": "metrics/aov.json"},
  "report":  {"kind": "workflow", "from": "../../workflows/report"},
  "helpers": {"kind": "module",   "from": "../../modules/helpers"}
}
```

`from` is the other folder's path relative to this one; `address` is its key
there — a `.sql` file, `metrics/<name>.json`, or nothing at all for a workflow,
data-app or module folder, whose one row is the folder. A metric is declared
`table`, as everywhere else. No `bind` entry is needed, and one beside an
address dependency is refused: they would be two answers to one question.

**How it resolves**, per stack, every time a command opens the folder
(`resolveAddressDependencies`):

1. The other folder's `ronja.json` is read, and its stack of the **same name** as
   the one being applied.
2. That stack must point at the same `url` and `tenantID`, or the dependency is
   refused `address_stack_mismatch` — an address never resolves across
   organizations or instances. The same-Space half of that rule has
   its hook in `addressSameSpace`, which answers nothing until a stack can name a
   Space.
3. The other folder must be ADOPTED (have a folder id) for that stack, or it is
   refused `address_not_adopted`: only an adopted folder keeps the server state
   an address is resolved through.
4. Its claim at the address is read — `GET /managed?feature=<that stack's
   feature>&folder=<that folder's id>` — and the claim's row is what the alias
   resolves to. No claim there (nothing applied yet), or a claim that is not
   `ok`, is `address_unresolved`, naming the folder and the address: apply that
   folder first, then this one. A `from` that names no folder this CLI can open,
   or one of a kind that cannot hold the declared kind, is
   `address_folder_unreadable`.

Before any of that, `from` must stay inside the repository the folder belongs
to, or it is refused `address_outside_repository`: the folder's git work tree
(the nearest ancestor holding a `.git` entry, symlinks resolved); without one,
the tree a `ronja sync` command walks; without either, the folder itself. A
committed folder cannot resolve an alias through some other folder on the
runner's disk. A tree not yet under git resolves its addresses when `ronja sync`
runs from its root.

A resolved address joins the folder's alias codec like a bind, so every loop —
push, status, the drift legs — reads it exactly as it reads a bound alias. An
unresolved one is refused by the pre-flight of every acting command, reported
`unknown` with its reason by `sync status`, and `unresolved` with its reason (and
`from` / `address`) by `sync check`; `sync apply` refuses the folder under the
same reason. A resolved table read is recorded as a cross-folder build edge
(`folder.crossFolderTableReads`).

**A table a WORKFLOW folder writes** is named with the workflow folder as
`from` and, as `address`, the name of a dependency that folder declares
`"write": true` or `"output": true`:

```json
"weekly_revenue": {"kind": "table", "from": "../../workflows/compute-weekly-revenue", "address": "weekly_revenue"}
```

Steps 1–3 above apply as they are; step 4 is `resolveWorkflowTable`: for a
`write` dependency, the workflow folder's binding of it in the stack of the same
name (a write source is always bound; none is `address_unresolved`); for an
`output`, its binding if the stack has one — a table of THAT STACK'S FEATURE,
or `address_stack_mismatch`: a stack block copied from prod binds prod's table,
and the dev reader would read production. That is checked on the table itself
(`GET /feature/model/:id`, one read per bound output — never the feature's
listing, which leaves out a hidden or archived table), so a bound output, like
an unbound one, needs a credential (`no_credential` without one) — else the table the workflow's runs
made under that logical name IN THAT STACK'S FEATURE — the workflow's id from
the run's overlay or its claim at "", then `GET /feature/model?featureID=` and
the row whose `workflowID` and `workflowLogicalName` match. The listing is read
once per feature per folder resolution, however many outputs ask it. Never a table of
that name in another feature: that is how a second stack's app read
production's table while its own workflow wrote a copy, with `bind` proposing
the production row and `status` silent. Before the workflow exists in the
stack, or before its first run has made the output, it is
`address_unresolved` with the remedy — run the workflow on that stack once
(`ronja wf run --stack S`), then apply again — and it is never deferred (a run
of the same apply creates the workflow, not its output). An `address` naming no
dependency of the workflow folder, or one it does not write (a plain read, a
`from` of its own), is `address_not_written`. The read is in-stack, so no
`write` or `run` binding is needed; the edge carries the stack's feature for an
unbound output and none for a bound row. It is a READ and nothing more
(`addressEdge.Written`): unlike a row another folder manages, it gives this
folder no authority to write the table, which the workflow folder may bind to
another team's feature. A docs sidecar (`tables/<alias>.json`) on it is gated
as on any table — admitted in the stack's own feature, or bound by this
folder's own `"write": true` source — and refused `writes_outside_target`
otherwise. `ronja import feature` writes this
form for every folder of the tree that reads a table a workflow folder of the
tree writes (`featureImport.writtenBy`) — a workflow folder too, whatever
order the folders are planned in, two workflows reading each other's tables
included: `planPortability` plans the workflow folders once to fill
`writtenBy`, then every folder with it whole — and reports it under Addresses. Format 7 already carries `from` / `address`; a
CLI that predates this form refuses the folder (`address_folder_unreadable`)
rather than misreading it, so no new format version is needed.

**Inside one `sync apply --allow-create`, a row the same run creates resolves in
memory**. An address whose only problem is that the other folder holds
no claim there YET is not refused at plan time but deferred (`addressPending`),
and the deferral is kept only when the same run deploys a unit of that folder at
that address (`resolveDeferredAddresses`, to a fixed point — a folder refused
for any reason takes its units away from every folder waiting on them). The
folders then run in dependency order (see `sync apply`), and just before a
folder with deferred addresses runs, they are resolved through the run's own
record of the rows it made or confirmed (`addressOverlay`, keyed by the other
folder's feature, folder id and address — its stack of the same name), the
declared kind checked against the row's as against a claim's. A deferred address
nothing in the run deploys is refused `address_unresolved` before any write,
saying so. Without `--allow-create` nothing is created, so nothing is deferred
and the refusal stands; every other command resolves through the claims the
server holds and nothing else.

#### Write-capable data sources and outputs

Two more keys on a dependency say what the folder's code does to the row:

```json
"dependencies": {
  "events":       {"kind": "table", "write": true},
  "orders_daily": {"kind": "table", "output": true}
},
"stacks": {"main": {"…": "…", "bind": {"events": "table-…", "orders_daily": "table-…"}}}
```

- **`"write": true` — a write-capable data source.** A row outside this folder
  that its code changes: a table written by `{{ write(...) }}` or by a
  `tools.replaceTable("table-…")`-style output literal, a codex the workflow may
  upsert into. It is bound per stack like any dependency, and an unbound one is
  refused like any other: a stack has to decide, deliberately, which row it
  writes. The alias also resolves inside `{{ write(...) }}`, which no other
  dependency does — and a `{{ write(...) }}` naming a dependency declared
  neither `write` nor `output` is refused, since it would go out as the name of
  a NEW table. ⚠️ An output literal is a call argument no codec rewrites: it
  stays an id in the code and writes THAT table whatever the stack binds. In an
  adopted folder the write gate (see "The write gate" under adopted folders)
  is what makes that safe: a stack whose feature does not hold the table and
  that does not bind THAT id as a write source is refused
  `writes_outside_target` — so binding the dependency to another row in a new
  stack refuses rather than silently redirecting nothing. Turn the literal into
  `{{ write('<alias>') }}` and the bind is what it writes. An unused-alias
  warning is never raised for a write dependency. `ronja bind` never proposes
  one: which row a stack may write is bound by hand, per stack.
- **`"kind": "feature", "write": true` — another feature an agent automation
  is given.** A `feature` entry in an agent action's `references` other than
  `"@feature"` (the stack's own): the run expands it to every live table of that
  feature and may write each one (`rscheduledjob.LoadReferencesExpanded`; the
  inventory reports it `read_write`). So it is a write source like any other —
  bound per stack on purpose, never proposed by `ronja bind`, and refused
  `writes_outside_target` by the write gate when the stack has not bound it.
- **`"run": true` — a run edge out of the stack's feature**. A
  workflow (`"kind": "workflow"`) or saved agent (`"kind": "agent"`) outside
  this repository that the folder RUNS: a `{{ workflow() }}` / `{{ agent() }}`
  in workflow code, an app's `allowedWorkflowIDs` / `allowedAgentIDs`, an
  automation's `workflow` / `saved_agent` action or `workflow` reference.
  Running a row acts with that row's authority, writes included, so in an
  adopted folder a run of one in another feature is as deliberate as a write
  there: the write gate refuses it `runs_outside_target` unless a `"run": true`
  dependency binds it IN THIS STACK. A target in the stack's own feature, or a
  row this repository manages in the stack (an address dependency), needs no
  flag. Refused beside `write`, `output` or `from`, and on any other kind.
- **`"kind": "folder", "write": true` — a folder write scope.** A path in the
  organization's file system the code writes files into (`{{ file('/exports',
  'write') }}`, `tools.saveFile(…, folder='/exports')` — a write-mode `file`
  marker's argument IS the folder scope). A folder belongs to no feature —
  every feature of the organization shares it — so the write gate admits a
  write into it only for a stack that binds this dependency to that path
  (`"bind": {"exports": "/exports"}`), compared EXACTLY in the server's
  normalized form (one leading `/`, no trailing `/`, duplicate slashes
  collapsed; `wfdir.NormalizeFolderPath`): there is no prefix match, so
  `/exports/daily` needs a bind of its own. Nothing is substituted into the code: the bind
  is the stack's permission, not a rewrite. A folder is declared only with
  `write`, and its bind must be a path, not the root, with no `.` or `..`.
- **`"output": true` — the folder's OWN output, by logical name.** The alias is
  the name the workflow writes the table under (`{{ write('orders_daily') }}`),
  which the runtime creates in the stack's own feature. A stack that already
  holds the row MAY bind it, so the marker round-trips the id the live row's code
  carries; a stack that binds nothing sends the name. Unlike every other
  dependency, an unbound output is legal in a `{{ write(...) }}` marker. A
  workflow that also READS its own output with `{{ ref(...) }}` would send the
  name there, which the server resolves and stores as an id — permanent drift.
  So in an adopted folder an unbound output the code reads by `ref` is resolved
  IN MEMORY to the table the workflow's runs made under that name in the stack's
  feature (`resolveOwnOutputs`, off the feature's table listing): it goes out as
  that id — in its `{{ write() }}` markers too, exactly as a bound output does —
  and reads back as the name. ⚠️ Before the workflow's first run in a
  stack there is no such table, and the server refuses a `{{ ref }}` naming a
  table that does not exist (`unresolved_ref`), so a workflow that reads its own
  output by `ref` can be pushed to a NEW stack only once that output exists
  there. `ronja bind` never proposes an output — the exact-name match would be
  the same output in another target, the one row it must never write.

Neither key may be combined with `from`, and they may not be combined with each
other.

**Format version 7.** A dependency carrying `from`, `address`, `write`, `output`
or `run` stamps `"formatVersion": 7`, and one that says less is refused naming the
line to add: an older CLI would read a write-capable alias as an ordinary one and
send it inside `{{ write() }}` as a NEW table's name. `run` is a version-7 key
rather than an 8: version 7 never shipped without it, so no CLI reads 7 and not
`run`, and one below 7 has no write gate at all.

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
  submits the draft and returns without re-anchoring, so once an admin or a
  maintainer commits it the folder names a draft that is gone and holds files that are usually
  exactly what went live. Telling that author to re-clone a perfectly good
  folder is what the split exists to stop.
- **Then the write gate, in an adopted folder**, over the LIVE row's code as
  the server holds it — the code that is about to run, which a republish may
  have changed since this folder synced — exactly as `wf test` gates the draft.
- **Then the legacy approval gate**, refused with the same wording `test` uses
  and, like it, naming the conversion to a mid-run approval rather than the
  off-switch.

Three server refusals are rendered rather than passed through as a status code.
Running a workflow needs only **read** access to it — whoever can see a workflow
may run it, shared Feature or private — so neither refusal is about the Feature's
scope. A **403** is the role (a read-only member may open a workflow but not
start runs) or a token whose scopes lack `automation:write`; the CLI cannot tell
the two apart and names both. A **404** is the run gate's oracle-safe answer for
a workflow the credential can no longer SEE — access withdrawn or the row moved
between the push and the run — and since the push just exercised exactly the
read access a run needs, it is rendered as the race it is rather than as
absence. A **409** is the `skip` concurrency
policy: it leads with the fact that *nothing ran*, and names the blocking run
from the response's own `blockingRunId` detail, never out of the message (see
`api.AsRunInFlight`, and `gt.NewRunInFlightConflict` on the server for why the
prose cannot be trusted to carry it). Everything else — the credit kill-stop
above all — arrives in the server's own words.

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

### Durable workflows (runtime 3)

A durable workflow journals the result of every `@tools.step` under a key
DERIVED from the function and its arguments, so a failed run can be **resumed**
instead of re-run: the journaled steps are replayed and only the work that never
finished executes again.

**Runtime 3 is what a new workflow is, and the only runtime it can be.** It is
durable **and** withholds every Ronja table credential from the container: its
code reads a table only through
`tools.query("SELECT ... FROM {{ ref('tbl::...') }}")`, never by reading a
parquet file itself. `wf init` pins the runtime the instance creates on and
writes the instance's starter `main.py` for it; a folder that declares no
`runtime` is pinned to the instance's default by the push that creates its
workflow, and the create summary names the runtime it was given. Runtime 1 and 2 are
retired: a new workflow on either is refused. The server refuses the create on
every CLI release; a release with the freeze refuses earlier — `wf init
--runtime 1` and `--runtime 2`, and a push that would create a workflow from a
folder declaring either, are refused before anything is written.

Resume itself is not durable-only. On the original runtime 1 a step is journaled
when the author gives it an explicit key (`tools.step("key", fn, ...)`), and
`--resume` replays exactly those. What a durable runtime changes is that nobody
has to write a key — so a loop journals per item, and forgetting one stops being
possible.

```bash
ronja wf init --feature collection-abc        # pins the instance's runtime, writes its starter main.py
ronja wf push                                 # the create sends the pinned runtime
ronja wf test                                 # it fails somewhere in the middle
$EDITOR main.py && ronja wf push              # fix the bug
ronja wf test --resume                        # continue from where it stopped
```

Three things are worth knowing, and each is a place the obvious behaviour would
be wrong:

- **The runtime only ever goes up, and only to 3: 1 → 3, 2 → 3.** The manifest's
  `runtime` rides the `POST /workflow` body of the push that creates the
  workflow, and afterwards a push from a folder declaring `3` **upgrades** a
  workflow on runtime 1 or 2 — as its own request, sent after the files land,
  because the server checks the raise against the file set it holds — on your
  draft, so the flip is reviewed and published with the code change that needs
  it (`ronja wf publish`). The upgrade is reported as
  `runtime  1 → 3 (Durable, tables via tools.query)`, and `ronja wf status`
  shows it as pending drift beforehand. A raise the server refuses stops the
  push with the files it landed recorded, and the next push sends the raise
  alone. The other direction does not exist: a durable workflow's journal is
  keyed by the derived keys, so lowering it would orphan every journaled step
  and silently re-run the pipeline. A folder declaring a LOWER runtime than the
  workflow already has is refused before the push writes anything — fix `ronja.json`,
  or clone the workflow again — and so is one declaring `2` against a runtime-1
  workflow. A raise is also refused while one of the workflow's runs is in
  flight. A run already started keeps the runtime it began with. A `--resume`
  after the raise continues a runtime-2 run on runtime 3 — the two key the
  journal alike, so its finished steps replay — but is refused for a run that
  began on runtime 1, whose author-keyed journal runtime 3 cannot match: start a
  new run.
  Upgrading does not rewrite your Python. Convert the code in the same push:
  every table read through `tools.query`, keyless `tools.step`,
  `tools.now()` / `tools.uuid()` / `tools.random()`, `tools.http` for outbound
  calls, and an entrypoint ending in a bare name. `ronja wf push` validates
  against the declared runtime, so the runtime-3 rules and the Durable
  advisories come back in the same command. Until the folder says `3`, a legacy
  workflow's changed code can be pushed to its draft but not test-run or
  published: the server refuses both, and `wf publish` exits non-zero with its
  sentence (a refused review included). Update the CLI (`ronja update`) before
  moving a folder to `3`: an older release sends the raise before the push's
  deletions, so a push that moves the folder and in the same change deletes a
  file the runtime-3 rules refuse cannot finish on it.
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

`wf init` writes the instance's starter `main.py` for the runtime it pins (on 3,
in the shapes a resume depends on: decorated steps, keys derived rather than
written, `tools.now()`, handles across step boundaries). A durable workflow
scaffolded from v1 code journals nothing and fails silently: the run works, and
the resume that was the point of it does not.

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
data-app folder is refused rather than allowed to overwrite the app's source with
Python.

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
add when it is a React export. A global a `<script>` provides is not a mistake:
reach it as a `window.X` member, which passes.

And nothing static can promise that `Compiles: yes` renders anyway — the
compiler does not check types, so a mistyped prop, a hook called after an early
return, a wrong table id, or a component that mounts and returns `null`
publishes green too. `status` prints the app's URL; open it.

**The app's spec version decides who styles it, and the folder does not own it.**
An app carries a `specVersion`: 1 means styling is the author's job (the
entrypoint imports `@/styles/theme` and `@tailwindcss/browser` itself), 2 means
the compiler installs both into the bundle and the entrypoint writes neither. An
app created today — including one `ronja app init` creates — is spec 2, because
that is the server's create default. The CLI's data-app **row mirror carries the
field**, so the generation an instance reports is decoded rather than guessed,
and a `0` from an older instance reads as spec 1 exactly as the server reads it.

There is deliberately **no `specVersion` key in `ronja.json`**, and no way to
push a migration from a folder. A CLI-created app already takes the server's
default, and nothing in a folder can usefully drive the 1 → 2 upgrade, so a key
would buy a manifest field, a drift-baseline entry and a load-time validator for
no behaviour at all. Migrate through the API instead —
`PUT /api/v2/dataapp/<id> {"specVersion": 2}` — or ask the agent to. ⚠️ **If the
key is ever added it MUST validate at load**, the way `wfdir.ValidRuntime` does
for a workflow's `runtime`: the record of what skipping that cost is in the
workflow `manifest.go`, and a hand-edited generation that only fails at push is
the same bug again.

**Some of what compiles clean fails silently, and `push` names what it can.**
Every successful file write also lints the whole set for the handful of shapes
the frame quietly swallows — a `layout.title` the chart theme strips, a
**spec-1** kit app with nothing to compile its classes (no
`import "@/styles/theme"` and `import "@tailwindcss/browser"` at the top of the
entrypoint), `enableTailwind()` beside the kit's own runtime — and `push` prints
them under the verdict as a `Warnings:` block, one `file:line: message` per finding (`file: message` for a
finding about the set as a whole). They are advisory: `Compiles: yes` stays yes,
the exit code is unchanged, and `--json` carries them as `warnings` (omitted
when there are none). What you see is the answer to the **last** file write of
the push, because that is the write that saw the folder complete — each write
lints the set as it stands, so an earlier write's findings (the kit component
that landed before the entrypoint importing the theme) are superseded, not
merged. Two pushes print none by construction: one whose only change is a
delete, since a file DELETE is not linted, and one that had nothing to push. Nor
does a push that stopped part-way or whose set does not compile, and a finding
about a file the same push deleted is dropped. `app validate` does not
repeat them; edit and push again, or edit through the app. A last write the
instance never answered but did land leaves the retry up to date and therefore
silent, and its warnings surface on the next edit. `ronja sync apply` carries
them too, as the folder's `note` lines — and still publishes, since a warning is
not a verdict.

One of those lines is not a mistake at all. Pushing to a **spec-1** app that
already renders with Tailwind prints the **migration advisory**: it names both
doors, `updateDataApp(specVersion: 2)` and
`PUT /api/v2/dataapp/:id {"specVersion": 2}`, because from a folder the second
one is the one you can actually call. Migrating needs no edit to the source —
the two import lines stay harmless under spec 2 — so bump the version on its own
and leave the folder alone.

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

**`icon.svg` at the folder root is the app's icon**, shown on its card on the
Apps page. `push` sends it like any other file (and `clone` brings it down);
the CLI never parses or checks the icon itself. The server checks it against
the icon grammar in the `creating-data-apps` skill
(`/docs/api/skills/creating-data-apps.md`) and refuses a file that breaks it with a 400 naming the problems. That refusal
**stops the push at that file**: the files written before it stay written, and
the entrypoint, which goes last, is not reached. Fix the file and push again —
unchanged files are skipped. An icon write compiles nothing, so its response
carries no `compileError` or `warnings` (a push whose only change is the icon
prints no `Warnings:` block); the server marks it with a `status` field
(`icon_saved` / `icon_deleted`). The CLI reads that status for one thing: a
write answered with one linted nothing, so it does not replace the warnings of
the last write that did — an icon pushed after a changed component still
reports that component's warnings (with a CLI built after app icons shipped;
an older binary prints no `Warnings:` block in that case). A `components/icon.svg`, or any icon not at
the root, is an ordinary file.

**`api/` holds the app's server functions.** Each `.ts` / `.js` file directly
in `api/` is a server function: backend code that runs on Ronja's servers and
that the app calls with `functions.call("<name>")`. Deeper files (`api/lib/…`)
are its helpers. To the CLI they are ordinary files: `push` sends them, `clone`
brings them down, `status` diffs them, and nothing here parses them. The server
builds and checks every function on every code write, so a bad one comes back as
`Compiles: NO` naming the file. That check reads each function's literal
`{{ ref('table-…') }}` refs, `env.metrics.query('table-…')` ids, `env.db('secret-…')` ids,
`{{secret::secret-….<field>}}` placeholders, `env.codex('cdx-…')` ids and `workflow-…` ids against the allowlists the push has just sent,
and a function that calls `env.ai.complete` against the app's `ai` capability, which is one more
reason `access` goes first (see the push order below). `env.db` is also held
to the app's `query_external` / `write_external` capabilities (which call needs
which is *Ids, and what the app declares* in the skill), and those are checked when the
function runs, not on push. `app test` exercises functions only
through the app, whose `ops` list each call as `/fn/<name>`. There is no verb
that calls a function directly, and `ronja api` cannot either, because the route
takes only the app's own token. The runtime and the limits of one call are in
the `creating-data-apps` skill (*Server functions*, *Limits and errors*), and
the `/service-api/v1/fn/:name` wire contract in the `dataapp-dev` guide (*Server
functions*).

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
  plus the commit bar for a shared app (an admin or a maintainer of its feature). `ronja.json` declares what the app
  should be allowed; the server decides what it is.
- **Every file write but the icon's recompiles the whole bundle.** A push of N
  code files is N esbuild compiles and N bundle uploads, so it takes a few
  seconds per file and says which file it is on. It also means intermediate states that do not
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
  `NO`.** The row carries one column, `validated_at`: every file write but the
  icon's clears it in the same transaction and only a successful compile
  re-stamps it. So NULL is equally true of a draft that failed to compile, one
  pushed with `--no-validate`, and one whose compile check never answered — no
  compile status is persisted anywhere. `ronja app validate` is what finds out which.
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
| `status` | local changes, remote lifecycle/draft, whether the draft compiles, allowlist differences, drift, and how many updates Ronja recommends for the app. Read-only, works signed out for the local half | `--json` |
| `fork` | copies one built-in kit file into the folder at the same path, so it shadows the kit's. Local only — changes nothing server-side, and never overwrites an existing file | `--json` |
| `push` | ensures a draft, syncs allowlists then files, then compiles. A `"title"` renamed in the web app since this folder last agreed with it is refused, not reverted (see **Names are declared attributes**) | `--no-validate`, `--force` |
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

**`status` names recommended updates, and they are optional.** When Ronja's apps
have changed since the app was built, the Remote block carries one line,
`N recommended updates (optional).`, followed by the two calls that act on it:
`ronja api /api/v2/dataapp/<id>/updates` lists what changed and what to
check, and `ronja api -X POST /api/v2/dataapp/<id>/rebuild` rebuilds the app as
your draft, leaving the live app alone. Nothing is printed when nothing is
recommended, for an app never published, when you cannot draft the app, or on an
instance without the route; a failed check is one line and never fails `status`.
`--json` carries it as `remote.updates` (`checked`, `count`, `notCheckedReason`).
`ronja sync status` does not check.

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
   against `allowedSecretIDs` (and a server function's table refs,
   metric ids and `env.db` ids against their lists), so a file that arrives before its grant
   fails to compile for a reason that is nowhere in the file.
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
ronja app test --viewport 1280x2400        # a tall frame, to see below the fold
ronja app test --steps steps.json          # admin only; the steps really execute
```

`--viewport` is `desktop` (1280x800, the default), `mobile` (390x844) or `WxH`,
each side 240–2560. A preset is sent as `viewportPreset` and a size as
`viewport`; a malformed or out-of-range size is refused locally. An instance
older than explicit sizes ignores the field and renders desktop, so when the
viewport the harness echoes differs from the size you asked for, the CLI warns:
"this server does not support custom sizes; the frame is desktop". No echo
confirms nothing either way, and prints nothing.

`--route` reaches the app the way a real deep link does: the hash is in
`location.hash` from document start and no `hashchange` fires for it, so the app
must read it at mount.

`--timeout` is the whole render budget, steps included, and the server clamps it
to 40s. With `--steps` and no `--timeout` the CLI sends 40s, because a step plan
is carved out of the same budget the page settles in; a plain render keeps 20s.
A step reported skipped `"budget"` never ran, and the CLI prints one hint: raise
`--timeout`, or that step's `timeoutMs`. It names no number, since an older
server clamps lower and says nothing about it.

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
The plan is a file you name — nothing is ever synthesised — holding either a
bare JSON array of steps or the HTTP body's `{"steps": [...]}` object. In the
object form `steps` must be the ONLY key: `route`, `viewportPreset`/`viewport`
and `timeoutMs` are refused with the flag that carries them (`--route`,
`--viewport`, `--timeout`), because honouring them would give the file a second
say over the command line and ignoring a `route` would render a different page
from the one written down. Both shapes are decoded **strictly**: a mistyped key
decoded leniently becomes a click with no selector, which the harness then
reports as a fact about the *app*, and a file holding two JSON values is refused
rather than half-run.

```json
[
  {"action": "click", "selector": "role=tab[name=\"Orders\"]"},
  {"action": "waitFor", "selector": "#orders-table", "timeoutMs": 8000},
  {"action": "screenshot", "selector": "#orders-chart"}
]
```

`click` and `waitFor` take a `selector` or `text`; `fill` a `selector` (or
`text`) and a non-empty `value`; `screenshot` nothing (the viewport) or a selector (that
element, scrolled into view — its own scroller still clips it). A step's
`timeoutMs` (default 5000, max 10000) is how long it waits for its element.
⚠️ `text` is Playwright's `text=` engine: a case-insensitive **substring**
matched against the **first** element in document order, so `"Orders"` can hit
a heading before the tab. Prefer `#id`, `role=tab[name="…"]`, or `text="…"` for
an exact match.

`domText` and console message bodies reach `report.json` but are never printed to
the terminal: they are the app's own rendered content (tenant rows, whatever an
external system last wrote), and in a terminal they would be indistinguishable
from the CLI's own words.

## Pipeline folders (`ronja pipeline`)

`ronja pipeline`, aliased `pl`, develops a feature's **derived tables** from a
folder of `.sql` files: clone → edit → push → publish. It is the fourth folder
loop, and the first one that binds *many* resources — one `.sql` file is one
derived table.

It also carries the **documentation** for those tables, in an opt-in header in
each `.sql` file, and for tables it does *not* build — integration, foundation,
dynamic, workflow-written — in a committed `tables/<alias>.json` sidecar. See
**Documentation** below. A table's **health checks** live beside its SQL in
`checks/<stem>.json` — see **Health checks** below.

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
  which is still reported. Three kinds of non-`.sql` file *are* read, each by a
  pass of its own rather than by the walk: a `tables/<alias>.json` docs sidecar
  (**Documentation**), a `metrics/<name>.json` definition (**Metrics**) and a
  `checks/<stem>.json` list of health checks (**Health checks**).
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

  A **fifth**, `metaSHA256`, arrives with documentation and is described in its
  own section below. It is a separate value for the same invariant: it is taken
  from a different part of the row and answers a different question, so folding
  it into `liveSHA256` would report "the SQL changed" for a description somebody
  edited in the web app.

  A baseline entry whose file has left both the folder *and* the `tables` map is
  pruned on the next `push` (and disappears from `status` immediately), so
  removing the `ronja.json` entry is the whole of the cleanup for a table this
  folder no longer manages.

A table's name is the file stem, stamped once at create, unless the file
DECLARES one with a `-- @name` header line (see **Names are declared
attributes**): then the name is managed, and a push renames the same row in
place. With no `-- @name`, push never writes the name again, so a rename in the
web app survives. The file's path stays its identity either way — renaming the
FILE is not renaming the table; it unbinds the file and the next push creates a
second table.

The stem is still a **round trip**, which is why `clone` derives filenames
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

**Stored code comes back in two forms, and the second one is not rare.** The
write API stores id-form verbatim, but the retired AI build — `POST /:id/build`
and `/fix` used to rewrite a table's SQL — persisted **positional**
`{{ ref('0') }}`, an index into the row's `input_models`. `/build` no longer
rewrites code, but every table it wrote still holds positional refs. `internal/tablerefs` is a
byte-for-byte mirror of `esql.IndexToTableIDRefs` (with that function's own test
cases as fixtures), and **every remote code read is canonicalized to id-form
before it is hashed or written to disk** — without which every such table
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
| `clone` | writes one `.sql` per **derived** table, one `metrics/<name>.json` per **metric** and one `checks/<stem>.json` per table with enabled health checks, plus manifest, lock and baseline. Prefers your own open draft over live. Creates nothing server-side; the directory must be empty or absent | — |
| `status` | local changes, per-table build health, open drafts and their verdicts, drift since the last sync, and per checks file the health checks a push would write, pending publish, changed in the app, orphaned, silenced or refused (a kind change). Read-only — not even a checkout. **Exits non-zero on drift *and* on "not checked"** (see below) | `--json` |
| `push [paths...]` | per changed file: create → resume/check out your draft → write SQL + derived inputs → sync → build → **documentation** → verdict and confidence report. Then, in passes of their own, each `tables/*.json` docs sidecar, each `metrics/*.json` definition and each `checks/*.json` list of health checks | `--force`, `--force-verified-metric`, `--prune` |
| `publish [paths...]` | per staged draft: commit onto the table or the metric, or submit it for review; a committed table's `checks/` file is applied right after the commit — with no `--prune` or `--force`, so drift or orphans found then are reported (non-zero, the commit standing) and left for a `push` after the commit has landed. A committed metric definition change re-fingerprints, so the report says the metric now reads *verified · review pending* | `--no-request-review`, `--overwrite-remote` |
| `discard [paths...]` | deletes your drafts, metric drafts included; the live rows and your local files are untouched | `--yes` |

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

`clone` writes one `.sql` per derived table **and one `metrics/<name>.json` per
metric**, and says on stderr only what it genuinely left behind: non-derived
kinds, as "not cloned (kinds: dynamic, integration)", which have no SQL and no
recipe a file could hold.

⚠️ **The metric line is deleted rather than reworded, and that is the point.** It
used to read "not cloned (out of scope for this loop)" — phrased with some care
so as not to claim metrics *could* not be cloned, because a metric runs the
identical draft flow and saying otherwise would send someone looking for a
capability the product already had. That sentence is now simply false, and a
message this loop no longer needs is one to remove. The single remaining skip is
a metric an older instance answers no `metricRecipe` for; it is named
individually and counted apart (`skipped.metricsWithoutRecipe`) rather than
refusing the whole clone, because a collective refusal would make `clone`
unusable against every instance that does not serve the field yet.

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
  is deliberately **not** mirrored — a name is stamped once at create and this
  loop never writes it again, so a field it cannot cause is a field nobody would
  keep in step.
- **Documentation** (`Docs: description written; 2 column(s) documented: …`) —
  what the file's `-- @table` header wrote, and which of its column names the
  table's data actually has. Read back from the write rather than predicted; see
  below.
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
  the read would buy a query that can take minutes for output the run does not
  print.

All of it degrades to a stderr note and never fails the push — the build
succeeded and the work is safe; the report is how you decide whether to publish
it. The one degradation worth recognising is the **scope quirk**: the authoring
loop is `data` scope but `/table/draft/*` lives in the `admin` scope group, so a
token scoped to `data` alone runs the whole loop except this read and
`request-review`, which sits in the same group. A full-access PAT — what
`ronja login` stores — is unaffected.

### Documentation — two carriers, one contract

A folder of SQL can say what its tables and columns hold, and a push writes it.
There are two places to write it and they are deliberately disjoint.

**1. An opt-in header in a `.sql` file**, for the tables this folder *builds*:

```sql
-- @table One row per invoice line, from Fortnox, refreshed nightly.
-- Amounts are in SEK; a credit note carries a negative quantity.
-- @column invoice_no: the supplier's own number, not Ronja's id
-- @column amount: line total, excluding VAT
SELECT ...
```

A `-- @name <text>` line may sit in the same header — after `-- @table`, or on
its own when the file documents nothing. **Write it after `-- @table`, never
before.** This CLI also reads a header that opens with `-- @name`, but an older
one opts in only when the first line is `-- @table`, so a teammate on an older
CLI would silently stop sending the documentation of a file that opens with
`-- @name`. It is not documentation: it
declares the table's NAME, and is read by its own rule (see **Names are
declared attributes**). A header that names its table and documents nothing
arms no documentation leg and sends no `fields`.

**Opt-in is the whole design.** The block is read only when the *first* comment
line of the file opens with `-- @table` (or `-- @name`); a leading comment that does not is
ordinary commentary and nothing is read, judged or reported. Without that rule
every pipeline folder in existence would start pushing its first comment as a
table description on the next release, silently, over prose an admin may have
written by hand.

The grammar in full: leading blank lines are skipped; a plain `-- text` line
**continues** whatever was last opened, joined with a single space, so a
description or a column note may run over several lines; `-- @column <name>:
<text>` opens a column, and everything after the *first* colon is the text; a
**bare `--`** ends the documentation, so anything below it in the same comment
block is ordinary commentary and is not read — but a `@table` or `@column` down
there is *reported*, once, with a count, because a documented name that goes
nowhere on a push that reports success is a drop nobody can see. A malformed line
— no colon, no name, no text, an unknown `@directive`, a second `@table` — is
**warned about and dropped**, never a refusal: nothing about a comment should be
able to stop a push of SQL that is perfectly good.

The header **stays in `code`**. It is sent as part of the SQL and hashes as
ordinary bytes, which is what makes an edit to a description show up as a
changed file in `status` and get picked up by a bare `push` with no new
machinery. The drift guard canonicalises `{{ ref }}` markers and nothing else.

**2. A committed docs sidecar**, `tables/<alias>.json`, for the tables this
folder does **not** build — integration, foundation, dynamic, workflow-written.
Those are where column prose matters most and they have no folder anywhere:

```json
{
  "description": "One row per Fortnox invoice line, synced hourly.",
  "columns": {
    "invoice_no": "the supplier's own number, not Ronja's id",
    "amount": "line total, excluding VAT, in SEK"
  }
}
```

The stem is an **alias**: a name `ronja.json` declares under `dependencies` with
kind `table`, which each stack's `bind` map points at the id *that* organization
uses — so one committed folder documents the right rows in every environment it
deploys to, and `ronja bind` reconciles the names. A literal `table-…` id is
accepted too, exactly as everywhere else in the alias layer, and means the
folder only ever works against the organization that minted it. Two keys and no
more: unknown keys are an **error**, because a dropped key is the failure this
whole loop exists to remove — the file says one thing, the row keeps another,
and nothing reports a difference.

It lives in the pipeline folder rather than in a `ronja table` loop of its own
because everything such a loop would need is already here: one manifest, one
bind map, one lock, one tree walk. The two carriers cannot fight over one table:
a sidecar naming a table this folder *builds* is refused, and the check is on the
id it resolves to rather than on its name — a sidecar called after a literal
`table-…` id, or an alias bound to one, collides with no filename at all.

**Three-state, both carriers.** A column you do not name is **unmanaged**: the
push sends nothing for it and the row keeps whatever it says, so a file may
document three columns of twelve. In the sidecar an explicit `""` is the
opposite claim — "there is no prose here" — and clears the row; the header has
no way to spell that, and a `-- @column x:` with nothing after the colon is a
warning rather than a clearance.

**The join rule: the data is the truth, and prose is joined onto it.** A
documented name the table does not have is **not** created as a column and is
**not** an error. Its prose is stored against the name, re-attaches by itself if
a later build produces the column, and the push reports it:

```
  revenue.sql — pushed (built)
    Docs:   description written; 1 column(s) documented: id; 1 name(s) the table
            does not have, kept for when it does: margin
```

That is what makes a committed docs file safe to keep in git: it may
legitimately drift from data that is rebuilt nightly, renamed in staging, or not
loaded yet. In `--json` the same answer is `docs.attachedColumns` /
`docs.unmatchedColumns` per file, taken from the server's own reply rather than
guessed locally — the CLI has no schema and must not predict one.

**The order is push → build → attach → report.** Documentation is written
*after* the build, on the draft, because the join attaches prose to the columns
the table has **measured** — sent with the SQL it would be judged against no
columns at all on a new table, and against the previous build's on an old one.
The header's `fields` do ride the `create` as well, so a table never exists even
briefly with no prose its own file already holds; on a create every name comes
back unmatched, which is the same rule against an empty left side. A sidecar has
no draft and writes the live row directly: a table this folder does not build
has nowhere for prose to wait, and documentation is the one change to a table
that cannot break a build.

Every role may do this. Documenting a table is not an admin act — a drafter
writes the prose and the admin reviewing the draft sees it before approving.

**A third drift leg.** `metaSHA256` fingerprints the live row's *whole*
documentation — its description plus every column that carries prose — and a
push carrying documentation is refused when it has moved since this folder
agreed with it, naming what moved. `--force` overwrites *and* re-records the
agreement, or the same difference would be reported on every later push and
`--force` would become a permanent part of the command line. It **arms only for
a folder that documents something**: a folder with no header and no sidecar
records nothing and behaves exactly as it did.

It covers the whole row rather than the columns this folder manages, and that is
deliberate. Hashing only the managed names would move the value whenever the
*file* changed — adding one `-- @column` line would compare a hash of six
columns against a recording of five and report drift nobody caused. Hashing the
row means it only ever moves when the **row** moves, which is the only thing the
leg asks about. The cost is that prose somebody else wrote on a column this
folder does not manage also pauses the next documenting push, once, with a
message saying exactly that.

A sidecar records **two** fingerprints, in `ronja.lock.json` under `tableDocs`:
that one, and `declaredSHA256` — what the *file* declared at the last push. The
second is not redundant. A sidecar is not in the folder's enumeration at all (a
pipeline folder syncs `.sql`), so nothing else can answer "has this file changed
since we pushed it" — and comparing the file against the row cannot answer it
either, because a documented name the table does not have is stored server-side
and never reported back, so such a file would read as pending for ever and be
re-sent on every push.

In an adopted folder both are also kept on the server, on the folder's own
anchor rather than on any claim, so a runner with no lock still has them — see
**The folder anchor holds the docs-sidecar and health-checks baselines** below.

**`status` and `sync status` understand both.** A `.sql` file's header shows as
a `docs:` line beside its `drift:` and `live:` legs. Each sidecar gets its own
entry saying which table it names, whether a push would write anything
(`pending`), and whether the row's prose moved. In the tree gate a pending
sidecar is a **local** change — the same shape as an edited `.sql` file, because
the organization does not yet hold what the folder committed — while a row whose
prose moved is drift, and one that could not be resolved or read is *unknown*,
never clean. `sync check` verifies the table a literal-id sidecar names and
reports one whose alias nothing binds; a sidecar's alias also counts as a **use**
of its dependency, or the alias pre-flight would call every such declaration
dead config on every command.

**Failure modes.** A documentation write that does not land while the SQL did is
its own outcome, `docs_failed`: the draft holds the work and is still
publishable, but the run exits non-zero, because a folder whose committed file
says one thing and whose row says another is exactly the silent divergence this
loop exists to remove. Such a file is **not** recorded as synced, so the next
bare `push` picks it up and tries the prose again — without that, a caller who
may not write column prose on this instance would fail on every documented file
of their first push and never be offered a retry. It is counted apart in the
summary, too: its SQL landed, and calling it a file that "did not land" would
send an author looking for a table that is there. **`publish` honours the same
withholding.** It runs in a separate process and remembers nothing, so before
banking a documented file it asks the live row whether it holds what the file
declares — judged under the join rule, so names the table does not have are
exempt. When it does not, the content baseline is left as `push` left it and the
file stays unsynced; banking it would let the next bare push skip it for ever,
with the committed header and the row disagreeing in silence. A sidecar that
cannot be parsed, whose alias nothing binds, or which names a table **this folder
builds** — one table, one carrier — is refused **before any request** and named;
the loop may only ever ask about names the folder already declares, never browse
the organization for a table with a similar name. A sidecar that declares
**nothing** (an empty `{}`, or a placeholder somebody committed and has not
filled in) is a **warning**, not a refusal — outcome `nothing_to_document`,
nothing sent, exit code untouched. As a refusal it broke the whole folder: every
later push exited non-zero for ever, including pushes naming unrelated files.

**The sidecar's write is guarded, not compare-and-swap, and the difference is
worth knowing.** The `.sql` half sends `baseCodeSha256` and the server refuses
the write outright if the row moved. A documentation write has no such
precondition to send — `PUT /feature/model` has one, and it is on `code` — so
the sidecar's guard is a read-then-write: it reads the row, compares the
fingerprint, then writes. A web edit landing inside that one-round-trip window is
overwritten. Bounded, and stated rather than papered over: it is prose only, the
losing edit is in the row's audit history, and the next push re-reads and reports
the divergence. Closing it properly means a `baseMetaSha256` on the server's
documentation body, and it belongs with that change.

### Metrics — a definition, not SQL

A metric is a **recipe**, so no `.sql` file can hold one. It gets a carrier of
its own inside the same folder — `metrics/<alias>.json` — and the loop's three
verbs mean for it exactly what they mean for a derived table, because a metric
runs the IDENTICAL draft flow: `push` stages it in your own draft and builds it,
`publish` commits it or asks an admin to, `discard` drops the draft.

That is what makes this a file type rather than a `ronja metric` loop. Everything
such a loop would need is already here — one manifest, one bind map, one lock,
one tree walk — and a second top-level command would be second copies of all
four. It is the docs sidecar's argument, and it applies with more force here,
because unlike documentation a metric really does have a draft, a build and a
publish, and those are the verbs this folder already has.

```json
{
  "recipe": {
    "source": "orders",
    "time": {"column": "created_at", "native_grain": "day"},
    "dimensions": [{"name": "country"}],
    "base_measures": [
      {"name": "revenue", "agg": "sum",   "column": "amount"},
      {"name": "orders",  "agg": "count", "column": "*"}
    ],
    "value": "revenue / orders"
  },
  "description": "Average order value, per day.",
  "reportingTimezone": "Europe/Stockholm",
  "tags": ["Finance", "Sales"]
}
```

**Four keys, and the asymmetry inside them is deliberate.** Unknown keys at the
TOP level are refused where they were written, on the sidecar's rule — a dropped
key is the failure this whole loop exists to remove, and a `verified` or an
`owner` somebody adds expecting it to be honoured must not be ignored on every
push for ever. Unknown keys INSIDE the `recipe` are preserved verbatim, because
the recipe grammar belongs to the server (`rdb.ValidateMetricRecipe`) and a Go
shape for it here would be a second thing to keep in step, whose drifted version
would accept a recipe the compiler then refused. There is deliberately no field
for anything DERIVED from the recipe — additivity, the time and value columns,
the dimension list, the compiled SQL, the source closure — and none for
verification state: a file that could set either could contradict its own
recipe, or mint a metric asserting somebody else had vetted it.

**Names.** The file's stem is the metric's identity: the name a new metric is
created under, or an alias a stack binds to a metric that already exists. A file
may also declare `"name": "…"`, which manages the metric's name on the same
rule as a table's `-- @name` (see **Names are declared attributes**) and is the
name a new metric is created under; absent, the name is stamped once and never
written again. The name is not part of the definition's fingerprint, so a file
that only gained a `"name"` still stages the rename. `recipe.source` resolves exactly as a `{{ ref }}`
does — a sibling `.sql` stem first, then a declared alias, then a literal
`table-…` id — which is what lets one committed folder define the same number in
several organizations.

**The pass runs AFTER the `.sql` pass, and that is where the ordering comes
from.** A metric contributes no `{{ ref }}` edge (no marker resolves a metric),
so it cannot join the folder's topological order; running the pass last buys the
same guarantee by construction. A metric MAY source a table this folder builds —
that is the main case, and there is no disjointness rule against it. The docs
sidecar's `builtBy` refusal does not transfer: that one is about a table having
ONE carrier, and a `recipe.source` is a reference to another row rather than a
second carrier of it.

**One namespace.** `requireNameFreeTx` has no `kind` predicate, so a metric and a
table collide server-side. A folder holding both `revenue.sql` and
`metrics/Revenue.json` is refused before the first byte is sent — the store stays
the truth, and the local guard exists only so the refusal names both files
instead of arriving half way through a push.

**Drift, two legs with two owners.** The LIVE metric's canonicalized recipe
against the lock's `liveSHA256`, checked client-side before anything is written —
no draft precondition can speak about the live row. And the DRAFT, owned
server-side by `baseRecipeSha256`, which is strictly better than a local baseline
because it also catches the author's own chat agent editing the same draft.

⚠️ **`liveSHA256` is taken over the ROW's recipe, never the file's.** The file
carries aliases and may omit a defaultable key; the row carries real ids with
every default filled in. Hashing the file would report drift on every stack, on
the first push, for ever. The bytes hashed are the ones the server SENT, hashed
verbatim — decoding a recipe into a map and re-marshalling it changes the key
order and breaks the compare-and-swap against a difference nobody made.

⚠️ **`--force` is not enough on a verified metric, and this is the one
destructive gate in the loop.** Leg (a) firing means a colleague committed a
definition change to the company's official KPI. So `--force` prints the remote's
canonical recipe diff before overwriting it — you see what you are replacing —
and REFUSES outright when the live metric's `metric_status` is `verified` unless
`--force-verified-metric` is passed as well. Overwriting a verified definition
somebody else wrote should cost a sentence you had to type.

**`publish` says what committing costs.** A recipe change re-fingerprints on the
next build, so a previously-verified metric lands *drifted*; the report says so
(`"reverified": true` in `--json`) because that is a governance consequence the
author caused and would otherwise meet in the UI.

⚠️ **A LEGACY unnamed `instances[]` folder keeps a metric's id in the git-ignored
local baseline**, exactly as it keeps everything else from before stacks existed.
So a colleague's fresh clone of such a folder has no id, tries to create by name,
and is recovered by adopt-by-name below when its file still holds the metric's
definition (`ronja bind` otherwise). A `--stack` folder commits the id and
never meets this.

**Recovery when the binding is not recorded.** The likelier failure is not a
crash but a 200 the CLI failed to persist: the metric exists, the lock does not,
and the next push tries to create the same name — which the store refuses with a
`name_taken` the author cannot act on from the folder. So on a `name_taken` at
create the loop looks the name up in the target feature, and if it resolves to a
single `kind=metric` row THERE that holds **exactly this file's definition** it
adopts it into the lock and carries on. A metric of that name with any other
definition is somebody else's — a colleague's, or one a declared `"name"`
collides with — and adopting it would skip every drift leg and stage this
file's recipe over theirs, so it is refused naming that metric; anything else is
reported as the collision it is, with `ronja bind` as the manual route. Same
shape as `reconcileTimedOutCreate`, and adopting only on a positive
identification is the whole of its safety. The equality is exact (a file that
leaves out a key the server defaults does not match), which keeps it on the
refusing side: such a file is pointed at its row with `ronja bind`.

⚠️ **Known limitation, documented rather than fixed.** `GET /api/v2/search`
reports a metric as kind `table` (`search.Hit.IsMetric` is `json:"-"`), so
`ronja bind` cannot tell a metric from a plain table of the same name and will
bind an exact-name match of either. There is deliberately no `metric` dependency
kind and there will not be one — a metric alias is spelled `{"kind": "table"}`,
for the reason `allowedMetricIDs` is above. The mistake surfaces at push, by
name, which is where it can be acted on.

#### Tags — `"tags"` in the metric file

`"tags": ["Finance", "Sales"]` is the folder's claim about the metric's tags,
three-state like `description`: **absent** means the folder does not manage
them (a push never reads or writes a tag), a **list** is the names the folder
asserts, and **`[]`** asserts none. Names are trimmed and otherwise kept as
spelled; every comparison is the server's, `lower(trim())`, so `finance` in the
file is satisfied by a `Finance` tag. The parse refuses a non-string, a blank
name, a name over **64 bytes** (bytes, as the server counts — 33 `ö`s are over),
more than 20 names, and two names that fold together.

**Tags are not drafted.** A tag sits on the LIVE metric, so a push reconciles
it with one `PATCH /api/v2/tag/of/table/<metric id>` — no checkout, no build, no
publish — and the tag half is independent of the recipe in both directions: an
up-to-date definition, a drift refusal or a failed build never skips it, and a
tag refusal never fails the recipe. `publish` and `discard` do not touch tags. A
tag-only edit makes the push not up to date, so `ronja sync apply` deploys it;
it is also kept out of the recipe fingerprint, so it never stages a draft.

**The folder asserts its names, following tag ids.** The lock records
`tagsApplied: [{id, name}]` — what this folder last put there — beside the
recipe fingerprints (in the committed lock on a named stack, in
`.ronja/state.json` on a legacy folder, and carried into the lock when the
folder is named). From that, one predicate that `push` and `status` share:

- a name the file lists that the metric does not hold is **added** — including
  one a colleague **removed** in the web app. The folder wins, and the push says
  `tags: +Sales`;
- a recorded tag the file no longer lists is **removed, by id** — so a tag
  renamed since is still found;
- a tag a colleague **added** in the web app is never touched, and never makes
  the folder pending;
- a tag an admin **renamed** in the web app is **kept**: the recorded id still
  on the metric satisfies the file's old name, so nothing is reverted and the
  old name is never re-minted beside the new one. `status` says
  `renamed in the web app: Fin → Finance — update the file`;
- with **no record** (a legacy folder's first push, a rebind, a folder's first
  `tags` key) the reconcile is **additive only**: nothing here can tell the
  folder's tags from anybody else's.

A direct name match wins over a renamed record: when the metric carries a tag
spelled as the file's name, that tag is the one recorded, and a recorded tag an
admin renamed away from it is no longer this folder's — it is **kept on the
metric, never removed**, like any tag the web app put there.

Push output is `Tags: +Sales −Operations`, one `created tag "Finanace"` note per
name the push minted into the organization's catalog (so a typo is seen the
first time it lands), and a note for a recorded tag already gone when the PATCH
ran. `status` adds a `tags:` line per tagged file: in place, what a push would
change, a rename, or a refusal. A pending tag change is local work in `ronja
sync status`, never drift.

**Archived or hidden metrics are skipped** — reported `skipped_archived`, not
pending, never redeployed by `sync apply`. An admin can still tag them in the
web app.

**The cap is the only refusal recorded, because it is the same for every
caller.** A change that would leave the metric over 20 tags — counting the
ones added in the web app — is refused by the server with code `tag_limit`. It
is reported as the tag half's `refused` outcome and recorded as
`tagsRefused: {fileTagsHash, reason}`: while the file's tag list is unchanged
the metric is not tag-pending and the request is not re-sent, so `sync apply`
cannot loop on it. A recorded cap refusal stays until the file's tags change or
`ronja pipeline push --retry-tags` asks again — nothing re-checks it when a tag
is removed in the web app, so `--retry-tags` is the way out once there is room.
It does not fail the push or the folder's `sync apply` verdict. The cap is
recorded even when a colleague's concurrent add is what tipped it over: the
server checks it under its lock. It is not purely the file's fault, which is
why the flag exists.

**Every other failure is never recorded, fails the push, and is retried by the
next one.** Each is a fact about the caller or the deploy rather than the file,
and recording it in the committed lock would stop every colleague's push — and
this one's, after the fix — from ever trying again:

- a role that may not change tags ("your role can't change tags");
- a metric this caller cannot see ("you can't see this metric");
- a token whose scope does not cover tags ("this token's scope doesn't allow
  write on tags — use a token with data:write");
- an organization whose Ronja has no tag route yet, a 404 or 405 ("this
  organization's Ronja doesn't have tag support yet — push again after it's
  updated");
- any other 400 (a tag deleted mid-merge), a 5xx or a dropped connection, and a
  rename race that repeats.

These are the tag half's `failed` outcome, printed as **Tags: NOT applied**. A
`failed` tag half makes the push exit non-zero and not up to date; `status`
reports a read that failed this way as `not checked`. In `ronja sync apply` it
does not hold the folder's SQL hostage — tags are not drafted, so the staged
SQL is still published, on the health-checks precedent — and the folder's
verdict is `tags_not_applied` (`unknown` when the tag write got no answer and
may have landed), which still makes the run exit non-zero. A rename race is the
server refusing the PATCH because an admin renamed a tag this push was removing
to a name it was adding, between the push's read and its write: the push
re-reads and re-plans **once**, and only a second race in a row is `failed`.

**Clone** writes each metric's tags into its file and records them as applied,
so the clone reads as clean. A metric with no tags gets no key. A tag read that
fails gets no key and a note on stderr — never `[]`, which would assert no tags
and strip every one on the next push.

⚠️ **Version skew.** The `PATCH` route and this key ship together, in the first
release after v0.32.0. Against an older organization, the tag half fails with
"this organization's Ronja doesn't have tag support yet — push again after it's
updated": never recorded, so the first push after the organization updates
applies it, and never holding back the folder's SQL in `sync apply`. An older
CLI meeting a folder whose metric files
carry `"tags"` refuses the whole metric file as an unknown key — run
`ronja update` (see [Keeping it current](#keeping-it-current)).

**Stated limits.** A tag assignment carries no author, so removing a name from
the file removes it for everyone who also chose it. A token's tags land in the
organization's default Space. Tables, workflows, apps and modules have no
`tags` key yet.

### Health checks — `checks/<stem>.json`

A table's health checks are code-defined beside its SQL: `checks/gl_lines.json`
holds the checks on the table `gl_lines.sql` builds. One flat file per `.sql`
stem; a checks file whose stem names no `.sql` file in the folder is refused
(checks for a table built elsewhere stay in the app).

```json
{
  "checks": [
    {
      "name": "Unik rad per id",
      "expression": "count(*) = count(distinct id)",
      "severity": "fail",
      "description": "Dubbletter betyder oftast att en join har multiplicerat raderna."
    },
    { "name": "Bygger om dagligen", "expectedIntervalHours": 24 }
  ]
}
```

Each entry is a `name` and exactly one body — `expression` (a boolean SQL
scalar over the table) or `expectedIntervalHours` (a freshness watchdog) — plus
optional `severity` (`warn`/`fail`), `description` and `enabled`. The parser
refuses an unknown key, both bodies or neither, two names that fold together
(lower case, trimmed — the server's unique index), and a second watchdog. The
expression grammar, lengths and the per-table cap are the server's.

**Three-state, like an automation file.** An absent `severity`, `description` or
`enabled` is a field the file does not manage: a create takes the server's
default (`warn`, empty, enabled) and an update never sends it. So a check
silenced in the app is not re-enabled by a push of a file that does not say
`enabled`, and that silence is not drift. A NEW entry that says
`"enabled": false` is created silenced in the one POST (the create takes
`enabled`), so it never runs live or alerts between a create and a silence.

**Not a `-- @check` header**, deliberately: a header lives inside the table's
`code`, so every check edit would be a SQL change — a new draft, a rebuild, a
commit, a version-history entry — for a thing that is ungoverned by design.

**When they are written — the live-matches rule.** Checks attach to the LIVE
table id (a check put on a draft would stay on the version snapshot the commit
leaves behind), so the checks pass — after the `.sql`, docs and metric passes,
over **every** checks file, not only the changed `.sql` files — writes a table's
checks only when the live row's SQL equals the file and you have no open draft
of it. Otherwise the file is `checks_pending_publish` and nothing is sent.
`publish` applies a table's checks right after its commit lands; if the publish
went to review instead, the next `push` after the approval lands them. A server
`409 table_not_built` (a live row that never built, so the expression cannot be
run) is pending too.

**One table's checks, in a load-bearing order:**

1. one `GET …/checks`;
2. **drift** — a check this folder owns that was changed in the app since the
   folder last verified it — refuses the table's checks before anything is
   written; `--force` overwrites. It is the push's one `--force`: the same flag
   that overwrites a table's SQL or a metric's definition changed on the server
   overwrites the checks too — there is no checks-only flag;
3. **orphans** — checks the folder created that no entry in the file resolves
   to — refuse the table's checks until `--prune`, which **silences** them
   (`enabled: false`; the CLI and the HTTP routes have no delete) *before* any create, so renaming a
   freshness watchdog (one enabled per table) or renaming at the per-table cap
   lands in one push. An orphan is decided **by row**: each entry's live row is
   found first (the check the folder owns under the entry's name, by id, then by
   name), and an owned row some entry resolves to is never an orphan — so a
   check renamed in the app and in the file is not silenced. Two entries that
   resolve to one row refuse the file. An orphan **changed in the app** since the
   folder last verified it is drift: `--prune` refuses it without `--force`. A
   **deleted checks file** is a file with no entries: every check it created that
   is still enabled is an orphan (a bare `push` visits the lock's record of it),
   except a check another checks file on the same table declares or owns — so
   renaming a table's stem with its checks file (and rebinding the table) hands
   the checks over. Its record is dropped once nothing it owns is still enabled;
4. **updates, then creates.** An update sends only the declared fields that
   differ — never an unchanged `severity` beside an edited expression, which
   would trip the admin gate on a `fail` check. An edited expression or
   interval is re-run by the server, and a check that cannot be evaluated is a
   400 and is NOT saved. A kind change is refused (and `status` lists it as
   `refused`, not pending): give the check a new name and `--prune` the old one.
   A lost create race (`409 name_taken`) re-reads the table and re-enters the
   diff for the check that won, as an adoption — never as one this folder
   created. A create that **timed out** is re-read too: a row saying exactly
   what the entry says is ours (`applied`, "created; verdict not returned"),
   anything else re-enters the diff. A name Go and Postgres fold differently
   (`İ`, `ß`) is not found by the re-read and ends `refused` with the server's
   `name_taken` message;
5. **verify, then record, per check.** One more read; a written check is
   recorded in `ronja.lock.json` (or `.ronja/state.json` on a legacy folder)
   only if the live row now says what the file says, otherwise `not_applied` —
   which is what catches an older server whose `PUT` accepts `{enabled,
   expression}` with a 200 and drops the expression.

A same-named check that pre-dates the file is **adopted** (`adopted`, or
`adopted_changed` when the push rewrote it — said apart, so overwriting a
colleague's chat-authored check is visible). A declared check that is silenced
on the table, with `enabled` undeclared, is `silenced`, never `unchanged`. Checks
on the table that are neither declared nor owned are left alone and listed.
Matching is by name, so **renaming a check in the file creates a new one** and
orphans the old; its history does not follow.

**A check deleted in chat comes back.** Ronja's agent can delete a table's
checks (`setTableChecks` with `remove`); the CLI cannot, and `--prune` still
only silences. A check the file still declares that a chat deleted is
re-created by the next `push`: its recorded id no longer resolves and no row
holds its name, so it is a create — a new id, no history — and, being an HTTP
write, a re-created check that is already failing notifies the admins. `--prune` silences a check
this folder manages; to remove one for good, take it out of the file, then ask
Ronja to remove it in a chat. The agent's tool description tells it to change
the file instead of deleting a check a file manages.

| outcome (`--json` `checks[]`) | means | exit |
|---|---|---|
| `applied` | created or updated; `changed` names the fields, `verdict` the run | 0 |
| `unchanged` / `adopted` | already matches (adopted: not created by this folder) | 0 |
| `adopted_changed` | a same-named check this folder did not create was rewritten | 0 |
| `silenced` | declared, silenced on the table, `enabled` undeclared | 0 |
| `checks_pending_publish` | the live table does not hold the file yet, or has never built (push its SQL and publish it — the checks are applied after its first build) | 0 |
| `drift` / `orphaned` | refused as above | non-zero |
| `refused` | 403, 400 (unevaluable, cap), a table the caller cannot read, or a write that got no answer (the file's result is then `unknown` to `sync apply`) | non-zero |
| `not_applied` | the write answered but the read-back disagrees | non-zero |
| `unsupported` | the server has no check-authoring route — a 405, or a 404 without a Ronja error body (an older server, or a proxy in front of one) — or its `PUT` is the older silence-only one (`400 enabled is required`, so a `--prune` still works and an edit does not); upgrade the server | non-zero |

A checks problem fails `push` and `publish` (exit non-zero) but blocks nothing
else they do: the SQL, docs and metric passes have already run, and a
`publish` whose commit landed keeps it. `ronja sync apply` goes one step
further and publishes a folder's SQL past a checks leg that did not apply,
reporting `checks_not_applied` — see [`sync apply`](#sync-apply--the-deploy).

A `fail` verdict is not a push failure: the check was stored and said something
true. **Over HTTP, a check that is failing when it is written notifies the
organization's admins like a background failure** (at most once per check per
day), unlike one authored in chat — a CI `ronja sync apply` creating an
already-failing check must not go unnoticed.

**Who can apply a folder with `fail` checks.** Creating, silencing or
downgrading a `fail` check needs an admin token (the store enforces it). A
maintainer's push lands the `warn` checks, reports each `fail` one `refused —
needs admin for a fail check, or no write access to this table` (the server does
not say which), and exits non-zero until an admin pushes; because recording is
per check, that retry covers only the refused checks.

`clone` writes each table's **enabled** checks — never an `enabled` key, since
silencing is the operator's — and records them as owned, so the push after it
is a no-op. Silenced checks are left out and read as unmanaged; a watchdog whose
interval is not a whole number of hours is skipped with a note. `discard` never
touches checks: they are never on a draft.

### Publishing, and why there is no `run`

`publish` decides its route **up front** from the two server facts that decide it
server-side — the feature's scope, read once for the run, and each draft's
`canDecide` (an admin, or a maintainer of the feature), read off the draft's own
GET — rather than by trying a commit and reading the rejection prose. Shared
feature + `canDecide` false → `request-review`, outcome `submitted_for_review`;
otherwise commit, outcome `published`. `--no-request-review` turns the review route into an error,
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
loop needs no `run` verb. A push, by contrast, syncs your DRAFT, which no table
reads, so it rebuilds nothing downstream (a sync of the LIVE table does cascade;
the HTTP guide's "When dependents rebuild" section has the whole contract). The
report prints a **folder-local, direct** count ("2 tables in this folder read
this one directly and will rebuild automatically") — never transitive and never
tenant-wide: the real cascade is both wider (it leaves the folder) and
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

A `.sql` (or `metrics/*.json`) file deleted from the folder is **not** a row this
loop deletes. It is reported on stderr and left alone — there is no delete verb
here on purpose, and quietly dropping a colleague's table because a merge removed
a file is exactly what a sync loop must not do. The note is rename detection (see
"Addresses: `moved` and `removed`"): it names the new file when there is one and
asks for a `moved` or `removed` entry, which is also how it stops being reported.

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
filename is the file's identity and, unless the file declares a `"name"`, the
name the automation is created under; everything that is not a `.json` file is
ignored, so the folder can sit inside an ordinary repository.

A file may declare `"name": "…"`, which manages the automation's name on the
name rule (see **Names are declared attributes**), with the filename as the
baseline when the lock records none. ⚠️ With **no** `"name"`, the name is
stamped at create and **never written again** — every push used to re-patch it
to the filename, which reverted any rename made in the web app.

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

### `prompt` belongs to an inline `agent` action too

A top-level `prompt` beside a `workflow` or a `saved_agent` action is refused for
the same reason: those kinds' parent prompt is pinned to a server-side sentinel —
stamped over the body at create, and dropped from the patch on every update — so
the declared value can never converge and `status` reports the same drift for
ever. Remove the key; a `saved_agent` action's per-run seed message is
`action.config.prompt`, a different field on a different object that the server
does read. This is a **load-time** refusal, so `ronja sync status` and
`ronja sync check` over a tree containing such a file refuse to load it rather
than reporting green-with-drift.

**Leaving `action` out does not leave this rule out either.** A file declaring
`prompt` and no `action`, against an automation that already runs a workflow or a
saved Agent, reaches the same pinned sentinel by the second door — same shape as
the `references` rule above. That one is refused against the ROW, so `push`
refuses the file and `status` reports it as a problem, and neither writes
anything.

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
first unattended run — which is the failure this loop exists to prevent.

Every reference kind has an alias kind: `model` is declared `table`, and
`workflow`, `note`, `secret`, `mailbox`, `dataapp`, `mcpserver` and `feature`
are declared under their own names (`ronja bind` searches all of them). A
`feature` reference to the automation's OWN feature is written `"@feature"`,
which the CLI resolves to the selected stack's `featureID` before sending — so
the same file references whichever feature each stack deploys to. A stack with
no `featureID` refuses it rather than send the token.

```json
"references": [
  {"kind": "dataapp",   "resourceID": "sales_dashboard"},
  {"kind": "mcpserver", "resourceID": "crm"},
  {"kind": "feature",   "resourceID": "@feature"}
]
```

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
| the row moved since the recorded `updatedAt` | there is no version and no compare-and-set here, so this anchor is the only drift guard there is. `--force` overwrites. ⚠️ A row that already holds **every field the file declares** is not drift, however far `updatedAt` moved: it is reported `unchanged` and the anchor is re-taken — the likeliest history is this folder's own write whose anchor was lost |
| a declared `"name"` the row holds neither as declared nor as the recorded name (or, with none recorded, the filename) | it was renamed somewhere else; `--force` renames it back |
| a file turns an automation back **on** that somebody paused after that anchor | a human pause is clearable server-side, so without this the automation is live again with nobody in the loop |
| a change to `triggerKind`, `referenceBased` or `eventName` | the update body carries **none of the three**, so sending the change answers `200` for a write that never happened |
| an `action` with no `kind` | the server reads a missing kind as the back-compat inline `agent` shape, so the action it stores is not the one the file describes |
| an `agent` action declared against a row that runs a `workflow` or a `saved_agent` | the update route writes a child action row for those two kinds only, so the switch back answers `200`, changes nothing, and leaves `action.kind` reporting drift for ever |
| a file this folder is bound to is gone, with no `moved` or `removed` entry covering it | those automations are LIVE, and a bad rebase must not stop one. A `moved` entry keeps the automation under the new file; a `removed` entry stops managing it and leaves it running. `--prune` is the explicit opt-in to DELETE, and it obeys the drift anchor too: a row somebody has been editing is the last one to delete because a file went missing |
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
knowing: because the binding is keyed by **path**, renaming a file without a
`moved` entry is an orphan plus a create rather than a rename — the same property
`ronja pipeline` has. A `moved` entry re-keys the binding and keeps the
automation; a `removed` entry stops managing it without deleting or pausing it
(see "Addresses: `moved` and `removed`").

## Module folders (`ronja module`)

`ronja module`, aliased `mod`, is the sixth folder loop.

A **module** is a versioned, named Python package that workflows *import* instead
of copying helper files between folders. It exists because the copying already
failed in production: three EMAB workflows shared a `tracker.py` kept in sync by
a push script's `cp`, and cloning them today yields three *different* files —
29,351 / 28,501 / 37,108 bytes, three distinct sha256s, with a function present
in one copy and missing from another. Nothing server-side could see that three
folders claimed the same file.

The loop is `ronja wf` with the workflow-only half removed, and that is
deliberate rather than convenient: it shares `wfdir`, the three-layer drift
guard, the committed head anchor, `checkPushable`, `confirm` and the whole
report vocabulary. `ronja mod` is an alias.

### The folder

```
emab-tracker/
  ronja.json          kind: "module", the package name, the title, the bindings
  ronja.lock.json     what deploys recorded: the module id, the head version
  .ronja/state.json   this checkout's sync baseline (git-ignored, never committed)
  __init__.py         what makes it an importable package
  tracker.py
  nesting.py
```

Two keys are module-specific:

- **`name`** is the Python package consumers import — the identifier that appears
  after `import`. It has to be a valid Python identifier, is unique among the
  live modules in the organization, and is refused when it shadows something the
  runtime already provides (`json`, `requests`, `tools`, …), because user modules
  are *appended* to `sys.path` and a shadowing name would be silently
  unimportable. A plain string with the same three-state rule `title` follows:
  `""` means "this folder does not manage the name", never "clear it" — an empty
  name is refused server-side anyway, since a module without one is not
  importable.
- **no `entrypoint`.** A module is a package, not a program, and the manifest
  carries no key for a concept the primitive does not have.

**`.py` only.** Everything else in the folder is skipped — silently as far as the
module is concerned, but *reported* on stderr, because a file the author believes
is part of the module and the CLI quietly ignores is the worst kind of surprise.
The restriction is the server's (V1): the runtime's module-name grammar mangles a
non-`.py` path, writing `data.csv` as `data/csv.py`. It is threaded through
`wfdir.Kind.SyncExt`, so `Enumerate` and `CheckLocalPaths` answer identically —
the rule that stops a baseline claiming a path no later walk returns.
A `.py` file whose file or folder names are not letters, digits and underscores,
or start with a digit (`my-helpers.py`, `calc.v2.py`, a digit-led folder), is
refused, naming the file and a spelling that loads (`my_helpers.py`): the runtime
stops on a hyphen or a leading digit before any code runs, so one would break
every workflow importing the module. `validate` reports every such
file and `push` refuses the first one it would write, before anything is written —
the server refuses one per file write, so leaving it to the server would stop a
push part-way. A file already on the server with the same content is not
written, so a module committed before the rule still takes edits to its other files. The
rule is `markers.IsRunModulePath`, a copy of the server's
`pymarkers.IsRunModulePath` that `cli/internal/markers`' parity test pins.

**`__init__.py` is not optional.** The server seeds one inside the create
transaction, and `module init` seeds one locally, and the two are not redundant:
without the local copy the first push would create the module, list back the
server's seed, find no local file at that path and try to *delete* it — which the
server refuses, stopping the very first push of every new folder. `push` and
`validate` both refuse a folder that is missing it, by name, before anything is
written.

### The verbs

| verb | what it does |
| --- | --- |
| `init <name> --feature <id>` | write the folder. Creates nothing server-side |
| `clone <module-id> [dir]` | copy an existing module down. Read-only — no draft is opened |
| `status` | local changes, remote state, drift |
| `validate` | check the folder **locally**; nothing is sent |
| `push` | sync the folder into the module's draft |
| `publish` | commit the draft, or — in a shared feature, for anyone but an admin or a maintainer of it — submit it for review. A pending **proposal** is already with the admins, so it is reported rather than sent again |
| `discard` | throw the draft away (`--delete-module` for one never published) |

⚠️ **`publish` on a pending proposal calls nothing.** A shared-feature create
files the proposal at `push` time; `POST draft/:id/request-review` is the
governance engine's DRAFT transition and answers `row is not a draft` for a
proposed row, and the commit path answers the same and then falls into
request-review for a second copy of it. So the routing short-circuits on
`LifecycleProposed` before either, reports `submitted_for_review` with a detail
saying the create already filed it, and exits zero. Approval is an admin's own
decision and deliberately has no CLI verb.

**There is no `run` and no `test`, and the absence is the primitive.** A module
has no entrypoint, no parameters and no run identity — there is nothing to run.
Testing a change means running a *consumer*: edit the module draft, then
`ronja wf test` in a workflow folder that imports it, which resolves the runner's
own open module draft on purpose. A `module test` would have to invent a workflow
to run the module in, and the invented one is the thing that would be wrong.
Discovery is absent for the usual reason: no `module list`, no `module delete`.

### `validate` is local-only

`ronja wf validate` exists to resolve `{{ ref }}` / `{{ secret }}` / `{{ agent }}`
markers against the author's access, which genuinely requires the server. **A
module refuses every Ronja marker at save** — it receives its database handles,
data and configuration as function arguments — so there is nothing to resolve and
no server-side pass to run. What is checkable is checked here: the path grammar,
the `.py` rule, the file-name rule, the size and NUL
limits, the case-collision rule, the package name's identifier shape, the
presence of `__init__.py`, and **the marker rule
itself**, which needs no server precisely because it resolves nothing — a lexical
scan naming the file and the marker family, ported from the server's own gate
(`pymarkers.FirstRonjaMarkerVerb`) with the same dead-text rule: a marker in a
docstring or a `#` comment is documentation, one in a live string literal is not.

That last check is the one the migration path walks into. Moving `tracker.py` out
of a workflow folder and into a module means moving code that may well have
carried a `{{ ref }}` there. `push` runs the same scan among its **local guards**,
before the first server write, so the refusal costs a folder edit rather than a
half-written draft: the server refuses a marker one file at a time, so a push that
let the folder through would create the module, write files until it reached the
offending one, and stop. `validate` lists every file that carries one; `push`
names the first and refuses.

The report says so on every run, clean or not, because "clean" would otherwise be
read as "the push will work". The two things it cannot see are whether the
package name is free and whether you may write to the feature; `push` reports
both the moment it asks.

### One draft, not one per author

This is the one place the module loop's *semantics* differ from the workflow
loop's rather than merely being smaller. A workflow draft is per-user; a **module
has exactly one draft**, because a draft is a snapshot of the whole file set and
two would each silently drop the other's edits at commit.

Three consequences, all of them visible in the output:

- `clone` may clone a **colleague's** open draft, and says so.
- `status` reports the draft's `drafterUserID`, because whose it is decides
  whether `discard` is tidying up or destroying somebody's afternoon.
- `discard` names the drafter in its confirmation prompt. "Discard your draft"
  would be a false statement half the time, and the half where it is false is the
  half that costs somebody their work.

A push into that shared draft is not a free-for-all, but **authority is the guard
here, not the compare-and-swap**. Only the draft's own drafter and an admin may
write to it at all; everyone else is refused, and refused with a **404** rather
than a 403, because naming whose draft it is would disclose a row they may not
see. `module push` translates that 404 rather than passing it on — a 404 on a
path-addressed route otherwise reads as "no such file" — and names the drafter
`status` already reported.

So the CAS matters admin-to-admin, and between two checkouts of the same person's
folder: there it is what refuses a file the other side changed since your last
sync instead of overwriting it.

### A first push creates *and* checks out

Where a module lands at create depends on the feature's scope, and the two
answers are not symmetric:

| feature | the create returns | writable? |
| --- | --- | --- |
| private | a **live** module | no — live rows are immutable |
| shared | a **proposal** an admin must approve | yes, to its proposer |

`rmodule.Add` stamps the drafter fields only on the shared branch, so the
lifecycle trigger classifies a private-feature module as `live` — and
`requireDraft` refuses a file write on it ("file mutations are only allowed on
draft or proposed modules"). A first push into a private feature therefore
creates the module, records the binding and the head anchor, and then **checks a
draft out of it** before writing a byte. A shared create is skipped: a proposal
is already writable, and checkout takes a live parent.

The binding is still recorded *before* the checkout, for the reason it is
recorded before any file write: the module exists from the create onwards, and a
push that died before saving the manifest would leave an orphan the next push
could not find and would try to create again — where "again" is a 409 on the
package name, for ever.

One consequence worth stating: in a private feature the package-name collision
lands at the **create**, on the first push, because the row is born live and the
uniqueness index fires at INSERT. Rename it in `ronja.json` and push again.

### The drift guard is the workflow loop's, unchanged

All three layers carry over, sharing `driftVerdict`, `filePreconditions`,
`headAgreement` and `anchorAfterPush` with `ronja wf`:

1. **Local baseline check** — remote versus `.ronja/state.json`, refusing a push
   that would overwrite what moved on the server. Metadata counts: the package
   name and the title are pushed as blindly as the code, so a colleague renaming
   the package is drift, not a silent revert.
2. **Per-file `baseSha256` CAS** on every PUT and DELETE, checked *inside* the
   write transaction under the module's row lock. Armed exactly where the
   baseline vouched, suppressed exactly where it was bypassed (`--force`, and a
   module this push created).
3. **Publish CAS on the committed head**, anchored in `ronja.lock.json` so a
   fresh CI checkout with no baseline can push under preconditions rather than
   past them. `--overwrite-remote` reads the head from `GET :id/versions` and
   confirms it explicitly — never scraped out of the 409's prose — and `--force`
   past a version this folder never saw **clears** the anchor rather than
   advancing onto a publish the repository does not contain.

Two small differences, both in the module's favour:

- The anchor after a publish comes **straight off the commit response**
  (`headVersionID` on `rmodule.CommitOutcome`) rather than from a second read.
- `cloneAnchor` is shared with the workflow loop verbatim, because `rmodule`
  stamps `base_version_id` the way `rworkflow` does. The `rdataapp` divergence
  that forced a second anchor function does not exist here.

### Metadata is patched on TWO rows

`PUT /module/:id` takes the live module and refuses a draft id — a module's name
is claimed against the live-uniqueness index, and a draft has no claim to make.
So where `wf push` patches one row (the draft, which the commit then publishes),
`module push` has to patch two:

- the **identity row** through `PUT /module/:id`, so the change is visible now
  and the drift guard and the baseline have something to compare against;
- the **open draft** through `POST /module/:id/checkout`, whose body is an
  `rmodule.DraftEditInput`, so the commit publishes the intended metadata.

The second one is not tidiness. `rmodule.commitDraftInTx` overwrites the parent's
name, title and description with the **draft's** copies — the copies taken when
the draft was checked out — so a title patched onto the live row alone survives
exactly until somebody publishes, and then reverts to a value nobody typed. The
two patches are computed independently (each against the row it is sent to), so a
draft left stale by a rename made in the browser is reconciled on the next push
whether or not the live row also needs one.

The checkout that *resolves* the row a push writes to still sends an **empty**
body, and runs before the drift guard: seeding a draft with a manifest title
nothing has compared against the live row yet would apply an edit the guard is
about to refuse. Metadata is written in one place, after the guard.

One consequence is stated rather than hidden: while a module is still
**unpublished** (a parentless draft, or a proposal in a shared feature) there is
no live row to patch and no draft *of* one, so its metadata cannot be patched at
all. The create carried the manifest's values; a manifest edited since reaches
the row at the first publish. `push` reports that as `metadataDeferred` instead
of sending a request that would be refused.

### Publishing says what it did not change

A shared-feature module's commit takes an admin or a maintainer of the feature —
a module edit fans out to every workflow that imports it, so it gets at least the
review a single workflow edit gets. `publish` attempts the commit and, when the
server refuses it with a 400, submits the draft for review and *says so*.
Reporting "published" there is the kind of lie people discover a week later,
after republishing three consumers against a version that does not exist.

The report closes with the blast radius and the rule that makes it mean
something:

```
  3 workflow(s) pin this module — each picks up the new version when it is next
  republished. Publishing a module changes nothing for a consumer on its own.
```

The count is **the server's**, carried on the commit response
(`dependentWorkflowCount`). It is not computed client-side, and could not honestly
be: it is the delete gate's own reverse lookup (`governance.FindDependents` over
`workflows.module_ids`), read through the RLS-scoped pool, so it counts every
workflow in the organization that imports this module — **the same set the delete
gate refuses on**, and one the caller may well not be able to enumerate. (That
gate discloses their names when it refuses, which is what makes counting them
here the right number rather than a leak.) It is capped at 100, like every other
reverse lookup. It is printed only on
a publish that actually landed, and only when positive — `0` means "none, or the
count could not be taken", and there is nothing to say about either.

### Consumers never hold module files

A workflow that imports a module carries only the marker, in import position:

```python
from {{ module('module-abc123') }} import tracker   # → from emab_tracker import tracker
```

`ronja wf clone` of a consumer is exactly as it was; module sources never appear
in a workflow folder. That is the whole point — one copy, one id, one version
history, and a graph that can answer "what does editing this affect".

### `sync` covers module folders — an upgrade effect on classic trees

Builds before this one left a module folder out of the tree commands: `sync
apply` over a tree holding a module folder with no folder id exited 2
`kind_not_covered` and wrote nothing (and `sync check` answered `unknown` for
it). This build covers modules in every tree command, so the same tree,
unchanged, behaves differently after a CLI upgrade alone: `sync check` gives the
module folder a real verdict, an unchanged one applies as `applied`, and **`sync apply` pushes and
publishes it** when its files differ from the live module — a classic folder
(no `"id"`) included. That is a deliberate exception to "a classic folder
behaves exactly as before", kept because a tree that cannot deploy one of
its kinds is the bug. What it means in practice: a CI job that ran `sync apply`
over a tree with an edited module folder used to fail and now publishes the
module. Consumers are protected by pinning — a workflow runs the module version
it was last published with, and picks a new one up only when it is saved and
published again — so the publish changes no running workflow by itself. A tree
that must not publish its modules from CI keeps them out of the directory
`sync apply --dir` walks. `sync status` gives the folder a real verdict too
(`clean` exit 0, `drifted` exit 1, or a refusal), and the `kind_not_covered`
reason no longer appears in any tree command's output — a script that matched
on it sees the module folder's own verdict instead.

### Other upgrade effects on classic folders

Four more changes reach a folder with no `"id"`, on a CLI upgrade alone:

- **Re-pointing a binding is a local change the push deploys** (a bug fix).
  A workflow or data-app folder that edited only `stacks.<s>.bind` in
  ronja.json was refused by its own push ("the draft changed on the server
  since your last sync") and `status` listed the server's untouched file as
  drift: the remote was de-aliased through the NEW binding, where its old id no
  longer turned back into the alias the baseline was hashed in. The local
  baseline (`.ronja/state.json`) now records the aliases it was taken under
  (`aliases`, with `aliasesRecorded: true` so a baseline that resolved none
  says so rather than reading as unrecorded), and the drift guard, its
  per-file preconditions (`onTheWire`, which accepts either rendering) and
  `status`'s drift line render the remote through them (`baselineCodec`).
  Until the push deploys it, the rebind is a LOCAL change: `wf status` and
  `app status` list it under Local changes (`rebound` in --json: alias, kind,
  from, to — for each binding a file uses), and `sync status` reports the
  folder `drifted` ("binding `orders` re-pointed from table-orders to
  table-orders-2", among the changes here never deployed), never `clean`
  while the server still holds the old id. A binding REMOVED counts too: an
  output then written by its name has resolved to nothing as its true state.
  Only an address or an unbound output whose lookup was owed and got no
  answer (`lookupMissed`: a recorded address error, or a signed-out run
  `resolveOwnOutputs` would have asked) is left out. A baseline written by an
  older CLI records none and compares as before: a binding re-pointed before
  this CLI has pushed or pulled the folder once still refuses — and when the
  refused files differ from the baseline only by that binding's ids
  (`reboundUnrecorded`), the refusal says so and names the way past it
  (`reboundWayPast`): put the binding back, push (or pull, for an adopted
  folder), then re-point it. `wf status`, `app status` and `sync status` say
  the same (`unrecordedRebind`): those files leave the drift — `pull` would
  write the old id into the folder and lose the rebind — and the rebind is
  listed with `refused` naming the way past, which `sync status` reports as
  something a push would refuse. `sync apply --dry-run` takes the push's
  refusal (below).

- **`<kind> init --stack` creates an ADOPTED folder.** It mints the folder id
  (`mintFolderIDAtInit`) and stamps `"formatVersion": 6`, where earlier builds
  wrote a classic stack folder. The first push therefore needs an instance that
  serves `/managed`, and a token with the `structure:read` and
  `structure:write` scopes beside the kind's own — an existing scoped CI token
  that carried only `automation:write` is refused by name — and a teammate on an
  older CLI cannot open the folder. `init` without `--stack` is unchanged, and
  `ronja sync unmanage` turns an adopted folder back into a classic one.
- **`{{ write('x') }}` of an alias declared only for READING is refused**
  (`misusedAliases`), on push, validate, `sync check` and `sync apply`. Earlier
  builds sent the alias as the name of a new table the run created in the
  stack's feature — the declared row was never written, and the push was green.
  The remedy is `"write": true` or `"output": true` on the dependency, which
  stamps format 7. A folder that read its own name-form output through a
  same-named read alias was correct before and is refused now; declaring the
  alias `"output": true` is the fix.
- **A bare table name in a select parameter's `optionsQuery` is refused**
  (`unresolvedTableNames`, which now reads the `optionsQuery` beside the files).
  It takes away nothing that pushed: the server binds an optionsQuery's tables
  by id and refused the name at save as an unresolved table.

## SQL escapes (`sqlSemantics`)

Ronja used to hand your SQL to DuckDB through a non-raw Python string, so
Python decoded the backslash escapes first: `'\\s'` on disk arrived as `'\s'`,
and `'\t'` arrived as a real tab.
Artifacts created from now on run **raw** — the bytes in the file are the bytes
DuckDB gets — and everything that already existed stays **legacy**, because
silently re-reading live SQL under new rules would change answers nobody asked
to change.

**Nothing in a folder declares this, and no push sends it.** The server moves a
legacy artifact to raw on its own the first time a write changes its code — a
push whose files or `.sql` differ from what the row holds. A push that changes
only metadata (a title, an allowlist, a runtime, a `tables/<name>.json` docs
sidecar) moves nothing, and neither does one that sends back the bytes already
stored. A `-- @table`/`-- @column` header is different: it lives inside the
`.sql` file's code, so editing a note there DOES move the table. For a
table, workflow, app or module the move lands on the draft the push writes (a
draft records the rules of the bytes it was checked out with, so it moves only
when its own code changes) and reaches the live artifact at `publish`, so a module keeps its old meaning for
every workflow that imports it until `module publish`. Restoring a version
brings back the escape handling that version was written under.

The server reports `"sqlSemantics": "legacy"` on a legacy row (for a table, only
one whose code is DuckDB SQL — never a metric or a table with no SQL) and omits the
key otherwise, and the CLI only reads it: `status` prints one line per legacy
artifact — `<name>: legacy SQL escapes — publishing a change to its code moves it to
the current behaviour` — on stderr (and as `sqlSemanticsNotice` in `--json`), so
the author can rewrite any backslash literal in the same edit. `ronja query` prints any
authoring advice the server returns about escapes on stderr, beside the
reporting timezone.

**Accepted residual.** Cloning a **legacy** artifact and pushing it as a
CREATE — into a new feature, or into another organization through `ronja bind` —
produces an artifact that is born **raw**, although its text was authored under
the old decoding. Nothing in the create path can say otherwise: a create makes a
new artifact, and new artifacts are raw. No push result says so — the author
learns it from `ronja query`'s advice when the SQL is run — and the population this
can reach today is effectively our own folders, so it is a thing to know rather than
a thing to guard.

## Whole-tree checks (`ronja sync`)

A repository holds many folders. The loops each answer "is this folder in
step with the organization?" for themselves, and answering it forty times by
hand is how a broken edge sits in a repo for a month. `ronja sync` asks two
questions of a whole tree at once, and answers a third:

```bash
ronja sync status                      # is the committed content what the org holds?
ronja sync check                       # does every reference that content makes resolve?
ronja sync apply                       # push AND publish all of it
ronja sync status --dir apps --stack prod --json
```

Two more move a tree between the two patterns — see "Server-side state:
adopted folders":

```bash
ronja sync adopt --stack prod [dir]     # give folders an id and claim their rows on the server
ronja sync unmanage --stack prod [dir]  # release the claims; back to running off the lock
```

All three walk **down** from `--dir` for every `ronja.json` — workflow, data-app,
pipeline, automation and module folders alike.

**Module folders are covered like the other draft kinds.** `status` compares the
folder with the module row (files, package name and title, trimmed the way the
push compares them) through `moduleStatusReportAt`, the report `ronja module
status` prints; a fresh clone has only the committed `headVersionID` pointer, as a
workflow does. `check` makes no request: a module's files are marker-free, so a
module folder makes no references — a marker in one is a finding, and a
`dependencies` block is a warning (nothing ever fills it; declare the dependency
in the consumer workflow). `apply` pushes and publishes it like a workflow (one
creation unit, gated by `--allow-create`), and `adopt` and `unmanage` claim and
release its row (resource kind `module`, address `""`).

`status` computes each folder's report
with the same function `ronja wf status` / `ronja app status` /
`ronja pipeline status` / `ronja automation status` / `ronja module status` run, so
the two surfaces cannot drift about what a folder's state IS — but the VERDICT it
folds that report into is the tree's own, and it reads signals the per-folder
commands deliberately leave out of theirs (see below). There is no repo-level
file to declare: a read-only command does not need one, and a committed file
format is the most expensive thing to get wrong.

A pipeline folder's verdict covers every carrier it holds — `.sql` files, docs
sidecars, metrics and `checks/` files. A health check the organization does not
hold yet (to create or update, waiting on a publish, or an orphan a push would
silence) is a local change; one changed in the app is drift; a checks file that
could not be judged is unknown.

`status` and `check` are read-only, and enforced: `TestSyncStatusWritesNothing`
and `TestSyncCheckWritesNothing` snapshot a fixture tree before and after and assert
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
| module folders | `automation:read` | — (a module makes no references) |
| data-app folders | `analytics:read` | — (the declared `access` ids are read by kind, below) |
| pipeline folders | `data:read` | `data:read` |
| any `{{ agent }}` reference | — | `agents:read` |
| any `{{ secret }}` reference, or an app granting one | — | `secrets:read` |
| any `{{ ref }}` / table grant | — | `data:read` |
| any `{{ workflow }}` reference | — | `automation:read` |
| any ADOPTED folder (one with an `"id"`) | `structure:read` (`GET /managed`) | `structure:read` |

`sync apply` needs each row kind's write scope (`automation:write` for workflow,
module and automation folders, `analytics:write` for apps, `data:write` for
pipelines). On an adopted folder it also needs `structure:write`: a create that
claims its address, a lazy claim, a move, an unmanage and an anchor write are
all writes of repository state, on top of each row's own kind scope.

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
| 2 | `unknown` | could not tell — a skipped folder, an unreadable manifest, a dead credential, or an **empty walk**; under `apply`, also a write the instance never answered (`api.Unanswered` / a timeout) and the folders an interrupt never reached |

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
edit is a yes. **The tree verdict changed; the per-folder commands did not.**

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
the answer is **not the same for every kind** and the difference is
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
| module | the same `HeadVersionID` pointer | only: has somebody published since |
| automation | `LockAutomation.AutomationID` **and** the row's `updatedAt` | **everything.** There is no per-user half at all — see below |

So **`ronja sync status` cannot tell, on a fresh clone, whether a workflow's, a
data app's or a module's committed files have been deployed.** It reports the folder `unknown`
rather than clean (there is no baseline, and the no-baseline branch answers
first), so it never *claims* they are in step — but it cannot turn that into the
`drifted` a pipeline folder gets.

**That is a consequence of a deliberate decision, and the decision is right.**
Those kinds sync their files into a **draft**, so any per-file
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

**An ADOPTED workflow, data-app or module folder is answered by the live row**
(`answeredByLive`): where the no-baseline branch would answer `unknown`,
`sync status` asks the comparison `sync apply --dry-run` asks of it —
`liveHoldsFolder`, reads only, through `openFolderForStatusAt` — and reports
`clean` when the live row holds the folder ("`sync apply` changes nothing for
it") and `drifted` when it does not. A classic folder, a read that fails and a
folder bound to no row keep `unknown`. That is not a per-file baseline: it
cannot say whether a difference was made here or in Ronja, only that a deploy
would change something, and the detail names `sync apply --dry-run` and
`<kind> pull`.

The practical reading for a CI gate: on a fresh clone, a **pipeline** or
**automation** folder is fully vouched for, and so is an adopted folder of the
other three kinds; a classic **workflow**, **data-app** or
**module** folder is vouched for against the published version only. Run `ronja wf
push` / `ronja app push` / `ronja module push` to find out whether their files differ — those loops read the
row's files directly and do not depend on a baseline.

### Two things `sync status` does not claim

A classic **workflow, data-app or module** folder that is bound and has no baseline for the
selected stack is `unknown`, not clean — the fresh-`git clone` case, where
`.ronja/` is correctly absent and its files could not be compared against
anything. (A **pipeline** folder that names a stack is the exception, and it is
the point of the section above: the committed lock gives it both legs, so a fresh
clone of one gets a real answer rather than `unknown`. A pipeline table with no
lock entry is still `unknown`, and `ronja pipeline status` deliberately exits
*zero* there — right for one folder run by its author, wrong for a tree gate.)
And `sync status` compares the folder's **content and its declarations**: a change made in Ronja to something the folder does not describe
at all is not drift here.

**An open draft of your own is a note, not drift.** A workflow's, data app's or
module's status measures the folder against the row a classic push writes to —
your own open draft, when there is one — so a pull taken with an unrelated draft
open (pull leaves the baseline, which describes that draft, alone) read as "never
been deployed" against the draft while live held the folder and `sync apply` sent
nothing. The tree verdict asks what apply would do: an ADOPTED folder (a
classic one keeps its verdict) that `drifted` against the caller's draft is re-asked the apply's own first question
(`heldByLive` → `liveHoldsFolder`), and when the live row holds it exactly it is
`clean`, with a detail naming the draft and what `status` / `push` / `discard`
do with it. A folder live does not hold stays `drifted`; a read that fails leaves
the verdict as it was. ⚠️ The converse is unchanged and still open: a folder
whose content sits only in an open draft (pushed, not published) reads `clean`
against that draft though `sync apply` would publish it.

`ronja wf status` and `ronja app status` still exit 0 unconditionally, exactly
as they always have. The verdict helper computes their answer for `sync` without
touching their exit codes; changing those is a scripting-visible break and is a
separate decision.

#### And a folder that has never been deployed is not clean either

⚠️ **This was a false green, on every kind, and it is the one worth
remembering.** Each loop has exactly one `NotCheckedReason` that is a
*conclusion* rather than a failure to look — `reasonNothingBound` /
`reasonNoWorkflowYet` / `reasonNoAppYet` / `reasonNoModuleYet`: the folder is bound to this very
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

This lives in `neverDeployedVerdict` and is reached from every verdict
function. Note what it is **not** about: an *unbound* folder was already
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

**Module folders make no references.** Their files are marker-free, so `check`
sends no request for one: a marker found in a module file is a finding (the
module push refuses it), and a `dependencies` block is a warning rather than an
unbound edge.

**Address dependencies are edges too.** Each one that resolved is checked like
any id (its row must still resolve), and one that did not is `unresolved` with
its reason — `address_unresolved`, `address_stack_mismatch`,
`address_not_adopted`, `address_folder_unreadable` or
`address_outside_repository` — and the edge carries
`from` and `address`. See "Dependencies by address" under `ronja bind`.

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

### `sync apply` — the deploy

`ronja sync apply` runs each folder's own **push and then its publish**, in
dependency order (below) — the report's `order` says `"dependency"`, where
builds before this one said `"path"`. Two more `--json` fields arrived with it,
both ADDITIVE, and a consumer that compares output exactly sees them on a
classic folder (no `"id"`) too: `sync apply --json` carries `sequence`, the
folders' paths in the order they ran (`folders` stays in path order); and a
workflow push's `--json` (`wf push`, classic included) carries
`bindings.workflowIDs`, the saved workflows the pushed code runs as validate
resolved them, `null` when it runs none. A `--dry-run` may also list a unit's
`action` as `unchanged` (see `--dry-run` below). Push alone stages a draft for four of the five kinds (workflow, data app,
pipeline, module) — every push report ends `Next: ronja … publish` — so an apply
that stopped there would be a repository of unpublished drafts reported as a
deploy. ⚠️ The automation kind has no draft at all: the automation loop writes production directly, so "apply
publishes" must not be read as "everything goes through a reviewable draft".

**It adds no writer.** Every byte it sends is sent by a loop that already exists,
under that loop's own guards. What it adds is refusal — and the local half of
every refusal comes from `sync_decision.go`, which is also what `--dry-run`
prints, so the preview and the run cannot disagree about what apply will attempt.

**Update-only, and that is the design rather than a limitation.** Apply refuses:

| refusal | why |
|---|---|
| `would_create` | nothing is bound to this unit — a `.sql` file, a `metrics/*.json` file, an automation `.json`, or a draft-kind folder — so a push would MAKE a row. There is no delete verb to undo one (cascade and purge are `HumanOnly`), and a CI runner that discards its checkout loses the created ids in `ronja.lock.json` and creates again next run — unless the folder is ADOPTED, whose ids the server keeps (see "Server-side state: adopted folders"), so a lost lock is never a create there |
| `no_drift_anchor` | an automation the folder is bound to and has never recorded agreeing with. An empty `LockAutomation.UpdatedAt` disarms **both** guards at once, so the write would overwrite whatever moved AND flip `enabled` false→true on a row somebody paused during an incident |
| `manifest_rewrite` | acting here would rewrite a committed `ronja.json` — `adoptStack` adopting a stranded lock stack, or `Manifest.Record` dropping an `instances[]` entry superseded by the **named** stack. Migration stays a per-folder act somebody watched |
| `orphaned_file` | rename detection: a table, metric or automation this stack binds whose file is gone, with no `moved` or `removed` entry covering it. Apply never infers a rename (which would be a create plus an orphan) or a delete from a missing file; the single-folder pipeline push only NOTES it |
| `address_unresolved`, `address_stack_mismatch`, `address_not_adopted`, `address_folder_unreadable`, `address_not_written` | a dependency declared by address did not resolve for this stack — see "Dependencies by address" under `ronja bind`. Under `--allow-create` an address whose row the same run deploys is deferred instead (below) |
| `writes_outside_target` | an ADOPTED folder would write outside its stack's feature through something no write source of that stack binds — a docs sidecar's table prose included — see "The write gate". At plan time for a workflow or app the run would create (so a refused folder leaves no row behind), for a pipeline's docs sidecars, and for every folder in a `--dry-run`; inside the push for every write |
| `no_write_targets` | an ADOPTED workflow folder's validate came back with no `writeTargets` at all: the instance is older than this CLI, so nothing can say where the code writes, and the gate refuses rather than admit it — upgrade the instance, use a matching `ronja`, or `sync unmanage` the stack. Its own reason so a script does not read "writes another feature" for "instance too old" |
| `runs_outside_target` | an ADOPTED folder would run a workflow or saved agent outside its stack's feature that no `"run": true` dependency of that stack binds — see "The write gate" → run edges. Same timing as `writes_outside_target` |
| `no_feature` | an ADOPTED folder whose selected stack names no `featureID`: it would deploy unclaimed and ungated. Add the feature, or mark the stack `"unmanaged": true` |
| `cyclic_dependency` | a pipeline folder's files read each other in a loop (`topoOrder`), or two pipeline FOLDERS read each other's tables by address — neither has an order to build in |
| `module_marker`, `no_module_name` | a module folder whose file carries a marker, or whose manifest names no package; a module with no `__init__.py` is `missing_entrypoint` |
| `address_conflict` | the folder's `moved` / `removed` cannot be applied to this stack: an old file still on disk, a removed file still on disk, both ends of a move already bound, a table moved without its `checks/<stem>.json`, or `moved` below format 5. A folder that could not be READ while evaluating them is `unreadable` instead |

**Dependency order, and a fresh stack in one apply** (`sync_apply_order.go`).
Every folder is PLANNED first; then the folders run in this order,
path order within each group, and the report names it (`"order":
"dependency"`, the folders in `sequence` as they ran; `folders` stays in path
order so two runs diff line for line):

1. **modules** — they reference nothing;
2. **pipelines, in build order.** A table one pipeline folder reads by address
   from another (`folder.crossFolderTableReads`, and a deferred one) is a build
   edge: a draft builds against its inputs, so the input's folder runs first.
   Ties break by path; a cycle of such reads refuses every folder on it
   `cyclic_dependency` before anything is written;
3. **pass one: every workflow another workflow of the run names by address
   that does not exist yet is CREATED**, with its claim and nothing else
   (`createWorkflowFirst` — no files, no parameters, which may read a table by an
   address still pending). A `{{ workflow }}` marker then always names a row that
   exists, so two workflows that run each other need no order at all. Which rows
   it makes is decided to a fixed point (`passOne`): only a workflow whose own
   addresses all resolve by now — the tables and modules it reads were made — and
   that a workflow in the same state names. One waiting on a row that never
   appeared (a pipeline that failed) is not made early, and neither is a chain
   onto it; every workflow nothing names creates its row in its own push, which
   validates first — so a folder its push refuses leaves no empty row behind;
4. **pass two: each workflow's code**, pushed and published as usual — the push
   finds the row pass one made as the CLI's own half-finished first push
   (`isUntouchedFirstPush`) and goes on from there;
5. **data apps**, then **automations** — nothing a pipeline or a workflow writes
   names either, and everything they name exists by now.

Just before a folder whose address dependencies were deferred runs, they are
resolved in memory to the rows the run has made (see "Dependencies by address").
That is what stands a FRESH stack up in one `sync apply --allow-create`: no
`ronja bind` of the rows the repository manages, no commit, no second apply. A
failure part way is safe — every create carries its claim, so the next
apply finds the rows through the server's state and writes them instead of
creating second copies (`TestFreshStackCrashBetweenPassesThenRerun`). Pass one
runs only for adopted folders; a classic folder's push creates its row as it
always has.

⚠️ **A plain legacy `instances[]` folder is NOT a `manifest_rewrite`, and deploys.**
`Manifest.Record` takes its `!sel.Named()` branch for an unnamed selection —
`SetBinding` in place, returning *before* the `instances[]` drop — so a push there
migrates nothing. Refusing it locked every pre-stacks repository out of the deploy
verb with no route to green in either direction: `manifest_rewrite` with no
`--stack`, `unnamed_stack` (exit 2) with one. The refusal is therefore gated on
the selection being **named** *and* the stack being declared, which is exactly the
condition under which `Record` really does drop the entry.

⚠️ **The create gate is per CREATION UNIT, never per folder.** `Lock.Binding`
carries `WorkflowID`/`DataAppID` at folder grain, but `Tables` and `Automations`
are `map[path]…` at FILE grain and both loops discriminate per file. A
folder-grain gate would call a pipeline folder with six bound `.sql` files and
one new one "bound", and `runPipelinePush` would then `CreateTable` the seventh —
a live empty table in a live organization, out of a command that promised to
create nothing. `TestSyncApplyRefusesACreateAtFileGrainInABoundFolder` is
mutation-proved on exactly that: remove the gate and the fake records the create.

⚠️ **A metric file is a creation unit too**, and that was a hole: the units used
to come from the `.sql` files alone, the metric pass ran later in the same push,
and nothing on that path asked the gate — so an apply without `--allow-create`
created metrics, never listed them in the dry-run and never counted them. A
pipeline folder's units are now its `.sql` files plus its `metrics/*.json` files
(checks stay sub-units of their table), and the gate is ALSO enforced inside the
push (`pipelinePushOptions.NoCreate`), on the path that actually creates, so the
property does not rest on the plan and the push agreeing about which files are
units.

**`--allow-create` opts into the first refusal, and only the first.** It counts
**files, not folders** — `Tables` and `Automations` are per-file maps, so a merge
adding one file to each of thirty folders is thirty rows — and it **names the
number, the organization and the destination FEATURES on stderr before the first
write**, because volume is the failure mode and thirty numbers announced folder
by folder is not a number anybody adds up. The features are the informed-consent
half: every create resolves its destination from `f.Binding.FeatureID` out of a
committed, hand-editable `ronja.json`, and all four decisions gate on nothing but
`FeatureID != ""` — so in the CI setting this flag is for, a commit editing one
folder's `featureID` silently redirects rows into any feature the runner's
credential can reach, and a line naming only a count cannot tell that run from
the intended one. Grouped and deduped, sorted by id so a CI log is
deterministic, counted on `Creates()` rather than `WouldCreate()` — a unit
refused for want of a feature has no destination, and that is the reason it has
none. That is what the plan/execute split in `syncApplyTree` is for:
every decision is local, so planning the whole tree first costs no request.
`--dry-run --allow-create` previews the same count and writes nothing. ⚠️
`summary.creates` is what the tree HOLDS and `summary.created` is what apply went
ahead with **and landed** — read off the loops' own per-row `created` flags after
the fact, never the plan, so a folder that cleared the gate and then failed
mid-push reports none. ⚠️ ONE EXCEPTION, and it is the report's own: under
`--dry-run` nothing runs, so `created` carries the PLANNED count instead and the
report says `dryRun: true` beside it (the human renderer says "would be
created"). A reader of the `--json` contract who takes a dry-run's `created` for
history is reading a prediction. The other two refusals stand whatever the flag says: neither is about a row that does not
exist, and one flag that loosened all three would be one nobody could reason
about from its name.

**No `--force`, no `--overwrite-remote`, deliberately.** Where apply refuses on
drift the escape is the folder's own command, run by somebody looking at that one
folder — and the refusal says so, because the first operator to hit thirty
refusals with no named escape asks for a flag instead.

**A data app's lint warnings ride as notes, and apply publishes past them.**
`ronja app push` prints the write-time lint under its verdict; apply runs that
same push and then publishes, and dropping `push.Warnings` on the way would
deploy straight past "fix them before publishing" with nothing in the log to
say so. Each finding is one `note` line on the folder (`notes` in `--json`),
the verdict is unchanged, and there is deliberately no "run `ronja app push` to
see them" hint: after apply the folder is up to date, so a re-push writes
nothing and prints nothing.

**It publishes with `--no-request-review`.** `publishRouting` sends a caller who
may not commit a shared feature's draft (`canDecide` false) to `request-review`, and `runPipelinePublish` counts
only `refused`/`conflict` as failed — so `submitted_for_review` returns a nil
error and reads as success. A naive apply from a CI credential would file one
review request per folder and exit 0 saying "published". Apply fails instead, and
independently treats a review request as a **non-deploy** in its own verdict.

**A pipeline folder's health checks never hold its SQL back — and still fail
the run.** When the ONLY problems a pipeline push reports are in its
`checks/` leg (orphans, drift, a refused or unsupported check write — decided
from the push's own problem tally, never from its error string), apply still
runs the publish; and a publish whose every commit landed but whose checks did
not apply is told apart from a commit that failed. Either way the folder's
verdict is **`checks_not_applied`**: `refused` (exit `1`) for an answer,
`unknown` (exit `2`) when the checks leg got none — a timeout, a 5xx, a read-back
that could not be read — and the detail names the tables that WERE published
and each checks file that did not apply. The asymmetry is deliberate: health
checks are ungoverned and advisory ("a failing check never blocks anything"),
so an unfinished checks leg must not hold a table deploy hostage, while a docs
sidecar or a metric's prose is governed and a failure there still blocks the
publish. Never on an interrupt: a Ctrl-C during the checks pass leaves what it
did not reach refused, and apply does not read that as "only the checks". Apply
passes neither `--prune` nor `--force`: silencing a check or overwriting one
changed in the app stays a per-folder `ronja pipeline push` decision. ⚠️ So a
tree run against an **older server** that has no check-authoring route
publishes the SQL and exits non-zero on every run until the server is upgraded.

**Order is dependency order and the report names it** (see "Dependency order,
and a fresh stack in one apply" above). For a tree whose rows all exist it
changes nothing a path order would not — a derived table's draft builds against
its inputs' LIVE data and a commit cascades server-side, so a folder deployed
"too early" self-heals on the next run. It exists for the rows that do NOT exist
yet: an input table, a workflow a marker runs, an app an automation names has to
be there before the folder that names it is pushed.

**An interrupt finishes the report.** Ctrl-C 20 folders into 30 leaves a tree
that is part live and part not, which is exactly the state that needs a report:
the folders already answered keep their answers, the rest are
`unknown{interrupted}`, and `--json` still emits.

**A refusal does not stop the run.** Every folder is attempted and reported, and
the exit code is the tree's, on the same three-valued contract `status` uses:
`0` applied, `1` refused or did not land, `2` could not tell. ⚠️ `1` is for an
answer, and a write the server never gave one to is a `2`: `api.Unanswered`, a
timeout — a write that may well have committed — and a Ctrl-C are `unknown`, on
the mapping every sibling tree command already uses for a failed read. Calling an
interrupted last folder "not deployed" beside one that may be deployed is the
report claiming certainty it does not have. Best-effort by construction: a loop
that reports its stop as prose rather than a wrapped error is still
indistinguishable from a definite refusal. `applied` /
`refused` are apply's spellings of `clean` / `drifted`, on the precedent
`sync check` set with `broken`: one rank, one exit code, a word that is true
about what this command did.

**Publish is skipped only when the folder has NOTHING STAGED, which is not what
an `UpToDate` push means.** "Nothing to push" has two causes and only one of them
means the folder is live: the draft is a copy of live and always was, or an
earlier run pushed and its publish then failed. Reading the push's verdict as an
answer about the publish reported the second one `applied`, exit 0, for ever —
sticky, because every later run takes the same short-circuit, and reachable with
no failure at all by a developer running `ronja wf push` before CI runs apply.

So the question is put to the record that knows, per kind. Pipeline: the files
the baseline records a draft id for — `pipelineStagedDrafts`, extracted from the
publish's own no-argument target selection so there is one definition of what a
publish targets. Workflow / data app / module: a draft that **pre-existed this push** —
which is a sufficient answer only because of the rule below, and was not before
it: "is a draft open" could not answer it (a push that goes on checks one out on
the way past), and "did it pre-exist" could not either, because the draft an
up-to-date apply used to leave behind pre-existed the NEXT run identical to live,
so publishing on that alone minted a version every second run.

**An unchanged tree writes nothing and leaves nothing behind.** Apply runs
the workflow, data-app and module pushes with `LiveFirst`: before anything is checked
out, the folder is compared with the **live** row — files, and every piece of
metadata the push writes (title, entrypoint, declared parameters, declared
calendar, a runtime upgrade; a data app's title and `access`; a module's
package name and title). When live already
holds it, the push ends there: no checkout, no file write, no patch, no validate
(a candidate save that is not going to happen — `sync check` is the verb that
verifies an unchanged tree's references), and the folder reports `nothing to
push`. The drift legs do not need to run there: live equal to desired is never
drift, and the two anchors re-take it instead — the applied title becomes
the live one and the head pointer moves onto the live version. The local sync
baseline is left alone, since it describes a draft and none was written.

- **Asked whether or not a draft is open**, and that is load-bearing. A draft
  open over a live row that already holds the folder is the author's own — a
  single-folder `push` that wrote nothing still checks one out — or a web edit in
  progress, and neither is this deploy's to write to or publish. Asked only when
  no draft was open, that copy of live would reach the publish gate pre-existing
  and up to date, and committing it would mint an empty version
  (`TestSyncApplyAfterAClassicPushThatWroteNothingMintsNoVersion`).
- **Parity is the contract.** `workflowRowHoldsFolder` / `appRowHoldsFolder` /
  `moduleRowHoldsFolder` are
  the push's own write rule read off the same functions (`filesAgree`,
  `metadataPatch` with the title compared the way `titleLeg` compares it, and
  `diffAccess`). A row they called different while the push then wrote nothing
  to a copy of it would leave a draft identical to live for the next run to
  publish. Anything the push learns to write goes there in the same change.
- **Metadata is a change.** A folder whose only edit is its title deploys — as
  long as the title leg can tell it is this folder's own rename: a workflow or
  module folder whose lock records no applied title (a fresh checkout of a lock
  written before the title was recorded, or a lock a build server never
  committed) refuses a title change; see "Names are declared attributes". A
  draft whose only difference from live is a title, an entrypoint or a calendar —
  staged by an earlier run whose publish failed — is published by the next one.
  The old files-only comparison read that draft as nothing staged.
- **The single-folder push is unchanged.** `ronja wf push`, `ronja app push` and
  `ronja module push` still check a draft out when they have nothing to write: that is the classic
  check-out gesture, and `wf test` runs the draft and refuses without one.

A pipeline folder gets the same question per file, because its fresh-checkout
case was worse: the local baseline that tells a bare push which `.sql` files
changed lives in the git-ignored `.ronja/`, and the committed lock's fingerprints
go stale at the first publish nobody commits back, so every CI run targeted every
file and apply checked out, rebuilt and published every table in the tree on
every run. Under `LiveFirst`, with no draft open:

- a bound table whose live row already holds its SQL, its documented header and
  its declared name is left alone;
- a metric whose live recipe is the same JSON as the file's, whose prose agrees,
  and whose declared calendar the row already holds is left alone. The calendar
  is compared on the recipe route's own rule (`metricZoneHeld`): an absent key
  writes nothing and so always agrees, an explicit `""` is stored as the literal
  `UTC`, and a name is stored trimmed — read off the row's `reportingTimezone`;
- a docs sidecar (`tables/<alias>.json`) whose live row already says exactly what
  it declares is up to date, whatever the lock's recording says. Its drift guard
  has run first, as before.

Documentation agrees only when every column the carrier documents is one the row
has **measured** (`docsHeldExactly`), not under the join rule's exemption that
`docsLanded` grants: prose for a column the table does not have yet is stored
against the name and never reported back, so a skip granting the exemption would
never send it.

**A recorded draft that is gone is not a draft to publish.** `pl push` stages an
edit and records its draft; an admin publishes it in the web app; the file is
unchanged since, so it is not a target. Apply's pipeline push asks the server
about every recorded draft of an unchanged file (one GET each, on the apply path
only — `withGoneDraftPointers`), and handles one that is gone as `discard`
would: no pointer, the file's baseline re-pointed at live, the file made a
target, so a published draft is skipped and a discarded one is pushed again.
Without it the publish went after the gone draft and refused the folder ("1 of 1
draft(s) were not published"). The LiveFirst table skip also drops a pointer it
finds standing, which a push whose build never finished leaves behind.

**A drift refusal leaves no draft behind.** A workflow, data-app or module push resolves
its target before its drift guard runs, so a refused apply had checked out a
copy of live — and a draft left standing is one every later fresh-checkout apply
finds pre-existing, which the head anchor cannot vouch for, so the folder was
refused ("you already have a draft … run discard") long after the author had
absorbed the change. On the apply path, a refusal before the first write — or a
stop where the server refused every write the push sent, so nothing landed —
discards a draft THIS push checked out (`discardUnwrittenCheckout`) — never one
that was already open, never one that holds anything but what live held when the
push read it (the checkout is get-or-create, so a draft the same user opened
between the push's inspection and its checkout comes back looking like this
run's), never a created row's (discarding that is a delete), and never after a
write whose outcome is unknown (a workflow PUT that timed out, a
data-app write in `uncertain`). A discard that fails keeps the refusal as the
error and names the leftover draft. The single-folder push keeps its draft
either way.

Automations needed nothing: that loop already compares the file against the row
and writes nothing when they agree. `TestSyncApplyTwiceSendsNoWriteTheSecondTime`
applies each kind twice — module included — after a real deploy and without one, in the same
checkout and in a fresh one per run, the pipeline fixture carrying a table, a
metric declaring its calendar, health checks and a docs sidecar — and fails on
any non-GET request in the second run. `TestSyncApplyDeploysAMetadataOnlyChange`
pins the parity, one row per attribute the skips compare.

⚠️ **Residue: prose for an unmeasured column is re-sent on every fresh-checkout
apply.** The cost of `docsHeldExactly`: a carrier documenting a column the table
does not have yet never reads as held, so a sidecar is PUT again and a `.sql`
table is checked out, rebuilt and published again — a version per run — until
the column exists. The server reports no way to tell the prose already landed.

A parentless draft is its own leg: it has no live row to compare against, so it
pre-exists by construction and awaits publication by definition. Without it a
workflow created under `--allow-create` whose publish failed stays unpublished
for ever.

**The local guards inside the loops are part of the preview too.** An orphaned
automation file, a dependency cycle and a positional ref all refuse before the
loop's first byte and live outside `applyDecision`, so `decideApplyPushPreflight`
runs them — by calling the loops' own functions, on the loops' own inputs — and a
dry-run reports the same refusal the apply makes. ⚠️ What is still outside it:
per-file *validity* refusals (an unparseable automation declaration, a file over
the size cap), which a dry-run reports as `applied` and the apply refuses. That
is the honest edge of the claim.

**`--dry-run`** prints, per creation unit, what apply would attempt, off the same
`applyDecision` values apply then executes. It counts **creates as well as
updates** — a folder is only previewed once every gate cleared, so a create it
lists is one `--allow-create` let through and apply is about to make. ⚠️ It deliberately does **not**
promise a clean remote: that is a network read whose answer can move between the
preview and the run, and promising it is what makes a dry-run a liar. For a
**workflow, data app or module** whose one unit is an `update`, the dry-run does
make apply's FIRST comparison, for an ADOPTED folder only (a classic one
previews as it always did) — does the live row already hold the folder
(`liveHoldsFolder`, by the same `*RowHoldsFolder` parity functions as
`pushNothingLiveHolds` and its twins; reads only) — and reports such a unit
`unchanged` with the line "nothing to push or publish", or the line "would push
and publish the …" when live differs. When live differs, a workflow or data
app with a local baseline and no draft open is then asked the drift guard apply
asks before its checkout (`compareWithLive` → `checkDrift` / `checkAppDrift`),
and a folder that guard refuses is `refused` (`not_deployed`, exit 1) with the
apply's own refusal as its detail — "the live … changed in Ronja since stack
… last applied it", or the re-pointed binding over an older CLI's baseline.
That is exact as of the read, and it is
one of the two places `unchanged` (`applyActionUnchanged`) appears: the
decision apply executes never carries it. The other is a **pipeline** or
**automation** folder (classic or adopted — apply runs both LiveFirst), compared
per FILE by `sync_apply_dryrun.go` through each push's own skip, never a second
copy of it: a `.sql` file by `tableHeldLive` (one unchanged since this checkout
last pushed it, with no staged draft, is skipped without a read, as the push
skips it), a metric by `metricHeld` plus `metricTagStatus` (and
`metricDefinitionMoved`, which apply refuses), and an automation by
`automationHeldLive` over the push's own one listing of the stack's feature
(`automationRowsOf`) — so a row no longer in that feature ("binding broken")
and a file declaring a field the server cannot patch
(`refuseUnpatchableAutomation`) are asked as the push asks them, then
`automationDelta`. A pipeline dry run also lists each
docs file (`docsUnchangedSinceRecorded` / `docsHeldExactly`) and checks file
(`checksPlanWritesNothing`, over `decideCheckWrite` — the per-entry decision
`applyTableChecks` makes) as a unit — never counted as creates; the docs files'
write gate runs at plan time (`planWriteGate`), in a dry run too. A unit apply
would REFUSE is `refuse` (a file apply refuses as it stands, a metric whose live
definition moved, the two automation refusals above), and the folder is then
`refused` with the reason the apply would report (`not_deployed`, or
`checks_not_applied` for a checks file) — exit 1, as the apply exits. A checks
file holds nothing else back (apply publishes the tables, then reports
`checks_not_applied`), so a folder whose only refused units are checks files
says what would publish and then "the checks would not apply", never that
apply would refuse its files. A unit
that could not be compared — a read failed, a draft is open (the publish may
commit it), the file does not parse — stays `update` with a `detail` saying
why. ⚠️ A documented column the table has not measured yet
never comes out `unchanged`: its prose is stored and never reported back, so
apply re-sends it every run, and the detail says so. The line then reads
"nothing to push or publish: the live rows already hold this folder" ("nothing
to write" for automations), or "would push and publish N of its M files, which
differ from what is live". A declared move is listed as a `move` unit
(`path` the old address, `to` the new one) and a declared removal as a `remove`
unit; neither writes to a row, so neither is counted among the files that
differ — the line names them for what they are ("records 1 declared move",
"stops managing 1 declared removal"). In an ADOPTED folder whose lock names a
row Ronja holds no claim for (the lazy backfill, `managedBackfill`), apply
claims it before anything else: a write to Ronja's records, not to a row. The
folder's line says so after its verdict ("nothing to push or publish …; apply
records 1 claim in Ronja for a row this folder's lock names (no row is
written)"), any kind, and the folder's `claims` and the summary's `claims` count
them (the human summary adds a line). A dry run's human summary line reads
"would apply — N would apply, N would be refused, …", and a folder-kind unit is
labelled by its kind's label ("the data app").

## Addresses: `moved` and `removed`

For the two kinds that bind one row per FILE — a pipeline's `.sql` and
`metrics/<stem>.json` files, an automation folder's `.json` files — the
folder-relative path is the row's **address**: it is what `ronja.lock.json`
(`instances[]` plus `.ronja/state.json` on a folder with no stack) keys the id
by. A renamed file therefore changes the address, and the only two ways an
address may change or end are declared in `ronja.json`:

```json
"moved":   [{ "from": "orders.sql", "to": "sales_orders.sql" }],
"removed": ["legacy_rollup.sql"]
```

**Declarative, not a `ronja mv` command**, because a rename has to reach EVERY
stack and each stack is applied at a different time, often by a different
runner. The declaration travels with the commit; each stack's own push or apply
evaluates it against that stack's own lock entry when it runs.

**Validated at load** (`wfdir/moves.go`), because no stack could apply any of
these: a key other than `from`/`to` in an entry, a path that is not an address
of the folder's kind or not in the lock's exact spelling (`./a.sql`, `../a.sql`,
a backslash), a move onto itself or across kinds (`.sql` → metric), two entries
moving one path or onto one path, a **cycle**, a chain listed **backwards**
(entries apply in declared order, so `[{B,C},{A,B}]` against a stack at A would
end at B with the file at C), a path both moved FROM and removed, and either key
in a workflow, data-app or module folder (those are keyed by the folder, so moving
the folder is all a rename takes). A cycle and a backwards chain are also what
REUSING a path looks like (`[{A,B},{C,A}]`: A renamed to B, and later C renamed
to A), so both refusals end with the one remedy for that: delete the earlier
entry once no stack in `ronja.lock.json` still binds its old path.

`removed` MAY name a move's destination, and that is the rename-then-retire
lifecycle: `orders.sql` was moved to `sales.sql`, and later `sales.sql` is
dropped. Keep the `moved` entry and add `"sales.sql"` to `removed`; removals
evaluate after every move, so a stack still at `orders.sql` is moved and then
dropped in the same apply, and one already at `sales.sql` is dropped. Deleting the
`moved` entry instead would leave the lagging stack bound at a path that is gone.

**Evaluated per stack, in declared order** (`commands/addresses.go`,
`folder.resolveAddresses`), against a simulation first so every refusal comes
before any edit:

| `from` | `to` | outcome |
|---|---|---|
| bound | not bound | re-key: everything recorded under the path moves — id, live/meta fingerprints, automation anchor, tags, recorded name, and a table's `checks/<stem>.json` entry |
| bound | bound | refused (`address_conflict`): one address, two rows |
| not bound | either | no-op — already applied on this stack, or never created here |
| still a file on disk | — | refused as ambiguous, whatever the state |
| bound (a table) | not bound | refused when `checks/<old stem>.json` is still on disk and `checks/<new stem>.json` is not: re-keyed alone, the checks record would sit at a file nothing holds, every check on the table would read as an orphan of a deleted checks file, and the `--prune` that suggests would SILENCE them |

A chain `[{A,B},{B,C}]` against a stack at A lands at C (reported as one move).
`removed` drops the entry (and a table's checks entry, so a later `--prune` cannot
silence checks on a row the folder promised to leave alone); a removed path still
on disk is refused, since dropping its entry would make the next push create a
second row. `removed` deletes nothing. In a LEGACY folder nothing is sent to the
server for either; in an ADOPTED one the state is the server's, so the same
evaluation runs against the server's claims (refreshed into the lock first) and
an acting run sends the result BEFORE the push: a move re-keys the claim
(`POST /managed/move`, one statement, the row untouched) and a removal releases
it (`DELETE /managed/:id`). A `removed` entry also releases a claim the server
refuses to deploy — its row lost, trashed, unreadable or moved out — since
letting the address go is all it asks.

**A table's checks file is part of its address.** `checks/<stem>.json` is named
by the `.sql` stem, so a moved table's checks file is renamed with it (the
refusal above enforces it), and a removed table's checks file is deleted with the
`.sql` — left behind, its stem names no `.sql` file and the checks pass refuses
it. The checks themselves are not touched by either: a move hands them to the new
file, a removal leaves them running, unmanaged.

Each still-on-disk refusal names the reuse remedy as well: a NEW file at a path a
declaration still names is a reuse, and the declaration may go only once no stack
in `ronja.lock.json` still binds the old row there (on a folder with no stack,
once `ronja.json` no longer binds it). Taken out sooner, a stack still at the
path reads the new file as its old row.

**A move changes only the address.** The row's name is an attribute (see "Names
are declared attributes"). A row with no recorded name has the file STEM as its
name baseline, so the move carries the OLD stem with the row before the stem
changes under it — otherwise the new stem would make the row's own name read as
somebody else's rename. It is carried as a STEM (`stem` beside `name` in the lock
entry, or in `.ronja/state.json` on a folder with no stack), never as a recorded
name: the rule compares against both the same way, but a drift refusal says which
it compared against, and only the stem's wording tells the reader the folder
never saw that name on the row — so to declare the current one first. A chain
keeps the first stem. ⚠️ A folder with no stack has nowhere to record anything
about an automation (the anchor layer is off there), so its baseline after a move
is the NEW filename: a declared `"name"` that differs from both the live name and
the new filename then reads as drift. Naming a stack is the remedy.

**In memory until an acting command writes it.** `resolveAddresses` mutates the
folder's lock, local baseline and (legacy) manifest binding in memory, cached on
the `folder`, so every later read in the same command sees the re-keyed state.
`runPipelinePush` and `runAutomationPush` call `saveAddresses` after their local
guards pass, and `runPipelinePublish` — which evaluates the declarations too, so a
draft staged before a rename is published as the renamed file's — calls it before
its first commit, and only when it has something to publish, since that is when a
publish writes the lock at all. `sync apply`'s plan evaluates on the same
`*folder` its execute pass pushes with, so the push finds the work cached and
persists it; a dry-run, `sync status`, `sync check` and the `status` commands
never write. A stack folder writes only `ronja.lock.json` and `.ronja/`; a folder
with no stack also writes `ronja.json`, because that is where its ids live
(`instances[]`). `.ronja/state.json` is written FIRST: a save that fails between
the two writes then heals on the next run (the binding still names the old path,
so the move applies again and the baseline's re-key is a no-op), where the other
order would leave the baseline's drift anchor and name stranded at the old path.

**Rename detection** runs after the declarations, over every address the STATE
binds — not the local baseline, which a fresh checkout does not have, and which
never listed a lock metric whose file was gone (that metric used to be orphaned in
silence). A bound path with no file and no declaration is a **note** in a classic
`pipeline push` (which still creates the new file's row, as it always has),
**refused** as `orphaned_file` by `sync apply`, and the automation push's
existing refusal without `--prune`. One sentence for all three, naming the new
file only when there is one:

```
orders.sql is bound but gone, and sales_orders.sql is new — add a "moved" entry to ronja.json if this is a rename, or a "removed" entry if orders.sql was dropped on purpose
```

A sibling still reading the old stem (`{{ ref('orders') }}`) is refused by the
existing alias check before anything is sent; the move adds no machinery for it.

⚠️ **`moved` is format version 5, and is demanded, not just stamped.** An older
CLI keeps the key, ignores it, reads the renamed file as new, and creates a second
row — the bar `ManifestFormatVersion` exists for. A person adds `moved` by hand
and a stack folder's push never rewrites `ronja.json`, so
`Manifest.CheckAddressFormat` refuses `moved` below 5 at every command that applies
it, naming the line to add — `dependencies`' precedent — and `sync check` reports
it as a finding on the folder.

`removed` is format 5 in an **automation** folder and needs no version in a
**pipeline** folder, because an older CLI ignoring it does different things in
each. In a pipeline folder it leaves the row alone, which is what `removed` asks.
In an automation folder it refuses the gone file as an orphan and suggests
`ronja automation push --prune` — which pauses and soft-deletes the automation
`removed` says to keep running — so the version makes that CLI refuse the folder
by version instead, and `CheckAddressFormat` demands it the same way. The
residual is the one stacks and dependencies carry: a CLI older than the gate
itself has no gate to trip.

**Entries are safe to keep for ever — unless a path is reused.** An entry whose
`from` is not bound is a no-op, so an applied entry does nothing again; the one
thing it blocks is a new file at a path it names (the still-on-disk refusals, and
a cycle or backwards chain at load). Delete it once no stack in
`ronja.lock.json` still binds the old path, never sooner: a stack that has not
applied it is then left at a path that is gone, which `sync apply` refuses as
`orphaned_file` but a classic single-folder `pipeline push` only NOTES — and then
creates the new file's row beside the old one.

⚠️ **Rollout: a long-deleted file now stops `sync apply`.** Rename detection
reads the lock rather than the local baseline, so a pipeline folder whose lock
still binds a `.sql` or metric file deleted before this release is refused as
`orphaned_file` on the first `sync apply` after upgrading. A `removed` entry is
the fix, and in a pipeline folder it needs no format bump.

## Names are declared attributes

A name is an ATTRIBUTE, never an identity. Which row a file or a folder is comes
from the lock; the name is something a push converges like any other field. One
rule for every kind:

- **A declared name is managed.** It is written when the row does not hold it,
  and drift-checked first.
- **An absent name is unmanaged.** It is stamped once, at create, and never
  written afterwards — a rename made in the web app stands.

| kind | declared in | baseline when the lock records none |
|---|---|---|
| table | a `-- @name <text>` line in the `.sql` header | the file's stem (what a create with no declared name stamped); `pipeline clone` and a create under a declared name record the exact name instead |
| metric | `"name"` in `metrics/<stem>.json` | the same |
| automation | `"name"` in the `.json` | the filename (what every push before the key wrote) |
| workflow, data app, module | `"title"` in `ronja.json` | the local baseline's title; with neither, none |

A live name that merely *slugs* to the stem is not a baseline: `clone` writes
"Sales orders" to `Sales-orders.sql`, but "Sales: orders" slugs there too, and
reading it as the folder's own would revert a colleague's rename. A folder
cloned before clone recorded names therefore refuses its first `-- @name` (or
`"name"`) on such a row; declare the current name and push once to adopt it, or
push `--force`.

**The drift rule**, with B the baseline (the applied name the lock records, else
the column above), L the live name and D the declared one:

- **L = D** — nothing to write, and the lock's applied name becomes D. Live equal
  to desired is never drift, whatever the anchor said.
- **L = B** — the folder's own last state, so D is a rename to send.
- **otherwise** — the row was renamed somewhere else: refused as drift, with the
  loop's usual `--force` override (which also acknowledges L, so the next push
  does not re-report it before the rename is published).

**Where the rename goes.** For the draft kinds — tables, metrics, workflows, data
apps — the rename is written on the push's **draft**, never on the live row: a
commit lands the draft's name on the live row, so a rename written on live while
a draft is open is reverted by the next publish. It reaches the live row at
`publish`, in place (same id, same references) — which is not the same as no
build: a `-- @name` line is part of the `.sql` file, so adding or changing it
changes the SQL the draft holds and the draft builds like any other edit, and a
metric's rename lands through a publish that rebuilds the metric. The server checks the name
is free in the feature there — a taken name is refused at publish with
`name_taken`, pointing at `-- @name` / `"name"`. An automation (no draft) and a
module (patched on its live row) are renamed directly. The applied name is
recorded only when it is TRUE that the live row holds it: a create, a publish
that read it back, a push that found L = D, or a `--force` acknowledging L.

**A folder kind with no recorded title** (a data app, workflow or module whose
lock predates the applied title, on a fresh checkout) has one other witness:
the committed head anchor (`headVersionID`).
- **A data app** cannot be retitled on its live row without a publish — a
  rename in the web app opens a draft and lands through a commit, which files a
  version — so when the anchor still matches the live app's head, the live title
  is this folder's own and a different `title` is a rename to send. When the
  head has moved, or the app has never been published, the push is refused.
- **A workflow or a module** can be retitled in place (a workflow in a private
  feature, any module), which files no version, so the anchor cannot vouch for
  a title. A mismatch is refused.

Either refusal names the live title; set `"title"` to match it, remove the key,
or push `--force` to rename. A refused push records nothing, so a colleague's
rename is never adopted as this folder's own and reverted one push later. A
draft holding this folder's own staged rename does not make the still-old live
title drift, so a rename can be pushed twice before it is published — and the
same is true of a table's `-- @name` and a metric's `"name"`, which are
remembered against the draft they were staged on (in `.ronja/`, since a draft is
per-user).

**In an adopted folder the applied name lives on the server**, with the rest of
the anchor (`applied` on the address's claim), so a CI runner that never commits
its lock renames a table twice with no `--force`: the second rename's baseline
is the first rename's name, read back from the server
(`TestManagedSecondRenameFromALockDiscardingCI`). A name or title refusal there
is therefore always a rename made elsewhere, and names `pull` beside `--force`
(`nameRemedy`) rather than the lock remedy below.

⚠️ **In a LEGACY folder the applied name and title live in `ronja.lock.json`, so
commit it after any deploy that renamed something — a build server's
included.** A CI runner that throws its checkout away records nothing: its
first rename lands (live still holds the stem), but its second compares the
first new name against the stem (or, for a workflow or module title, against
nothing at all), reads it as a rename made in Ronja, and refuses. `sync apply`
has no `--force`. The refusal names the way out (`lockRemedy`): run the
folder's own push with `--stack <stack> --force` from a checkout — it renames
the row as declared and records the name — then commit `ronja.lock.json` (or
adopt the folder). The `moved` cleanup rule ("delete the entry once no stack in
`ronja.lock.json` still binds the old path") depends on the same commit. See
BL-8519.

### Live equals desired is never drift, on every leg

An anchor (a lock fingerprint, a head version, a local baseline) is written in a
separate step after a push or a publish, so a crash, a dropped lock commit or a
discarded CI checkout leaves it behind a server this same folder wrote. Every
drift leg therefore asks whether the server already holds what the push would
leave there before it calls a difference drift, and re-anchors when it does:
pipeline SQL (live table and own draft), header documentation and docs sidecars
(under the join rule), the metric recipe (the file is put into the server's stored
form — every key present, `version` defaulted to 1, `source` trimmed, unknown keys
dropped — and compared with the row; a key the row holds that this CLI does not
know keeps them different), names and titles,
automation fields, module metadata, and workflow / data-app / module files. A
file is excused when the server holds the local bytes, or neither side holds it;
with NO local baseline, a moved head over a row that holds exactly the folder's
files **and** metadata is let through and re-anchored (files alone are not
enough). Health checks already worked this way.

## Server-side state: adopted folders

A folder whose `ronja.json` carries an `"id"` is **adopted**: its state — which
row each address manages — lives on the server (`managed_resources`,
`/api/v2/managed`), and `ronja.lock.json` is a **cache** of it. A folder with no
id is **legacy** and runs exactly as it always has, off the lock alone, sending
no request to `/managed` and no `managed` key on a create
(`TestLegacyFolderNeverTouchesManagedState`). The id is the whole switch between
the two patterns: the classic one — clone, edit, push, test, publish — and the
serious one, a repository deployed by `sync apply` from CI.

```json
{ "kind": "pipeline", "id": "0b6f7c2e-3a41-4f0e-9d4c-7a5e2b9c1d08", "title": "Sales", "stacks": { … } }
```

**An address is (the stack's feature, the folder id, the file).** `""` for a
workflow, data-app or module folder; the folder-relative path for a table
(`orders.sql`), a metric (`metrics/aov.json`) or an automation (`nightly.json`) —
the keys the lock already uses. The feature half is the created row's own, never
a value the CLI sends, so a copy of the folder pointed at another feature is a
different address, and only someone who may write a feature can claim inside it.
A stack's NAME is not part of the address: two stacks targeting one feature from
one folder deploy the same thing to the same place. Checks and docs sidecars are
not claimed (a check is its table's child; a sidecar documents a row the folder
does not build).

### Only a local command mints an id

`init --stack <name>` (workflow, data app, pipeline, automation, module), `ronja
sync adopt` and `ronja import feature`. Never `clone`, `push` or `sync apply` — an
id minted on a CI runner and lost with its checkout would make the next run mint
another and duplicate every row. `init` WITHOUT `--stack` mints nothing: that
folder is written in the legacy `instances[]` shape and has no stack to key state
by. A module folder's create claims its address like any other
(`TestManagedModuleCreateClaims`).

⚠️ **The id is format version 6, and is demanded, not just stamped.** In an
adopted folder the committed lock goes stale by design — CI deploys without
committing it — so a row a CI run created, or a `moved` it applied, is recorded
on the server and nowhere in the repository. An older CLI would keep the id,
ignore it, run the folder in legacy mode off that stale lock, and create a second
row for every file whose row the lock never heard of. So `sync adopt` and `init
--stack` stamp `"formatVersion": 6` when they write the id, an older CLI refuses
the file, and an id written by hand into a file that declares less is refused at
load, naming the line to add by hand — the rule `moved` and `dependencies`
follow (`sync adopt` loads the same file and is refused by the same check, so it
is not the remedy there).

`sync unmanage` removing the id is the one removal that lowers the number, and
only from 6 (`Manifest.RemoveFolderID`): 6 means nothing but the id, and
unmanage first writes everything the server's state names into the lock, so the
classic folder it leaves is one an older CLI reads correctly. It lowers to what
the remaining content still needs — 5 for `moved`, 4 for a JavaScript workflow —
and a 7, for an address, write, output or run dependency, stays (`TestSyncUnmanageLowersTheFormatTheIDRaised`). Editing the
number down by hand while the id is there is refused at load.

`sync adopt` reads the server BEFORE it writes the id: a stack with no
`featureID` (`no_feature`), an instance without `/managed`, or a token without
`structure:read` is reported with nothing written, so a failed adopt never
leaves a working classic folder adopted and refusing every deploy
(`probeManagedState`, `TestSyncAdoptWritesNothingTheServerCannotHold`).

A folder id is a lowercase UUID, validated at load. Two folders in one walked
tree carrying the same id — a `cp -r` — are both refused
(`duplicate_folder_id`, each naming the other): one id is one set of addresses,
so the two would adopt each other's rows.

### The server wins

Before any loop runs, an adopted folder that targets a stack reads `GET
/managed?feature=<stack feature>&folder=<id>` and rewrites its in-memory lock to
match (`folder.refreshManaged`). Every loop then runs unchanged, because each
already reads the lock and `f.Binding`. The outcomes, as built:

| server says | outcome |
|---|---|
| claim → X, `ok` | proceed on X, with the anchor from `applied` — the drift guard is armed with no lock at all. A lock that said otherwise (or nothing) is rewritten by an acting run, with a note; the unit is listed as **`adopt`** |
| claim → X, `not_readable` | refused `not_readable`, naming the claimant (typically their unpublished draft). They publish it or run `ronja sync unmanage`; anyone else who may write the feature needs `ronja sync unmanage --force`, since their lock cannot record a row they may not read (see below) |
| claim → X, `trashed` | refused `trashed`: restore it, unmanage, or declare it `removed` |
| claim → X, `lost` | refused `lost`: `sync unmanage` (which drops the dead id from the lock), `sync adopt`, then `sync apply --allow-create` recreates it |
| claim → X, `moved_out` | refused `moved_out`. Almost always the claim is a **tombstone** (`departedAt`): an admin moved the row to another feature, and the move left the claim at its address so nothing is created there in the row's place. The refusal names both features (the destination only when this credential may read the row) and both remedies: move the row back into the stack's feature, which revives the claim with its address and anchor, or `sync unmanage` (which drops the moved row from the lock, so a classic push never writes into another feature), `sync adopt`, then `sync apply --allow-create` to create the file's row here anew (`TestManagedTombstone`). Where the server can tell, the destination is said exactly: a tombstone whose row is gone (it reads unreadable with its id kept, like a `lost` claim) says the row or its feature no longer exists and offers no move back; an unreadable one says "one you may not read, or one that no longer exists" (`TestManagedTombstoneNamesWhereItsRowWent`). A tombstone whose row is **already back** in the stack's feature — the move back raced the move out, so the claim was not revived — says to unmanage the address instead: `sync unmanage` keeps that row in the lock and `sync adopt` claims it again (`TestManagedTombstoneWhoseRowCameBack`) |
| no claim; the lock has X | the **lazy claim**: an acting run claims X (`POST /managed/claim`) before it pushes — refused `claimed_elsewhere` (naming the other address), `moved_out`, `trashed` or `lost` when the server says so |
| no claim; no lock entry | create, claiming the address in the create's own transaction |
| a claim this folder's kind cannot hold at its address | refused `claim_mismatch` — two folders share the id |
| a lock entry naming a row an `ok` claim holds at ANOTHER address | the entry is dropped (its checks record carried to the new stem): a `moved` another run applied and never committed. Without this a run that restored an older committed lock read the declaration as one row at two addresses and refused `address_conflict` for good (`TestManagedMovedFromAStaleCommittedLock`) |

**The server wins both ways**: address → row (a claim rewrites the lock entry at
its address) and row → address (a lock entry whose row the server holds
elsewhere is dropped). Every rewrite is a note on an acting run and in memory on
a read-only one.

A **read-only path rewrites nothing**: `status`, `sync status`, `sync check` and
`sync apply --dry-run` refresh in memory, report what they found (a dry-run lists
the `adopt` units and notes which bound rows the apply would claim), and send no
write to the server or the disk (`TestManagedReadOnlyPathsWriteNothing`). A
`GET /managed` that gets no answer degrades a status like a failed organization
lookup and stops an acting command.

**Only a deploy writes server state.** Push and publish of every kind open the
folder with `openFolderForDeploy`, which settles what the refresh found pending
— `moved`, `removed`, `--adopt-foreign`, the lazy claims — writes back a lock
the server disagreed with, and records the anchors each lock save changes;
`sync apply` does the same in its execute pass. Every other acting command — `wf
run`, `wf`/`app validate`, `bind`, `discard` — refreshes in memory and writes no
claim, move, release or anchor (`TestManagedNonDeployCommandsWriteNoServerState`).
A create such a command makes still carries its claim: a create and its claim
are one transaction whoever sends them.

**Token scopes.** The `/managed` routes are `structure`-scoped: an adopted folder
needs `structure:read` for `status`, `sync status` and `sync check` (a refusal
names the scope), and `structure:write` to deploy — every managed create, claim,
move, release and anchor — beside each row kind's own scope (`automation` for
workflows, modules and automations, `analytics` for apps, `data` for tables and
metrics). A row whose kind scope the token lacks at read reads `not_readable`.
Claiming, anchoring or creating a table or metric needs the admin role.

**CI no longer needs to commit the lock.** An adopted folder applied from a
checkout with no lock — or with the lock committed at adopt time, gone stale
since — creates nothing, rewrites the lock from the server, arms its drift guard
from `applied`, and on an unchanged tree sends no write at all
(`TestManagedLostLock`, the touchstone scenario `managed/lost-lock`, per kind, after a real
deploy and without one, with the lock deleted and with it restored). A `moved`
or `removed` one run applied is already applied for every later run, whatever
lock it restores (`TestManagedMovedFromAStaleCommittedLock`). Commit `ronja.json`
— the id is what tells every runner where the state lives. A `sync apply` that
rewrote an adopted folder's lock says so once, after its summary (and lists the
folders under `locksRewritten` in `--json`): the lock is a local cache of the
records in Ronja, so a deploy never needs it, and a merge conflict in it is
settled by taking either side and then running `sync apply`, which rewrites it
from Ronja — until then the dry run and status read the side taken, which can
name a deployed `.sql` file as never deployed (a false red) or plan rows as
`adopt`. It is
still COMMITTED, because two read-only answers in a fresh checkout read it
rather than Ronja's record (BL-e7b3): with no lock, `sync apply --dry-run`
plans every bound row as an `adopt` unit it does not compare, so an unchanged
tree reads "would push and publish", and a pipeline folder's `sync status`
loses the leg that names a `.sql` file holding SQL never deployed
(`undeployedAgainstLock`, which opens the folder locally).

### Creates claim their address

Every create an adopted folder sends — `POST /workflow`, `/dataapp`, `/module`,
`/automations`, `/feature/model`, `/feature/model/metric` — carries
`managed: {folderID, address, stackLabel, sourceLabel}`, and the server inserts
the claim in the create's own transaction. Two concurrent applies at one address
therefore converge on ONE row:

- **`409 address_claimed`**: the create was rolled back whole, and the body
  names the holder (`claim`, beside `error` and `code` — the server renders an
  error's details at the top level, not under `details`). When its `state` is
  `ok`, that row is this file's: adopted BY ID, and the push continues as the
  update it now is, drift guards and all (a workflow, data-app or module push
  runs once more; a table, metric or automation continues in place). Any other
  state refuses with that state's outcome and adopts nothing — a tombstone
  (`state: moved_out`) refuses `moved_out`, and `sync apply` reports the folder
  under that reason even where a pipeline push folds the refusal into its
  per-file count.
- **`409 name_taken`** (tables and metrics only — their name gate runs before the
  claim): the folder re-reads its state. A claim now at this address is the
  winner, adopted by id; none means the name belongs to a row this folder does
  not manage, refused with the remedy (record its id in the lock and run `sync
  adopt`, or rename).
- **Metric adopt-by-name is retired for adopted folders.** A legacy folder still
  adopts a `name_taken` metric holding exactly its file's definition (its ids
  live in a git-ignored baseline there); an adopted folder never takes a row
  over by its name (`TestAdoptedFolderNeverAdoptsAMetricByName`, with the legacy
  control beside it).
- A table create that **timed out** asks its address, not its name.

### Anchors live in `applied`

The anchor is exactly what the lock carries beside each id: a workflow's, data
app's or module's `headVersionID` and applied `title`; a table's `liveSHA256`,
`metaSHA256`, applied `name` and carried `stem`; a metric's `liveSHA256`,
`declaredSHA256`, `name`, `stem` and tag record; an automation's `updatedAt`,
`name` and `stem` (`wfdir.Lock.Anchor` / `SetManaged`, one translation each
way). A table's **checks** record (the table its checks file last applied to and
each owned check's projection) is on the FOLDER's anchor, not the table's — see
below. So a lock-less run still refuses a check re-severitied in the app
(`TestManagedLocklessApplyKeepsTheChecksDriftGuard`). Every lock save in an adopted folder records the anchors it changed with
`PUT /managed/:id/applied`, a compare-and-set on the claim's `revision`, merged
over the claim's current anchor so a key a newer CLI recorded survives. On a
`409 stale_revision` it re-reads once: the claim still holding the anchor this
write was based on (only its revision moved — a relabel) is retried; one already
holding this anchor is done; one holding a different anchor was written by
another apply meanwhile, and is surfaced as `anchor_not_recorded` rather than
overwritten. A `404` is the server's answer both for a claim released meanwhile
and for a row this credential may no longer write, so the claim is re-read and
its state named: none left is `lost` (with `ronja sync adopt` as the remedy), a
refusing state is that state's outcome. A `409 moved_out` — the row was moved to
another feature while the run went on, and the claim is a tombstone — is
`moved_out`. Either way the change itself landed, and live equal to desired is
never drift, so the next run re-anchors. An anchor write also fills in the
claim's `sourceLabel` when it is empty or names this repository at another path
(a folder moved inside it); a different repository is left to
`--adopt-foreign`.

An **automation** whose claim carries no anchor — a crash or a failed anchor
write right after its create — would otherwise refuse `no_drift_anchor` on every
lock-less apply, with a per-folder push as the only remedy, which CI does not
run. In an adopted folder `sync apply` reads the row, and when it already holds
everything the file declares the unit is an update that writes nothing but the
anchor (live equals desired is never drift); one that differs still refuses
(`TestManagedAnchorlessAutomationAtDesiredIsReanchored`). The plan's read is not
what keeps the write safe, though: the push runs after every earlier folder, and
with no anchor both of its guards are off, so an operator who paused the
automation in between would have it turned back on. The push therefore decides
again on the row **it** reads (`RequireAnchor`): still equal to the file, it
records the anchor and writes nothing else; different, it refuses
`no_drift_anchor` and sends no write
(`TestManagedAnchorlessAutomationReDecidedAtWriteTime`). A single-folder
`automation push` keeps writing such a row — that write is how a folder earns its
anchor. An anchor failure never fails the save that triggered it (its local
half landed); a single-folder command reports it as its error at the end, and
`sync apply` as the folder's verdict.

### The folder anchor holds the docs-sidecar and health-checks baselines

A docs sidecar (`tables/<alias>.json`) documents a table the folder does **not**
build, so it has no claim, and its record (`tableDocs` in the lock) belongs to no
row. It lives on the folder's own anchor instead — one per feature and folder
id, read from the same `GET /managed?feature=&folder=` as the claims (its
`folder` object) and written with `PUT /managed/folder/applied`, a compare-and-set
on the anchor's own `revision` (0 creates it). The health-checks records
(`checks` in the lock, keyed by checks file) live there too: one entry per check,
about 160 bytes each, which on the table's own claim anchor — capped at 16 KiB —
failed every anchor write of a table with a hundred checks
(`TestManagedTableWithManyChecksRecordsItsAnchor`). That is the whole of what the
folder anchor holds, by decision: everything else the lock records has a claim
to ride on, and a folder's configuration is in `ronja.json`, which every runner
has.

- It depends on no claim. A folder that builds nothing, a host file declared
  `removed`, an address refused a claim at adopt, and any number of sidecars
  (the server's cap is 256 KiB, thousands of them) all keep their baselines, so
  a lock-less run still refuses prose rewritten in the app
  (`TestManagedDocsOnlyFolderKeepsItsDriftGuardWithoutALock`,
  `TestManagedManySidecarsLoseNoBaseline`,
  `TestManagedDocsBaselinesSurviveTheirFormerHost`,
  `TestManagedLocklessApplyKeepsTheDocsDriftGuard`). A folder of docs sidecars
  alone applies with no publish, since there is nothing to commit.
- After every lock save it is written **before** the claims' anchors and
  independently of them: a refused folder anchor never stops a claim's, nor the
  reverse. The server wins for every sidecar it records; one only the lock
  records is kept and written up by the next save. A key a newer CLI recorded is
  carried through.
- A `409 stale_revision` re-reads once: the anchor already holding this state is
  done; one still holding the state this write was based on (only its revision
  moved) is retried; another apply's different state is surfaced as
  `anchor_not_recorded`, not written over. A `413 applied_too_large` writes
  nothing and is refused as `folder_anchor_too_large`, naming the sidecar and
  checks-file counts — split them across folders (`TestManagedFolderAnchorArms`).
- A claim anchor an earlier build wrote with `tableDocs` on it is read once, while
  the folder anchor holds none, and the key is cleared from the claim only after
  the folder anchor has recorded it
  (`TestManagedLegacyDocsOnAClaimMoveToTheFolderAnchor`). The same holds for a
  table's `checks` record an earlier build kept on the table's claim: read as a
  fallback (the folder anchor's record for the same file wins), and cleared from
  the claim once the folder anchor has recorded it.

### `ronja sync adopt` and `ronja sync unmanage`

```bash
ronja sync adopt --stack prod [dir]
ronja sync unmanage --stack prod [dir]
```

**`adopt`** goes classic → serious, for every folder under `dir` (or `--dir`)
that declares the stack: it mints an id where there is none (the one write to
`ronja.json`), claims every row that stack's lock records in the stack's feature
in one batch (each row reports its own outcome — `claimed`, `already_claimed`,
or `refused` with the server's code), and records the lock's anchors on the
claims, so the very first lock-less run is guarded. A folder that refuses does
not stop the others. It is idempotent: run again, it claims nothing and writes
nothing. Commit `ronja.json`. A legacy `instances[]` folder has no stack to adopt
into; name one first (`--stack <name> push`).

**`unmanage`** goes serious → classic. First it writes every `ok` claim's row
and anchor into the lock, and the folder anchor's docs-sidecar baselines, and
saves it — a row a lock-less CI run created is known to the server alone, and
the lock is the classic pattern's only state, so without this the next classic
push created a second one (`TestSyncUnmanageKeepsTheRowsCIMade`,
`TestSyncUnmanageReturnsTheDocsBaselinesToTheLock`). Then it marks the stack
`"unmanaged": true` in `ronja.json` and saves that **before releasing anything**,
so a run that dies part way leaves a stack running from the lock it just wrote
rather than an id whose next apply claims every row back; a second unmanage
finishes (`TestSyncUnmanageMarksTheStackBeforeReleasing`). Then it releases every
claim of the folder in the stack's feature (anyone who may write the feature
may, including a claim on a row they cannot read) and the folder anchor, leaves
the ROWS exactly as they are, drops from the lock the rows of `lost` claims (gone
for good) and of tombstones (moved to another feature on purpose — a classic
push must not write there), and removes the folder id and the mark. Commit both
files.

**A claim on a row this credential may not read** names no id, so unmanage
cannot write it into the lock, and a classic push of that file afterwards would
create a second row. Unless the lock already records the address, unmanage
refuses `not_readable` before writing anything, naming each such address and its
claimant: they publish the row (after which it is readable and recorded) or run
unmanage themselves. `--force` releases it anyway and says what that costs — the
alternative, releasing silently, is exactly the second-row bug the lock write
exists to prevent (`TestSyncUnmanageRefusesARowItCannotRecord`).

**The id stays when another stack of the folder still holds claims or a folder
anchor under it** — removing it would strand that state under an id nothing
carries: every row the claims hold would then refuse a later `adopt` as
`claimed_elsewhere`, and the other stack's docs baselines would be lost to it.
Another stack this credential cannot ask (another instance or organization)
keeps the id too. The unmanaged stack then keeps its `"unmanaged": true` mark,
so it STAYS released: it runs from the lock, claiming nothing, until `ronja sync
adopt --stack <name>` clears the mark. Removing the id clears every mark.
Unmanage each stack to return the folder to the classic pattern
(`TestManagedTwoStacksKeepTheirOwnClaims`,
`TestSyncUnmanageKeepsTheIDForAnotherStacksFolderAnchor`).

Against an instance that does not serve `/api/v2/managed` at all — the refusal
there names `sync unmanage` as the remedy — the route's plain-text 404 means no
server state exists to strand, so unmanage removes the id and the folder runs
from the lock (`TestSyncUnmanageOnAnInstanceWithoutManagedState`).

### Other writers: the notice, and `ronja <kind> pull`

A row an adopted folder deploys can still be changed by anyone who may change
it — in the web app, through the agent, or from a cloned folder. Nothing is
blocked. Two things make that coexist with the repository.

**The classic-loop notice** (`managed_notice.go`). Each kind's single-row GET
(`/workflow/:id`, `/dataapp/:id`, `/module/:id`, `/feature/model/:id`,
`/automations/:jobID`) carries a `managedBy` object for a row with a live claim
— `sourceLabel`, `stackLabel`, `folderID`, `address`, `featureID`, `updatedAt`;
a draft answers its root's, a tombstone none
(`backend/resource/rmanaged/managed_by.go`). `clone`, single-folder `push` and `publish` of a
workflow, app, module and pipeline (tables and metrics), and `automation push`,
read it from the GET they already make and print one line to stderr when the
row is managed by a folder OTHER than this one — the same folder id deploying to
the same feature is the managing folder, and prints nothing:

```
  Note: managed in git (github.com/acme/data, folder pipelines/sales, stack "prod") — this change will show as drift there; `ronja pipeline pull` in that repository absorbs it.
```

The label splits on the first `//` that is not a scheme's `://`
(`api.SplitSourceLabel`, the web chip's rule). A deploy from a checkout with no
git remote records the repository directory's name and the folder's path,
marked `(local, no git remote)` — `roastery/apps/dashboard (local, no git
remote)` — where an older CLI recorded nothing (and the line names the folder
id instead, for such a claim). A local label names no repository: it is never
foreign (`sourceRemote` reads no remote half in it), and an anchor write from a
local checkout never replaces a label a checkout with a remote recorded
(`relabelTo`) — only an empty one. A label is still refreshed only when an
anchor is written (the quiet relabel); a claim no apply writes to keeps its old
one, so a remote added to a repository reaches each row's label with the next
change to it. See BL-7c2e. A clone says "a change
made from this copy will show as drift there". One line per managing folder,
however many of its rows the command touched; `--json` carries one entry per row
under `managedInGit`. It is a notice, never a refusal, and an instance that
sends no `managedBy` says nothing. The one command that pays a request for it is
`automation push`, which compares against the feature's listing (no
`managedBy`): it GETs each row it UPDATED, best-effort, and an adopted folder —
whose every row is claimed at its own address — sends none.
`sync apply` collects and prints none of it.

**`ronja <kind> pull [dir]`** (`pull*.go`), for `wf`, `app`, `module`,
`pipeline` and `automation`, in an ADOPTED folder only:

```bash
ronja wf pull                 # rewrite the folder from live, print the diff, record the anchor
ronja pipeline pull --dry-run # print the diff, write nothing anywhere
```

1. **Opened read-only.** The refresh reads the server's state and refuses a
   claim that is not `ok` with its 2b outcome (`not_readable`, `trashed`,
   `lost`, `moved_out`, `foreign_claim`, `claim_mismatch`); nothing is settled —
   no `moved`, `removed` or lazy claim is sent.
2. **Refused before reading** when the folder has no id (`not_adopted`, naming
   `clone` and `sync adopt`), a stack marked `"unmanaged"`, no claim for what it
   would read (`not_claimed`: the stack never deployed it, or a lock row has no
   claim — `sync adopt` claims it), or a `moved` / `removed` the server has not
   applied (`pending_declarations`: deploy it first — a pull from the claims
   would otherwise write the old path back). A draft kind whose row was never
   published is refused `not_live`: there is no live version to pull. A
   pipeline or automation claim whose ADDRESS the folder cannot hold as a file —
   `ronja.json`, a dot-path like `.github/workflows/x.json`, a path
   `wfdir.ValidatePath` refuses — is refused `inexpressible` before anything is
   written (`refuseUnholdableClaims`): those pulls write each claim's file at its
   address, the server checks an address only for its shape, and the folder id
   is public with the repository.
3. **The dirty rule** (`refuseUncommitted`, skipped by `--dry-run`, overridden
   by `--force`): `git status --porcelain=v1 -z --untracked-files=all -- .` in
   the folder; any modified, staged, deleted or untracked path in it refuses
   (`uncommitted_changes`, naming them), except `ronja.lock.json` and `.ronja/`
   — the lock is a cache of the server's state, which the pull rewrites from the
   server anyway. **Outside a git work tree** (or with no git binary) it refuses
   too (`outside_git`): inside git a committed file is recoverable and the dirty
   check protects the rest, but outside it nothing can say which edits are work
   and nothing could bring an overwritten one back, so the pull wants `--force`
   — the author's word that there is nothing to lose. The printed diff is not a
   way back.
4. **Render** the folder from the LIVE row — never the author's draft — in the
   folder's disk form, and check it before writing a byte:
   - **workflow / app / module:** the file set (added, rewritten, deleted) via
     the codec's `toDisk`, and the manifest metadata the folder ALREADY manages —
     title, entrypoint, parameters (`parametersToDisk`), reporting timezone,
     runtime; an app's title and `access` (each id its codec names written as
     the alias); a module's title and package name. A key the manifest leaves
     out stays out. The plan must pass the push's own parity function
     (`workflowRowHoldsFolder` / `appRowHoldsFolder` / `moduleRowHoldsFolder`),
     or the pull is refused `inexpressible` and nothing is written.
   - **pipeline:** each claimed table's `.sql` is the live SQL canonicalized
     and de-aliased with the file's own sibling spellings, then any other sibling
     by its stem (an id the file spelled literally stays literal), then declared
     aliases; its opt-in header keeps its lines, each DECLARED value taking the
     row's current description, column note or `-- @name`
     (`tabledocs.RewriteHeader`, which never adds a declaration and keeps a
     column the row has not measured). Each claimed metric's file takes the live
     recipe, its `source` spelled as the file spells it while that still names
     the live table, and the `description`, `reportingTimezone`, `name` and
     `tags` it declares; a file that already holds the live metric keeps its
     bytes. Each claimed table's `checks/` file keeps its entries: one a live
     check resolves to takes its body and declared fields, one the folder owned
     whose check is gone (deleted in the app) is dropped, one never applied is
     kept. Each `tables/` sidecar takes the row's prose for what it declares.
   - **automation:** each claimed automation's file takes the row's values for
     the keys it DECLARES — three-state kept, so an undeclared `enabled` stays
     undeclared — with each reference field keeping its spelling while it still
     resolves to the live id, else the codec's alias, `"@feature"`, or the
     literal id; a claimed file gone from the folder is written whole, as import
     writes one. Every file is parsed, resolved and compared with the row the
     way a push does (`automationDelta` empty) before anything is written.

   A claimed address whose file is gone is written back; a file the server holds
   no claim for has never been deployed from here and is left alone.

   **Undeployed changes** (`undeployed_changes`). Each unit is compared with
   what its stack last APPLIED, so a change committed but never deployed is
   never replaced silently. A unit the folder changed since then, whose live row
   changed too, is refused, naming the files (`--force` replaces them, and the
   report lists them under `replaced`; `--dry-run` lists them and does not
   refuse). A unit only the folder changed, on a live row that has not moved,
   has nothing to absorb: it is left as it is and listed under `kept`, and the
   next apply deploys it.
   - **workflow / app / module** — the unit is the row, read off the head-version
     anchor: recorded == current means no publish since, else ONE GET of the
     recorded version's files (`GET :id/files` on the version id), compared with
     the folder in its disk form; the applied-title anchor stands for
     `ronja.json`'s title. Live moved = a new head or a title the anchor does
     not hold. A stack that records no head cannot be compared and pulls as
     before; a failed read refuses (without `--force`) rather than guess. A
     recorded head equal to the row's OWN id (the row had no committed version
     when the stack applied it — an import of an unversioned row, or a row a
     `sync apply` created and never updated) is not a snapshot: its files are
     the live row's, rewritten by every publish, and the server keeps no copy of
     what the row held before its first version (a commit turns the DRAFT into
     the version). That case compares the folder with this checkout's local
     baseline instead, used only when it was read off this live row (`sourceID`
     the row, lifecycle live — not another stack's row, BL-4e71, and not a
     draft) at least 2 s before the oldest version's `committedAt` (a discard
     re-reads live after the publish, and such a baseline describes the
     publish). With no such baseline (a fresh checkout) the pull refuses, saying
     the comparison cannot be made and why, unless the folder already holds
     every live file; `--force` takes what is live. A baseline read less than
     2 s before that commit is refused the same way and the refusal says it was
     too close to tell (both stamps are to the second, from two clocks).
     Waiting does not help: neither stamp moves. Other
     manifest metadata (entrypoint, parameters, runtime) has no applied anchor
     and is not compared.
   - **pipeline** — the unit is the file, against its recorded fingerprints: a
     `.sql` file's bytes against the recorded live SQL hash (live moved = the
     live SQL in the file's spelling hashes differently, or a declared header
     value changed); a metric file's declared fingerprint against the recorded
     one (live moved = the recipe hash moved, or a declared description,
     timezone, name or tag is not held — the conservative side, since those
     have no recorded value); a sidecar's fingerprint against the recorded one
     (live moved = the row's prose hash moved); each owned check entry
     (`checkfile.Entry.SHA256`) against its recorded `liveSHA256` (live moved =
     the live check's projection moved, or it was deleted).
   - **automation** — no comparison: its anchor is the row's `updatedAt`, which
     records no content, so a pull replaces an automation file's declared keys
     as before.

   **Files git cannot bring back** (`unrecoverable_paths`, overridden by
   `--force`). The dirty rule never lists a file git IGNORES, and the folder's
   enumeration does not read `.gitignore`, so every path the pull would write or
   delete must be tracked by git (`git ls-files -z -- .`) or absent; one that
   is ignored or untracked (classified by `git status --ignored=matching`) is
   refused, naming it.

   **Symbolic links** (`symlinked_path`, NOT overridden by `--force`). A path
   that passes through a link in any component, the file itself included, is
   refused: the write would land wherever the link points.
   **`ronja.json`'s `dependencies` and binds are never touched.** A live
   reference the folder's aliases cannot express is written as the literal id and
   listed per line (or per field, for a JSON file) under `literals` — never as a
   new alias, so nothing is silently re-bound; a literal the folder already
   spelled is its own choice and is not listed.
   **The human report escapes what it prints from the server**
   (`escapeTerminal`): the diff, notes and pending lines show every C0/C1
   control character but tab and newline, and the bidi and zero-width
   characters, as `\x1b` / `\r` / `\u202e` — the diff is what the author
   reviews before committing, and an escape sequence in a live file could
   otherwise hide a line the pull writes. `--json` is left to `encoding/json`.
   **The write gate** (`plan.gate`, `pullGateRefusal`), last before anything is
   written and in a `--dry-run` too: the same check the deploy makes, over the
   live row the pull would take in — a workflow's stored files (`gateWorkflowRow`),
   an app's live `access` when the folder manages it, the planned automation
   files, a pipeline's docs sidecars. A live row that writes or runs outside the
   stack's feature is refused `writes_outside_target` / `runs_outside_target`
   with nothing written: the folder could not deploy it, and committing it would
   make every later apply refuse. `--force` does not override it.
5. **Write** the files and the manifest, then **anchor**: the kind's own deploy
   push in its live-first mode — `runPush` / `runAppPush` / `runModulePush` with
   `LiveFirst` (the head-version and applied-title anchors), `runPipelinePush`
   with the new `Pull` option, `runAutomationPush` with `Pull` — run under an
   `api.ReadOnlyGuard` on the context that refuses, before it is sent, every
   request but `PUT /managed/:id/applied`, `PUT /managed/folder/applied` and
   the write gate's `POST /workflow/validate`, which persists nothing
   (`anchorWriteOnly`; the workflow push gates before its live-first check). So
   the anchors are recorded through the same paths 2b's lock saves use, a
   lock-less CI run sees them, and a pull that did not reproduce live exactly
   cannot deploy anything: the guard names what it would have sent
   (`would_write`). The guard covers `api.DoRaw` as well as `Do`. A
   `would_write` pull has already WRITTEN its files and recorded nothing
   (`anchored: false`): the files are the live version, to commit; if `status`
   then shows no difference the next apply only re-anchors (live equal to
   desired is never drift), and if it shows one that is what the next apply
   would send — either way a bug worth reporting. Pipeline `Pull` mode records
   each leg as seen from the live row — SQL, documentation, name, metric
   definition, tags, checks — the acknowledgement a `--force` records without
   its write, writes no row, and leaves the open draft and its pointer alone;
   but it records a leg only where the file HOLDS the live row. A sidecar is
   recorded when it holds the row exactly or is short only of unmeasured
   columns, and otherwise reported `pending` with nothing recorded; a metric
   file that does not hold the live definition records nothing and refuses the
   pull `inexpressible` (a kept one is `pending`); a sidecar whose planning read
   fails fails the pull before anything is written. Recording a leg the folder
   never took in would let the next apply overwrite the web edit with no drift
   refusal. Automation `Pull` mode leaves a never-deployed file `pending`
   instead of creating it.
6. **Report** the unified diff (`unified_diff.go`, Myers, git's format, pinned
   against `git diff`) of what is now on disk — the files and `ronja.json` —
   plus `literals`, `notes` and `pending`. **Pending** is what the committed
   pull still deploys: a `.sql` header rewritten for a description or name
   changed in the app is part of the table's SQL, so the next apply publishes
   that table once; a file never deployed is created by the next `--allow-create`;
   a kept file is deployed by the next apply. `replaced` and `kept` name the
   undeployed changes the pull replaced (`--force`) or left.
   Otherwise committing the diff and applying again sends no write, from the same
   checkout and from a fresh one (`TestPullAbsorbsAWebEdit`, per kind, and
   `TestPipelinePullAbsorbsEachLeg`).

**The open draft.** Pull reads live and never touches a draft. A draft open
when it runs keeps its files, and its drift guard is what it was: the draft
kinds' local baseline is refreshed from live only when no draft is open (the
baseline describes the draft a push writes to), and a pipeline table's draft
pointer stays. Publishing that draft afterwards puts its content live over what
was pulled, exactly as before. The JSON's `notes` say a draft was open.

**The drift refusals name it.** In an adopted folder every drift refusal of
push, publish and `sync apply` — the draft guard, a moved head, a folder with no
baseline, a renamed title or name, a table's SQL or documentation, a metric's
definition (verified or not), a check, an automation's anchor and its re-enable
guard — adds "Or, to take the change made on the server into this folder
instead: `ronja <kind> pull` rewrites the folder with what is live, and refuses if
that would replace a change of this folder's that was never deployed"; the
pipeline and automation summaries `sync apply` reports name it
when a unit was refused for drift (`folder.pullHint`). A classic folder's
refusals are unchanged.

**The workflow and data-app drift guard leads with the action**
(`baselineDriftRefusal`). Its first line says what changed — the live
workflow or app, or the user's own open draft — and, for an adopted folder and
the live row, the command that takes it in: `the live data app changed in Ronja
since stack "prod" last applied it — run `ronja app pull` to take it into this
folder`. The changed files follow, then how to see or overwrite it — under
`sync apply`, which has no --force, "run `ronja app push --force` in this
folder, then apply again" (`pushOptions.Apply`). `sync apply` indents every line
of a folder's detail under it. Under `sync apply` the guard is also asked
BEFORE the checkout when no draft is open (the checkout would only copy the
live row it just read), so a refused folder opens and withdraws no draft and
leaves no `draft_withdraw` in the audit log; the pull's anchor step keeps the
guard after its checkout, where its read-only guard catches the write. A
`sync status` drift line for an adopted folder names `<kind> pull` beside what
changed on the server.

### `--adopt-foreign`

Each claim records a `sourceLabel`: the normalized git remote and the folder's
path in it (`github.com/acme/data//pipelines/sales`). A claim whose REMOTE
differs from this checkout's — a fork or a template copy that kept the folder
ids and targets the SAME feature — is refused `foreign_claim`, naming both.
`--adopt-foreign` (on every folder command and on `sync`) takes the addresses
over: the claims are relabelled with `PUT /managed/:id/applied` and the run
proceeds. Only the remote half decides — a folder moved inside its repository
keeps its id and is not foreign — and a side with no git remote is never
foreign, since nothing can tell it apart.

The remote is normalized before it is compared (`normalizeGitRemote`, applied to
the recorded label too): no scheme, credentials (a password holding an
unencoded `/` included), port or `.git`; the scp form read as its URL; a host's
aliases folded (`ssh.github.com`, `altssh.gitlab.com`, and Azure DevOps'
HTTPS, SSH and `visualstudio.com` forms as `dev.azure.com/<org>/<project>/<repo>`);
and the whole of it LOWERCASED. GitHub, GitLab, Bitbucket and Azure DevOps
resolve paths case-insensitively, and a case-sensitive label made a CI checkout
(`Acme/Data`) and a laptop clone (`acme/data`) refuse each other's claims, with
`--adopt-foreign` handing them back and forth. A self-hosted server with two
repositories that differ only in case reads as one source — the cheaper miss for
a slip guard.

### Rename detection refuses in an adopted folder

A single-folder `pipeline push` in an adopted folder REFUSES a bound file gone
with no `moved` or `removed`, as `sync apply` does — the address is server state,
and a note would let the next create claim a second row beside it.

Two runs applying one `moved` at once cannot both re-key it: the loser's `POST
/managed/move` answers `404 not_claimed`, and when the claim already sits at `to`
holding the same row that is the declaration applied, not a failure
(`TestManagedConcurrentMovedIsAlreadyApplied`). A move that loses to a concurrent
move of the same claim mid-request (an uncoded `409`) refuses the folder; re-run
it.

### The write gate: a stack writes only its own feature

`write_gate.go`. A deployment to a stack writes only inside
that stack's feature, except through a write-capable data source bound FOR THAT
STACK. It closes the slip scenario: `import feature Sales` from Main, a
`dev` stack added by hand, `sync apply --stack dev --allow-create` — the imported
workflow's `tools.replaceTable(df, "table-<Main>")` is an output literal no codec
rewrites, and without the gate the Dev deployment overwrites production on its
first run, whatever dev binds.

**Where.** Every stack of an ADOPTED folder (`folder.adoptedHere`: a folder id, a
named stack with a feature, not `unmanaged`), on every path that puts code or a
grant in front of that stack: `sync apply` (at PLAN time for a workflow or data
app the run would create — so a refused folder leaves no empty, claimed row
behind — for a pipeline folder's docs sidecars, and for every folder in a
`--dry-run`, whose pushes never run; and inside the push for every write),
`pipeline push` (its docs sidecars, before anything is written), `wf push` (also under
`--no-validate`: the gate's request is not a check to skip), `wf publish` and
`wf test` (over the DRAFT the server holds, which the web app may have edited),
`wf run` (over the LIVE row it runs), `app push`, `app publish` and `app test`
(over the access of the row it renders, whose grants the render runs with),
`automation push`, and `ronja <kind> pull` (over the live row it would take
in, before writing a file). A push gates BEFORE its live-first "the live row
already holds this folder" check, so a folder that writes outside is refused
whether or not it changed — an adopted workflow folder therefore sends one
`POST /workflow/validate` on every deploy, unchanged or not (it persists
nothing). A classic folder (no folder id) is checked exactly as before.
Turning it off is one committed line — `"unmanaged": true` on the stack, or
deleting `"id"` — so `sync apply` says so: a note on each folder it deploys
unmanaged where the gate could apply (a stack marked `"unmanaged"`, or no id in
a run whose other folders are adopted; `noteUnmanagedFolders`), whose first
line names the `ronja sync adopt --stack <name>` that turns it back on. An
all-classic tree gets none. An adopted folder whose selected stack names no `featureID` would have run
classic — unclaimed and ungated — so it is refused `no_feature`, naming the line
to add (or the `"unmanaged": true` that deploys the stack the classic way on
purpose).

**What, per kind.**

- **A workflow:** POST `/workflow/validate`'s `writeTargets`, computed by the
  server from the files the push is about to send — the WHOLE folder (the row's
  output bindings derive from every file), after the stack's codec resolved it.
  One scanner, the server's
  (`backend/resource/rworkflow/validate_write_targets.go`): `{{ write() }}` markers, output
  literals (the table argument of `tools.replaceTable` / `appendToTable` /
  `saveAsTable` and their `…FromDir` forms), `{{ codex() }}` markers (a run may
  upsert and delete), write-mode `{{ file() }}` markers and `saveFile` /
  `saveTo…` `folder=` literals. An instance that answers no `writeTargets` key
  at all is refused `no_write_targets`: an absence is not "writes nothing" (see
  "A CLI that needs a newer server" under Releasing). Its RUN EDGES are the same answer's
  `resolved.workflowIDs` and `resolved.agentIDs` — the server's resolution of the
  `{{ workflow() }}` / `{{ agent() }}` markers in the posted files; the CLI only
  finds the line each resolved id is on, to name it.
- **A data app:** its `access.allowedCodexIDs` — the app token's codex grant
  covers upsert and delete — and, as run edges, its `allowedWorkflowIDs` and
  `allowedAgentIDs`. Its table and metric allowlists only read, and are not
  gated. An app with the `write_external` capability writes an external
  database through a secret in `allowedSecretIDs` (`CapWriteExternal`), as a
  workflow does through a `{{ secret() }}`: a side effect outside Ronja, scoped
  to the organization rather than to any feature, so the gate does not place it —
  which secret, and so which database, a stack writes through is that stack's
  bind of the secret dependency, decided per stack like every other.
- **An automation:** the tables (`model`) in an agent action's reference set — a
  headless run may write rows into any table it was given (a metric there is a
  recipe nothing writes, and passes) — and the FEATURES there other than
  `"@feature"`, whose every table the run is given. As run edges: a `workflow`
  action's workflow, a `saved_agent` action's agent, and every `workflow`
  reference.
- **A pipeline:** its docs sidecars (`tables/<alias>.json`), each of which
  writes its table's description and column prose. A sidecar documents a table
  the folder does NOT build — an integration table, another feature's dynamic
  table, a literal `table-…` stem — so nothing places it in the stack's feature,
  and `ronja bind` would happily propose Main's table for a Dev stack's read
  alias. The tables the folder builds are created in the stack's feature by
  construction and are not gated.

**The rule, per target.**

| target | admitted when |
|---|---|
| a table or codex (a docs sidecar's table included) | it is in the stack's own feature (readable, and its feature is the stack's), or it is the id a `"write": true` dependency of its kind is bound to IN THIS STACK; a sidecar's table is also admitted when an address dependency resolved its alias (a table this repository manages in the stack) |
| a feature (an automation reference) | it is the stack's own feature (`"@feature"`), or a `"kind": "feature", "write": true` dependency is bound to it IN THIS STACK |
| a table written by name (`{{ write('orders_daily') }}`) | always — the runtime creates or reuses it in the run's own feature |
| a folder write scope | a `"kind": "folder", "write": true` dependency is bound in this stack to that path |
| anything this credential cannot read | never — nothing can say which feature it is in |

An `output` dependency is not a write source: bound to a table in another
feature (a bind copied from another stack), its `{{ write() }}` is refused like
any foreign table. A stack inherits no write bind from another. Anything else is
refused `writes_outside_target` — one refusal listing every write it found,
each by file and line (or manifest field), the target, and its feature when
readable — and nothing is written.

**Run edges.** Checked after the writes, on the same paths. A
workflow or saved agent the folder would run is admitted when it is in the
stack's own feature, when it is a row this repository manages in the stack (an
address dependency resolved to it — whatever feature that folder's stack
names), or when a `"run": true` dependency binds it IN THIS STACK; anything
else, an unreadable target included, is refused `runs_outside_target`, naming
the position, the target and its feature. The edges FAIL CLOSED: validate drops
a workflow or agent the caller cannot reach from `resolved` and reports it as an
`unresolved_workflow` / `unresolved_agent` finding instead, so each such
finding's marker is an edge too, and a run-edge leg over the binding cap (a
`too_many_bindings` finding on `workflowIDs` / `agentIDs`) is refused outright.
An address still pending in a fresh stack (its row is created by the same run)
is admitted at plan time in its alias spelling; the push re-gates once it
resolves. ⚠️ The gate decides WHETHER a stack
runs a row, not what the row then does: a bound run edge runs with the target's
own authority, writes included, by design — binding Main's workflow in a Dev
stack lets Dev's runs start it, and it writes what it writes.

**Notes on what the gate admitted.** An admitted write or run bind that lands
in another feature is not refused (the stack decided it), but it is said:
a note naming the position, the target and its feature, and — loudest — any
other stack of the same folder that binds the very same row, which is what a
bind copied between stacks looks like. A row an ADDRESS dependency resolved to
is noted the same way when its claim is in another feature — the folder that
manages it deploys this stack somewhere else, which a stack block copied from
another stack also looks like. A single-folder command prints it (`Note:` on
stderr); `sync apply` puts it on the folder's `notes`, a `--dry-run` included.

**Folder writes, and why they are declared rather than flagged.** A folder is a
path, shared by every feature of the organization, so it can never be "inside"
a stack's feature, and a second stack in the same organization writes the same
folder as the first. A per-run `--allow-folder-writes` would have to be passed
on every push of the first stack too, and would wave through every folder at
once; a declared `folder` write source is decided once per stack, in review,
and never inherited — the same deliberate per-stack bind every write
needs. `ronja import feature` declares each folder its workflows write and
binds it in the importing stack, so the tree deploys back unchanged; a new stack
binds its own path (or the same one, deliberately).

### Limits

- **The gate reads, then the write happens.** `wf publish`, `app publish` and
  `wf test` gate the draft as the server holds it at that moment, and commit or
  run it in a later request. An edit made in the web app between the two is
  committed or run unchecked. Closing it needs the server itself to check a
  managed row's commit, which it does not do today (BL-b3c7); the window is
  the length of one command.
- **Path reuse.** A `moved` / `removed` entry is evaluated by path, and the
  server does not record which row a declaration took away from an address, so
  reusing a path needs the old entry deleted first, as in a legacy folder.
- **A copied folder pushed on its own.** Two folders sharing an id (a `cp -r`)
  are refused by the tree walk, which sees both; a single-folder `push` inside
  the copy sees one folder and is not refused — it deploys against the
  original's claims. Give the copy its own id (delete `"id"`, run `sync adopt`).
- **An anchor that did not record.** The change landed and the command exits
  non-zero naming it; the next run re-reads the server's anchor. The one silent
  consequence: an edit made in the web app that returns a table's SQL to exactly
  the text the stale anchor recorded matches it, reads as no drift, and is
  overwritten by the next apply.
- **A row deleted by `discard --delete-workflow`** leaves its claim `lost`; the
  next apply refuses it, and `sync unmanage` then `sync adopt` clears it.
- **A pipeline header changed only on the server.** A `.sql` file's
  documentation header is part of its SQL, so a description or name changed in
  the web app is pulled into the header and the next apply publishes the table
  once to write it (`pending`). The notice does not cover a docs sidecar's write
  into a table another folder deploys.
- **`sync status`'s undeployed-against-the-lock leg** reads the committed lock
  with no refresh, so for an adopted pipeline folder whose committed lock is
  stale it can name a file as undeployed that the server's anchor knows is live.

## Importing a feature (`ronja import feature`)

```bash
ronja import feature Sales                      # into the current (empty) directory
ronja import feature collection-abc123 ./sales --stack main --json
```

`import feature` turns what already runs in a feature into a repository tree of
**adopted** folders, one per resource, whose references are written as stems,
addresses and declared dependencies instead of this organization's ids — and
claims every row for one stack, so the first `sync status` of the tree is clean
and the first `sync apply` of it sends no write.

**Defaults.** The name is matched EXACTLY among the organization's features (a
private feature is imported by its id — a name could otherwise mean somebody's
private one); a name matching none, several, or only a full page of search hits
is refused. `--stack` defaults to `main`, pinned to the feature's id. The
destination defaults to the current directory and must be empty but for a
`.git` (a directory just `git init`ed, or a clone of an empty repository, is
where a repository starts; the tree is moved in beside it): import never
re-imports into a tree (`sync status` answers "what changed on the server"). A
hidden `.ronja-import-…` directory an interrupted import left there is named on
its own and not removed — nothing tells it from the staging directory of an
import still running into the same place — so remove it and run again.

**Who may import tables.** A table or metric claim needs the **Admin** role, so
the import reads the caller's role (`/me`) before planning. For anyone else it
writes no pipeline folder: every derived table and metric is listed under
`notImported` ("tables and metrics need an Admin to manage; they were declared
as data sources") and declared as a read data source — a write-capable one where
it is written — wherever it is referenced, bound in the importing stack. A
credential that cannot call `/me` (a scoped token) is planned as an Admin; a
claim the server then refuses is withdrawn as step 5 describes.

**The tree.**

```
pipelines/<feature>/     every derived table, its metrics and enabled checks (what `pipeline clone` writes)
workflows/<name>/        one folder per workflow
apps/<name>/             one folder per data app
modules/<name>/          one folder per module
automations/<feature>/   one .json file per automation
```

Names are slugged from the rows' names, collision-safe (`report`, `report-2`).
Connector, foundation, manual and dynamic tables have no SQL a file could hold,
and saved agents, notes, secrets, codexes and MCP servers are not folder kinds:
none of them is imported, each is a data source wherever it is referenced, and
each is listed under `notImported` with why.

**Steps, in this order** (`import.go`):

1. Read the feature's **reference inventory** (`GET /api/v2/feature/:id/references`,
   every page until `next` is absent — a page can be short or empty and still have
   a next one). A kind the token may not read (`omittedKinds`) refuses the import:
   its rows would be left out and their references unrewritten.
2. Read the feature's claims (`GET /managed?feature=`, no folder) **before
   anything else**. A row another folder already manages is not imported;
   references to it become data sources, and the report names it.
3. Read each row through the clone readers — the LIVE row, never the caller's
   draft, since the live row is what gets claimed and anchored — and run the
   portability pass (below). Nothing has been written yet.
4. Write each folder — into a hidden `.ronja-import-…` directory INSIDE the
   destination, whose top-level entries are moved into the destination only
   once every folder is written. A failure part way (the writers read each
   workflow's, app's and module's files and head version as they go) removes
   the staging directory and every entry already moved, so the destination is
   left as it was: empty. The destination itself is never removed or replaced —
   it is the current directory by default, and a shell standing in it keeps
   standing in it — through its clone command's own writer (`writeWorkflowClone`,
   `writeDataAppClone`, `writeModuleClone`, `writePipelineClone`; automations are
   new, see below), mint its folder id (format version 6, 7 where a dependency
   needs it), and record the anchors a push would record: head versions, applied
   titles, a pipeline's live fingerprints and names, each automation's
   `updatedAt`.
5. Claim every row in `POST /managed/claim` batches of at most 200, each row
   reporting its own outcome, then record the anchors on the claims. A row the
   server REFUSES — one of the known refusal codes (`address_claimed`,
   `resource_claimed`, `moved_out`, `trashed`, `not_root`, `not_claimable`,
   `not_found`, `forbidden`, `insufficient_scope`, `invalid`) — leaves the tree,
   as a row claimed elsewhere never entered it, and every folder that held or
   referenced it runs the portability pass again without it and is written
   again (into a staging directory inside the tree, then swapped in): a
   reference to it becomes a data source bound in the importing stack, never an
   address nothing will claim. A folder whose every row was refused is removed.
   That second pass claims nothing, so it cannot be refused anything new and
   the import converges in it. Any other answer — a batch with no per-row
   answer (a refused or lost request), a per-item `internal_error` (a server
   fault the server's docs say to retry), a code this build does not know —
   says nothing about the row: it keeps its lock entry, is reported `unclaimed`
   once, and `ronja sync adopt --stack <name>` in the tree claims it. The anchor
   step leaves an unclaimed row alone rather than claim it behind the report's
   back. Either way the import exits 1.
6. Report.

**The portability pass, from the inventory alone** (`import_plan.go`):

| the reference is… | written as |
|---|---|
| a read of a table the same pipeline folder builds (a `{{ ref }}` in its SQL, a metric's source) | the sibling's stem: `{{ ref('orders') }}`, `"source": "orders"` |
| a read or run of anything imported into ANOTHER folder: table, metric, workflow, app, module | an address dependency (`from` + `address`), no bind |
| `{{ write('table-X') }}` where X is this workflow's own output (`ownOutput` with a logical name) | the name form `{{ write('orders_daily') }}`, declared `"output": true` and bound in the importing stack — own outputs choose their names first, so another table called the same takes `orders_daily_2` instead |
| any other write position: a write marker naming a table the workflow does not own, a codex marker or a codex grant (`read_write`), a table in an agent automation's reference set, a feature there other than its own (`read_write` since the inventory widened it), any write whose target this credential cannot read | a write-capable data source, `"write": true`, bound in the importing stack |
| a read of anything not imported — a connector table, another feature's row, a note, a row claimed by another folder, a row this credential cannot read | a read data source, bound in the importing stack (an unreadable row to the id the reference carries) |
| a secret, a mailbox, an MCP server | a side-effect data source, bound in the importing stack |
| a saved agent, a workflow outside the feature (a `{{ workflow }}` / `{{ agent }}` marker, an app's allowlist, an automation's action or reference) | a run data source, `"run": true`, bound in the importing stack — so the tree deploys back unchanged, and a new stack binds it, or another row, on purpose |
| an automation `references` entry naming the automation's own feature | `"@feature"` |
| an output literal (`tools.replaceTable("table-…")`) | LEFT IN PLACE and reported; its table is declared write-capable all the same |
| an id literal in app source (`useQueue({tableID: "table-…"})`, `queryExternal("secret-…")`) | LEFT IN PLACE and reported |
| a folder write (`{{ file('…', 'write') }}`, `saveFile(…, folder=…)`) | left as written (a path, not a row) and reported; the folder is declared a folder write source (`"kind": "folder", "write": true`) bound to its path in the importing stack, which is what the write gate admits it through |

The alias form is produced two ways, both of which round-trip. In a position the
codec reads both ways — a marker, an `optionsQuery`, a write marker — the
dependency is declared and the server's content is de-aliased through the
folder's own codec, the same `toDisk` every later status runs. In an
outbound-only position — an app's `access` block, a metric's `recipe.source`, an
automation's fields — the alias is written directly. Aliases are taken from the
rows' names (slugged to the alias charset, kept clear of the folder's stems),
falling back to the kind and the id's suffix for a row with no usable name. A
binding with no position in a file — a workflow's explicit secret bindings, a
binding column no marker explains, a folder write scope, a table's input-model
leftovers — is not carried by the folder, exactly as a clone does not carry it,
and each is listed under `notCarried` with what changes where the tree creates
the resource anew (a workflow runs without that secret, a table does not list
that input). Deploying back to the same row leaves such a binding as it is: a
push never sends those fields.

**Automations** are imported for the first time. Each becomes the automation
loop's own curated `.json` file — every field the row holds a value for, ids in
alias form — so `ronja automation status`, `ronja automation push` and `ronja
sync` treat it like one `init` wrote. Each is checked the way a push checks it
(parsed, resolved through the folder's codec, compared with the row) and one that
would not read back with no delta refuses the import before anything is written.
Its `updatedAt` anchor is in the lock, so the drift guard is armed from the first
run.

**The report** (`--json`): `folders` (each with its folder id and rows),
`claims` (one outcome per row: `claimed`, `refused` or `unclaimed`), `literals` (each left-in-place literal with its
folder, file, line, call and target, and the alias an output literal's table was
declared under), `dataSources` (alias, kind, `read` / `write` / `run` /
`side_effect`, and the id it is bound to in the importing stack), `addresses`,
`outputs` (each own output declared by name, and the row the importing stack
binds it to), `claimedElsewhere`, `notImported` (every resource the inventory
lists and no folder holds — a non-derived table, a workflow's output, a saved
agent, a note, a secret, a codex, an MCP server, a derived table or metric a
non-Admin cannot manage, a metric the instance answered no recipe for — each
with why), `notCarried` (each structured-field binding no file carries: folder,
resource, field, target and effect), `problems` and `verdict`. The human report
prints the same sections. Exit 0 when every row was claimed and anchored,
1 otherwise — the tree is written either way.

**What the tree is for.** Import prepares a tree that deploys back to the stack it
was imported for: commit it, and `sync apply --stack main` keeps that feature in
step with the repository.

**Deploying it to a second feature of the same organization** is one `sync apply
--allow-create`, once the new stack is declared. Two limits shape it, and both
belong before the walk-through:

- ⚠️ **One checkout per stack.** The local `.ronja/state.json` baseline is keyed
  by instance and organization, not by stack (BL-4e71), so a checkout
  that has deployed one stack reads another stack of the same organization
  against that stack's baseline and refuses its rows as a repointed binding —
  never writing over them. Switching stacks (dev, then main) in ONE checkout is
  therefore refused; deploy each stack from its own clone. A CI checkout (no
  `.ronja/`) is unaffected. Stacks in different organizations have separate
  baselines and are unaffected too.
- ⚠️ **One live module per organization.** A module's package name is unique
  among an organization's live modules, so every stack deploys the module folder
  to the feature it already lives in, and applying any stack publishes that one
  copy (the `shared_module` warning below).

The stack is declared by hand (one command for it is BL-6179)
in EVERY folder's `ronja.json` — the same `url` and `tenantID` as `main`, the new
feature's id — and its data sources are bound there:

- every READ or side-effect source (a connector table, another feature's table,
  a secret, an MCP server) — to the original rows, usually, or to the new
  stack's own; `ronja bind --stack <new>` proposes name matches for these;
- every WRITE source (`"write": true`, a folder or a feature included) — by
  hand, to a row this stack may write. `ronja bind` never proposes one;
- every RUN source (`"run": true`) — by hand, to the workflow or agent this stack
  may run: the original only when the new stack should really start it.
  `ronja bind` never proposes one either;
- nothing for an address or an own output: those resolve in the new stack
  through the run itself.

Then `sync apply --stack <new> --allow-create` creates every row in the new
feature in dependency order, each reference between imported resources resolving
to the new copy as it is made. Two things in the tree still name the ORIGINAL
feature's rows, and the import report lists both: an id literal in app source
(edit it by hand), and an output literal — which the write gate refuses in the
new stack, naming the line, until it is written as `{{ write('<alias>') }}` so
the stack's bind is what it writes. A module's package name is unique in the
organization, so its folder's new stack names the feature the module already
lives in: every stack of one organization imports the same module.

⚠️ **So applying the new stack publishes the organization's ONE module** — the
copy every feature's workflows import, production's included. `sync apply`
says so loudly whenever it would push a module folder into a feature its other
folders do not all deploy to (or the run holds no other folder): a run-level
warning, `shared_module`, at the top of the human report and under `warnings`
in `--json` (`reason`, `path`, `detail`). It is a warning, not a refusal: one
shared module is the only shape the tree can have, and an edit to it reaches
every stack whichever one you deploy. The warning is computed AFTER the run, so
it tells a planned change from a no-op: "This publishes …" when the module
changes (or the dry run cannot tell), and "Nothing in module … changes in this
run" when the dry run found the live module holding the folder or the apply's
push had nothing to push (`moduleUnchangedIn`). Both say when a change reaches a
consumer: a workflow pins a module version — resolved at its draft save, frozen
at its commit (claude/primitives/MODULE.md, "Version pinning") — so a published
module reaches a workflow only once that workflow is saved and published again.

**Warnings and notes lead with the action.** The `shared_module` warning, the
write gate's notes on a bind admitted outside the stack's feature
(`noteBoundOutside`, `noteManagedOutside`) and the unmanaged-folder notes put
one sentence first — what happened and what to do — and the explanation on the
lines after it: in `--json` after a `\n` in `detail` or the note, and indented
under the first line by the human report (`indentContinuation`). The
`writes_outside_target` / `runs_outside_target` refusals do the same: the first
line names the two ways out, the targets follow, then how to bind one and why.

`TestImportedTreeStandsUpAFreshStackInOneApply` is the import proof's step 2:
the fixture imported, a `dev` stack on an empty feature, one apply — every
reference between imported resources points at the new copies, every data source
at the original rows, every create is claimed, a second apply changes nothing,
and the report named the one TSX literal. `TestSlipScenarioIsRefusedBeforeAnyWrite`
is the slip scenario: refused `writes_outside_target` with no row created.

`TestImportFeatureProof` is the import fixture end to end, against one fake
instance holding every kind (`import_harness_test.go`: the four kind fakes behind
one router, one managed ledger, and the reference inventory as 3a serves it,
paging included): the tree's `sync status` and `sync check` are clean, `sync
apply --stack main` sends no write, the report names exactly the two literals
left in place, and every reference is a stem, an address, an own-output name or a
data source bound in `main`. `TestImportLeavesNoUnreportedLiteral` scans every
written file for an id the report does not name. One test per row of the
table above is in `import_portability_test.go`.

## Design rules

These are load-bearing for the agent use case — please keep them true:

- **Sync verbs yes, resource verbs no.** A stateful filesystem-to-resource loop
  earns a command; wrapping a call does not.
- **A credential handshake creates or stores one credential and prints none.** The
  interactive half of a credential (an echo-off prompt, a browser approval, a
  value pasted on a Ronja page, a provider's consent screen) may be a command; listing, showing, rotating or
  deleting one may not. No value is ever a flag, and no value reaches stdout,
  stderr or an error message. A link that completes a handshake (a login code,
  a provider sign-in) goes to stderr only, never into `--json`.
- **A Ronja token is minted in a person's browser, never by the CLI.** `POST
  /api/v2/authentication/token` refuses every machine credential, the CLI's own
  login included, and no command calls it — a test fails the whole package on
  any request to it. `ronja token create` hands the mint to the browser: it
  opens a pre-filled form the server built, and learns only the new token's id.
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
  app) and which file extension is synced at all (`.sql` for a pipeline, `.py`
  for a module). Let those two disagree and the baseline records a file the walk will
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
                     secret, stdin, bind, sync_*, import*, pull*, managed*,
                     write_gate, address_deps, workflow_*)
internal/api/        HTTP client + the endpoint shapes it mirrors, plus raw.go
                     (transport only, mirrors nothing), query.go and search.go
internal/config/     the CLI config file (os.UserConfigDir()/ronja/config.json),
                     one profile per (instance, organization), mode 0600
internal/wfdir/      the synced folder: ronja.json, ronja.lock.json,
                     .ronja/state.json, local enumeration and the sha256 drift
                     baseline — one Kind per loop (workflow / data app /
                     pipeline / module), and stacks in stack.go / lock.go
internal/tablerefs/  the {{ ref('…') }} grammar: canonicalization to id form,
                     and the input list derived from it
internal/markers/    the marker families the alias layer reads, mirrored from
                     the server's patterns; writes.go is `{{ write() }}`, read
                     on its own, and both rewrite through one splice
```

The workflow commands are one file per verb (`workflow_push.go`,
`workflow_testcmd.go`, `workflow_runcmd.go`, …) with the shared folder plumbing
in `workflow.go` — `test` carries the `…cmd` suffix because `workflow_test.go`
is a test file to the Go toolchain, and `run` matches it rather than colliding by
name with the api package's `workflow_run.go`.
Their endpoint mirrors live in `internal/api/workflow.go`, `workflow_write.go`
and `workflow_run.go`, hand-mirrored against
`backend/api/v2/workflow/handler.go`. The data-app and pipeline commands follow
the same shape (`dataapp_*.go`, `pipeline_*.go`, `module_*.go`, with the shared
folder plumbing in `dataapp.go` / `pipeline.go` / `module.go`); the pipeline
mirrors are `internal/api/table.go` and `table_write.go`, against
`backend/api/v2/feature/api_model.go` and the draft-review route in
`backend/api/v2/governance/`, and the module mirrors are
`internal/api/module.go` and `module_write.go`, against
`backend/api/v2/module/handler.go`.

The module loop shares more than a shape: `head_version.go`'s anchor machinery,
`workflow_push.go`'s `driftVerdict` / `filePreconditions` / `acknowledgedRemote`,
`workflow_discard.go`'s `confirm` and `workflow_publish.go`'s outcome vocabulary
are all used verbatim by `module_*.go`. That reuse is the point — a second copy
of the drift guard would be a second place for "armed exactly where the baseline
vouched" to come to mean something else.

`internal/commands/bind.go` is the one command that belongs to no loop: it is
kind-agnostic, because `dependencies` is a manifest key all three folder kinds
carry, so it is registered at the root and works in whichever folder it is run
from. Its mirror is `internal/api/search.go`, against
`backend/api/v2/search/handler.go` and `backend/lib/search/hit.go` — four fields
of eleven, plus the two server constants whose absence would be misread
(`minQueryLen`, which answers a short term with an empty result and no error, and
the fact that the endpoint has no `codex` or `module` leg at all).

`internal/commands/import*.go` is `ronja import feature`: `import.go` the
command, its steps and the staged write, `import_plan.go` the folder plan and
the portability pass, `import_rewrite.go` the references rewritten into the
form the plan chose, `import_claim.go` the claims and the settle pass over the
rows the server refused, `import_report.go` the report, and
`import_automation.go` the automation files. It writes every folder
through the clone commands' own writers (`cloneHooks` carries what it changes),
and its one mirror is `internal/api/references.go`, against
`backend/engine/erefs/view.go` and `backend/api/v2/feature/api_references.go`.
`internal/commands/address_deps.go` resolves dependencies declared by address.
`internal/commands/pull*.go` is `ronja <kind> pull`: `pull.go` the command,
its plan, refusals and guarded anchor step, `pull_git.go` the git rules that
keep a pull from overwriting work (and the one way the package runs git),
`pull_report.go` the report, `pull_draftkinds.go` the skeleton the workflow,
data-app and module pulls share (`pullDraftKind`, one adapter per kind),
`pull_pipeline.go` and `pull_automation.go` the per-file kinds' renderers;
`unified_diff.go` the diff it prints, and `internal/api/guard.go` the read-only
request guard. `internal/commands/managed_notice.go` is the classic-loop notice,
read off each kind's `managedBy` (mirrored in `internal/api/managed.go` against
`backend/resource/rmanaged/managed_by.go`).

`internal/commands/managed*.go` is an adopted folder's SERVER-SIDE STATE:
`managed.go` reads it into the lock (`refreshManaged`) and settles what it
finds pending (`applyManaged`, `settleManaged`); `managed_anchor.go` records the
folder's and each claim's anchor after every lock save; `managed_describe.go`
words its refusals and notes; `managed_source.go` is the source label and the
foreign-repository check. `internal/api/managed.go` mirrors
`backend/api/v2/managed`, and `internal/wfdir/managed.go` translates a lock
entry to and from an anchor. `sync_adopt.go` is `ronja sync adopt` and
`ronja sync unmanage`. `write_gate.go` is the write gate — every path that puts
code or grants in front of an adopted stack calls one of its `gate*` functions.
`sync_apply_order.go` is `sync apply`'s dependency order and the fresh stack in
one apply; `sync_apply_managed.go` its adopted-folder steps,
`sync_apply_modules.go` the shared-module warning, and `sync_apply_dryrun.go`
the dry run's per-file comparison for pipeline and automation folders.
`internal/wfdir/dependency_shape.go` holds the shape rules of a dependency
declared by address, as a write target, an output or a run edge.

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
eval "$(ronja env)"        # sets RONJA_URL + RONJA_TOKEN; eval consumes the output
eval "$(ronja db env ID)"  # sets PG* incl. PGPASSWORD, for psql and pg_dump
```

Being able to READ a credential and having it ECHOED are different things, and
only the second puts it in a terminal scrollback, a CI log, or an agent
transcript. **Two commands emit the token, and only as something to eval**:
`ronja env` and `ronja db env`. Both print it if you run them bare — that is
unavoidable for a command meant to be eval'd — so both warn on stderr when
stdout is a TTY, and every doc shows them only through `eval`. `ronja db
connect` does not print it at all: the token goes to psql through the child
process's own environment.

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
the developer's real profile. It is also how a non-interactive caller — an agent
signing in once with `--with-token` instead of carrying a token on every command
— keeps its login out of the shared store: `ronja login` makes the profile it
creates current, so the same login without the variable re-points which
organization the developer's own bare `ronja api` reaches.

**A job that publishes is bound by the organization's self-approval policy.**
`wf publish`, `app publish`, `module publish`, `pipeline publish` and `sync
apply` all commit a draft, and the token counts as a person — a PAT as its user,
an API token as the **Admin** who created it. Where the organization has turned
self-approval off, the job cannot commit a draft that person authored, so on a
shared resource one job cannot both push a change and take it live: a different
Admin (or the feature's maintainer) publishes it, or the job submits it for
review. Under the same policy, deleting the job's API token — or the Ronja API
credential that holds it — strands whatever it filed and nobody has decided
yet: a draft, proposal or move request filed by a token that no longer exists
can be approved by no one. Rotating means create-new-then-delete-old, so publish
or withdraw what the job filed first. A PAT files as its user, so deleting one
strands nothing.

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
