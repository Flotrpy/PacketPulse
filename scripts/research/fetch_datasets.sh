#!/usr/bin/env bash
# Re-downloads every open dataset in data/raw/. Keyed sources run only when
# their env var is set (MAXMIND_ACCOUNT_ID + MAXMIND_LICENSE_KEY, IPINFO_TOKEN,
# OPENCELLID_TOKEN, PEERINGDB_API_KEY).
set -euo pipefail
R="$(cd "$(dirname "$0")/../.." && pwd)/data/raw"
YM="$(date +%Y-%m)"
get() { mkdir -p "$(dirname "$1")"; curl -fsSL --retry 3 -m 600 -o "$1" "$2"; echo "ok  $1"; }

# TeleGeography submarine cables (CC BY-NC-SA 3.0)
TG=https://www.submarinecablemap.com/api/v3
get "$R/telegeography/cable_cable-geo.json" "$TG/cable/cable-geo.json"
get "$R/telegeography/landing-point_landing-point-geo.json" "$TG/landing-point/landing-point-geo.json"
get "$R/telegeography/cable_all.json" "$TG/cable/all.json"
mkdir -p "$R/telegeography/cables"
python3 -I -c "import json,sys;print('\n'.join(c['id'] for c in json.load(open(sys.argv[1]))))" "$R/telegeography/cable_all.json" \
  | xargs -P 8 -I{} curl -fsS -m 30 -o "$R/telegeography/cables/{}.json" "$TG/cable/{}.json"
tar -C "$R/telegeography" -czf "$R/telegeography/cables-detail.tar.gz" cables && rm -rf "$R/telegeography/cables"
gzip -f9 "$R/telegeography/cable_cable-geo.json" "$R/telegeography/landing-point_landing-point-geo.json"

# PeeringDB full snapshot
AUTH=(); [ -n "${PEERINGDB_API_KEY:-}" ] && AUTH=(-H "Authorization: Api-Key $PEERINGDB_API_KEY")
for t in org net ix ixlan ixpfx fac netfac netixlan ixfac carrier carrierfac campus; do
  curl -fsSL --retry 3 -m 600 "${AUTH[@]}" "https://www.peeringdb.com/api/$t?depth=0" | gzip -9 > "$R/peeringdb/$t.json.gz"
  echo "ok  peeringdb/$t"; [ ${#AUTH[@]} -eq 0 ] && sleep 4
done

# CAIDA (latest monthly files)
CA=https://publicdata.caida.org/datasets
rel=$(curl -fsS "$CA/as-relationships/serial-1/" | grep -o '[0-9]*\.as-rel\.txt\.bz2' | sort -u | tail -1)
org=$(curl -fsS "$CA/as-organizations/" | grep -o '[0-9]*\.as-org2info\.jsonl\.gz' | sort -u | tail -1)
get "$R/caida/$rel" "$CA/as-relationships/serial-1/$rel"
get "$R/caida/$org" "$CA/as-organizations/$org"

# IP -> ASN / geo (keyless)
get "$R/iptoasn/ip2asn-combined.tsv.gz" https://iptoasn.com/data/ip2asn-combined.tsv.gz
get "$R/dbip/dbip-city-lite-$YM.mmdb.gz" "https://download.db-ip.com/free/dbip-city-lite-$YM.mmdb.gz"
get "$R/dbip/dbip-asn-lite-$YM.mmdb.gz" "https://download.db-ip.com/free/dbip-asn-lite-$YM.mmdb.gz"
get "$R/rir/nro-delegated-stats.txt" https://ftp.ripe.net/pub/stats/ripencc/nro-stats/latest/nro-delegated-stats && gzip -f9 "$R/rir/nro-delegated-stats.txt"

# Cloud provider ranges
get "$R/cloud/aws-ip-ranges.json" https://ip-ranges.amazonaws.com/ip-ranges.json && gzip -f9 "$R/cloud/aws-ip-ranges.json"
get "$R/cloud/gcp-cloud.json" https://www.gstatic.com/ipranges/cloud.json
get "$R/cloud/oracle-public-ip-ranges.json" https://docs.oracle.com/en-us/iaas/tools/public_ip_ranges.json
get "$R/cloud/cloudflare-ips-v4.txt" https://www.cloudflare.com/ips-v4
get "$R/cloud/cloudflare-ips-v6.txt" https://www.cloudflare.com/ips-v6
AZ=$(curl -fsS "https://www.microsoft.com/en-us/download/details.aspx?id=56519" | grep -o 'https://download.microsoft.com/download/[^"]*ServiceTags_Public_[0-9]*\.json' | head -1)
get "$R/cloud/azure-service-tags.json" "$AZ" && gzip -f9 "$R/cloud/azure-service-tags.json"

# Cellular, landmarks, borders, African terrestrial fiber
get "$R/mcc-mnc/mcc-mnc-list.json" https://raw.githubusercontent.com/pbakondy/mcc-mnc-list/master/mcc-mnc-list.json && gzip -f9 "$R/mcc-mnc/mcc-mnc-list.json"
get "$R/ripe-atlas/probes-meta-latest.bz2" https://ftp.ripe.net/ripe/atlas/probes/archive/meta-latest
get "$R/ourairports/airports.csv" https://davidmegginson.github.io/ourairports-data/airports.csv && gzip -f9 "$R/ourairports/airports.csv"
NE=https://raw.githubusercontent.com/nvkelso/natural-earth-vector/master/geojson
get "$R/naturalearth/ne_50m_admin_0_countries.geojson" "$NE/ne_50m_admin_0_countries.geojson"
get "$R/naturalearth/ne_10m_admin_0_disputed_areas.geojson" "$NE/ne_10m_admin_0_disputed_areas.geojson"
get "$R/naturalearth/ne_50m_admin_1_states_provinces.geojson" "$NE/ne_50m_admin_1_states_provinces.geojson"
gzip -f9 "$R"/naturalearth/*.geojson
get "$R/afterfibre/africa-fiber.tilejson" https://d316kar6yg8hyq.cloudfront.net/africa-fiber.json
get "$R/afterfibre/AfTerFibre_cities.geojson" https://afterfibre.opentelecomdata.org/data/other/AfTerFibre_cities.geojson
get "$R/afterfibre/AUC_2025.geojson" https://afterfibre.opentelecomdata.org/data/other/AUC_2025.geojson

# Keyed sources
if [ -n "${MAXMIND_ACCOUNT_ID:-}" ] && [ -n "${MAXMIND_LICENSE_KEY:-}" ]; then
  for ed in GeoLite2-City GeoLite2-ASN ${MAXMIND_EXTRA_EDITIONS:-}; do   # e.g. MAXMIND_EXTRA_EDITIONS="GeoIP2-ISP GeoIP2-City"
    curl -fsSL -u "$MAXMIND_ACCOUNT_ID:$MAXMIND_LICENSE_KEY" -o "$R/maxmind/$ed.tar.gz" --create-dirs \
      "https://download.maxmind.com/geoip/databases/$ed/download?suffix=tar.gz" && echo "ok  maxmind/$ed"
  done
fi
[ -n "${IPINFO_TOKEN:-}" ] && get "$R/ipinfo/ipinfo_lite.mmdb" "https://ipinfo.io/data/ipinfo_lite.mmdb?token=$IPINFO_TOKEN"
[ -n "${OPENCELLID_TOKEN:-}" ] && get "$R/opencellid/cell_towers.csv.gz" "https://opencellid.org/ocid/downloads?token=$OPENCELLID_TOKEN&type=full&file=cell_towers.csv.gz"
echo done
