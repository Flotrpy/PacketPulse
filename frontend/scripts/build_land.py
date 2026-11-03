"""Builds public/data/land.json from Natural Earth 1:50m admin-0 countries
(data/raw/naturalearth). Rings are simplified (Douglas-Peucker) and quantised
to 0.01 degrees so the globe ships its own basemap (~0.5 MB, no tile server).

    python3 -I frontend/scripts/build_land.py

Output: {"countries":[{"iso2":"FR","rings":[[lon*100,lat*100,...],...]}]}
"""
import gzip
import json
import pathlib

ROOT = pathlib.Path(__file__).resolve().parents[2]
SRC = ROOT / "data" / "raw" / "naturalearth" / "ne_50m_admin_0_countries.geojson.gz"
OUT = ROOT / "frontend" / "public" / "data" / "land.json"
TOL = 0.06  # degrees


def perp_dist(p, a, b):
    (x, y), (x1, y1), (x2, y2) = p, a, b
    dx, dy = x2 - x1, y2 - y1
    if dx == dy == 0:
        return ((x - x1) ** 2 + (y - y1) ** 2) ** 0.5
    t = max(0, min(1, ((x - x1) * dx + (y - y1) * dy) / (dx * dx + dy * dy)))
    return ((x - x1 - t * dx) ** 2 + (y - y1 - t * dy) ** 2) ** 0.5


def simplify(pts, tol):
    # Iterative Douglas-Peucker.
    if len(pts) < 4:
        return pts
    keep = [False] * len(pts)
    keep[0] = keep[-1] = True
    stack = [(0, len(pts) - 1)]
    while stack:
        i, j = stack.pop()
        best, idx = 0, -1
        for k in range(i + 1, j):
            d = perp_dist(pts[k], pts[i], pts[j])
            if d > best:
                best, idx = d, k
        if best > tol:
            keep[idx] = True
            stack += [(i, idx), (idx, j)]
    return [p for p, k in zip(pts, keep) if k]


def main():
    fc = json.load(gzip.open(SRC, "rt"))
    out = []
    for f in fc["features"]:
        p = f["properties"]
        iso = p["ISO_A2"] if p["ISO_A2"] != "-99" else p["ISO_A2_EH"]
        g = f["geometry"]
        polys = g["coordinates"] if g["type"] == "MultiPolygon" else [g["coordinates"]]
        rings = []
        for poly in polys:
            for ring in poly[:1]:  # outer rings only; holes are negligible at globe scale
                s = simplify(ring, TOL)
                if len(s) < 4:
                    continue
                flat = []
                for lon, lat in s:
                    flat += [round(lon * 100), round(lat * 100)]
                rings.append(flat)
        if rings:
            out.append({"iso2": iso if iso != "-99" else "", "name": p["NAME"], "rings": rings})
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps({"source": "Natural Earth 1:50m (public domain)", "scale": 100, "countries": out}, separators=(",", ":")))
    print(OUT, OUT.stat().st_size, "bytes,", sum(len(c["rings"]) for c in out), "rings")


if __name__ == "__main__":
    main()
