# PacketPulse — Network Intelligence & Sovereignty: Research Blueprint

Status: **research phase, nothing built yet.** Snapshot date 2026-11-02.
Companion files:
- `data/SOURCES.md`: every database/API, its license, access method and what we downloaded
- `data/curated/`: hand-built reference tables (Tier-1 backbones, compliance zones, law directory)
- `docs/schemas/packet-journey.schema.json` + `docs/schemas/examples/`: the core data contract
- `scripts/research/`: the fetch script and the Tier-1 table builder

---

## 0. Findings that shape the design

1. **Traceroute can't see the medium.** No packet says "I'm on a subsea cable". Medium is *inferred* from geography (do two consecutive hops sit on different landmasses?), RTT (does the latency delta fit a known cable's length?) and ownership (is the hop's ASN a cable owner?). The UI must present cables as **ranked candidates with probabilities**, never as fact.
2. **Registry country ≠ physical location.** iptoasn/RIR data marks Tata's Mumbai and Marseille routers as **US** (the block is held by Tata Communications America). Compliance must run on *physical* location (Hoiho rDNS, RIPE IPmap, IXP prefix match). Registry/corporate country is a separate "who controls it" signal (e.g. CLOUD Act reach).
3. **Commercial IP-geo DBs are weak on backbone routers.** They are tuned for end users. Router hops should use this priority: **CAIDA Hoiho (hostname) → RIPE IPmap → PeeringDB IXP prefix → MaxMind/IPinfo → DB-IP**, then an RTT feasibility check. Both Hoiho and IPmap were tested live and work (e.g. `if-be-2-2.ecore1.emrs2-marseille.as6453.net` → Marseille 43.297, 5.381).
4. **Lumen (AS3356) and AT&T (AS7018) publish zero facilities in PeeringDB**, so for these two the rDNS parsing path is essential, not optional.
5. **No open global terrestrial fiber dataset exists.** TeleGeography covers subsea only. AfTerFibre covers Africa. Terrestrial arcs elsewhere are drawn as estimates between known facilities of the same ASN, labelled "estimated route".
6. **TeleGeography data is CC BY-NC-SA.** Commercial launch needs a TeleGeography license (or a switch to Infrapedia/commercial data). This is a hard go-to-market blocker to resolve early.
7. **Pure transit is legally different from transfer/storage.** Under GDPR, routing encrypted packets through a country is generally not treated as a "transfer". Some regimes are stricter (CN important data, US DOJ bulk data rule). The policy engine therefore separates **RISK (transit)** from **VIOLATION (termination/storage)**, and each policy template sets `transit_counts_as_transfer`.
8. **Cellular gateways are far from the user.** Mobile traffic exits at the MNO packet gateway (PGW/UPF), often in another city. The first public hop is the gateway. Device-side MCC/MNC and cell ID are only available from an Android agent (iOS has blocked carrier info since iOS 16).
9. **Recent legal changes to encode** (as of Nov 2026): EU–Brazil mutual adequacy (Jan 2026), UK adequacy renewed to 2031, China CSL amendments + certification route (Jan 2026), Vietnam PDPL (Jan 2026), India DPDP Rules (Nov 2025, phased to 2027), US DOJ bulk-data rule (Apr 2025). EU-US DPF upheld by General Court (Sept 2025) but under renewed EDPB scrutiny (Jul 2026).

---

## 1. Architecture overview

```
 ┌────────────────────────── Edge / Probe Layer ──────────────────────────┐
 │ Desktop agent (Go): eBPF/libpcap flow capture → new-destination trigger │
 │   → scamper / paris-traceroute (UDP+ICMP+TCP443), MTR loop for jitter   │
 │ Android agent (Kotlin): TelephonyManager MCC/MNC/cell-id + traceroute   │
 │ Server-side vantage: RIPE Atlas measurements (API) for reverse paths    │
 └───────────────┬────────────────────────────────────────────────────────┘
                 │ gRPC/QUIC stream (protobuf RawTrace, FlowEvent)
 ┌───────────────▼──────────────── Backend ───────────────────────────────┐
 │ Ingest gateway → NATS JetStream (or Kafka) topics: raw.traces, raw.flows │
 │                                                                         │
 │ Enrichment workers (Go or Rust, stateless, horizontally scaled)         │
 │   1 IP resolver   (in-memory MMDB + LPM tries; Hoiho/IPmap cache)       │
 │   2 Hop locator   (evidence fusion + RTT constraint solver)             │
 │   3 Segment classifier (medium + cable candidates + terrestrial route)  │
 │   4 Cellular resolver (MCC/MNC → operator; OpenCellID tower; PGW)       │
 │   5 Health scorer (latency excess, jitter, loss → green/amber/red)      │
 │   6 Compliance engine (point/line-in-polygon → zones → policy → alerts) │
 │       → topic enriched.journeys, alerts.compliance                      │
 │                                                                         │
 │ Stores: PostgreSQL+PostGIS (reference geo + journeys),                  │
 │         TimescaleDB/ClickHouse (hop metrics time series),               │
 │         Redis (hot IP-enrichment cache, live session fan-out)           │
 │ API: GraphQL/REST for history + WebSocket/SSE for live JourneyUpdate     │
 └───────────────┬────────────────────────────────────────────────────────┘
                 │ WebSocket (JSON / CBOR)
 ┌───────────────▼──────────────── Frontend ──────────────────────────────┐
 │ CesiumJS globe (3D Tiles, terrain, WGS84-accurate polylines, LOD)       │
 │ Layers: arcs | cables | landing pts | IXPs | facilities | towers |      │
 │         jurisdiction polygons | EEZ                                     │
 │ Panels: Path inspector, Segment drill-down, Business Mode, Law Directory│
 └────────────────────────────────────────────────────────────────────────┘

 ┌──────────────── Data Ingestion (batch reference pipelines) ────────────┐
 │ Scheduled jobs (Dagster/Airflow or cron): fetch → validate → diff →     │
 │ load to PostGIS → build MMDB/LPM artifacts → publish versioned bundle   │
 └────────────────────────────────────────────────────────────────────────┘
```

**Globe choice: CesiumJS over raw Three.js.** Cesium gives geodesic polylines, ellipsoid-accurate coordinates, GeoJSON/CZML loading, camera fly-to and level-of-detail for free. That fits "zoom from macro arc to cable path to cell tower". Three.js (via `globe.gl`) is lighter and prettier for the macro view but would need custom LOD and geodesy work. Recommendation: CesiumJS (Apache-2.0; Cesium ion basemap optional, can self-host OSM/Natural Earth tiles).

---

## 2. Data ingestion layer (reference data)

| Pipeline | Source (see SOURCES.md) | Cadence | Output |
|---|---|---|---|
| ip_geo | MaxMind GeoIP2-City + ISP, IPinfo Lite, DB-IP Lite | weekly/daily | MMDB files mounted in workers; version tag |
| ip_asn | iptoasn, CAIDA as-org2info, MaxMind ISP | daily | LPM trie (prefix → asn, org, registry cc) |
| rir | NRO delegated-extended | daily | prefix → RIR, registry country |
| peeringdb | PeeringDB API (12 tables) | daily | `ix`, `ixpfx` (LPM), `fac` (points), `netfac`, `netixlan` (IP→ASN@IX) |
| as_graph | CAIDA as-rel + clique | monthly | provider/peer graph; tier-1 set |
| cables | TeleGeography v3 (geo + per-cable detail) | weekly | `cable` (MultiLineString), `landing_point` (Point), `cable_landing` (m:n), owners parsed → `cable_owner` mapped to ASNs via org-name matching + manual table |
| terrestrial | AfTerFibre tiles, national open data, (Infrapedia if licensed) | monthly | `terrestrial_route` lines |
| cellular | OpenCellID (per-MCC dumps), MCC-MNC list, MaxMind ISP mobile fields | monthly | `cell_tower` (Point, H3 index), `mno` (mcc,mnc → operator, ASNs) |
| cloud | AWS/GCP/Azure/Oracle/Cloudflare ranges | daily | prefix → provider, region → country |
| borders | Natural Earth admin-0 + disputed, Marine Regions EEZ | yearly | `jurisdiction_polygon` (MultiPolygon, simplified + full) |
| law | curated `jurisdictions.json`, `compliance_zones.json`; manual review against DLA Piper/UNCTAD/IAPP | monthly review | `law`, `jurisdiction`, `zone_membership`, `policy_template` |
| landmarks | OurAirports, RIPE Atlas probes/anchors | monthly | IATA/CLLI → lat/lon for hostname decoding; ground-truth set for accuracy tests |

Every bundle is versioned (`dataset_versions` in each journey) so a compliance alert can be reproduced for audit.

### Core relational model (PostGIS)

```
jurisdiction(iso2 PK, name, geom MultiPolygon, eez_geom MultiPolygon, disputed bool)
zone(id PK, label) ; zone_member(zone_id, iso2, condition text)
law(id PK, iso2, short, citation, type, in_force, summary, localization_level, source_url)
policy(id PK, label, allowed_zones[], blocked_zones[], transit_counts_as_transfer bool)

ix(id PK, name, city, country, geom Point) ; ix_prefix(ix_id, prefix cidr)
facility(id PK, name, city, country, geom Point) ; net_facility(asn, fac_id)
net_ix_ip(asn, ix_id, ipaddr inet)
cable(id PK, name, length_km, rfs_year, is_planned, owners text[], geom MultiLineString)
landing_point(id PK, name, country, geom Point) ; cable_landing(cable_id, landing_id)
cable_operator_asn(cable_id, asn, confidence)
terrestrial_route(id PK, operator, source, status, geom LineString)
cell_tower(radio, mcc, mnc, area, cell, geom Point, range_m, samples, h3_r7)
mno(mcc, mnc, operator, brand, country, asns int[])
cloud_prefix(prefix cidr, provider, region, service, country)

journey(id PK, probe_id, created_at, policy_id, verdict, payload jsonb)   -- PacketJourney doc
hop_metric(time, probe_id, dst, ttl, ip, rtt_min, rtt_avg, jitter, loss)  -- Timescale hypertable
compliance_alert(id PK, journey_id, severity, rule, iso2, created_at, ack_by, ack_at)
```

---

## 3. Backend enrichment algorithms

### 3.1 Hop geolocation (evidence fusion)
For each responding hop IP:
1. Private/CGNAT (`10/8`, `172.16/12`, `192.168/16`, `100.64/10`) → `role=lan_gateway|mno_core`, location inherited from source.
2. Collect evidence: Hoiho (rDNS → place/IATA/CLLI), RIPE IPmap `/locate/{ip}/best`, IX prefix match (exact IX city), `netixlan` IP match, MaxMind/IPinfo/DB-IP, cloud prefix.
3. Score each candidate location by `source_weight × confidence`. Weights start as Hoiho 0.9, IX prefix 0.95, IPmap 0.8, MaxMind 0.5, DB-IP 0.3, then get calibrated against RIPE Atlas anchors (known locations).
4. **RTT feasibility filter:** a candidate is rejected if `great_circle(prev_hop, cand) > (minRTT(hop) − minRTT(prev_located_hop)) / 2 × 204 km/ms` with a tolerance. Also check against the source: `dist(source, cand) ≤ minRTT/2 × 204`.
5. If nothing survives, interpolate between neighbours (`location_method=interpolated`) and widen `accuracy_km`.
Cache per IP in Redis (TTL 24h for router IPs).

### 3.2 Segment medium classification
For consecutive located hops A→B:
- `same metro (<50 km)` → `metro_fiber` (or `ixp_fabric` if B is on an IX LAN).
- A and B on different continents/landmasses (Natural Earth polygon test; the straight line crosses ocean over a threshold) → **subsea candidate search**:
  - Landing points within R km (R = 150 + accuracy_km) of A and of B, or of the coastline nearest to each.
  - Cables connecting a landing near A with a landing near B (`cable_landing` join).
  - Score = `w1·owner_match(ASN of A/B ∈ cable owners)` + `w2·rtt_fit(cable segment length vs ΔRTT)` + `w3·recency(rfs_year, not is_planned)` + `w4·capacity prior`. Normalize to probabilities and keep the top 5.
  - Render the selected cable's geometry clipped between the two landings.
- Same landmass, >50 km, both hops in one ASN → `terrestrial_fiber` with `backbone_asn`. Geometry: an AfTerFibre/open route if one runs within a corridor, else a great-circle arc through intermediate `net_facility` points of that ASN.
- Source side: `cellular_ran` and `mno_backhaul` when the agent reports cellular access.
- Inside a cloud ASN (16509, 15169, 8075, 31898, 13335) → `cloud_backbone`.

### 3.3 Cellular resolution
- Device context: MCC/MNC, radio, TAC, cell ID from the Android agent. Tower lat/lon from OpenCellID (`mcc, mnc, area, cell`).
- Operator: MCC-MNC table (e.g. 405/874 → Jio Mumbai). Operator ASNs from MaxMind ISP mobile fields + PeeringDB org name.
- Gateway: first public hop whose ASN ∈ operator ASNs → `mno_gateway`. Located by IPmap/commercial DB. RTT bound from the device sets the max distance.
- Without the Android agent (desktop on a hotspot): detect CGNAT + a mobile ASN (MaxMind ISP `mobile_country_code`) → mark cellular with operator only.

### 3.4 Health colouring (per segment and per path)
- `latency_excess_ratio = ΔRTT / min_feasible_rtt` (feasible = 2·km/204).
- Jitter = stdev of RTT samples (MTR loop, rolling 60 s). Loss from unanswered probes, but **only counted when downstream hops also lose packets** (ICMP rate-limiting at a single router is not real loss).
- Default thresholds (configurable): **green** loss<0.5% & jitter<10 ms & excess<2.0; **amber** loss<2% or jitter<30 ms or excess<3.5; **red** otherwise; **grey** = unlocated/unresponsive.

### 3.5 Compliance engine
1. Classify flow → `data_classification` by policy rules (SNI/domain lists, destination service, port, agent tags). Never inspect payload; this keeps the product itself privacy-safe.
2. For each hop location: point-in-polygon → iso2 (disputed-area overlay → `kind=disputed`).
3. For each segment geometry: line-in-polygon over land + EEZ → `jurisdictions_crossed` (with confidence = min of endpoint confidences × medium confidence; subsea candidates contribute weighted by probability).
4. Expand iso2 → zones (`compliance_zones.json`).
5. Apply the policy: destination outside allowed zones → **critical/violation**. Transit outside → **warning/risk**, or critical if `transit_counts_as_transfer`. Hop in a `blocked_zone` → critical. Location confidence < 0.5 → downgrade to `unknown` with an "insufficient evidence" note (avoids false alarms).
6. Emit an alert with `law_refs` → the Law Directory renders the laws for every jurisdiction in `jurisdictions_touched`.
7. Also emit `corporate_control` entries (destination/transit operator HQ country, e.g. US for AWS → CLOUD Act) as an informational sovereignty signal.

### 3.6 Real-time streaming contract
```json
{ "type": "journey.update", "journey_id": "…", "seq": 7,
  "patch": [{"op": "add", "path": "/hops/5", "value": {"ttl": 6, "ip": "…", "location": {}}}],
  "health": {"segment": 4, "status": "amber"},
  "alerts": [] }
```
The globe draws the source→destination arc immediately (dst geolocated from cloud ranges / DB). It then refines it hop by hop as patches arrive (JSON Patch RFC 6902 over WebSocket). Budget: first arc < 300 ms after the flow is seen; full path < 10 s.

---

## 4. Frontend modules

| Module | Responsibility |
|---|---|
| `globe-core` | Cesium viewer, imagery (self-hosted Natural Earth/OSM), camera, time/animation clock |
| `layers/paths` | Animated arcs per journey (`PolylineGlowMaterial`, colour by health), packet "comet" animation along the path |
| `layers/infrastructure` | Cables (GeoJSON, highlight selected candidate), landing points, IXPs, facilities, terrestrial routes; clustered at low zoom |
| `layers/cellular` | Tower point cloud (H3-aggregated at low zoom), gateway marker, RAN/backhaul dashed segments |
| `layers/jurisdictions` | Country/EEZ polygons, zone fills (EU/EEA, adequacy, DOJ countries of concern), disputed hatching |
| `panels/path-inspector` | Hop table (TTL, IP, rDNS, ASN, location+method+confidence, RTT/jitter/loss) |
| `panels/segment-drilldown` | On click/zoom: medium, cable candidates with probabilities and owners/RFS/length, terrestrial operator, cell tower + gateway |
| `business-mode` | Policy selector, compliance timeline, alert toasts, red pulsing on offending segments, evidence view |
| `law-directory` | Tab listing every jurisdiction touched → laws, localization level, transfer regime, regulator, links (DLA Piper per-country URL) |
| `state` | Zustand/Redux store fed by WebSocket patches; IndexedDB for recent journeys |

Stack: React + TypeScript + Vite, CesiumJS (`resium` bindings), deck.gl optional for dense tower layers.

---

## 5. Implementation plan (phased)

| Phase | Scope | Exit criteria |
|---|---|---|
| **P0 – Data foundation** | Ingestion jobs for all open sources in `data/raw`; PostGIS schema; versioned bundles; MaxMind/IPinfo/OpenCellID keys provisioned; TeleGeography license decision | All tables loaded; nightly refresh green |
| **P1 – Trace + enrich (CLI)** | Desktop agent with scamper; enrichment workers 3.1–3.4; outputs PacketJourney JSON validated against the schema | ≥80% of hops located within 100 km on the RIPE Atlas anchor test set |
| **P2 – Globe MVP** | Cesium macro view, live arcs, health colours, hop inspector | Live trace renders in <10 s |
| **P3 – Drill-down** | Subsea candidate ranking + cable rendering, terrestrial estimates, IXP/facility layers | Cable inference top-1 matches a known ground-truth set (operator-confirmed paths, published looking-glass traces) ≥60%, top-3 ≥85% |
| **P4 – Cellular** | Android agent, OpenCellID towers, gateway detection | Gateway city correct for test SIMs on 3+ MNOs |
| **P5 – Business Mode** | Compliance engine, policy templates, alerts, Law Directory, audit export (PDF/CSV with dataset versions) | Policy test suite (synthetic journeys) passes; legal review sign-off on law dataset |
| **P6 – Hardening** | Multi-tenant auth, RBAC, SSO, retention policies, SOC 2 controls, rate-limit handling for external APIs | Load test: 1k probes × 1 trace/10 s |

---

## 6. Risks and limits to communicate in-product

- **Inference, not observation:** subsea cable and terrestrial route are probabilistic. Always show confidence.
- **Invisible layers:** MPLS tunnels (hidden hops), VPN/SASE/Zero-Trust tunnels, anycast destinations (CDN IPs exist in many cities at once), and asymmetric return paths (traceroute shows forward only). Mitigations: RIPE Atlas reverse measurements, anycast detection via IPmap `simple-anycast` engine, MPLS label display.
- **ICMP filtering/rate-limiting:** some carriers hide hops. Use TCP-443 traceroute as a fallback.
- **Legal content liability:** the law directory is an informational aid, not legal advice. It needs counsel review and a monthly refresh process. Summaries are our own; tracker content (DLA Piper, IAPP) is linked, not copied.
- **Licensing:** TeleGeography (NC), OpenCellID (share-alike), IPinfo Lite (attribution), DB-IP Lite (attribution), MaxMind GeoLite2 EULA. All need an attribution page, and commercial use needs the TeleGeography license.
- **Privacy of the product itself:** flow capture runs on endpoints. Store only metadata (5-tuple, SNI, bytes); never payload. This is GDPR-relevant for the vendor too.

---

## 7. Open questions for you

1. Commercial product or internal/research tool? (Decides the TeleGeography license and Infrapedia spend.)
2. Which agent platforms first: macOS/Windows/Linux desktop, Android, or server-side only?
3. Which policies matter most for launch customers (EU GDPR, India RBI, China, US DOJ)?
4. Budget for paid MaxMind GeoIP2 ISP / IPinfo Plus (better router and carrier data) vs. free tiers?
