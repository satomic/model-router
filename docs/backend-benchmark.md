# Backend performance: Python vs Go

Model Router ships two interchangeable backends. This is the measured case for the Go one, and
the evidence for when the Python one is still the right choice.

Both were run against identical configuration, identical fixtures and the same load generator.
Every number below is reproducible with the commands in [Reproducing the numbers](#reproducing-the-numbers).

---

## The short version

| | Python (FastAPI + uvicorn) | Go | |
|---|---|---|---|
| Throughput, 1 client | 646 req/s | **2,225 req/s** | 3.4× |
| Throughput, 256 clients | 377 req/s | **5,625 req/s** | 14.9× |
| p99 latency, 256 clients | 5,532 ms | **70 ms** | 79× better |
| In-flight capacity (3 s upstream, 1,024 clients) | ~515 held | **~1,008 held** | 2.0× |
| Latency the gateway *adds* at that load | +1,828 ms (+61 %) | **+30 ms (+1 %)** | 61× better |
| Idle memory | 98–120 MB | **31 MB** | 3–4× smaller |
| Cold start | 359 ms | **17 ms** | 21× faster |
| Deployable artefact | 67 MB of runtime + deps | **9.4 MB, one file** | 7× smaller |

The single most important line is the in-flight one, and it is explained in
[Why the gap widens under load](#why-the-gap-widens-under-load).

---

## What was measured, and why

An LLM gateway is not a compute service. Almost all of a request's wall-clock time is spent
waiting on an upstream model — hundreds of milliseconds to tens of seconds. The router's own CPU
work per request is small: parse JSON, apply the routing decision, convert protocols, relay SSE
frames, append a trace.

So raw single-request speed is the *least* interesting number. What decides whether a gateway
holds up is **how many requests it can keep in flight at once while each one waits**. Three
things were measured accordingly:

1. **Throughput and latency against a fast upstream** — isolates the router's own overhead.
2. **In-flight capacity against a slow (3 s) upstream** — the realistic shape of the workload.
3. **Footprint** — memory, start-up time, artefact size: what it costs to run and to ship.

---

## Test environment

| | |
|---|---|
| Host | Apple M5 Pro, 15 cores, 24 GB RAM, macOS 26.6.2 |
| Go | 1.27.1 (darwin/arm64) |
| Python | 3.13.15 — FastAPI 0.141.1, uvicorn 0.52.4, openai 3.13.0, httpx 0.28.1, pydantic 2.13.5 |
| Upstream | A local stdlib stub, so no network variance and no provider rate limit enters the numbers |
| Load generator | Purpose-built in Go — see [A note on the load generator](#a-note-on-the-load-generator) |
| Configuration | Identical `config.yaml` for both: `rule-then-ai` strategy, 3 models, 2 connections, stickiness on |
| Process model | One process each. uvicorn was run **single-worker**, matching the project's own `CMD` |

Each cell ran for 6 seconds (20 seconds for the in-flight test) after a warm-up, with both
backends restarted on clean state beforehand.

---

## 1. Throughput against a fast upstream

Non-streaming `POST /v1/chat/completions`. The upstream answers instantly, so this is the
router's own cost plus its ability to overlap work.

| Concurrency | Go req/s | Python req/s | Go advantage | Go p99 | Python p99 |
|---:|---:|---:|---:|---:|---:|
| 1 | 2,225 | 646 | 3.4× | 0.8 ms | 1.8 ms |
| 4 | 4,033 | 729 | 5.5× | 3.5 ms | 6.3 ms |
| 16 | 4,696 | 665 | 7.1× | 9.7 ms | 49 ms |
| 64 | 5,319 | 505 | 10.5× | 23 ms | 172 ms |
| 256 | 5,625 | 377 | **14.9×** | 70 ms | **5,532 ms** |

Two things stand out, and they matter more than the ratio:

- **Go scales with concurrency; Python does not.** Go goes from 2,225 to 5,625 req/s as clients
  are added. Python peaks at 729 req/s with 4 clients and then *declines* to 377 — adding load
  makes it slower, not faster.
- **Python's tail collapses.** At 256 clients its p99 is 5.5 seconds for a request the upstream
  answered instantly. Go's is 70 ms.

Streaming (SSE) shows the same shape:

| Concurrency | Go req/s | Python req/s | Go p99 | Python p99 |
|---:|---:|---:|---:|---:|
| 1 | 1,520 | 381 | 1.1 ms | 2.9 ms |
| 16 | 1,386 | 433 | 18 ms | 48 ms |
| 64 | 1,432 | 410 | 66 ms | 201 ms |
| 256 | 1,305 | 385 | **290 ms** | **3,915 ms** |

> **Caveat, stated plainly:** the streaming throughput figures are capped by the stub upstream,
> which writes and flushes one SSE frame per character. Both backends are waiting on it, so the
> ~1,400 req/s ceiling is the *fixture's*, not Go's. The latency spread is the meaningful part of
> this table, and the p99 column is where the difference lives.

---

## 2. In-flight capacity — the measurement that matters

This is the realistic test. The upstream deliberately takes **3 seconds** per call, exactly as a
real model does. A backend that genuinely holds N requests open at once will reach N/3 req/s;
one that queues them will not.

| Clients | | Held in flight | Mean latency | p99 | Added by the gateway | RSS |
|---:|---|---:|---:|---:|---:|---:|
| 64 | Go | 64 / 64 | 3,017 ms | 3,029 ms | +17 ms | 56 MB |
| | Python | 62 / 64 | 3,056 ms | 3,117 ms | +56 ms | 112 MB |
| 256 | Go | 254 / 256 | 3,024 ms | 3,063 ms | +24 ms | 60 MB |
| | Python | 218 / 256 | 3,222 ms | 3,483 ms | +222 ms | 126 MB |
| 1,024 | Go | **1,008 / 1,024** | 3,030 ms | 3,167 ms | **+30 ms** | 156 MB |
| | Python | **515 / 1,024** | 4,828 ms | **11,891 ms** | **+1,828 ms** | 179 MB |

Read the 1,024-client row carefully, because it is the whole argument:

- Go keeps **98 %** of the offered concurrency actually in flight and adds **30 ms** to a 3-second
  upstream call — **1 % overhead**, invisible to a user.
- Python keeps **50 %**. The other half queue, and the queuing is not free: the average user waits
  **1.8 extra seconds**, and the unlucky 1 % wait **8.9 extra seconds** — on a call the upstream
  finished in 3.

Neither backend errored. Python did not fall over; it got slow in a way the user feels directly
and that no amount of upstream tuning can fix.

---

## Why the gap widens under load

The cause is not "Go is a faster language". Per request, both do a trivial amount of work. The
cause is the **GIL**.

CPython executes one thread of bytecode at a time. A single uvicorn worker therefore serialises
every request's JSON parsing, protocol conversion, SSE frame handling and trace assembly — no
matter how many requests are notionally concurrent. The waiting on the upstream overlaps fine;
the small slice of real work around it does not. That is precisely the shape of the two tables
above: Python's throughput is flat-to-declining, and latency rises linearly with concurrency
because requests are standing in a queue.

Go has no such serialisation. Each request is a goroutine scheduled across all cores, a parked
one costs a few KB of stack, and the runtime's netpoller holds tens of thousands of idle
connections without a thread each. Throughput rises with concurrency until the CPU is genuinely
busy.

**This is also why "just add workers" only partly helps.** Running uvicorn with N workers would
multiply Python's ceiling by roughly N, at N× the memory (98 MB each) and with N separate
in-process caches — the model-policy memoisation, the GitHub membership TTL cache and the client
pool are all per-process. The shared `data/` directory and its file leases exist precisely so
multiple workers *can* run, so this is a supported configuration; it just costs memory and cache
efficiency to reach a throughput Go gets from one process.

---

## 3. Footprint

| | Python | Go |
|---|---:|---:|
| Idle RSS | 98–120 MB | **31 MB** |
| RSS holding ~1,000 connections | 179 MB | **156 MB** |
| Cold start (launch → `/healthz` 200) | 359 ms | **17 ms** |
| Deployable artefact | 67 MB (interpreter deps) + app | **9.4 MB**, one static binary |
| Container image | 319 MB (`python:3.11-slim`) | **29 MB** (`alpine:3.21`) |
| Runtime dependencies | 30 Python packages | none |

The Go binary carries the built console inside it (`embed`), so the deployable unit is literally
one file. The container image is Alpine plus that binary — no interpreter and no dependency tree,
but a shell kept deliberately so a misbehaving container can still be opened with `docker exec`.

Cold start matters more than it looks: it is what a Kubernetes rollout, a scale-to-zero wake-up
and a crash-loop recovery each pay.

---

## What is *not* better

Stated plainly, because a report that only lists wins is not useful:

- **The Go backend is more code.** 14,510 lines against the Python backend's 8,760 — roughly
  1.7×, which is the ordinary cost of static typing plus explicit error handling. The module
  layout mirrors `app/` one-for-one, so the two are readable side by side, but there is more to
  read.
- **Python is easier to change in a hurry.** Editing `app/main.py` and restarting is faster than
  a compile, and `--reload` makes the loop immediate. For exploratory work on routing logic, the
  Python backend is still the more comfortable place to start.
- **The Python backend remains the reference implementation.** It is what the behaviour was
  specified in. Where the two disagree, the Python one is right by definition until a decision
  says otherwise.
- **These numbers are from one machine.** An M5 Pro with 15 cores flatters a runtime that can use
  all of them. On a 2-core container the absolute figures fall for both, and Go's multiplier
  shrinks — though the in-flight result, which is about concurrency rather than cores, largely
  holds.
- **A real upstream is slower than this fixture**, so at low concurrency the *relative* difference
  disappears into the model's own latency. The gap only shows up under concurrency — which is
  exactly when it matters.

---

## Two deliberate output differences

Behaviour is otherwise identical — verified line-by-line across the whole API surface and by
driving the real console in Chromium against both. Two differences are intentional:

1. **`usage` objects.** Python's OpenAI SDK materialises every field of its typed model, so a
   response carries `"prompt_tokens_details": null` and friends. Go relays the upstream's JSON
   verbatim. Go's is the truer proxy behaviour: it invents no fields and preserves
   provider-specific usage fields the SDK would silently drop.
2. **The final streaming chunk.** Same cause — Python emits `"delta": {"content": null,
   "function_call": null, "refusal": null, ...}`, Go emits what the upstream sent.

Neither field is read by the console or by any OpenAI-compatible client. If byte-identical
responses ever become a requirement, the Go side can pad them; it does not today because
padding would mean discarding real upstream fields.

A third difference runs the other way: on a `config.yaml` with Windows line endings, a console
save from the Python backend rewrites the file to LF, while the Go backend preserves it. The
content both write is identical — see [backends.md](backends.md#configyaml-writes).

---

## Which backend to run

**Run Go when** the deployment serves real concurrent traffic, latency tails matter, memory is
budgeted, or the operational story is better as one static binary — which is to say, in
production.

**Run Python when** you are developing the routing logic itself and want an edit-reload loop, or
when a deployment has a Python toolchain and no Go one and the traffic is low enough that none of
the above bites.

They share `data/`, `config.yaml`, the API and the trace format, so switching is stopping one and
starting the other. There is no migration.

```bash
./run.sh                      # Go (default)
./run.sh --backend python     # Python
```

---

## Reproducing the numbers

The fixtures live outside the repository because they carry test credentials. To rebuild them:

```bash
# 1. A stub upstream that answers instantly, and one that takes 3 s.
python3 verify/verify_stub_upstream.py                 # :8899

# 2. Both backends on separate data directories, same config.
MR_DATA_DIR=/tmp/go-data ./server-go/bin/model-router --port 8010
MR_DATA_DIR=/tmp/py-data python -m uvicorn app.main:app --port 8011

# 3. Create an API key on each (console or POST /v1/keys), then drive load.
#    Any HTTP load generator works; use one that is not itself GIL-bound, or the
#    client becomes the bottleneck and both backends look identical.
```

> **A note on the load generator.** The first run of this benchmark used a threaded Python
> client and reported Go at ~3,700 req/s. Re-running with a Go generator moved that to 5,625
> — the client had been the limit. Any Python-driven measurement of this comparison
> understates the Go side; that is worth knowing before trusting a number from one.

### How behaviour parity was checked

Separately from performance, and worth recording because the numbers above only mean something
if the two backends do the same thing:

- **An API-level parity harness** drove ~60 identical calls against both backends — auth, key
  management, every routing strategy, all four protocol-conversion combinations, streaming,
  interaction folding, error cases, config validation, usage, credits, the SPA fallback — and
  diffed the normalised responses line for line. The only differences are the two
  [documented above](#two-deliberate-output-differences).
- **The real console, in Chromium**, signed in and walked all eleven pages against each backend,
  created an API key through the UI, opened a trace, and round-tripped a configuration save.
  Both runs were clean and produced the same result on every step.

The scripts under [`verify/`](../verify/) are HTTP-level and therefore backend-agnostic by
construction, but they expect a live deployment's own `config.yaml` and model names, so they were
not part of this run. Pointing them at a Go instance on `:8000` is the natural next check for a
real deployment.
