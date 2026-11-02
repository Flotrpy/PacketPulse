"""Builds data/curated/infrastructure/tier1_backbones.json from the raw
PeeringDB snapshot and CAIDA AS-relationship file.

Run from the repo root:  python3 -I scripts/research/build_tier1_backbones.py
"""
import bz2
import collections
import gzip
import json
import pathlib

ROOT = pathlib.Path(__file__).resolve().parents[2]
RAW = ROOT / "data" / "raw"
OUT = ROOT / "data" / "curated" / "infrastructure" / "tier1_backbones.json"


def pdb(table):
    return json.load(gzip.open(RAW / "peeringdb" / f"{table}.json.gz", "rt"))["data"]


# asn: (display name, HQ country, rDNS domains, hostname pattern hint)
META = {
    3356: ("Lumen Technologies (ex-Level 3/CenturyLink)", "US", ["level3.net", "lumen.tech", "centurylink.net"], "ae-2-3602.ear2.Frankfurt1.Level3.net -> <if>.<role><n>.<City><n>.Level3.net"),
    6939: ("Hurricane Electric", "US", ["he.net"], "100ge0-35.core2.fra1.he.net -> <if>.core<n>.<iata><n>.he.net"),
    6453: ("Tata Communications", "IN", ["as6453.net"], "if-ae-2-2.tcore1.ldn-london.as6453.net -> <if>.<role><n>.<code>-<city>.as6453.net"),
    1299: ("Arelion (ex-Telia Carrier)", "SE", ["twelve99.net", "telia.net"], "ffm-bb2-link.ip.twelve99.net -> <city>-<role><n>-link.ip.twelve99.net"),
    174: ("Cogent Communications", "US", ["cogentco.com"], "be3036.ccr22.lon13.atlas.cogentco.com -> <if>.<role><n>.<iata><n>.atlas.cogentco.com"),
    2914: ("NTT Global IP Network", "JP", ["gin.ntt.net"], "ae-1.r20.lsanca07.us.bb.gin.ntt.net -> <if>.r<n>.<clli6><n>.<cc>.bb.gin.ntt.net"),
    3257: ("GTT Communications", "US", ["gtt.net"], "ae1.cr1-fra2.ip4.gtt.net -> <if>.cr<n>-<iata><n>.ip4.gtt.net"),
    3491: ("PCCW Global", "HK", ["pccwbtn.net"], None),
    6762: ("Telecom Italia Sparkle", "IT", ["seabone.net"], None),
    701: ("Verizon Business", "US", ["alter.net"], None),
    7018: ("AT&T", "US", ["ip.att.net"], "cr1.n54ny.ip.att.net -> cr<n>.<clli-ish>.ip.att.net"),
    6461: ("Zayo", "US", ["zayo.com"], "ae5.cs1.fra6.de.eth.zayo.com -> <if>.<role><n>.<iata><n>.<cc>.eth.zayo.com"),
    5511: ("Orange International Carriers", "FR", ["opentransit.net"], None),
    6830: ("Liberty Global", "GB", ["aorta.net"], None),
    12956: ("Telxius (Telefonica)", "ES", ["telefonica-wholesale.net"], None),
    3320: ("Deutsche Telekom", "DE", ["dtag.de"], None),
    4637: ("Telstra International", "AU", ["telstraglobal.net"], None),
}
FOCUS = {3356, 6939, 6453}  # carriers named in the product brief


def main():
    net = {n["asn"]: n for n in pdb("net")}
    fac = {f["id"]: f for f in pdb("fac")}
    netfac = collections.defaultdict(list)
    for x in pdb("netfac"):
        netfac[x["local_asn"]].append(x["fac_id"])
    ixconn = collections.Counter(x["asn"] for x in pdb("netixlan"))

    clique, cust, peer = [], collections.Counter(), collections.Counter()
    for line in bz2.open(RAW / "caida" / "20261001.as-rel.txt.bz2", "rt"):
        if line.startswith("# input clique:"):
            clique = [int(a) for a in line.split(":")[1].split()]
        if line.startswith("#"):
            continue
        a, b, rel = line.strip().split("|")[:3]
        if rel == "-1":
            cust[int(a)] += 1
        else:
            peer[int(a)] += 1
            peer[int(b)] += 1

    rows = []
    for asn, (name, hq, domains, pattern) in META.items():
        n = net.get(asn, {})
        facs = [fac[i] for i in netfac.get(asn, []) if i in fac]
        countries = collections.Counter(f["country"] for f in facs)
        rows.append({
            "asn": asn,
            "name": name,
            "hq_country": hq,
            "named_in_brief": asn in FOCUS,
            "peeringdb_net_id": n.get("id"),
            "peeringdb_name": n.get("name"),
            "caida_transit_free_clique": asn in clique,
            "caida_customer_links": cust[asn],
            "caida_peer_links": peer[asn],
            "peeringdb_facility_count": len(facs),
            "peeringdb_ix_connections": ixconn[asn],
            "facility_countries_top15": dict(countries.most_common(15)),
            "rdns_domains": domains,
            "rdns_example": pattern,
        })

    doc = {
        "description": "Tier-1 / major transit backbones used to label terrestrial-fiber hops.",
        "sources": ["PeeringDB API snapshot 2026-11-02", "CAIDA AS-relationships serial-1 20261001"],
        "caida_clique": clique,
        "caveats": [
            "PeeringDB facility lists are self-reported. Lumen AS3356 and AT&T AS7018 list ~0 facilities, so their routers must be placed via rDNS hostname parsing (CAIDA Hoiho API) and RIPE IPmap.",
            "Hurricane Electric AS6939 is not in CAIDA's transit-free clique but runs a global backbone with the largest IXP footprint; treat it as backbone.",
            "rdns_example values are illustrative formats; production parsing should use Hoiho rulesets, not these hints.",
        ],
        "backbones": rows,
    }
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(doc, indent=1))
    for r in rows:
        print(r["asn"], r["peeringdb_facility_count"], r["caida_transit_free_clique"], r["caida_customer_links"])


if __name__ == "__main__":
    main()
