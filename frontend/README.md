# PacketPulse globe (Three.js)

An embeddable WebGL globe that draws packet paths as animated, latency-coloured arcs. Any number of paths renders in **one draw call for all arcs and one for all hop markers** (6 draw calls for the whole scene).

![500 concurrent paths](docs/globe-500-paths.png)
![Clicking a hop flies the camera to it](docs/hop-focus.png)

```bash
npm install
```

```bash
npm run dev
```

Then open `http://localhost:5173/?demo=500`. Other modes: `?example=1` (the backend's Mumbai → Frankfurt journey) and `?live=1` (follows `backend/` via `/v1/stream`; run `ingestd -serve :8080` first, and Vite proxies `/v1`).

```bash
npm test
```

```bash
npm run build
```

## Component API

```ts
import { PacketGlobe } from "./src/PacketGlobe";

const globe = new PacketGlobe({ container: document.getElementById("globe")! });
globe.setPath({
  id: "trace-42",
  hops: [
    { lat: 19.07, lon: 72.88, rttMs: 0 },
    { lat: 43.30, lon: 5.37, rttMs: 112.4, jitterMs: 3.2, lossPct: 0, medium: "subsea_cable", cable: "IMEWE" },
    { lat: 50.11, lon: 8.68, rttMs: 128.6 },
  ],
});
globe.onSelect((sel) => console.log(sel?.hop));   // fired when a hop is clicked
await globe.focusHop("trace-42", 1);               // fly the camera to a hop
await globe.focusPath("trace-42");                 // frame a whole path
globe.setPath({ ...sameIdNewLatencies });          // same geometry -> colours glide to the new state
globe.removePath("trace-42");
```

`journeyToPath()` (`src/data/journeyAdapter.ts`) converts backend PacketJourney documents; `connectBackend()` wires the live stream.

## How it works

| File | What it does |
|---|---|
| `src/render/ArcLayer.ts` + `arcShader.ts` | **Instanced arcs.** Every hop-to-hop segment is an instance of one 48-step ribbon strip. All per-segment data (4 Bézier control points, colour state, timing, style: 26 floats) lives in one interleaved `InstancedInterleavedBuffer`. The vertex shader evaluates the cubic Bézier and extrudes the strip in screen space (constant pixel width). Slots are recycled through a free list, and only the dirty slot range is uploaded (`addUpdateRange`). Updating one path's latencies uploads about 600 bytes. |
| `src/core/curve.ts` | Arc shape. Control points sit above the great circle at ¼ and ¾. Their radius is solved so the midpoint peaks at a height that grows with distance (flat metro hops, high intercontinental arcs). The same maths runs on CPU and GPU. |
| `src/core/health.ts` | **Latency → colour.** Each hop gets a severity in [0, 1] from latency excess (ΔRTT ÷ speed-of-light RTT for the distance), jitter and loss, using the backend's green/amber thresholds. The gradient stops are emitted as GLSL, so markers and arcs match. Loss ≥ 2% flashes red. Colour changes glide from the colour currently on screen, so rapid updates never jump. |
| `src/render/arcShader.ts` (fragment) | Soft glow, a packet pulse that travels the whole path (stable phase per path), draw-in when a path appears, dotted cellular links and dashed links of unknown medium, and dimming of other paths while one is selected. |
| `src/render/HopLayer.ts` | All hop markers in one `THREE.Points` with ranged buffer uploads, plus a pulsing ring on the selected hop. |
| `src/camera/GlobeControls.ts` | Drag-rotate with inertia (speed scales with altitude), exponential wheel/pinch zoom on altitude, and `flyTo` (great-circle slerp with ease-in-out that pulls back on long flights). Near the ground the camera orbits south of the target for an oblique view while the target stays centred. Any input cancels a flight. |
| `src/PacketGlobe.ts` | Component: path diffing (same geometry → colour retarget only), screen-space hop picking (front hemisphere only), selection and focus, stats. |
| `src/render/Earth.ts` | Dot-matrix Earth rasterised at runtime from Natural Earth polygons (`public/data/land.json`, 300 KB, built by `scripts/build_land.py`), plus borders, atmosphere and stars. No tile server. |

## Measured (headless Chromium, software GPU)

| 1,000 paths / 4,809 segments | |
|---|---|
| Draw calls for the whole scene | 6 |
| Insert all 1,000 paths (JS) | 22 ms |
| Live latency update for 100 paths (JS) | 3.5 ms |
| GPU upload for one path update | 624 bytes |

Frame rate needs a real GPU (the CI sandbox rasterises on the CPU).
