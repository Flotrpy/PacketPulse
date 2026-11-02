import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { computeHealth, hashPhase, segColors } from "../PacketGlobe";
import { latLonToVec3 } from "../core/geo";
import { statusOf } from "../core/health";
import { journeyToPath, type PacketJourney } from "./journeyAdapter";
import { Simulator } from "./simulator";

const example = JSON.parse(readFileSync(new URL("../../public/data/example-journey.json", import.meta.url), "utf8")) as PacketJourney;

describe("journeyToPath (backend example: Jio Mumbai -> AWS Frankfurt)", () => {
  const path = journeyToPath(example)!;

  it("keeps only confidently located hops, in order", () => {
    expect(path.hops.map((h) => h.city)).toEqual(["Mumbai", "Navi Mumbai", "Mumbai", "Marseille", "Paris", "Frankfurt"]);
    expect(path.verdict).toBe("violation");
  });

  it("carries medium, cable, ASN and border flags onto hops", () => {
    const marseille = path.hops[3];
    expect(marseille.medium).toBe("subsea_cable");
    expect(marseille.cable).toBe("IMEWE");
    expect(marseille.asn).toBe(6453);
    expect(marseille.borderViolation).toBe(true);
    expect(path.hops[1].medium).toBe("cellular_backhaul");
  });

  it("folds loss from unanswered hops into the next drawn hop", () => {
    // TTL 2 never answered (100% loss) and sits between Mumbai and Navi Mumbai.
    expect(path.hops[1].lossPct).toBe(100);
  });

  it("scores the subsea crossing green (1.4x light speed)", () => {
    const h = computeHealth(path.hops, path.hops.map((x) => latLonToVec3(x.lat, x.lon)));
    expect(statusOf(h[3].severity)).toBe("green");
    expect(h[1].flash).toBe(true); // the 100%-loss stretch flashes
    expect(segColors(h, 0)).toEqual([0, h[1].severity, 0.35, 1]);
  });
});

describe("Simulator", () => {
  it("is deterministic per seed", () => {
    const a = new Simulator({ seed: 3 }).spawn(20);
    const b = new Simulator({ seed: 3 }).spawn(20);
    expect(a).toEqual(b);
  });

  it("produces physically plausible RTTs", () => {
    for (const p of new Simulator({ seed: 9 }).spawn(200)) {
      const h = computeHealth(p.hops, p.hops.map((x) => latLonToVec3(x.lat, x.lon)));
      for (let i = 1; i < p.hops.length; i++) {
        expect(p.hops[i].rttMs).toBeGreaterThanOrEqual(p.hops[i - 1].rttMs);
        // Healthy links never beat light: excess >= 1 when distance is meaningful.
        if (h[i].latencyExcess > 0 && p.hops[i].rttMs - p.hops[i - 1].rttMs > 2) expect(h[i].latencyExcess).toBeGreaterThan(0.99);
      }
    }
  });

  it("raises incidents and churns routes over time", () => {
    const sim = new Simulator({ seed: 5, incidentRate: 0.5, churnRate: 0.05 });
    const start = sim.spawn(100);
    let updates = 0;
    let removed = 0;
    let sawRed = false;
    for (let t = 1; t <= 20; t++) {
      const r = sim.step(t, 1);
      updates += r.updated.length;
      removed += r.removed.length;
      for (const p of r.updated) {
        const h = computeHealth(p.hops, p.hops.map((x) => latLonToVec3(x.lat, x.lon)));
        if (h.some((x) => statusOf(x.severity) === "red")) sawRed = true;
      }
    }
    expect(sim.size).toBe(start.length);
    expect(updates).toBeGreaterThan(100);
    expect(removed).toBeGreaterThan(20);
    expect(sawRed).toBe(true);
  });

  it("gives each path a stable pulse phase", () => {
    expect(hashPhase("abc")).toBe(hashPhase("abc"));
    expect(hashPhase("abc")).not.toBe(hashPhase("abd"));
  });
});
