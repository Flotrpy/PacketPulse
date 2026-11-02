/**
 * Converts backend PacketJourney documents (backend/schema/packet_journey.schema.json)
 * into globe PathInputs. Only hops with a confident location are drawn;
 * unlocated hops are folded into the next located hop (their loss carries over
 * so a silent, lossy stretch still shows up red).
 */
import type { HopInput, Medium, PathInput } from "../core/types";

interface JLocation {
  lat: number;
  lon: number;
  city?: string;
  country_iso2: string;
  confidence: number;
}
interface JHop {
  hop_number: number;
  responded: boolean;
  ip?: string;
  hostname?: string;
  asn?: number;
  as_org?: string;
  rtt_ms: { min: number; jitter: number };
  loss_pct: number;
  location?: JLocation;
  border_violation: boolean;
}
interface JSegment {
  from_hop: number;
  to_hop: number;
  medium: Medium;
  subsea_cable?: { name: string };
}
export interface PacketJourney {
  journey_id: string;
  metadata: {
    destination: { ip: string; hostname?: string; location?: JLocation };
    source: { ip: string; location?: JLocation };
  };
  hops: JHop[];
  infrastructure: { segments: JSegment[] };
  compliance: { verdict: string };
}

/** Locations below this confidence (e.g. registry-country guesses) are not drawn. */
export const MIN_DRAW_CONFIDENCE = 0.5;

export function journeyToPath(j: PacketJourney): PathInput | null {
  const mediumInto = new Map<number, JSegment>();
  for (const s of j.infrastructure.segments) mediumInto.set(s.to_hop, s);

  const hops: HopInput[] = [];
  const src = j.metadata.source.location;
  if (src) {
    hops.push({ lat: src.lat, lon: src.lon, rttMs: 0, hopNumber: 0, ip: j.metadata.source.ip, city: src.city, country: src.country_iso2 });
  }

  let carriedLoss = 0;
  let carriedMedium: Medium | undefined;
  for (const h of j.hops) {
    const seg = mediumInto.get(h.hop_number);
    if (seg && seg.medium !== "unknown") carriedMedium = seg.medium;
    carriedLoss = Math.max(carriedLoss, h.loss_pct);
    const loc = h.location;
    if (!h.responded || !loc || loc.confidence < MIN_DRAW_CONFIDENCE) continue;
    const prev = hops[hops.length - 1];
    if (prev && prev.lat === loc.lat && prev.lon === loc.lon) {
      // Same point (e.g. router and next router in one city): merge.
      prev.rttMs = h.rtt_ms.min;
      prev.lossPct = Math.max(prev.lossPct ?? 0, carriedLoss);
      carriedLoss = 0;
      continue;
    }
    hops.push({
      lat: loc.lat,
      lon: loc.lon,
      rttMs: h.rtt_ms.min,
      jitterMs: h.rtt_ms.jitter,
      lossPct: carriedLoss,
      medium: carriedMedium,
      hopNumber: h.hop_number,
      ip: h.ip,
      hostname: h.hostname,
      asn: h.asn,
      asOrg: h.as_org,
      city: loc.city,
      country: loc.country_iso2,
      cable: seg?.subsea_cable?.name,
      borderViolation: h.border_violation,
    });
    carriedLoss = 0;
    carriedMedium = undefined;
  }

  // Destination located only at the endpoint level (e.g. cloud region prefix).
  const dst = j.metadata.destination.location;
  const last = hops[hops.length - 1];
  if (dst && dst.confidence >= MIN_DRAW_CONFIDENCE && (!last || last.lat !== dst.lat || last.lon !== dst.lon)) {
    const lastHop = j.hops[j.hops.length - 1];
    hops.push({ lat: dst.lat, lon: dst.lon, rttMs: lastHop?.rtt_ms.min ?? last?.rttMs ?? 0, ip: j.metadata.destination.ip, city: dst.city, country: dst.country_iso2 });
  }

  if (hops.length < 2) return null;
  return { id: j.journey_id, hops, label: j.metadata.destination.hostname ?? j.metadata.destination.ip, verdict: j.compliance.verdict };
}
