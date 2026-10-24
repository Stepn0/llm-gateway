# llm-gateway

A production-style reverse proxy for LLM APIs in Go: per-key token-bucket rate limiting, response caching, retries with backoff, circuit breaker and multi-provider fallback. Zero dependencies.

[![CI](https://github.com/Stepn0/llm-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/Stepn0/llm-gateway/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Stepn0/llm-gateway.svg)](https://pkg.go.dev/github.com/Stepn0/llm-gateway)
[![Go 1.22](https://img.shields.io/badge/go-1.22-00ADD8)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

## Why

LLM APIs are flaky, expensive and rate-limited. Every service that calls one ends up re-implementing the same defensive layer: retry on 503, back off on 429, don't pay twice for the same deterministic prompt, stop hammering a provider that is down, and have somewhere to go when it is. Put that layer in **one gateway** in front of every provider instead of in every client.

`llm-gateway` speaks the OpenAI chat-completions wire format, so anything that can talk to OpenAI (SDKs, LangChain, `curl`) can point at it unchanged. Upstreams can be OpenAI, Azure OpenAI, Groq, Together, vLLM, Ollama or any other OpenAI-compatible endpoint; the gateway swaps in the right API key per provider.

It is also deliberately small (about 1,400 lines of Go excluding tests, standard library only) so the whole thing can be read in one sitting.

## Architecture

```
                         +-----------------------------------------------------------+
                         |                        llm-gateway                        |
                         |                                                           |
 client ---------------->|  rate limit  -->  cache  -->  for each upstream (priority) |
 POST /v1/chat/          |  (token bucket    (LRU+TTL,       |                        |
      completions        |   per API key)     SHA-256 of     |  breaker closed?       |
 Authorization: Bearer   |      |             canonical      |     |                  |
                         |     429            JSON)          |  retry w/ backoff ---->|-----> provider 1 (openai)
                         |  Retry-After        |             |  on 5xx/429/net err    |         |  5xx / timeout
                         |                   HIT: serve      |     |                  |         v
                         |                   from cache      |  all attempts failed   |      breaker opens
                         |                                   |  or breaker open -->   |
                         |                                   v   next upstream  ----->|-----> provider 2 (ollama)
                         |                                                           |
                         |  GET /metrics  (Prometheus text)    GET /healthz           |
                         +-----------------------------------------------------------+
```

Request path, in order:

1. **Rate limit** - the `Authorization: Bearer <key>` value identifies the caller (falls back to client IP). Each key has a token bucket; an empty bucket means `429` with a `Retry-After` header. The provider never sees the request.
2. **Cache** - if the request has `"temperature": 0` (deterministic) or the caller sends `X-Cache: true`, the body is canonicalized (keys sorted, whitespace removed) and hashed with SHA-256. A hit is served straight from memory with `X-Cache: HIT`. Streaming requests always bypass the cache.
3. **Upstream selection** - providers are tried in config order. An upstream whose circuit breaker is open is skipped without a network call.
4. **Retry** - a `5xx`, `429` or network error is retried on the same upstream with exponential backoff and jitter, up to `retry.maxAttempts`. Every failure feeds the breaker; if the breaker trips mid-loop, the loop stops early.
5. **Fallback** - when an upstream exhausts its attempts the next one is tried. If every upstream fails the client gets a `502` whose body lists what went wrong per provider.
6. **Stream** - the response body is copied through as it arrives; SSE responses are flushed after every chunk so tokens reach the client immediately.

## Quickstart

```sh
git clone https://github.com/Stepn0/llm-gateway && cd llm-gateway
make build                      # -> bin/llm-gateway, bin/mock-upstream

cp config.example.json config.json
export OPENAI_API_KEY=sk-...    # keys live in the environment, never in the file
./bin/llm-gateway -config config.json
```

The example config lists OpenAI as the primary provider and a local Ollama as the fallback. Any OpenAI client works against it; the API key you present to the gateway is only used for rate limiting and is replaced with the provider's real key on the way out:

```sh
curl -s http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer my-team-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","temperature":0,"messages":[{"role":"user","content":"What is 2+2?"}]}'
```

Streaming works the same way; add `"stream": true` and use `-N` so curl does not buffer:

```sh
curl -sN http://localhost:8080/v1/chat/completions \
  -H 'Authorization: Bearer my-team-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Stream please"}]}'
```

Or with Docker:

```sh
docker build -t llm-gateway .
docker run --rm -p 8080:8080 -e OPENAI_API_KEY -v $PWD/config.json:/etc/llm-gateway/config.json llm-gateway
```

## Demo: retries, fallback and the breaker in action

No real provider is needed to see the gateway work. `cmd/mock-upstream` is a tiny OpenAI-compatible server that returns a canned completion and can be told to fail a fraction of requests. Below, `primary` fails 60% of the time and `fallback` is healthy; the gateway is configured with 2 attempts per upstream, a breaker that trips after 3 consecutive failures, and a 5 req/s limit with a burst of 5:

```sh
./bin/mock-upstream -addr 127.0.0.1:9001 -name primary -fail 0.6 &
./bin/mock-upstream -addr 127.0.0.1:9002 -name fallback &
./bin/llm-gateway -config demo.json      # see "demo.json" below
```

```json
{
  "listen": "127.0.0.1:8089",
  "rateLimit": { "rps": 5, "burst": 5 },
  "cache": { "size": 256, "ttlSeconds": 60 },
  "retry": { "maxAttempts": 2, "baseBackoffMs": 50, "maxBackoffMs": 500 },
  "breaker": { "failureThreshold": 3, "openSeconds": 10 },
  "providers": [
    { "name": "primary",  "baseURL": "http://127.0.0.1:9001/v1", "timeoutMs": 5000 },
    { "name": "fallback", "baseURL": "http://127.0.0.1:9002/v1", "timeoutMs": 5000 }
  ]
}
```

A deterministic request, sent twice with the JSON keys in a different order the second time:

```
$ curl -si ... -d '{"model":"gpt-4o-mini","temperature":0,"messages":[{"role":"user","content":"What is 2+2?"}]}'
HTTP/1.1 200 OK
Content-Type: application/json
X-Cache: MISS

{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"Hello from primary! You said: \"What is 2+2?\"","role":"assistant"}}], ...}

$ curl -si ... -d '{"messages":[{"content":"What is 2+2?","role":"user"}], "temperature": 0, "model": "gpt-4o-mini"}'
HTTP/1.1 200 OK
Content-Type: application/json
X-Cache: HIT

{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"Hello from primary! You said: \"What is 2+2?\"","role":"assistant"}}], ...}
```

A streaming request is relayed chunk by chunk:

```
$ curl -sN ... -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Stream please"}]}'
data: {"choices":[{"delta":{"content":"Hello"},"index":0}],"id":"chatcmpl-mock","model":"gpt-4o-mini","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":" from"},"index":0}],"id":"chatcmpl-mock","model":"gpt-4o-mini","object":"chat.completion.chunk"}

data: {"choices":[{"delta":{"content":" primary!"},"index":0}],"id":"chatcmpl-mock","model":"gpt-4o-mini","object":"chat.completion.chunk"}
...
data: [DONE]
```

Twelve requests at 4/s against the flaky primary all return `200` to the client, and then ten back-to-back requests from another key show the limiter kick in after the burst of 5:

```
200 200 200 200 200 200 200 200 200 200 200 200
200 200 200 200 200 429 429 429 429 429

$ curl -si ... (one more from the throttled key)
HTTP/1.1 429 Too Many Requests
Content-Type: application/json
Retry-After: 1

{"error":{"message":"rate limit exceeded","type":"rate_limit_exceeded"}}
```

The gateway's structured log (one line per request) shows what happened behind those uniform `200`s. Annotations added on the right:

```
msg=request method=POST path=/v1/chat/completions upstream=primary  status=200 latency=41.67ms  cache=bypass retries=1   # 503 -> retry -> 200
msg=request method=POST path=/v1/chat/completions upstream=primary  status=200 latency=800µs    cache=miss   retries=0   # temperature 0: cached
msg=request method=POST path=/v1/chat/completions upstream=cache    status=200 latency=80µs     cache=hit    retries=0   # served from memory
msg=request method=POST path=/v1/chat/completions upstream=primary  status=200 latency=284.25ms cache=bypass retries=0   # stream, 7 SSE chunks
msg=request method=POST path=/v1/chat/completions upstream=fallback status=200 latency=42.68ms  cache=bypass retries=1   # 503, 503 -> fallback
msg=request method=POST path=/v1/chat/completions upstream=primary  status=200 latency=410µs    cache=bypass retries=0
msg=request method=POST path=/v1/chat/completions upstream=primary  status=200 latency=350µs    cache=bypass retries=0
msg=request method=POST path=/v1/chat/completions upstream=primary  status=200 latency=340µs    cache=bypass retries=0
msg=request method=POST path=/v1/chat/completions upstream=primary  status=200 latency=420µs    cache=bypass retries=0
msg=request method=POST path=/v1/chat/completions upstream=fallback status=200 latency=38.25ms  cache=bypass retries=1   # 503, 503 -> 2 failures
msg=request method=POST path=/v1/chat/completions upstream=fallback status=200 latency=570µs    cache=bypass retries=0   # 503 -> breaker trips, no retry
msg=request method=POST path=/v1/chat/completions upstream=fallback status=200 latency=380µs    cache=bypass retries=0   # primary skipped (open) ...
msg=request method=POST path=/v1/chat/completions upstream=fallback status=200 latency=440µs    cache=bypass retries=0   # ... for the next 10 s
...
msg=request method=POST path=/v1/chat/completions upstream=none     status=429 latency=50µs     cache=bypass retries=0   # rate limited, no upstream call
```

And `GET /metrics` after the run, pasted verbatim:

```
# HELP llm_gateway_requests_total Requests completed, by upstream that served them and HTTP status.
# TYPE llm_gateway_requests_total counter
llm_gateway_requests_total{upstream="cache",status="200"} 1
llm_gateway_requests_total{upstream="fallback",status="200"} 13
llm_gateway_requests_total{upstream="none",status="429"} 6
llm_gateway_requests_total{upstream="primary",status="200"} 7
# HELP llm_gateway_retries_total Retried upstream attempts.
# TYPE llm_gateway_retries_total counter
llm_gateway_retries_total 3
# HELP llm_gateway_cache_requests_total Cache lookups for cacheable requests.
# TYPE llm_gateway_cache_requests_total counter
llm_gateway_cache_requests_total{result="hit"} 1
llm_gateway_cache_requests_total{result="miss"} 1
# HELP llm_gateway_rate_limited_total Requests rejected with 429 by the rate limiter.
# TYPE llm_gateway_rate_limited_total counter
llm_gateway_rate_limited_total 6
# HELP llm_gateway_breaker_skipped_total Upstreams skipped because their circuit breaker was open.
# TYPE llm_gateway_breaker_skipped_total counter
llm_gateway_breaker_skipped_total{upstream="primary"} 10
# HELP llm_gateway_request_duration_seconds End-to-end request latency.
# TYPE llm_gateway_request_duration_seconds histogram
llm_gateway_request_duration_seconds_bucket{le="0.005"} 23
llm_gateway_request_duration_seconds_bucket{le="0.01"} 23
llm_gateway_request_duration_seconds_bucket{le="0.025"} 23
llm_gateway_request_duration_seconds_bucket{le="0.05"} 26
llm_gateway_request_duration_seconds_bucket{le="0.1"} 26
llm_gateway_request_duration_seconds_bucket{le="0.25"} 26
llm_gateway_request_duration_seconds_bucket{le="0.5"} 27
llm_gateway_request_duration_seconds_bucket{le="1"} 27
llm_gateway_request_duration_seconds_bucket{le="2.5"} 27
llm_gateway_request_duration_seconds_bucket{le="5"} 27
llm_gateway_request_duration_seconds_bucket{le="10"} 27
llm_gateway_request_duration_seconds_bucket{le="+Inf"} 27
llm_gateway_request_duration_seconds_sum 0.4143063289999999
llm_gateway_request_duration_seconds_count 27
```

Of 27 requests, 21 reached a provider and succeeded, 1 was a cache hit, 6 were rate limited, 3 upstream attempts were retried, and the primary was skipped 10 times while its breaker was open. The client saw only `200`s and well-formed `429`s.

## Features

| Feature | How it works | Where |
|---|---|---|
| OpenAI-compatible endpoint | `POST /v1/chat/completions`, body and headers forwarded untouched; `Authorization` replaced with the provider's key | `internal/proxy` |
| Per-key rate limiting | Token bucket per `Bearer` key (or client IP), lazy refill, `429` + `Retry-After`; a janitor evicts idle buckets | `internal/ratelimit` |
| Response cache | LRU with TTL; key is SHA-256 of canonical JSON; opt-in via `"temperature": 0` or `X-Cache: true`; `X-Cache: HIT/MISS` header; only `200` responses are stored | `internal/cache` |
| Retries | Exponential backoff with equal jitter on `5xx`, `429` and network errors; `4xx` pass straight through | `internal/proxy` |
| Circuit breaker | One per upstream: closed -> open after N consecutive failures -> half-open after the open window admits a single probe | `internal/breaker` |
| Multi-provider fallback | Providers tried in priority order; open breakers skipped; `502` with per-provider reasons only when all fail | `internal/proxy` |
| Streaming | SSE bodies flushed chunk by chunk; the per-attempt timeout covers time-to-first-byte only | `internal/proxy` |
| Metrics | Hand-rolled counters and a fixed-bucket histogram rendered in Prometheus text format at `/metrics` | `internal/metrics` |
| Structured logs | `log/slog`, one line per request: method, path, upstream, status, latency, cache result, retries | `cmd/llm-gateway` |
| Config | JSON with defaults and strict validation; unknown fields are rejected; API keys come from env vars | `internal/config` |
| Ops | Graceful shutdown on SIGINT/SIGTERM, `/healthz`, distroless Docker image, `Makefile`, CI | `cmd/llm-gateway` |

## Configuration reference

Every field is optional except `providers`. Omitted fields take the default; set a field to `0` explicitly to disable the optional features (`rateLimit.rps`, `cache.size`).

| Field | Default | Meaning |
|---|---|---|
| `listen` | `":8080"` | Address the gateway listens on |
| `rateLimit.rps` | `10` | Sustained requests per second per API key; `0` disables rate limiting |
| `rateLimit.burst` | `20` | Bucket capacity: how many requests a key can send at once |
| `rateLimit.idleEvictSeconds` | `600` | Idle time after which a key's bucket is dropped from memory |
| `cache.size` | `1024` | Maximum cached responses; `0` disables the cache |
| `cache.ttlSeconds` | `300` | Lifetime of a cached response |
| `retry.maxAttempts` | `3` | Attempts per upstream before falling back to the next one |
| `retry.baseBackoffMs` | `100` | Delay before the first retry; doubles each retry |
| `retry.maxBackoffMs` | `2000` | Cap on the backoff delay |
| `breaker.failureThreshold` | `5` | Consecutive failures that open an upstream's breaker |
| `breaker.openSeconds` | `30` | How long an open breaker rejects requests before letting one probe through |
| `providers[].name` | required | Label used in logs and metrics; must be unique |
| `providers[].baseURL` | required | Base URL including the version prefix, e.g. `https://api.openai.com/v1`. `/v1/<rest>` on the gateway maps to `<baseURL>/<rest>` |
| `providers[].apiKeyEnv` | `""` | Name of the environment variable holding the provider's API key. Empty means no `Authorization` header is sent (local Ollama, vLLM) |
| `providers[].timeoutMs` | `30000` | Per-attempt time allowed for the upstream to start responding |

See [`config.example.json`](config.example.json) for a complete file.

## Metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `llm_gateway_requests_total` | counter | `upstream`, `status` | Completed requests; `upstream` is the provider that answered, `cache`, or `none` for requests the gateway rejected itself |
| `llm_gateway_retries_total` | counter | | Upstream attempts that were retries |
| `llm_gateway_cache_requests_total` | counter | `result` | `hit` or `miss` for cacheable requests |
| `llm_gateway_rate_limited_total` | counter | | Requests answered with `429` by the limiter |
| `llm_gateway_breaker_skipped_total` | counter | `upstream` | Times an upstream was skipped because its breaker was open |
| `llm_gateway_request_duration_seconds` | histogram | | End-to-end latency, buckets from 5 ms to 10 s |

## Design decisions

**Token bucket, not a sliding window.** A token bucket is two floats per key (tokens, last-refill time) and refills lazily on access, so the hot path is one map lookup and a few arithmetic operations under a mutex. It gives exactly the semantics you want for LLM traffic: allow a burst (a batch job firing off 20 prompts) while enforcing a sustained rate. The `Retry-After` value falls out of the arithmetic for free. A sliding-window log would need a timestamp per request and give no better fairness.

**Hash the canonical JSON, not the raw body.** SDKs serialize the same request differently (key order, whitespace, `1.0` vs `1`), so hashing raw bytes would miss most real duplicates. The body is decoded with `UseNumber` (so large integers like seeds survive) and re-encoded by `encoding/json`, which sorts object keys. SHA-256 keeps the key fixed-size and collision-free without storing the full prompt in the key.

**Cache only when the caller says it is safe.** A sampled completion is not idempotent, so the gateway never caches by default. `"temperature": 0` is the OpenAI idiom for "deterministic", and `X-Cache: true` lets a caller opt in for anything else (say, embeddings-style workloads where they accept staleness). Streams are never cached: the value of a stream is latency, and buffering it to cache it would defeat the point. Error responses are not cached either, so a transient upstream failure cannot poison the cache.

**One breaker per upstream, not one global.** Providers fail independently. A global breaker would take the healthy fallback offline the moment the primary degraded. The half-open state admits exactly one probe, so a recovering provider gets one request rather than a thundering herd of them.

**Retries exhaust an upstream before falling back.** Transient blips (a single 503, a dropped connection) are far more common than outages, and a retry on the same provider keeps latency and billing predictable. The breaker makes the loop short-circuit once a provider is clearly down: a failure that trips the breaker ends the retry loop immediately and the request moves to the next provider.

**Equal jitter on the backoff.** Backoff spreads retries over time; jitter spreads them across clients. "Equal jitter" (half fixed, half random) keeps a floor on the delay so retries never collapse into an immediate hammer while still decorrelating them.

**Timeouts cover time-to-first-byte for streams.** A 30 s timeout on the whole exchange would kill any long completion. The per-attempt timer cancels the request if the upstream has not started responding; once headers arrive on a streaming response the timer is stopped and the body runs until the upstream finishes or the client disconnects. Non-streaming responses keep the full-exchange bound.

**`Accept-Encoding` is stripped on the way out.** Letting Go's transport negotiate gzip means the gateway always holds identity-encoded bodies, so a cached response can be served to any client regardless of what encoding it asked for.

**Keys from the environment, strict config.** API keys never touch the config file, so the file can be committed. The decoder rejects unknown fields so a typo like `"listne"` fails at startup rather than silently running with the default.

### Deliberately left out

- **Authentication of callers.** The `Bearer` token is used as a rate-limit key, not verified. Put the gateway behind your ingress auth, or add a key allow-list; it is a ten-line change in `clientKey`.
- **Distributed state.** Rate limits, cache and breaker state are per process. A multi-replica deployment behaves as N independent gateways, which is often fine; moving the limiter and cache to Redis is the obvious next step and is isolated behind the package APIs.
- **Model routing and request rewriting.** The gateway forwards what it gets. Mapping `gpt-4o` to `llama3` on the fallback is policy that belongs in the caller or a thin layer above.
- **Token counting and cost accounting.** Would require model-specific tokenizers; `usage` from the upstream response is passed through untouched for callers that want it.

## Testing

Each package has its own table of unit tests and the proxy is tested end to end against `httptest` servers. Time-dependent logic (bucket refill, TTL expiry, breaker open window) uses an injected clock, so the whole suite runs in a few seconds with no `time.Sleep`.

```
$ go test -race -count=1 ./...
ok  	github.com/Stepn0/llm-gateway/internal/breaker	1.014s
ok  	github.com/Stepn0/llm-gateway/internal/cache	1.013s
ok  	github.com/Stepn0/llm-gateway/internal/config	1.015s
ok  	github.com/Stepn0/llm-gateway/internal/metrics	1.023s
ok  	github.com/Stepn0/llm-gateway/internal/proxy	6.149s
ok  	github.com/Stepn0/llm-gateway/internal/ratelimit	1.012s
```

What is covered:

- `ratelimit`: burst admitted, over-limit denied with the right `Retry-After`, refill over time, cap at burst, key isolation, idle eviction.
- `cache`: hit/miss, LRU eviction order, overwrite refreshes recency, TTL expiry, canonical keys (key order, whitespace, big integers), invalid JSON rejected.
- `breaker`: closed -> open at threshold, success resets the count, half-open admits one probe, probe success closes, probe failure reopens and restarts the window.
- `metrics`: exact Prometheus text output for counters, labelled counters (with escaping) and histograms; concurrent updates are not lost.
- `config`: defaults, full parse, keys from env, explicit zero disables, every validation error, unknown fields.
- `proxy`: headers and body forwarded, `Authorization` rewritten, retry on 503 then success (with backoff bounds), fallback on 5xx and on connection refused, `502` when all fail, `4xx` not retried, `429` with `Retry-After`, cache miss then hit across key order, errors not cached, breaker opens and the upstream is skipped, breaker stops the retry loop, SSE streamed and flushed and not cached, upstream timeout falls back, oversized bodies rejected, metrics recorded.

```sh
make lint   # go vet + gofmt check
make test   # go test -race
make build  # bin/llm-gateway, bin/mock-upstream
```

## Roadmap

- [ ] Redis-backed rate limiter and cache for multi-replica deployments
- [ ] Per-key quotas (requests per day, tokens per month) using the upstream `usage` field
- [ ] Model aliasing per provider (`gpt-4o` -> `llama3.1:70b` on the fallback)
- [ ] Request hedging: fire the fallback after a latency threshold instead of waiting for failure
- [ ] `/v1/embeddings` and `/v1/completions` passthrough with the same policy
- [ ] Hot config reload on SIGHUP

## License

MIT, see [LICENSE](LICENSE).
