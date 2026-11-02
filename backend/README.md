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

## Infrastructure enrichment (`internal/enrich`, `cmd/infraenrich`)

Takes the raw hop IPs from the traceroute engine and returns one **InfraMap** JSON document (`schema/infra_map.schema.json`; full example: `schema/examples/mumbai-jio-to-aws-frankfurt.inframap.json`).

| Package | Role |
|---|---|
| `enrich/geoip` | IP-to-location middleware. The `Resolver` interface has four implementations: `MaxMind` (GeoIP2/GeoLite2 City, ASN, ISP with mobile MCC/MNC; DB-IP Lite works, same MMDB format), `IPinfo` (standard and Lite APIs, bounded concurrency, retry/backoff honouring Retry-After), `Static` (overrides and recorded replays) and `Chain` (field-level merge by priority, with per-field `sources`). These sit behind `Cache` (LRU + TTL, negative caching, one upstream call per IP even under concurrency). Private/CGNAT/reserved IPs never leave the process. |
| `enrich/gis` | Great-circle maths and a gridded Natural Earth land index. `AnalyzeCrossing` samples the path every 10 km, classifies land/water, and reports water runs. A run is an ocean crossing if it is at least 30 km and not *coastal* (same country on both sides, never more than 100 km offshore: e.g. Mangaluru→Mumbai along the Konkan coast). |
| `enrich/subsea` | Loads cable GeoJSON in TeleGeography's shape (`data/curated/infrastructure/submarine_cables.mock.geojson`: 42 real routes with owners and landings merged in, built by `tools/gen_cable_mock.py`; the raw `cable-geo.json.gz` also loads). A 2° grid finds cables near each hop. Each cable becomes a routable graph (branches snapped, joined across the antimeridian), and Dijkstra gives the along-cable distance between the hops' projections. The score combines RTT fit vs. light speed over backhaul + cable (35%), endpoint proximity (25%), owner match with the hops' AS organisation (20%) and corridor overlap with the great-circle water stretch (20%). Results include the selected cable's route geometry and the landing stations on that stretch. |
| `enrich/mobile` | The MNO registry combines a verified seed, PeeringDB × MCC-MNC name matches (734 operators, lower confidence) and MaxMind ISP mobile codes. Gateway hubs come from PeeringDB: the operator's own facilities, then its IXPs, then the country's main interconnection metros (labelled as an estimate). OpenCellID is supported via API client or CSV dump. |
| `enrich` | `Enricher.Enrich(trace, device)`. It resolves hops concurrently and rejects locations that light speed rules out from the device, or that imply a detour their neighbours' RTTs can't hold. It detects the cellular first leg when the first public hop is an MNO ASN: the leg is flagged `cellular_backhaul` and the gateway is assigned to the nearest regional hub (anchored at the serving cell, then the device fix, then the gateway's GeoIP). It classifies every link (`cellular_backhaul`, `metro_fiber`, `terrestrial_fiber`, `subsea_cable` + cable match) and emits the ordered network→infrastructure `mapping` array. |

```bash
go build -o infraenrich ./cmd/infraenrich
```

Replay the recorded Jio → Tata → AWS trace with real DB-IP answers:

```bash
./infraenrich -data-dir ../data -trace internal/enrich/testdata/mumbai_jio.rawtrace.json \
  -hostnames internal/enrich/testdata/mumbai_jio.hostnames.json \
  -geoip-static internal/enrich/testdata/geoip_dbip_2026-11.recorded.json \
  -device 19.0596,72.8295,1 -device-country IN -access cellular -mcc 405 -mnc 874
```

With real databases/APIs (`-mmdb-isp` adds mobile codes; `-cell LTE:405:874:TAC:CELLID` with `-opencellid-key` locates the serving tower):

```bash
sudo ./infraenrich -target 1.1.1.1 -mmdb-city GeoLite2-City.mmdb -mmdb-asn GeoLite2-ASN.mmdb -ipinfo -rdns
```

`-record-geoip FILE` saves every GeoIP answer so a session can be replayed offline with `-geoip-static`.

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
