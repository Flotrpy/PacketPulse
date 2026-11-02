# PacketPulse

Network Intelligence & Sovereignty: live traceroute paths on a 3D globe, with subsea/terrestrial/cellular infrastructure inference and data-sovereignty compliance checks.

**Status: research phase. Nothing is built yet.**

| Read this | For |
|---|---|
| [docs/BLUEPRINT.md](docs/BLUEPRINT.md) | Architecture, algorithms, phased plan, risks |
| [data/SOURCES.md](data/SOURCES.md) | Every database/API: license, access, what was collected |
| [docs/schemas/packet-journey.schema.json](docs/schemas/packet-journey.schema.json) | Core data contract (JSON Schema 2020-12) |
| [docs/schemas/examples/](docs/schemas/examples/) | Worked example: Jio 5G (Mumbai) → Tata → subsea → AWS Frankfurt |
| `data/curated/` | Tier-1 backbone table, compliance zones, law directory |
| `data/raw/` + `data/MANIFEST.json` | Downloaded dataset snapshot (2026-11-02) with checksums |
| `scripts/research/fetch_datasets.sh` | Re-download everything (keyed sources via env vars) |
