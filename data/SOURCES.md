# Data Source Catalog

Research snapshot taken **2026-11-02**. "In repo" means a copy sits under `data/raw/`. Re-fetch everything with `scripts/research/fetch_datasets.sh`.

## 1. IP geolocation and ISP / ASN ownership

| Source | What it gives us | Access | License | Refresh | In repo |
|---|---|---|---|---|---|
| **MaxMind GeoIP2 ISP** (`GeoIP2-ISP.mmdb`) | ASN, AS org, ISP, organization, **mobile_country_code / mobile_network_code** per IP range | Paid; account ID + license key, `geoipupdate` | Commercial EULA | Weekly | No (needs paid key) |
| **MaxMind GeoIP2 City / GeoLite2-City, GeoLite2-ASN** | lat/lon + accuracy radius, country, ASN | Free account + license key (`MAXMIND_ACCOUNT_ID`, `MAXMIND_LICENSE_KEY`) | GeoLite2 EULA (attribution, 30-day update rule) | Twice weekly (City), weekdays (ASN) | No (needs key) |
| **IPinfo Lite** | Country + ASN for all IPv4/IPv6, unlimited API | Free token (`IPINFO_TOKEN`), MMDB/CSV/Parquet | CC BY-SA 4.0 (attribution) | Daily | No (needs token) |
| **IPinfo Core/Plus API** | City lat/lon, ASN, company, carrier (`/carrier` for mobile), privacy flags | Paid token | Commercial | Live | No |
| **DB-IP Lite (City + ASN)** | City-level lat/lon + ASN; key-free fallback | Direct download | CC BY 4.0 | Monthly | Yes, `dbip/` (git-ignored: 63 MB) |
| **iptoasn.com** | IP range → ASN, country, AS name (TSV) | Direct download | PDDL (public domain) | Hourly | Yes, `iptoasn/` |
| **RIPE IPmap** | Router-IP geolocation built from latency, crowdsourced and IXP evidence. Best tool for **infrastructure** IPs. | Free REST: `https://ipmap-api.ripe.net/v1/locate/{ip}/best` | RIPE NCC terms | Live | No (API) |
| **CAIDA Hoiho** | Router **hostname → city/IATA/CLLI + lat/lon** (e.g. `ae-1.r20.lsanca07.us.bb.gin.ntt.net` → Los Angeles) | Free REST: `POST https://api.hoiho.caida.org/lookups` with a JSON list of hostnames. Tested OK. | CAIDA AUA | Ruleset 2024-08 | No (API) |
| **NRO delegated stats** | Country of **registration** for every IP/ASN block (a legal-jurisdiction signal, different from physical location) | Direct download | Public | Daily | Yes, `rir/` |
| **Cloud provider ranges** | Destination IP → cloud provider + **region**. Gives the data-residency answer for cloud endpoints. | Direct: AWS `ip-ranges.json`, GCP `cloud.json`, Azure Service Tags, Oracle, Cloudflare | Public | Daily/weekly | Yes, `cloud/` |

**Accuracy notes (decide the engine design):**
- Commercial DBs are tuned for *end-user* IPs. Backbone router interfaces are often geolocated to the operator's HQ (e.g. every Lumen IP shows up in "Broomfield, CO"). For hops, use this priority: **Hoiho hostname hint > RIPE IPmap > PeeringDB IXP prefix match > MaxMind/IPinfo > DB-IP**. Then run an RTT speed-of-light sanity check (round-trip distance can't exceed about 100 km per ms of RTT in fiber).
- PeeringDB lists **0 facilities** for Lumen AS3356 and AT&T AS7018 (see `curated/infrastructure/tier1_backbones.json`). Their hops can only be placed via rDNS (Hoiho) and IPmap.

## 2. Subsea and terrestrial infrastructure

| Source | What it gives us | Access | License | In repo |
|---|---|---|---|---|
| **TeleGeography Submarine Cable Map** v3 API | 713 cables (`cable-geo.json` MultiLineString per cable), 1,928 landing points, per-cable owners/suppliers/length/RFS/landing list | `https://www.submarinecablemap.com/api/v3/cable/cable-geo.json`, `/landing-point/landing-point-geo.json`, `/cable/all.json`, `/cable/{id}.json` | CC BY-NC-SA 3.0. **Commercial use needs a TeleGeography license.** | Yes, `telegeography/` |
| **AfTerFibre** (NSRC / Open Telecom Data) | African terrestrial fiber routes (status, operator, go-live) | Vector tiles `https://d316kar6yg8hyq.cloudfront.net/africa-fiber/{z}/{x}/{y}.mvt`, cities GeoJSON. The PMTiles archive returned HTTP 502 during research. | Open (check per-layer; CC BY) | Tilejson + cities + AU members |
| **Infrapedia** | Global terrestrial + subsea fiber, data centers, IXPs (crowdsourced) | Account required; API access by agreement | Proprietary/freemium | No |
| **ITU Broadband Maps (BBmaps)** | Terrestrial transmission backbone (fiber/microwave) per country | WMS service at bbmaps.itu.int | ITU terms | No |
| **Open Fibre Data Standard** | Schema for publishing fiber nodes/spans (GeoJSON/NDJSON) | github.com/Open-Telecoms-Data | CC BY | Not a dataset; use as our internal span schema |
| **PeeringDB facilities** | 5,886 colocation facilities with lat/lon. Backbone routers live here, so they are the anchor points for terrestrial paths. | Snapshot | CC0-like (PeeringDB AUP) | Yes, `peeringdb/fac.json.gz` |

**No open global terrestrial long-haul fiber dataset exists.** Tier-1 route maps (Lumen, HE, Zayo) are published as images/PDFs only. Plan: model terrestrial segments as **great-circle or road-following arcs between consecutive geolocated facilities of the same ASN**, labelled with the ASN owner. Draw real geometry only where open (AfTerFibre, national open-data portals). Infrapedia or a commercial feed (e.g. Geo-Tel for US long-haul) is the upgrade path.

## 3. Peering, IXPs and cellular

| Source | What it gives us | Access | License | In repo |
|---|---|---|---|---|
| **PeeringDB** | 35.8k networks, 1,337 IXs, 2,665 IX prefixes (**match a hop IP inside an IX LAN prefix → exact IXP + city**), 66.6k netixlan (which ASN has which IP on which IX), 62.7k netfac, 288 carriers, 76 campuses | `https://www.peeringdb.com/api/{obj}`. Anonymous is rate-limited; use an API key or the daily CAIDA PeeringDB archive. | PeeringDB AUP (open) | Yes, all 12 tables gzipped |
| **CAIDA AS Relationships** | Provider/customer/peer edges plus the transit-free clique (tier-1 list) | Public download | CAIDA AUA | Yes, `caida/20261001.as-rel.txt.bz2` |
| **CAIDA AS-to-Organization** | Groups sibling ASNs (Lumen AS3356/AS209/AS3549…) | Public download | CAIDA AUA | Yes |
| **OpenCellID** | Tens of millions of cell towers (MCC, MNC, LAC/TAC, cell ID, lat/lon, radio type) | Free API key; full dump `https://opencellid.org/ocid/downloads?token=KEY&type=full&file=cell_towers.csv.gz`; per-MCC files | CC BY-SA 4.0 | No (needs key) |
| **MCC-MNC list** (pbakondy/mcc-mnc-list, from Wikipedia/ITU E.212) | 3,094 MCC/MNC → operator, brand, country, bands | GitHub raw | MIT | Yes, `mcc-mnc/` |
| **RIPE Atlas probes** | Probe metadata (ASN, lat/lon, anchors). Ground-truth landmarks and a vantage-point fleet for active measurement. | Public archive + API | RIPE NCC terms | Yes, `ripe-atlas/` |
| **OurAirports** | IATA/ICAO → lat/lon. Decodes city codes in router hostnames. | Direct CSV | Public domain | Yes, `ourairports/` |

**Mobile gateway logic:** a phone's traffic leaves the MNO core at a **PGW/UPF (packet gateway)**, which is often hundreds of km from the user. Detection signals: (a) first public hop's ASN is in an MNO's ASN set, from the MaxMind ISP `mobile_country_code`/`mobile_network_code` fields or the IPinfo carrier API; (b) CGNAT range `100.64.0.0/10` in early hops; (c) device-side MCC/MNC from the OS telephony API (Android `TelephonyManager`, iOS has no public API since iOS 16). The tower location comes from OpenCellID using the device's serving cell ID (Android only). The gateway location comes from the first public hop's IPmap/Hoiho geolocation.

## 4. Data-protection and cyber-law trackers

| Source | Use | Access | Note |
|---|---|---|---|
| **DLA Piper — Data Protection Laws of the World** | Per-country law, transfer, security and breach pages. URL pattern `https://www.dlapiperdataprotection.com/?t={law|transfer|security|breach}&c={ISO2}` (verified). | Free web, no API | Link out; don't scrape-and-republish (copyright). |
| **UNCTAD Global Cyberlaw Tracker** | Enacted / draft / none status for 194 countries across data protection, cybercrime, e-transactions, consumer protection | unctad.org (returned **403 to this sandbox**; works from a browser) | Use the status per country as `law_status` field. |
| **IAPP** (Global Privacy Law & DPA directory; data localization resources) | DPA contacts, law summaries | Partly member-only | Reference only. |
| **EU Commission adequacy list** | Authoritative GDPR adequacy list | Public web | Encoded in `curated/regulations/compliance_zones.json`. |
| **US DOJ Data Security Program** | Countries of concern list | Public web | Encoded in compliance zones. |

Curated output: `data/curated/regulations/jurisdictions.json` (36 jurisdictions, ~80 laws) and `compliance_zones.json` (EU/EEA, adequacy, UK, DOJ, strict localization, plus policy templates).

## 5. Borders / map layers

| Source | Use | License | In repo |
|---|---|---|---|
| **Natural Earth Admin-0 countries (1:50m)** | Country polygons for point-in-polygon jurisdiction lookup and globe overlay | Public domain | Yes |
| **Natural Earth disputed areas (1:10m)** | Mark contested territory (Crimea, Kashmir, etc.) so jurisdiction can be shown as "disputed" | Public domain | Yes |
| **Maritime boundaries (Marine Regions EEZ v12)** | Subsea cable segments → whose EEZ they cross | CC BY 4.0 | No: 120 MB+. Download from marineregions.org when building. |

## Keys the build phase will need

| Env var | For |
|---|---|
| `MAXMIND_ACCOUNT_ID`, `MAXMIND_LICENSE_KEY` | GeoLite2 (free) / GeoIP2 ISP (paid) |
| `IPINFO_TOKEN` | IPinfo Lite (free) or paid tiers |
| `OPENCELLID_TOKEN` | OpenCellID tower dump |
| `PEERINGDB_API_KEY` | Higher rate limits (optional) |
| `RIPE_ATLAS_KEY` | Creating measurements from Atlas probes (optional) |
| TeleGeography commercial license | Required for commercial use of cable data |
