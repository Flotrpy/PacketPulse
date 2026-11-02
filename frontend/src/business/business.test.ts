import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { COMPLIANCE, GLSL_COMPLIANCE, statusCode } from "../core/compliance";
import { inframapToPath, type InfraMap } from "../data/inframapAdapter";
import { applyEvaluation, buildPacket, sortAlerts, type Alert, type Evaluation } from "./packet";

const infra = JSON.parse(readFileSync(new URL("../../public/data/example-inframap.json", import.meta.url), "utf8")) as InfraMap;

describe("inframapToPath", () => {
  const path = inframapToPath(infra)!;

  it("keeps device + accepted hops and carries cable landings on the subsea link", () => {
    expect(path.hops.map((h) => h.city)).toEqual(["device", "Mumbai", "Mumbai", "Marseille", "Paris", "Frankfurt am Main"]);
    const marseille = path.hops[3];
    expect(marseille.cable).toBe("SeaMeWe-4");
    expect(marseille.landings?.map((l) => l.country)).toEqual(["SA", "EG", "EG"]);
    // Every candidate cable crosses Egypt; only SeaMeWe-4's stretch lands at Jeddah.
    expect(marseille.landings![0].confidence).toBeLessThan(0.3);
    expect(marseille.landings![1].confidence).toBeGreaterThan(0.8);
    expect(path.hops[0].locationConfidence).toBe(1); // device fix
    expect(path.hops[1].locationConfidence).toBe(0.6); // MNO gateway placed at its hub
    expect(path.startedAt).toBe("2026-11-02T13:05:01Z");
  });
});

describe("buildPacket", () => {
  const path = inframapToPath(infra)!;
  const start = new Date("2026-11-02T13:05:01Z");
  const { packet, index } = buildPacket(path, ["sensitive_eu_citizen_data"], start);

  it("orders landings before the hop their link arrives at and keeps a seq index", () => {
    expect(packet.route.map((r) => r.kind)).toEqual(["source", "hop", "hop", "subsea_landing", "subsea_landing", "subsea_landing", "hop", "hop", "destination"]);
    expect(packet.route.every((r, i) => r.seq === i)).toBe(true);
    expect(index[3]).toEqual({ hop: 3, landing: 0 });
    expect(index[6]).toEqual({ hop: 3 });
    expect(packet.packet_id).toBe(path.id);
  });

  it("timestamps hops at start + RTT/2 and interpolates landings", () => {
    const t = packet.route.map((r) => Date.parse(r.timestamp) - start.getTime());
    for (let i = 1; i < t.length; i++) expect(t[i]).toBeGreaterThanOrEqual(t[i - 1]);
    expect(t[6]).toBeCloseTo(112.4 / 2, 0); // Marseille: 112.4 ms RTT
    expect(packet.route[3].timestamp_source).toBe("interpolated_along_cable");
  });
});

describe("applyEvaluation", () => {
  const path = inframapToPath(infra)!;
  const { index } = buildPacket(path, ["x"], new Date());
  const statuses = ["violation", "violation", "violation", "suspected", "violation", "violation", "compliant", "compliant", "compliant"] as const;
  const mkAlert = (seq: number, suspected: boolean, severity: Alert["severity"], at: string): Alert =>
    ({ point: { seq, kind: "hop", location_confidence: 0.7 }, suspected, severity, entered_at: at, territory: { country_iso2: "IN", regions: [], lat: 0, lon: 0, located_by: "polygon" } }) as unknown as Alert;
  const ev: Evaluation = {
    packet_id: path.id,
    verdict: "violation",
    profiles: [],
    alerts: [mkAlert(0, false, "critical", "2026-11-02T13:05:01Z"), mkAlert(3, true, "high", "2026-11-02T13:05:01.02Z"), mkAlert(4, false, "critical", "2026-11-02T13:05:01.03Z")],
    route: statuses.map((s, seq) => ({ seq, kind: "hop", location: { source: "polygon" }, regions: [], status: s })),
  };

  it("maps point statuses to hops and landing statuses to the segment they sit on", () => {
    const a = applyEvaluation(path, ev, index);
    expect(a.compliance.hops).toEqual([3, 3, 3, 1, 1, 1]);
    // Segment 2 (Mumbai -> Marseille) crosses violating landings even though Marseille is compliant.
    expect(a.compliance.segmentEnds).toEqual([0, 0, 3, 0, 0]);
    expect(a.alerts.map((x) => [x.hop, x.landing])).toEqual([[0, undefined], [3, 0], [3, 1]]);
  });

  it("sorts firm alerts before suspected, then by severity", () => {
    const sorted = sortAlerts(applyEvaluation(path, ev, index).alerts);
    expect(sorted.map((x) => x.alert.point.seq)).toEqual([4, 0, 3]);
  });
});

describe("compliance codes", () => {
  it("are ordered and mirrored in GLSL", () => {
    expect(statusCode("violation")).toBe(COMPLIANCE.violation);
    expect(statusCode(undefined)).toBe(0);
    expect(GLSL_COMPLIANCE).toContain("vec3 complianceColor(float c)");
  });
});
