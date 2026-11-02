"""Builds data/curated/infrastructure/submarine_cables.mock.geojson: an
internal dataset mimicking TeleGeography's Submarine Cable Map
(`/api/v3/cable/cable-geo.json`): a FeatureCollection of MultiLineString
features with the same `id`, `name`, `color`, `feature_id` properties, plus
the per-cable details TeleGeography serves separately (owners, RFS year,
length, landing points with ISO2 country and coordinates) merged in.

Geometry is real (TeleGeography snapshot 2026-11-02, CC BY-NC-SA 3.0) for a
curated set of major routes, so matching can be tested on true paths.

    python3 -I backend/tools/gen_cable_mock.py
"""
import gzip
import json
import pathlib
import tarfile

ROOT = pathlib.Path(__file__).resolve().parents[2]
RAW = ROOT / "data" / "raw"
OUT = ROOT / "data" / "curated" / "infrastructure" / "submarine_cables.mock.geojson"

CABLES = [
    # Europe <-> Asia via Red Sea / Suez
    "imewe", "seamewe-4", "seamewe-5", "seamewe-6", "asia-africa-europe-1-aae-1", "2africa", "peace-cable",
    "seacomtata-tgn-eurasia", "europe-india-gateway-eig", "fea", "india-europe-xpress-iex",
    # Transatlantic
    "marea", "dunant", "grace-hopper", "amitie", "atlantic-crossing-1-ac-1", "apollo-east-and-west",
    "flag-atlantic-1-fa-1", "ellalink", "equiano", "south-atlantic-cable-system-sacs", "west-africa-cable-system-wacs",
    # Transpacific / intra-Asia
    "jupiter", "faster", "pacific-light-cable-network-plcn", "unity", "asia-america-gateway-aag-cable-system",
    "southeast-asia-japan-cable-2-sjc2", "asia-pacific-gateway-apg", "sea-us", "hawaiki", "southern-cross-cable-network-sccn",
    "indigo-west", "australia-singapore-cable-asc",
    # Regional
    "tata-tgn-western-europe", "tata-tgn-pacific", "tata-tgn-tata-indicom", "bay-of-bengal-gateway-bbg",
    "i2i-cable-network-i2icn", "tata-tgn-gulf", "gulf-bridge-international-cable-systemmiddle-east-north-africa-cable-system-gbicsmena", "falcon",
]


def country_names():
    ne = json.load(gzip.open(RAW / "naturalearth" / "ne_50m_admin_0_countries.geojson.gz", "rt"))
    m = {}
    for f in ne["features"]:
        p = f["properties"]
        iso = p["ISO_A2"] if p["ISO_A2"] != "-99" else p["ISO_A2_EH"]
        for k in ("NAME", "NAME_LONG", "ADMIN", "FORMAL_EN", "NAME_SORT", "BRK_NAME", "GEOUNIT"):
            if p.get(k) and iso != "-99":
                m[p[k].lower()] = iso
    m.update({"united states": "US", "russia": "RU", "korea, rep.": "KR", "congo, dem. rep.": "CD", "congo, rep.": "CG",
              "côte d'ivoire": "CI", "egypt": "EG", "hong kong": "HK", "taiwan": "TW", "turkey": "TR", "vietnam": "VN",
              "yemen": "YE", "syria": "SY", "iran": "IR", "brunei": "BN", "gambia": "GM", "bahamas": "BS"})
    return m


def main():
    names = country_names()
    geo = json.load(gzip.open(RAW / "telegeography" / "cable_cable-geo.json.gz", "rt"))
    by_id = {f["properties"]["id"]: f for f in geo["features"]}
    lp = json.load(gzip.open(RAW / "telegeography" / "landing-point_landing-point-geo.json.gz", "rt"))
    coords = {f["properties"]["id"]: f["geometry"]["coordinates"] for f in lp["features"]}
    feats, missing = [], []
    with tarfile.open(RAW / "telegeography" / "cables-detail.tar.gz") as t:
        members = {m.name for m in t.getmembers()}
        for cid in CABLES:
            if cid not in by_id or f"cables/{cid}.json" not in members:
                missing.append(cid)
                continue
            d = json.load(t.extractfile(f"cables/{cid}.json"))
            f = by_id[cid]
            lands = []
            for l in d.get("landing_points", []):
                iso = names.get(l["country"].lower())
                if l["id"] in coords and iso:
                    lon, lat = coords[l["id"]]
                    lands.append({"id": l["id"], "name": l["name"], "country": iso, "lat": round(lat, 5), "lon": round(lon, 5)})
            length = (d.get("length") or "").split(" ")[0].replace(",", "")
            feats.append({
                "type": "Feature",
                "properties": {
                    "id": cid,
                    "name": f["properties"]["name"],
                    "color": f["properties"]["color"],
                    "feature_id": f["properties"]["feature_id"],
                    "length_km": float(length) if length.replace(".", "").isdigit() else None,
                    "rfs_year": d.get("rfs_year"),
                    "is_planned": bool(d.get("is_planned")),
                    "owners": [o.strip() for o in (d.get("owners") or "").split(",") if o.strip()],
                    "landing_points": lands,
                },
                "geometry": {
                    "type": "MultiLineString",
                    "coordinates": [[[round(x, 5), round(y, 5)] for x, y in line] for line in f["geometry"]["coordinates"]],
                },
            })
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps({
        "type": "FeatureCollection",
        "name": "packetpulse-submarine-cables-mock",
        "source": "Derived from TeleGeography Submarine Cable Map (CC BY-NC-SA 3.0), snapshot 2026-11-02",
        "features": feats,
    }, separators=(",", ":")))
    print(OUT, OUT.stat().st_size, "bytes,", len(feats), "cables; missing:", missing)


if __name__ == "__main__":
    main()
