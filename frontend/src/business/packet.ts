/**
 * Bridges globe paths and the compliance engine:
 *   buildPacket   PathInput -> engine Packet (+ index from route seq back to hop / landing)
 *   applyEvaluation  engine Evaluation -> per-hop/segment status codes and alert views
 * Kept free of DOM and Three.js so it can be unit-tested.
 */
import { statusCode } from "../core/compliance";
import type { PathCompliance } from "../PacketGlobe";
import type { PathInput } from "../core/types";

export interface RoutePoint {
  seq: number;
  ttl?: number;
  kind: "source" | "hop" | "subsea_landing" | "destination";
  ip?: string;
  asn?: number;
  operator?: string;
  label?: string;
  lat: number;
  lon: number;
  country_hint?: string;
  timestamp: string;
  timestamp_source: string;
  location_confidence: number;
  location_source?: string;
}

export interface Packet {
  packet_id: string;
  timestamp: string;
  classifications: string[];
  route: RoutePoint[];
  source: string;
}

/** Where a route point came from on the globe path. */
export interface SeqRef {
  hop: number; // hop index (for a landing: the hop its link arrives at)
  landing?: number; // index into hops[hop].landings
}

export const DEFAULT_CONFIDENCE = 0.7;

/**
 * Hop timestamps are estimated as start + RTT/2 (one-way arrival); landings
 * are interpolated between the two hops of their link.
 */
export function buildPacket(path: PathInput, classifications: string[], start: Date): { packet: Packet; index: SeqRef[] } {
  const t0 = start.getTime();
  const iso = (ms: number) => new Date(t0 + ms).toISOString();
  const route: RoutePoint[] = [];
  const index: SeqRef[] = [];
  const n = path.hops.length;
  path.hops.forEach((h, i) => {
    const prevMs = i > 0 ? path.hops[i - 1].rttMs / 2 : 0;
    const hereMs = h.rttMs / 2;
    const ls = h.landings ?? [];
    ls.forEach((ld, j) => {
      index.push({ hop: i, landing: j });
      route.push({
        seq: route.length, kind: "subsea_landing", operator: h.cable, label: ld.name, lat: ld.lat, lon: ld.lon, country_hint: ld.country,
        timestamp: iso(prevMs + ((hereMs - prevMs) * (j + 1)) / (ls.length + 1)), timestamp_source: "interpolated_along_cable",
        location_confidence: clamp01(ld.confidence), location_source: "subsea_cable_match",
      });
    });
    index.push({ hop: i });
    route.push({
      seq: route.length,
      ttl: h.hopNumber && h.hopNumber > 0 ? h.hopNumber : undefined,
      kind: i === 0 ? "source" : i === n - 1 ? "destination" : "hop",
      ip: h.ip,
      asn: h.asn,
      operator: h.asOrg,
      label: [h.city, h.country].filter(Boolean).join(", ") || undefined,
      lat: h.lat,
      lon: h.lon,
      country_hint: h.country,
      timestamp: iso(hereMs),
      timestamp_source: i === 0 ? "trace_start" : "probe_sent_plus_half_rtt",
      location_confidence: clamp01(h.locationConfidence ?? DEFAULT_CONFIDENCE),
    });
  });
  return { packet: { packet_id: path.id, timestamp: start.toISOString(), classifications, route, source: "packetpulse-globe" }, index };
}

function clamp01(v: number): number {
  return Math.max(0, Math.min(1, Number.isFinite(v) ? v : DEFAULT_CONFIDENCE));
}

// ---- engine response types (subset of backend/internal/compliance) ----

export interface Law {
  code: string;
  title: string;
  citation: string;
  articles?: string[];
  url?: string;
}

export interface Alert {
  type: "ComplianceViolationAlert";
  alert_id: string;
  packet_id: string;
  profile_id: string;
  severity: "info" | "low" | "medium" | "high" | "critical";
  suspected: boolean;
  classification: string;
  territory: { country_iso2: string; country_name?: string; subdivision_name?: string; regions: string[]; lat: number; lon: number; located_by: string };
  entered_at: string;
  entered_at_source: string;
  point: { seq: number; kind: string; ip?: string; asn?: number; operator?: string; label?: string; location_confidence: number };
  last_compliant_point: { label?: string; country_iso2: string } | null;
  law: Law;
  rule: string;
  exception?: string;
  mitigation: { action: string; recommendation: string; proxy?: { id: string; name: string; cloud_region: string; city: string }; proxy_distance_km?: number; alternatives?: string[] };
}

export interface ResolvedPoint {
  seq: number;
  kind: string;
  label?: string;
  location: { country_iso2?: string; country_name?: string; subdivision?: string; subdivision_name?: string; source: string };
  regions: string[];
  status: "not_evaluated" | "compliant" | "suspected" | "violation";
  flagged_by?: string[];
}

export interface Evaluation {
  packet_id: string;
  verdict: "compliant" | "violation" | "suspected_violation" | "not_applicable";
  profiles: { profile_id: string; result: string; alerts: number }[];
  alerts: Alert[];
  route: ResolvedPoint[];
}

/** An alert placed back on the globe path. */
export interface AlertView {
  alert: Alert;
  pathId: string;
  hop: number;
  landing?: number;
}

export interface Applied {
  compliance: PathCompliance;
  alerts: AlertView[];
  /** resolved route point per hop (for the inspector) */
  hopPoints: ResolvedPoint[];
}

export function applyEvaluation(path: PathInput, ev: Evaluation, index: SeqRef[]): Applied {
  const hops = new Array<number>(path.hops.length).fill(0);
  const segmentEnds = new Array<number>(Math.max(0, path.hops.length - 1)).fill(0);
  const hopPoints: ResolvedPoint[] = new Array(path.hops.length);
  for (const rp of ev.route) {
    const ref = index[rp.seq];
    if (!ref) continue;
    const code = statusCode(rp.status);
    if (ref.landing === undefined) {
      hops[ref.hop] = code;
      hopPoints[ref.hop] = rp;
    } else if (ref.hop > 0) {
      segmentEnds[ref.hop - 1] = Math.max(segmentEnds[ref.hop - 1], code);
    }
  }
  const alerts: AlertView[] = [];
  for (const a of ev.alerts) {
    const ref = index[a.point.seq];
    if (ref) alerts.push({ alert: a, pathId: path.id, hop: ref.hop, landing: ref.landing });
  }
  return { compliance: { hops, segmentEnds }, alerts, hopPoints };
}

const SEVERITY_RANK: Record<string, number> = { critical: 4, high: 3, medium: 2, low: 1, info: 0 };

/** Firm before suspected, then severity, then most recent entry. */
export function sortAlerts(list: AlertView[]): AlertView[] {
  return [...list].sort(
    (a, b) =>
      Number(a.alert.suspected || !!a.alert.exception) - Number(b.alert.suspected || !!b.alert.exception) ||
      SEVERITY_RANK[b.alert.severity] - SEVERITY_RANK[a.alert.severity] ||
      // Parse, don't compare strings: "…:01Z" sorts after "…:01.03Z" lexically.
      Date.parse(b.alert.entered_at) - Date.parse(a.alert.entered_at),
  );
}
