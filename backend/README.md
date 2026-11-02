# PacketPulse ingestion backend (Go)

Runs concurrent traceroutes, turns every hop into a structured payload, infers the transport medium (cellular backhaul, terrestrial fiber, subsea cable, cloud backbone), checks data-residency policies, and stores/streams the result as a **PacketJourney** JSON document.

```
             ┌──────────────── ingest.Coordinator ────────────────┐
 targets ──► │ validate → coalesce duplicates → bounded queue      │
 (CLI/HTTP)  │      → worker pool (N concurrent traces)            │
             └───────┬─────────────────────────────┬───────────────┘
                     │ traceroute.Tracer           │ pipeline.Pipeline
                     ▼                             ▼
   traceroute.Engine (raw ICMP, shared      normalize → resolve → classify → infer_infrastructure
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
| `internal/classify` | **Data classification at ingestion.** Rules tag traffic with data classes (`payment_system_data`, `eu_personal_data`, …). See below. |
| `internal/bridge` | Sends classified journeys to the compliance engine (in-process or a remote `complianced`), which raises alerts into its operational stream. |
| `internal/api` | HTTP API + Server-Sent Events stream. |
| `schema/` | `packet_journey.schema.json` (JSON Schema 2020-12, strict), embedded and used by `schema.Validate`. Example output: `schema/examples/`. |

## Data classification at ingestion (`internal/classify`)

Every journey carries `metadata.data_classification`: which data classes the traced traffic carries, so the compliance engine knows which sovereignty profiles apply (`payment_system_data` → RBI localisation, `eu_personal_data` → GDPR Chapter V, …).

```json
"data_classification": {"labels": ["payment_system_data"], "source": "rules", "rule_ids": ["india-payment-apps"], "app": "upi-payments", "port": 443}
```

- **Explicit labels win.** A trace request's `data_classifications` are used as given (`source: "request"`).
- **Otherwise rules match** on what ingestion knows about the flow, never on payload: destination hostname glob (`*.razorpay.com` also matches `razorpay.com`), destination CIDR, ASN, cloud provider, port, and the agent's `app` label and TLS `sni`. Within a field any value matches; across fields all must match. Every matching rule adds its labels (`source: "rules"`, with `rule_ids`).
- **Untagged traffic** gets `labels: []`, `source: "none"`. No profile governs it; it is not evaluated.
- The `classify` stage runs after `resolve`, so rules can use the destination's ASN and cloud.
- Identical requests with different labels or flow context are never coalesced into one job.

Rules live in a JSON file (`-rules`, default `classification-rules.json`, created from the examples in `internal/classify/defaults.go`) and are changed at runtime without a restart:

| Endpoint | |
|---|---|
| `GET /v1/classification/rules` | current rule set (`ETag` = version) |
| `PUT /v1/classification/rules` | replace, validated and saved atomically; admin bearer token (`INGEST_ADMIN_TOKENS="actor:token"`), `If-Match: "<version>"` optional |
| `POST /v1/classification/test` | dry run: `{"target":"api.razorpay.com","port":443}` → labels and matching rules |

**Alerts from live traffic.** With `-compliance-url http://complianced:8090` (token in `COMPLIANCE_TOKEN`), ingestd sends every classified journey to the compliance engine, which evaluates it and **publishes** `ComplianceViolationAlert`s to `/v1/compliance/alerts` and its SSE stream. The globe's Business Mode, by contrast, only previews.

```bash
sudo ./ingestd -data-dir ../data -app upi-payments -port 443 -compliance-url http://localhost:8090 razorpay.com
```

## All-in-one server (`cmd/packetpulse`) and test deployment

`packetpulse` runs ingestion, the compliance engine and the globe on one port: `/v1/compliance/*` (compliance API), `/v1/*` (ingestion API), `/healthz`, and `/` (the built globe). Classified journeys go straight to the in-process engine, so alerts from live traffic appear in `/v1/compliance/alerts`.

```bash
docker build -t packetpulse .
docker run --rm -p 8080:8080 -e PACKETPULSE_ADMIN_TOKENS="me:$(openssl rand -hex 24)" packetpulse
```

- `POST /v1/traces` always needs a bearer token (`PACKETPULSE_TRACE_TOKENS` or `PACKETPULSE_ADMIN_TOKENS`), and targets resolving to private, loopback, CGNAT or link-local addresses are refused (`-public-targets-only`, on by default). An internet-facing host must not run traceroutes for anonymous callers or map its own network.
- Admin tokens also manage compliance profiles and classification rules. `PACKETPULSE_READ_TOKENS`, if set, protects compliance reads.
- `/healthz` reports `"tracing": false` when the host can't open raw ICMP sockets (most PaaS containers, including Render). The globe and compliance API still work, and **remote agents** supply the traffic: any machine with root runs `ingestd` and pushes each journey to `POST /v1/journeys` (token from `PACKETPULSE_AGENT_TOKENS`). The server classifies journeys the agent left untagged with its own rules, stores them, streams them to the globe and evaluates them:

```bash
sudo FORWARD_TOKEN=<agent token> ./ingestd -data-dir ../data -forward-url https://<your-host> -app upi-payments -port 443 razorpay.com
```
- The image seeds the recorded example journey (Jio Mumbai → AWS Frankfurt, tagged `payment_system_data`), so the globe and alert stream are not empty before the first live trace. Remove `-seed` from the entrypoint for a clean start.
- State (profiles, rules, journeys, alerts) is in memory and `/tmp`, so it resets when the container restarts.

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

## Compliance engine (`internal/compliance`, `cmd/complianced`)

An independent rule engine. Its core package imports nothing else from PacketPulse: it reads a `Packet` (classifications + ordered, timestamped route points) and returns an `Evaluation` with `ComplianceViolationAlert`s (`schema/compliance_alert.schema.json`; example: `schema/examples/gdpr-strict-paris-to-ashburn.evaluation.json`).

| Piece | What it does |
|---|---|
| **Boundary registry** (`registry.go`, `defaults.go`) | Regions are unions of ISO countries, ISO 3166-2 subdivisions (state lines: `us-ca`) and/or custom GeoJSON polygons. Built in: `eu`, `eea` (GDPR territory), `schengen` (border area: includes CH, excludes IE/CY; *not* the GDPR boundary), `gdpr-adequate`, `us-ca`, `us`, `in`, `ru`, `cn-mainland`, `us-doj-concern`. Profiles bind data classifications to regions: `allow_list` or `deny_list` mode, `all_hops` (transit counts) or `destination_only` (storage rules), severity, minimum location confidence, law citation, mitigation template, and exceptions (e.g. adequacy = allow, US = downgrade pending DPF check). Seven profiles ship: GDPR Ch. V strict and adequacy-aware, CCPA/CPRA (informational: CCPA has no localisation rule), RBI payment data, 242-FZ, China DSL/PIPL, and the US DOJ bulk-data rule. |
| **Locator** (`geo.go`) | Point-in-polygon on Natural Earth admin-0 and admin-1 with a 1° grid. Coastal points that fall just offshore at 1:50m snap to the coast within 50 km; points in open water belong to no territory. |
| **Rule engine** (`engine.go`) | Walks the route once per matching profile and raises one alert per *entry* into a non-permitted territory. Re-entry after leaving raises a new alert; staying in the territory does not. Each alert carries the territory (country, subdivision, regions), the exact entry timestamp and how it was derived, the offending point (IP, ASN, operator), the last compliant point, the law (e.g. GDPR Chapter V), the rule text and a concrete mitigation. The mitigation names the nearest registered proxy *inside* the permitted territory, measured from the last compliant point (e.g. "Reroute via EU-isolated AWS eu-central-1 proxy…"). Points below the profile's location confidence give *suspected* alerts one severity lower. |
| **Hot reload** | The registry is an immutable snapshot behind an atomic pointer. Evaluations are lock-free and see either the old or the new definitions, never a mix (race-tested). Updates are validated, written atomically to the store file, audited, then swapped in. No restart. |
| **Adapter** (`adapt`) | `FromInfraMap` turns enrichment output into a route: device, accepted hops (timestamp = trace start + RTT/2), and subsea landing stations on the matched cable, weighted by how many candidate cables land in that country. On the Mumbai example, Egypt is firm (89%) and Saudi Arabia is suspected (23%: only SeaMeWe-4's stretch lands at Jeddah). |
| **Globe endpoints** | `POST /v1/compliance/evaluate/batch` (≤500 packets, `publish:false` = preview without raising alerts), `GET /v1/compliance/regions/{id}/geometry` (simplified GeoJSON borders), `GET /v1/compliance/laws?countries=` (law directory; EU members also get the EU entry). Each evaluated route point carries a `status` (`compliant`/`suspected`/`violation`/`not_evaluated`) and `flagged_by`, so clients can colour whole stretches, not just entry points. Country-level profiles ignore internal state lines; subdivisions matter only for profiles drawn on them (e.g. `us-ca`). |
| **REST API** (`api`) | See the package doc. Admin mutations need a bearer token (`COMPLIANCE_ADMIN_TOKENS="actor:token"`); optimistic concurrency uses `ETag`/`If-Match` (412 on stale writes); `PATCH` is an RFC 7396 merge patch. Invalid definitions return 422, deleting an in-use region returns 409, and alerts stream over SSE. |

```bash
COMPLIANCE_ADMIN_TOKENS="alice:$(openssl rand -hex 24)" go run ./cmd/complianced -listen :8090 -store compliance-registry.json
```

```bash
curl -XPATCH -H "Authorization: Bearer $TOKEN" localhost:8090/v1/compliance/profiles/gdpr-ch5-strict -d '{"regions":["eea"],"severity":"high"}'
```

```bash
curl -XPOST localhost:8090/v1/compliance/evaluate -d '{"inframap": {...}, "classifications": ["sensitive_eu_citizen_data"]}'
```

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
| `POST /v1/traces` | `{target, protocol, ip_version, policy_id, access_type, first_ttl, max_hops, probes_per_hop, timeout_ms, data_classifications, app, sni, port}` → 202 + job |
| `GET /v1/traces/{id}` | job status (`queued`/`running`/`done`/`failed`, warning for partial traces) |
| `GET /v1/journeys/{id}` · `GET /v1/journeys?destination=&verdict=&since=&limit=` | stored journeys |
| `POST /v1/journeys` | accept a journey from a remote agent (agent or admin token; schema-validated) |
| `GET /v1/stream` | SSE, one `journey` event per finished trace |
| `GET /v1/policies` · `GET /v1/schema` · `GET /healthz` | metadata |

Useful flags: `-workers` (concurrent traces, default 8), `-pps` (global probe rate, default 200), `-protocol icmp|udp`, `-access cellular`, `-no-rdns`, `-list-policies`, `-rules`, `-classify a,b` / `-app` / `-sni` / `-port` (batch-mode classification), `-compliance-url`, `-forward-url` (push journeys to a PacketPulse server, token in `FORWARD_TOKEN`), `-public-targets-only`.

## Test

```bash
go test -race ./...
```

The engine tests send real probes to 127.0.0.1 (skipped without raw-socket rights). The pipeline tests replay a Jio 5G (Mumbai) → Tata AS6453 → subsea → Marseille → AWS Frankfurt path and validate every journey against the schema. `UPDATE_EXAMPLES=1 go test ./internal/pipeline -run Example` regenerates the example file. `python3 tools/gen_seed.py` regenerates the seed fixtures from `../data/raw`.

## What the inference can and cannot tell you

- Medium and cable are **inferred** (geography, RTT, ownership), so every segment carries a `confidence` and cables come ranked with probabilities.
- Registry country (`registry_country`) is the address holder, not the router's location. Locations from it get confidence 0.2 and are ignored by medium inference and compliance.
- Without real geolocation databases (MaxMind/IPinfo, RIPE IPmap, CAIDA Hoiho), many backbone hops stay unlocated, and the verdict becomes `inconclusive` rather than a false `compliant`.
