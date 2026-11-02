/**
 * Converts an enrichment InfraMap (backend/schema/infra_map.schema.json)
 * into a globe path. Unlike the PacketJourney adapter it carries the subsea
 * cable's landing stations, so Business Mode can check the territories a
 * cable lands in between two hops.
 */
import type { HopInput, LandingInput, Medium, PathInput } from "../core/types";

interface Landing {
  name: string;
  country: string;
  lat: number;
  lon: number;
}
interface Candidate {
  name: string;
  probability: number;
  route_landings: Landing[];
}
export interface InfraMap {
  map_id: string;
  trace: { target: string; started_at: string };
  device: { location: { lat: number; lon: number; source: string } | null; country?: string } | null;
  hops: {
    ttl: number;
    ip?: string;
    hostname?: string;
    asn?: number;
    as_org?: string;
    role: string;
    rtt_ms: { min: number; jitter: number };
    loss_pct: number;
    location_status: string;
    geo: { lat: number; lon: number; city?: string; country_iso2?: string; sources?: Record<string, string> } | null;
  }[];
  links: {
    from: { ttl: number; country_iso2?: string };
    to: { ttl: number; country_iso2?: string };
    medium: Medium;
    subsea?: { selected: Candidate | null; alternatives: Candidate[] };
  }[];
}

/** Location confidence by enrichment source (mirrors backend compliance/adapt). */
const SOURCE_CONFIDENCE: Record<string, number> = {
  device: 1, opencellid_api: 0.95, opencellid_dump: 0.95, rdns_hint: 0.85, maxmind: 0.7, ipinfo: 0.7, mno_gateway_hub: 0.6, static: 0.7,
};

export function inframapToPath(m: InfraMap): PathInput | null {
  const linkInto = new Map(m.links.map((l) => [l.to.ttl, l]));
  const hops: HopInput[] = [];
  const dev = m.device?.location;
  if (dev) {
    hops.push({ lat: dev.lat, lon: dev.lon, rttMs: 0, hopNumber: 0, city: "device", country: m.device?.country, locationConfidence: SOURCE_CONFIDENCE[dev.source] ?? 0.5 });
  }
  let carriedLoss = 0;
  for (const h of m.hops) {
    carriedLoss = Math.max(carriedLoss, h.loss_pct);
    if (h.location_status !== "accepted" || !h.geo) continue;
    const link = linkInto.get(h.ttl);
    hops.push({
      lat: h.geo.lat,
      lon: h.geo.lon,
      rttMs: h.rtt_ms.min,
      jitterMs: h.rtt_ms.jitter,
      lossPct: carriedLoss,
      medium: link?.medium,
      hopNumber: h.ttl,
      ip: h.ip,
      hostname: h.hostname,
      asn: h.asn,
      asOrg: h.as_org,
      city: h.geo.city,
      country: h.geo.country_iso2,
      cable: link?.subsea?.selected?.name,
      locationConfidence: SOURCE_CONFIDENCE[h.geo.sources?.location ?? ""] ?? 0.5,
      landings: link ? landingsOf(link) : undefined,
    });
    carriedLoss = 0;
  }
  if (hops.length < 2) return null;
  return { id: m.map_id, hops, label: m.trace.target, startedAt: m.trace.started_at };
}

/**
 * Intermediate landing stations of the selected cable. Confidence that the
 * traffic touches a country = probability mass of the candidate cables whose
 * matched stretch lands there.
 */
function landingsOf(l: InfraMap["links"][number]): LandingInput[] | undefined {
  const sel = l.subsea?.selected;
  if (l.medium !== "subsea_cable" || !sel) return undefined;
  const mass = new Map<string, number>();
  for (const c of [sel, ...(l.subsea?.alternatives ?? [])]) {
    for (const cc of new Set(c.route_landings.map((x) => x.country))) mass.set(cc, (mass.get(cc) ?? 0) + c.probability);
  }
  const n = sel.route_landings.length;
  const out: LandingInput[] = [];
  sel.route_landings.forEach((ld, i) => {
    if ((i === 0 && ld.country === l.from.country_iso2) || (i === n - 1 && ld.country === l.to.country_iso2)) return;
    out.push({ name: `${ld.name} (${sel.name})`, country: ld.country, lat: ld.lat, lon: ld.lon, confidence: Math.min(0.95, Math.round((mass.get(ld.country) ?? 0) * 1000) / 1000) });
  });
  return out.length ? out : undefined;
}
