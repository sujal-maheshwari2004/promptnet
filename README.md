# PromptNet — Phase 1 (Transport & Serving)

PromptNet is a self-hostable, language-agnostic system for serving prompts to AI
agents over a network — the way a Git server serves repositories. You run the
server, your prompts live in your infrastructure, and your agents fetch them by
URI over gRPC.

This repository is **Phase 1**: the foundation. It is a single Go binary that
**stores**, **validates**, and **serves** prompts, plus a **Python client** for
agents to fetch them. Caching, a full CLI, and pub/sub distribution come in
later phases (see [Roadmap](#roadmap)).

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

`put` flags: `-uri`, `-file` (`-` for stdin), `-slot` (repeatable), `-db`.
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

`PromptClient(host, token=None, tls=False, ca_cert=None)`:

- `host` — `address:port` of the server.
- `token` — sent as the `authorization: Bearer <token>` header; omit if the
  server has no token set.
- `tls` / `ca_cert` — use a TLS connection, optionally pinning a CA certificate.

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
}

message GetPromptRequest  { string uri = 1; }
message GetPromptResponse {
  string uri = 1;
  string template = 2;
  repeated string slots = 3;
  string version_hash = 4;
}
```

Phase 1 is read-only over the wire — writes happen via the `put` CLI. Errors are
returned as standard gRPC status codes:

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
  then send `authorization: Bearer <token>`; the check is constant-time. If the
  variable is unset, the server runs open (useful for local dev).
  > This is a single shared token for now. Per-organization API keys are a
  > later-phase concern.
- **TLS** — pass `-tls-cert` and `-tls-key` to `serve` to terminate TLS. On the
  client, set `tls=True` (and optionally `ca_cert=...`). Without these flags the
  server listens in plaintext.

---

## Storage & version hashing

[internal/store/store.go](internal/store/store.go) uses the pure-Go
`modernc.org/sqlite` driver, so there's no cgo and the binary stays
self-contained. One table:

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
| **v0.1** | gRPC server, Python adapter, validation | **this repo** |
| v0.2 | L1/L2 caching (local + Redis), keyed on `version_hash` | planned |
| v0.3 | `promptctl` CLI: push/pull/commit/diff/promote, Git-backed versioning | planned |
| v0.4 | Pub/sub distribution over NATS, TTL sync, subscriber model | planned |

Also deferred: a PostgreSQL backend for multi-node enterprise deployments
(SQLite covers single-node today).
