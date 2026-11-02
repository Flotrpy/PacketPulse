# PacketPulse ingestion backend (Go)

Runs concurrent traceroutes, turns every hop into a structured payload, infers the transport medium (cellular backhaul, terrestrial fiber, subsea cable, cloud backbone), checks data-residency policies, and stores/streams the result as a **PacketJourney** JSON document.

```
             ┌──────────────── ingest.Coordinator ────────────────┐
 targets ──► │ validate → coalesce duplicates → bounded queue      │
 (CLI/HTTP)  │      → worker pool (N concurrent traces)            │
             └───────┬─────────────────────────────┬───────────────┘
                     │ traceroute.Tracer           │ pipeline.Pipeline
                     ▼                             ▼
   traceroute.Engine (raw ICMP, shared      normalize → resolve → infer_infrastructure
   listener + demux, ICMP/UDP, v4/v6,         → compliance → persist
   global pps limiter)                              │
                                                    ▼
                                   store.ReferenceStore / store.JourneyRepository
                                   (in-memory mock seeded with real data;
                                    bulk-loads ../data when present)
```

| Package | Role |
|---|---|
| `internal/traceroute` | **Traceroute abstraction.** `Tracer` interface + native `Engine`. One raw ICMP socket per address family is shared by all traces, and replies are matched to probes by echo id/seq or UDP ports quoted in ICMP errors. All TTLs are probed in parallel (round 0 finds the destination distance, later rounds stop there). Reads RFC 4950 MPLS labels. |
| `internal/ingest` | **Ingestion coordinator.** Validation, bounded queue (`ErrQueueFull` → HTTP 429), worker pool, coalescing of identical in-flight requests, per-trace timeout, partial-trace handling, subscriber fan-out, graceful drain. |
| `internal/pipeline` | **Pipeline stages.** `normalize` (per-TTL hop, ECMP siblings, RTT min/avg/max/stdev/jitter, loss), `resolve` (ASN, concurrent reverse DNS, geolocation with a speed-of-light RTT check, roles), `infer_infrastructure` (medium + ranked subsea cables, health colour), `compliance` (boolean border flags, verdict, alerts), `persist`. Required stages abort; enrichment stages record errors and continue. |
| `internal/store` | **Mock database service layer.** `ReferenceStore` + `JourneyRepository` interfaces with in-memory implementations. Seeded from real data (iptoasn ranges, TeleGeography cables, MNO/cloud ASNs, policies); `LoadDataDir` adds the full research snapshot (~580k IP ranges, 690 cables, ~5k AWS/GCP region prefixes, Natural Earth countries, compliance zones) in about 1 s. Swap in PostGIS later without touching the pipeline. |
| `internal/api` | HTTP API + Server-Sent Events stream. |
| `schema/` | `packet_journey.schema.json` (JSON Schema 2020-12, strict), embedded and used by `schema.Validate`. Example output: `schema/examples/`. |

## Run

Raw sockets need root or `CAP_NET_RAW`.

```bash
cd backend && go build -o ingestd ./cmd/ingestd
```

```bash
sudo setcap cap_net_raw+ep ./ingestd
```

Batch mode writes JSON Lines to stdout:

```bash
./ingestd -data-dir ../data -policy eu-gdpr-strict -source-country DE -source-lat 50.11 -source-lon 8.68 8.8.8.8 1.1.1.1 example.com
```

Service mode:

```bash
./ingestd -serve :8080 -data-dir ../data
```

```bash
curl -XPOST localhost:8080/v1/traces -d '{"target":"1.1.1.1","protocol":"udp","policy_id":"us-doj-bulk"}'
```

| Endpoint | |
|---|---|
| `POST /v1/traces` | `{target, protocol, ip_version, policy_id, access_type, first_ttl, max_hops, probes_per_hop, timeout_ms}` → 202 + job |
| `GET /v1/traces/{id}` | job status (`queued`/`running`/`done`/`failed`, warning for partial traces) |
| `GET /v1/journeys/{id}` · `GET /v1/journeys?destination=&verdict=&since=&limit=` | stored journeys |
| `GET /v1/stream` | SSE, one `journey` event per finished trace |
| `GET /v1/policies` · `GET /v1/schema` · `GET /healthz` | metadata |

Useful flags: `-workers` (concurrent traces, default 8), `-pps` (global probe rate, default 200), `-protocol icmp|udp`, `-access cellular`, `-no-rdns`, `-list-policies`.

## Test

```bash
go test -race ./...
```

The engine tests send real probes to 127.0.0.1 (skipped without raw-socket rights). The pipeline tests replay a Jio 5G (Mumbai) → Tata AS6453 → subsea → Marseille → AWS Frankfurt path and validate every journey against the schema. `UPDATE_EXAMPLES=1 go test ./internal/pipeline -run Example` regenerates the example file. `python3 tools/gen_seed.py` regenerates the seed fixtures from `../data/raw`.

## What the inference can and cannot tell you

- Medium and cable are **inferred** (geography, RTT, ownership), so every segment carries a `confidence` and cables come ranked with probabilities.
- Registry country (`registry_country`) is the address holder, not the router's location. Locations from it get confidence 0.2 and are ignored by medium inference and compliance.
- Without real geolocation databases (MaxMind/IPinfo, RIPE IPmap, CAIDA Hoiho), many backbone hops stay unlocated, and the verdict becomes `inconclusive` rather than a false `compliant`.
