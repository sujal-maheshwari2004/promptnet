# PromptNet — Phases 1–4 (Transport, Serving, Caching, Versioning & Distribution)

PromptNet is a self-hostable, language-agnostic system for serving prompts to AI
agents over a network — the way a Git server serves repositories. You run the
server, your prompts live in your infrastructure, and your agents fetch them by
URI over gRPC.

This repository covers **Phases 1–4**: a single Go server binary that **stores**,
**validates**, **serves**, **caches**, and **distributes** prompts over embedded
NATS pub/sub; a **Python client** for agents to fetch and subscribe; and
**`promptctl`**, a git-backed authoring CLI with a semantic propagation diff.

---

## Table of contents

- [What it does](#what-it-does)
- [Why these choices](#why-these-choices)
- [Concepts](#concepts)
- [Toolchain](#toolchain)
- [Build](#build)
- [Quick start](#quick-start)
- [Using the Python client](#using-the-python-client)
- [How the code is organized](#how-the-code-is-organized)
- [The wire protocol](#the-wire-protocol)
- [Validation rules](#validation-rules)
- [Authentication & TLS](#authentication--tls)
- [Storage & version hashing](#storage--version-hashing)
- [Regenerating code from the proto](#regenerating-code-from-the-proto)
- [Roadmap](#roadmap)

---

## What it does

1. **Store a prompt** — you write a prompt template and declare its slots
   (variables), then run `promptnet put`. The prompt is validated and saved to a
   local SQLite database.
2. **Serve it** — `promptnet serve` starts a gRPC server. Agents call
   `GetPrompt(uri)` and get back the template, its slots, and a content hash.
3. **Keep it correct** — every prompt is validated at write time *and* again at
   serve time, so a broken prompt can never reach an agent.

A "prompt" here is a small record:

| Field | Example | Meaning |
| --- | --- | --- |
| `uri` | `promptnet://acme/onboarding/welcome` | unique address of the prompt |
| `template` | `Hi {name}, welcome to {org}!` | the text, with `{slot}` placeholders |
| `slots` | `["name", "org"]` | the variables the template expects |
| `version_hash` | `80ec4e4d...` | sha256 of template+slots; changes when content changes |

The agent fetches the template and fills in the slots itself.

---

## Why these choices

- **gRPC** for transport — strongly typed, fast, with TLS and auth built in. The
  `.proto` file is the single source of truth; both Go and Python clients are
  generated from it, so they can never disagree about the message format.
- **SQLite (pure-Go driver)** for storage — gives real ACID guarantees (a write
  fully succeeds or fully fails) with **zero external dependencies**. The whole
  server is one binary you can copy and run.
- **Validation at every layer** — the same check runs on write and on read.
  A prompt that fails it never enters a valid state.
- **No novel algorithms** — it's gRPC + SQLite + a small validator. The value is
  in the combination and the self-hostability, not in clever code.

---

## Concepts

- **URI** — every prompt has a unique address: `promptnet://org/repo/name`. It is
  stored and looked up as a plain string; nothing parses the parts in Phase 1.
- **Slot** — a `{name}` placeholder in the template. Slots must be declared when
  you store a prompt, and the declared set must exactly match what the template
  uses (see [Validation rules](#validation-rules)).
- **Version hash** — a sha256 over the template and slots. Identical content
  always produces the same hash; any change produces a new one. Phase 2 will use
  it as a cache key, so cache invalidation is automatic.

---

## Toolchain

Phase 1 needs:

- **Go** 1.23+ — <https://go.dev/dl/>
- **buf** — <https://buf.build/docs/installation>
  (this is the only proto tool you need; no `protoc` or plugin installs — `buf`
  uses remote plugins)
- **Python** 3.13 + `pip install grpcio "protobuf>=7.35"` — only for the adapter
  (buf's remote plugin emits recent protobuf gencode; an older runtime errors out)

---

## Build

```sh
make build        # buf generate + go build -> ./promptnet(.exe)
make test         # go test ./...
```

Or by hand:

```sh
buf generate                                   # proto -> Go + Python stubs
go build -o promptnet ./cmd/promptnet          # build the binary
go test ./...                                  # run tests
```

---

## Quick start

```sh
# 1. write a template
echo "Hi {name}, welcome to {org}!" > welcome.txt

# 2. store it (declares the two slots; validation runs here)
./promptnet put -uri promptnet://acme/onboarding/welcome \
  -file welcome.txt -slot name -slot org
# -> stored promptnet://acme/onboarding/welcome (80ec4e4d88e6)

# 3. serve it (set a token to require auth; omit PROMPTNET_TOKEN for open access)
PROMPTNET_TOKEN=secret ./promptnet serve -addr :8443
```

If `put` overwrites an existing prompt, it first runs the
[Semantic Propagation Diff](#semantic-propagation-diff-phase-3) between the stored
version and your edit, prints the report, and **refuses a structural change**
unless you pass `-force`:

```sh
./promptnet put -uri promptnet://x/y/z -file edited.txt
#   ... => structural
# structural change detected; re-run with -force to store   (exit 1)
```

This uses the same `PROMPTNET_EMBED_*` model config as the server (offline
lexical by default).

`put` flags: `-uri`, `-file` (`-` for stdin), `-slot` (repeatable), `-db`,
`-force`, `-embed-url`, `-embed-model`.
`serve` flags: `-addr`, `-db`, `-tls-cert`, `-tls-key`. Auth token comes from the
`PROMPTNET_TOKEN` environment variable.

Trying to store a prompt whose template and slots disagree fails loudly and
writes nothing:

```sh
./promptnet put -uri promptnet://acme/bad/x -file welcome.txt -slot name
# -> validation failed: template uses undeclared slots: org   (exit 1)
```

---

## Using the Python client

```python
import sys
sys.path.insert(0, "adapters/python")   # or pip-install the adapter package

from promptnet import PromptClient

client = PromptClient(host="localhost:8443", token="secret")
prompt = client.get("promptnet://acme/onboarding/welcome")

print(prompt.template)      # Hi {name}, welcome to {org}!
print(list(prompt.slots))   # ['name', 'org']
print(prompt.version_hash)  # 80ec4e4d...

# fill the slots yourself
text = prompt.template.format(name="Sujal", org="Acme")
```

`PromptClient(host, token=None, tls=False, ca_cert=None, cache_ttl=0, nats_url=None)`:

- `host` — `address:port` of the server.
- `token` — sent as the `authorization: Bearer <token>` header; omit if the
  server has no token set.
- `tls` / `ca_cert` — use a TLS connection, optionally pinning a CA certificate.
- `cache_ttl` — L1 cache TTL in seconds (Phase 2); `nats_url` — for `subscribe()`.

### JavaScript (Node)

A Node adapter mirrors the Python one in [adapters/js/client.js](adapters/js/client.js).
It loads the `.proto` at runtime (`@grpc/proto-loader`), so there's no codegen:

```js
const { PromptClient } = require("./adapters/js/client");
const client = new PromptClient({ host: "localhost:8443", token: "secret" });
const prompt = await client.get("promptnet://acme/onboarding/welcome");
console.log(prompt.template, prompt.version_hash);
```

`npm i @grpc/grpc-js @grpc/proto-loader` (and `nats` for `subscribe()`).

---

## How the code is organized

```text
proto/promptnet/v1/prompt.proto   The contract. Defines PromptService.GetPrompt
                                  and the request/response messages. Everything
                                  else is generated from or built around this.

gen/                              Generated Go code (do not edit by hand).
adapters/python/promptnet/        The Python adapter:
  client.py                         hand-written PromptClient wrapper
  v1/                               generated Python stubs (do not edit)

internal/validate/validate.go     Pure function: is this prompt well-formed and
                                  do its slots match the template? The single
                                  source of "valid". Has a unit test.

internal/store/store.go           SQLite-backed storage. Open/Get/Put a prompt,
                                  plus Hash() for the version hash. This is where
                                  ACID durability comes from.

internal/server/server.go         The gRPC handler. GetPrompt looks up the
                                  prompt, re-validates it, and returns it. Also
                                  the auth interceptor that checks the token.

cmd/promptnet/main.go             The CLI entry point. Two subcommands:
                                    serve  - run the gRPC server
                                    put    - validate and store a prompt locally
```

**Request flow (`serve`):** agent → gRPC → `AuthInterceptor` (token check) →
`Server.GetPrompt` → `store.Get` (SQLite) → `validate.Prompt` (serve-time check)
→ response.

**Write flow (`put`):** read template file → `validate.Prompt` (write-time
check) → `store.Put` (computes hash, upserts into SQLite).

---

## The wire protocol

Defined in [proto/promptnet/v1/prompt.proto](proto/promptnet/v1/prompt.proto):

```proto
service PromptService {
  rpc GetPrompt(GetPromptRequest) returns (GetPromptResponse);
  rpc DiffPrompt(DiffPromptRequest) returns (DiffPromptResponse);  // semantic propagation diff
}

message GetPromptRequest  { string uri = 1; }
message GetPromptResponse {
  string uri = 1;
  string template = 2;
  repeated string slots = 3;
  string version_hash = 4;
}

message DiffPromptRequest  { string uri = 1; string new_template = 2; }
// DiffPromptResponse returns the per-hunk three-signal analysis (see below).
```

Writes still happen via the `put` CLI; `GetPrompt` and `DiffPrompt` are the
read/analyze surface. Errors are returned as standard gRPC status codes:

| Code | When |
| --- | --- |
| `NotFound` | no prompt at that URI |
| `Unauthenticated` | missing or wrong token |
| `DataLoss` | the stored prompt failed serve-time validation |
| `Internal` | storage/lookup error |

---

## Validation rules

A prompt is valid when ([internal/validate/validate.go](internal/validate/validate.go)):

1. The URI is non-empty.
2. The template is non-empty.
3. No slot name is empty.
4. Every `{placeholder}` in the template is a declared slot (no **undeclared** slots).
5. Every declared slot actually appears in the template (no **unused** slots).

Placeholders are matched with the pattern `{word}` (letters, digits, underscore).
The same function is the write-time gate (in `put`) and the serve-time gate (in
`GetPrompt`), so the rules can never drift between the two.

---

## Authentication & TLS

- **Auth** — set `PROMPTNET_TOKEN` when starting the server. Every request must
  then send `authorization: Bearer <token>`; the check is constant-time. With no
  tokens configured the server runs open (useful for local dev).
  - **Multiple keys + org scoping** — pass `-tokens-file tokens.txt` with
    `token [org]` lines (`#` comments allowed). A bare token is **admin** (all
    orgs); `token acme` scopes that key to `promptnet://acme/…`. `PROMPTNET_TOKEN`
    is always an admin key.

    ```text
    # tokens.txt
    s3cr3t-admin          # admin: every org
    acme-key      acme    # scoped: only promptnet://acme/…
    ```

    Every RPC (`GetPrompt`, `DiffPrompt`, `PublishPrompt`) checks the URI's org
    against the caller's scope and returns `PermissionDenied` on a mismatch.
    > Authorization is org-prefix scoping only; finer-grained per-prompt or
    > read-vs-write rules aren't modeled yet.
- **TLS** — pass `-tls-cert` and `-tls-key` to `serve` to terminate TLS. On the
  client, set `tls=True` (and optionally `ca_cert=...`). Without these flags the
  server listens in plaintext.

---

## Storage & version hashing

[internal/store/store.go](internal/store/store.go) runs on **SQLite** (pure-Go
`modernc.org/sqlite`, no cgo, single binary — the self-hosted default) or
**PostgreSQL** for multi-node enterprise. The backend is chosen from the `-db`
value: a file path uses SQLite; a `postgres://…` DSN uses Postgres (via `pgx`).
The schema and the upsert are portable across both; only the driver and
placeholder style differ.

```sh
promptnet serve -db promptnet.db                          # sqlite (default)
promptnet serve -db postgres://user:pass@host:5432/prompts  # postgres
```

One table:

```sql
CREATE TABLE prompts (
  uri          TEXT PRIMARY KEY,
  template     TEXT NOT NULL,
  slots        TEXT NOT NULL,   -- JSON array
  version_hash TEXT NOT NULL
);
```

`Put` is an atomic upsert (insert, or update on URI conflict). `Hash` is
`sha256(template + "\0" + slots)`, recomputed on every write so the hash always
reflects current content. The `-db` flag chooses the file (default
`promptnet.db`).

---

## Caching (Phase 2)

Two layers, both opt-in and both keyed by URI with TTL-bounded freshness. Only
**validated** prompts are ever cached, so a malformed prompt can never be served
from cache.

- **L2 (server-side)** — an in-process cache in front of SQLite. Enabled by
  default; tune with `serve -cache-ttl 30s` (`0` disables). A `put` that changes
  a prompt is reflected within the TTL.
- **L1 (client-side)** — an in-process cache in the Python client. Off by
  default; enable with `PromptClient(..., cache_ttl=30)` (seconds).

```python
client = PromptClient(host="localhost:8443", token="secret", cache_ttl=30)
client.get("promptnet://acme/onboarding/welcome")  # first call hits the server
client.get("promptnet://acme/onboarding/welcome")  # served from L1 for 30s
```

By default L2 is **in-process**, keeping the single-binary promise. For
multi-node deployments, point it at **Redis** so instances share one cache:

```sh
promptnet serve -redis-url redis://localhost:6379/0   # or PROMPTNET_REDIS_URL
```

Both implement the same `Cache` interface
([internal/server/cache.go](internal/server/cache.go),
[internal/server/redis_cache.go](internal/server/redis_cache.go)); responses are
stored as marshaled protobuf with the TTL as key expiry, and every op is
best-effort — a Redis hiccup degrades to a cache miss, never a serving error.
A changed prompt produces a new `version_hash`, so content is always
self-identifying; TTL just bounds how long a URI→version mapping can lag.

---

## Semantic Propagation Diff (Phase 3)

A text diff tells you *that* line N changed. This tells you *how far the meaning
shift ripples* through the surrounding prompt — the difference between a safe
local tweak and a structural change that quietly rewires how nearby instructions
relate.

For each changed hunk it measures three signals
([internal/semdiff/semdiff.go](internal/semdiff/semdiff.go)):

1. **Signal 1** — the changed region (a line-level LCS diff hunk).
2. **Signal 2** — semantic delta *at the point of change*: `1 - cosine(old, new)`.
3. **Signal 3** — propagation profile: grow the window outward (±2, ±4, ±6 …),
   **up and down independently**, recomputing the delta at each step until the
   curve **flattens** (propagation stopped) or hits the **file boundary**.

High Signal 2 + flat Signal 3 → **localized tweak**. Signal 3 still high at the
boundary → **structural** (the dangerous one).

The diff runs **server-side**, against the **stored** prompt (the original),
using the embedding model the **operator configured at server startup** — so the
analysis is consistent for everyone and prompts never leave your infrastructure.

**The operator chooses the embedding model when starting the server.** With no
URL the server falls back to an offline, zero-dependency *lexical* embedder
(hashed bag-of-words) — enough to run and demo the mechanics, but it scores
overlap, not meaning. Point `-embed-url` at any OpenAI-compatible endpoint
(Ollama, text-embedding-inference, llama.cpp) for real semantics:

```sh
# real model (also reads PROMPTNET_EMBED_URL / _MODEL / _KEY)
promptnet serve -embed-url http://localhost:11434/v1/embeddings -embed-model nomic-embed-text
```

Then diff an edited template against the stored original through the server:

```sh
promptnet diff -uri promptnet://acme/support/agent -file edited.txt -addr localhost:8443
# change @ new lines 3-3 (old 3-3): replace
#   Signal 2 (point delta): 0.470
#   Signal 3 up:   ±2=0.095 (boundary)
#   Signal 3 down: ±2=0.153 ±4=0.153 (flat)
#   => localized tweak
```

Or from the Python adapter: `client.diff("promptnet://…", edited_template)`.

The same engine powers `promptctl diff` for local authoring (below).

---

## Authoring with `promptctl` (Phase 3)

`promptctl` is the local authoring CLI. Prompts live as `*.prompt` files in a git
repo; **versioning, history and lineage are plain git, embedded via go-git** (no
external git binary needed). promptctl adds the prompt-specific layer: validation,
the semantic propagation diff, and environment promotion. A prompt's URI is its
path — `acme/support/agent.prompt` → `promptnet://acme/support/agent`; slots are
read from the template's `{placeholders}`.

```sh
promptctl commit -m "msg"              # validate every *.prompt, then commit all changes
promptctl diff [ref]                   # semantic propagation diff vs ref (default HEAD)
promptctl log acme/support/agent.prompt   # version lineage of one prompt
promptctl promote acme/x.prompt dev prod  # bring a prompt from branch dev onto prod
promptctl push [-server host:port]     # validate, git push, then publish to the live server
promptctl pull                         # sync with origin
promptctl publish -server host:port [paths…]  # publish to the live server + notify subscribers
```

**Authoring flows into distribution.** `promptctl push -server …` git-pushes
*and* publishes each prompt to the live server, so subscribers are notified in
the same step. Publishing is idempotent server-side (`PublishPrompt` is a no-op
when the version hash is unchanged), so only actually-changed prompts fire a
notification. git stays the source of truth; the server is the live serving copy.

- **commit / push** validate first — a malformed prompt blocks the whole commit.
- **diff** compares each changed `*.prompt` between a ref and the working tree
  using the embedding model from `PROMPTNET_EMBED_*` (offline lexical by default).
- **promote** is the dev → staging → prod model: environments are branches, and
  promoting copies a validated prompt from one branch onto another and commits it.
- **push / pull** use go-git, so they don't read git's credential helper — set
  `PROMPTNET_GIT_TOKEN` for HTTPS remotes; SSH uses your ssh-agent.

---

## Pub/Sub distribution (Phase 4)

Prompt servers are publishers; agents are subscribers. The server embeds a
**NATS** server in-process (still one binary), and the write-through
`PublishPrompt` RPC stores a new version *and* notifies subscribers.

```sh
# server: embedded NATS is on by default (-nats-addr "" disables it)
promptnet serve -nats-addr 127.0.0.1:4222

# publish a new version -> stored, cache invalidated, subscribers notified
promptnet publish -uri promptnet://acme/support/agent -file v2.txt -slot name
# published promptnet://acme/support/agent (1e8284f35650) — subscribers notified
```

Each prompt maps to a NATS subject — `promptnet://acme/support/agent` →
`promptnet.acme.support.agent`. An agent subscribes in one line (push), and the
`cache_ttl` from Phase 2 is the pull side:

```python
client = PromptClient(host="…:8443", cache_ttl=30, nats_url="nats://…:4222")
client.subscribe("promptnet://acme/support/agent",
                 lambda version: print("new version", version))  # needs `pip install nats-py`
```

**Honest consistency model:** this layer is eventually consistent — a push can be
missed (network), so TTL is the convergence guarantee, not the push. Notify is
best-effort; the version is durably stored regardless, and subscribers converge
on their next poll. Same tradeoff as Kafka/NATS generally.

---

## Regenerating code from the proto

Anything under `gen/` and `adapters/python/promptnet/v1/` is generated. After
editing the `.proto`, run:

```sh
buf generate
```

[buf.gen.yaml](buf.gen.yaml) configures the output (Go into `gen/`, Python into
`adapters/python/`) using buf's remote plugins, so you don't install `protoc` or
any plugins locally. Don't hand-edit generated files — they'll be overwritten.

---

## Roadmap

| Version | Ships | Status |
| --- | --- | --- |
| **v0.1** | gRPC server, Python adapter, validation | done |
| **v0.2** | L1/L2 caching (TTL), keyed on `version_hash` | **this repo** |
| **v0.3** | `promptctl` CLI: commit/diff/log/promote/push/pull, go-git versioning | **this repo** |
| **v0.4** | Pub/sub distribution over embedded NATS, TTL sync, subscriber model | **this repo** |

Also deferred: a PostgreSQL backend for multi-node enterprise deployments
(SQLite covers single-node today).
