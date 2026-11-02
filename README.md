# PacketPulse

Network Intelligence & Sovereignty: live traceroute paths on a 3D globe, with subsea/terrestrial/cellular infrastructure inference and data-sovereignty compliance checks.

**Status:** research complete; Go ingestion backend in [`backend/`](backend/README.md); Three.js globe in [`frontend/`](frontend/README.md).

| Read this | For |
|---|---|
| [docs/BLUEPRINT.md](docs/BLUEPRINT.md) | Architecture, algorithms, phased plan, risks |
| [data/SOURCES.md](data/SOURCES.md) | Every database/API: license, access, what was collected |
| [docs/schemas/packet-journey.schema.json](docs/schemas/packet-journey.schema.json) | Core data contract (JSON Schema 2020-12) |
| [docs/schemas/examples/](docs/schemas/examples/) | Worked example: Jio 5G (Mumbai) → Tata → subsea → AWS Frankfurt |
| `data/curated/` | Tier-1 backbone table, compliance zones, law directory |
| `data/raw/` + `data/MANIFEST.json` | Downloaded dataset snapshot (2026-11-02) with checksums |
| `scripts/research/fetch_datasets.sh` | Re-download everything (keyed sources via env vars) |
| [backend/](backend/README.md) | Go ingestion service: concurrent traceroutes → pipeline → PacketJourney JSON |
| [frontend/](frontend/README.md) | Three.js globe: instanced latency-coloured arcs, hop picking, camera fly-to, Business Mode |
| `Dockerfile` | All-in-one test deployment (`backend/cmd/packetpulse`): ingestion with data classification, compliance engine and globe on one port |
