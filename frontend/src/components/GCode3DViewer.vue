<template>
  <div class="gcode-3d-viewer">
    <div class="gcode-debug">
      G-code length: {{ (gcode || "").length }} chars
      <span v-if="lastTruncated"> • truncated</span>
    </div>
    <div ref="canvasContainer" class="gcode-canvas"></div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref, watch, nextTick } from "vue";
import * as THREE from "three";
// use named import, but *never* use THREE types in annotations
import { OrbitControls } from "three/examples/jsm/controls/OrbitControls.js";

const props = defineProps<{
  gcode: string;
  cursorLine?: number | null;
}>();

const emit = defineEmits<{
  (e: "lineSelected", lineIndex: number): void;
}>();

const canvasContainer = ref<HTMLDivElement | null>(null);

// three.js objects – all typed as any so TS ignores THREE namespace
let scene: any = null;
let camera: any = null;
let renderer: any = null;
let lineObject: any = null;
let controls: any = null;
let highlightSphere: any = null;
let raycaster: any = null;
let mouseNDC: any = null;
let animationId: number | null = null;

let currentSourceLineCount = 0;
let resizeHandler: (() => void) | null = null;
let clickHandler: ((e: MouseEvent) => void) | null = null;

const lastTruncated = ref(false);
const isThreeReady = ref(false);

// conservative caps to keep huge programs from nuking the UI
const MAX_CHARS = 2_000_000;
const MAX_LINES = 200_000;
const MAX_POINTS = 120_000;
const TARGET_POINTS = 50_000;

interface ParseResult {
  geometry: any;           // THREE.BufferGeometry, but `any` for TS
  sourceLineCount: number;
  truncated: boolean;
}

function parseGcode(raw: string): ParseResult | null {
  let truncated = false;
  let gcode = raw;

  if (gcode.length > MAX_CHARS) {
    console.warn(
      `[GCode3DViewer] large file (${gcode.length} chars) – only parsing first ${MAX_CHARS} chars`
    );
    gcode = gcode.slice(0, MAX_CHARS);
    truncated = true;
  }

  let lines = gcode.split(/\r?\n/);

  if (lines.length > MAX_LINES) {
    console.warn(
      `[GCode3DViewer] many lines (${lines.length}); only using first ${MAX_LINES}`
    );
    lines = lines.slice(0, MAX_LINES);
    truncated = true;
  }

  const rawPoints: any[] = [];
  let x = 0;
  let y = 0;
  let z = 0;
  let currentMode: number | null = null; // 0,1,2,3

  for (let idx = 0; idx < lines.length; idx++) {
    const rawLine = lines[idx];
    const line = rawLine.trim().toUpperCase();
    if (!line || line.startsWith("(")) continue;

    const gMatches = [...line.matchAll(/G(\d+)/g)];
    if (gMatches.length) {
      const gNum = parseInt(gMatches[gMatches.length - 1][1], 10);
      if ([0, 1, 2, 3].includes(gNum)) currentMode = gNum;
    }

    const startX = x;
    const startY = y;
    const startZ = z;

    const xMatch = line.match(/X(-?\d+(\.\d+)?)/);
    const yMatch = line.match(/Y(-?\d+(\.\d+)?)/);
    const zMatch = line.match(/Z(-?\d+(\.\d+)?)/);
    const iMatch = line.match(/I(-?\d+(\.\d+)?)/);
    const jMatch = line.match(/J(-?\d+(\.\d+)?)/);

    if (xMatch) x = parseFloat(xMatch[1]);
    if (yMatch) y = parseFloat(yMatch[1]);
    if (zMatch) z = parseFloat(zMatch[1]);

    const hasMotion = xMatch || yMatch || zMatch;
    if (!hasMotion) continue;

    if (rawPoints.length === 0) {
      rawPoints.push(new THREE.Vector3(startX, startY, startZ));
    }

    if ((currentMode === 2 || currentMode === 3) && (iMatch || jMatch)) {
      // Arc in XY using I/J, Z linear
      const cx = startX + (iMatch ? parseFloat(iMatch[1]) : 0);
      const cy = startY + (jMatch ? parseFloat(jMatch[1]) : 0);

      const startVecX = startX - cx;
      const startVecY = startY - cy;
      const endVecX = x - cx;
      const endVecY = y - cy;

      const r = Math.hypot(startVecX, startVecY) || 0.0001;

      const startAngle = Math.atan2(startVecY, startVecX);
      const endAngle = Math.atan2(endVecY, endVecX);

      let delta = endAngle - startAngle;

      if (currentMode === 2 && delta > 0) delta -= Math.PI * 2; // CW
      else if (currentMode === 3 && delta < 0) delta += Math.PI * 2; // CCW

      const arcLen = Math.abs(delta * r);
      const segments = Math.max(8, Math.min(64, Math.ceil(arcLen / 0.5)));

      for (let s = 1; s <= segments; s++) {
        const t = s / segments;
        const ang = startAngle + delta * t;
        const px = cx + Math.cos(ang) * r;
        const py = cy + Math.sin(ang) * r;
        const pz = startZ + (z - startZ) * t;
        rawPoints.push(new THREE.Vector3(px, py, pz));

        if (rawPoints.length >= MAX_POINTS) {
          truncated = true;
          break;
        }
      }
    } else {
      rawPoints.push(new THREE.Vector3(x, y, z));
    }

    if (rawPoints.length >= MAX_POINTS) {
      truncated = true;
      break;
    }
  }

  if (rawPoints.length < 2) {
    console.warn("[GCode3DViewer] not enough motion to build geometry");
    return null;
  }

  let usedPoints = rawPoints.length;
  let finalPoints = rawPoints;

  if (rawPoints.length > TARGET_POINTS) {
    const step = Math.ceil(rawPoints.length / TARGET_POINTS);
    const decimated: any[] = [];
    for (let i = 0; i < rawPoints.length; i += step) {
      decimated.push(rawPoints[i]);
    }
    finalPoints = decimated;
    usedPoints = decimated.length;
    truncated = true;
  }

  const geometry = new THREE.BufferGeometry().setFromPoints(finalPoints);

  console.log("[GCode3DViewer] parse result", {
    lines: lines.length,
    points: rawPoints.length,
    usedPoints,
    truncated,
  });

  return {
    geometry,
    sourceLineCount: lines.length,
    truncated,
  };
}

function ensureHighlightSphere() {
  if (!scene) return;
  if (highlightSphere) return;

  const geom = new THREE.SphereGeometry(0.05, 12, 12); // small
  const mat = new THREE.MeshBasicMaterial({ color: 0xff0000 });
  highlightSphere = new THREE.Mesh(geom, mat);
  scene.add(highlightSphere);
}

function clearLine() {
  if (scene && lineObject) {
    scene.remove(lineObject);
    lineObject.geometry.dispose();
    if (lineObject.material && typeof lineObject.material.dispose === "function") {
      lineObject.material.dispose();
    }
  }
  lineObject = null;
}

function initThree() {
  const el = canvasContainer.value;
  if (!el) {
    console.warn("[GCode3DViewer] initThree: no canvasContainer");
    return;
  }

  const width = el.clientWidth || 400;
  const height = el.clientHeight || 300;

  scene = new THREE.Scene();
  scene.background = new THREE.Color(0x1e1e1e);

  camera = new THREE.PerspectiveCamera(50, width / height, 0.1, 5000);
  camera.position.set(0, 0, 200);

  const ambient = new THREE.AmbientLight(0xffffff, 0.7);
  scene.add(ambient);

  const dir = new THREE.DirectionalLight(0xffffff, 0.5);
  dir.position.set(100, 200, 100);
  scene.add(dir);

  renderer = new THREE.WebGLRenderer({ antialias: true });
  renderer.setPixelRatio(window.devicePixelRatio);
  renderer.setSize(width, height);
  el.appendChild(renderer.domElement);

  controls = new OrbitControls(camera, renderer.domElement);
  controls.enableDamping = true;
  controls.dampingFactor = 0.1;
  controls.enablePan = true;
  controls.screenSpacePanning = true;
  controls.target.set(0, 0, 0);

  raycaster = new THREE.Raycaster();
  mouseNDC = new THREE.Vector2();

  resizeHandler = () => {
    if (!renderer || !camera || !canvasContainer.value) return;
    const w = canvasContainer.value.clientWidth || 400;
    const h = canvasContainer.value.clientHeight || 300;
    renderer.setSize(w, h);
    camera.aspect = w / h;
    camera.updateProjectionMatrix();
  };
  window.addEventListener("resize", resizeHandler);

  clickHandler = (event: MouseEvent) => {
    if (!renderer || !camera || !lineObject || !raycaster || !mouseNDC) return;
    if (event.button !== 0) return; // left click only

    const rect = renderer.domElement.getBoundingClientRect();
    mouseNDC.x = ((event.clientX - rect.left) / rect.width) * 2 - 1;
    mouseNDC.y = -((event.clientY - rect.top) / rect.height) * 2 + 1;

    raycaster.setFromCamera(mouseNDC, camera);
    const hits = raycaster.intersectObject(lineObject, false);
    if (!hits.length) return;

    const hit = hits[0] as any;
    const index = hit.index ?? 0;
    const posAttr = lineObject.geometry.getAttribute("position") as any;
    const count = posAttr.count || 1;

    const clampedIndex = Math.max(0, Math.min(count - 1, index));
    const x = posAttr.getX(clampedIndex);
    const y = posAttr.getY(clampedIndex);
    const z = posAttr.getZ(clampedIndex);

    ensureHighlightSphere();
    if (highlightSphere) {
      highlightSphere.position.set(x, y, z);
    }

    if (currentSourceLineCount > 0) {
      const approxLine = Math.round(
        (clampedIndex / Math.max(1, count - 1)) * (currentSourceLineCount - 1)
      );
      emit("lineSelected", approxLine);
    }
  };

  renderer.domElement.addEventListener("click", clickHandler);

  const animate = () => {
    if (!renderer || !scene || !camera) return;
    if (controls) controls.update();
    renderer.render(scene, camera);
    animationId = requestAnimationFrame(animate);
  };
  animate();

  isThreeReady.value = true;
  console.log("[GCode3DViewer] initThree complete");
}

function updateGeometry(gcode: string) {
  if (!scene || !camera) {
    console.warn("[GCode3DViewer] updateGeometry: scene/camera not ready");
    return;
  }

  clearLine();

  const result = parseGcode(gcode || "");
  if (!result) return;

  const { geometry, sourceLineCount, truncated } = result;
  currentSourceLineCount = sourceLineCount;
  lastTruncated.value = truncated;

  const mat = new THREE.LineBasicMaterial({ color: 0x4287f5 });
  lineObject = new THREE.Line(geometry, mat);
  scene.add(lineObject);

  const bbox = new THREE.Box3().setFromObject(lineObject);
  const size = new THREE.Vector3();
  const center = new THREE.Vector3();
  bbox.getSize(size);
  bbox.getCenter(center);

  const maxDim = Math.max(size.x || 1, size.y || 1, size.z || 1);
  const dist = maxDim * 2.5;

  camera.position.set(center.x + dist, center.y + dist, center.z + dist);
  camera.lookAt(center);
  camera.updateProjectionMatrix();

  if (controls) {
    controls.target.copy(center);
    controls.update();
  }

  const posAttr = geometry.getAttribute("position") as any;
  console.log(
    "[GCode3DViewer] built",
    posAttr.count,
    "points; truncated =",
    truncated
  );
}

function highlightLine(line: number | null | undefined) {
  if (!scene || !lineObject || line == null || line < 0) return;
  const geom = lineObject.geometry;
  const posAttr = geom.getAttribute("position") as any;
  const count = posAttr.count || 0;
  if (!count || currentSourceLineCount <= 0) return;

  const t = Math.max(
    0,
    Math.min(1, line / Math.max(1, currentSourceLineCount - 1))
  );
  const idx = Math.round(t * (count - 1));

  const x = posAttr.getX(idx);
  const y = posAttr.getY(idx);
  const z = posAttr.getZ(idx);

  ensureHighlightSphere();
  if (highlightSphere) {
    highlightSphere.position.set(x, y, z);
  }
}

// ---------- lifecycle ----------

onMounted(() => {
  console.log(
    "[GCode3DViewer] onMounted, initial gcode length =",
    (props.gcode || "").length
  );

  nextTick(() => {
    initThree();

    // once Three is ready, build geometry for the initial G-code
    if (isThreeReady.value) {
      console.log(
        "[GCode3DViewer] onMounted -> calling updateGeometry, len =",
        (props.gcode || "").length
      );
      updateGeometry(props.gcode || "");
    } else {
      console.warn(
        "[GCode3DViewer] initThree did not complete, skipping initial updateGeometry"
      );
    }
  });
});

watch(
  () => props.gcode,
  (val) => {
    console.log("[GCode3DViewer] gcode watcher, len =", (val || "").length);

    if (!isThreeReady.value) {
      console.warn(
        "[GCode3DViewer] gcode changed before Three ready; ignoring this change"
      );
      return;
    }

    updateGeometry(val || "");
  },
  {
    immediate: false, // <— key change
  }
);

watch(
  () => props.cursorLine,
  (line) => {
    highlightLine(line ?? null);
  }
);

onBeforeUnmount(() => {
  if (animationId != null) {
    cancelAnimationFrame(animationId);
  }

  if (resizeHandler) {
    window.removeEventListener("resize", resizeHandler);
    resizeHandler = null;
  }

  if (renderer && clickHandler) {
    renderer.domElement.removeEventListener("click", clickHandler);
    clickHandler = null;
  }

  clearLine();

  if (controls) {
    controls.dispose();
  }

  if (renderer) {
    renderer.dispose();
    if (renderer.domElement && canvasContainer.value?.contains(renderer.domElement)) {
      canvasContainer.value.removeChild(renderer.domElement);
    }
  }

  scene = null;
  camera = null;
  renderer = null;
  controls = null;
  raycaster = null;
  mouseNDC = null;
  highlightSphere = null;
});
</script>

<style scoped>
.gcode-3d-viewer {
  position: relative;
  width: 100%;
  height: 100%;
  background: #1e1e1e;
}

.gcode-debug {
  position: absolute;
  top: 4px;
  left: 8px;
  font-size: 11px;
  color: #ccc;
  z-index: 1;
}

.gcode-canvas {
  width: 100%;
  height: 100%;
}
</style>
