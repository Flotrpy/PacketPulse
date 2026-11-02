/**
 * Compliance alert markers (Business Mode): one THREE.Points draw call with
 * a pulsing warning ring at every territory entry the engine flagged.
 * Alerts are few (one per entry), so the buffer is rebuilt on change.
 */
import * as THREE from "three";
import { latLonToVec3 } from "../core/geo";

const vertexShader = /* glsl */ `
attribute vec3 aColor;
attribute vec2 aStyle; // x = phase, y = 1 firm violation | 0 suspected
uniform float uSize;
uniform float uPixelRatio;
varying vec3 vColor;
varying vec2 vStyle;
void main() {
  vec4 mv = modelViewMatrix * vec4(position, 1.0);
  gl_Position = projectionMatrix * mv;
  gl_PointSize = uSize * uPixelRatio * clamp(3.2 / -mv.z, 0.7, 2.5);
  vColor = aColor;
  vStyle = aStyle;
}
`;

const fragmentShader = /* glsl */ `
uniform float uTime;
varying vec3 vColor;
varying vec2 vStyle;
void main() {
  vec2 p = gl_PointCoord * 2.0 - 1.0;
  float r = length(p);
  if (r > 1.0) discard;
  float speed = mix(0.5, 1.1, vStyle.y);
  float k = fract(uTime * speed + vStyle.x);
  float ring = (1.0 - smoothstep(0.0, 0.09, abs(r - (0.25 + 0.7 * k)))) * (1.0 - k);
  float core = 1.0 - smoothstep(0.16, 0.26, r);
  // Suspected alerts: dashed static ring instead of a solid pulse.
  float dash = step(0.5, fract(atan(p.y, p.x) * 1.9099));
  float sring = (1.0 - vStyle.y) * (1.0 - smoothstep(0.0, 0.07, abs(r - 0.62))) * dash;
  float a = clamp(core + ring * vStyle.y + sring + ring * 0.4 * (1.0 - vStyle.y), 0.0, 1.0);
  gl_FragColor = vec4(vColor * (0.8 + 0.6 * core), a);
}
`;

export interface AlertMarker {
  lat: number;
  lon: number;
  rgb: [number, number, number];
  suspected: boolean;
}

export class AlertLayer {
  readonly points: THREE.Points<THREE.BufferGeometry, THREE.ShaderMaterial>;
  private material: THREE.ShaderMaterial;
  positions = new Float32Array(0);

  constructor(sizePx = 22) {
    this.material = new THREE.ShaderMaterial({
      vertexShader,
      fragmentShader,
      uniforms: { uSize: { value: sizePx }, uPixelRatio: { value: 1 }, uTime: { value: 0 } },
      transparent: true,
      depthWrite: false,
      blending: THREE.AdditiveBlending,
    });
    this.points = new THREE.Points(new THREE.BufferGeometry(), this.material);
    this.points.frustumCulled = false;
    this.points.renderOrder = 4;
    this.points.visible = false;
  }

  setMarkers(markers: AlertMarker[]): void {
    const pos = new Float32Array(markers.length * 3);
    const col = new Float32Array(markers.length * 3);
    const sty = new Float32Array(markers.length * 2);
    const v = [0, 0, 0] as [number, number, number];
    markers.forEach((m, i) => {
      pos.set(latLonToVec3(m.lat, m.lon, 1.004, v), i * 3);
      col.set(m.rgb, i * 3);
      sty.set([(i * 0.137) % 1, m.suspected ? 0 : 1], i * 2);
    });
    const g = this.points.geometry;
    g.setAttribute("position", new THREE.BufferAttribute(pos, 3));
    g.setAttribute("aColor", new THREE.BufferAttribute(col, 3));
    g.setAttribute("aStyle", new THREE.BufferAttribute(sty, 2));
    g.setDrawRange(0, markers.length);
    this.positions = pos;
  }

  update(time: number, pixelRatio: number): void {
    this.material.uniforms.uTime.value = time;
    this.material.uniforms.uPixelRatio.value = pixelRatio;
  }

  dispose(): void {
    this.points.geometry.dispose();
    this.material.dispose();
  }
}
