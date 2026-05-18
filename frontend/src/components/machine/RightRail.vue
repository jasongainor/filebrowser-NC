<template>
  <aside class="m-rail">
    <div class="m-card">
      <div class="m-card__label">{{ t("machine.progress") }}</div>
      <div class="m-progress-row">
        <span class="m-progress-num">
          {{ lineCurrent.toLocaleString() }} / {{ lineTotal.toLocaleString() }}
          <span v-if="currentBlock" class="m-progress-nblock" :title="t('machine.progressNBlockTitle')">
            N{{ currentBlock }}
          </span>
        </span>
        <span v-if="etaLabel" class="m-progress-eta">{{ etaLabel }}</span>
      </div>
      <div class="m-progress-bar">
        <div class="m-progress-fill" :style="{ width: pctWidth }" />
      </div>
      <div v-if="motionMode && motionMode !== 'unknown'" class="m-motion-row">
        <span
          v-if="motionMode === 'rapid'"
          class="m-motion m-motion--rapid"
          :title="t('machine.motionRapidTitle')"
        >RAPID</span>
        <span
          v-else
          class="m-motion m-motion--feed"
          :title="t('machine.motionFeedTitle')"
        >
          <span class="m-motion__label">F</span>
          <span class="m-motion__val">{{ motionFeed !== null && motionFeed !== undefined ? motionFeed.toFixed(1) : "—" }}</span>
        </span>
      </div>
    </div>

    <div class="m-card">
      <div class="m-card__label">{{ t("machine.positionLabel") }}</div>
      <div class="m-pos-table" :style="{ 'grid-template-rows': `auto repeat(${axes.length}, auto)` }">
        <div class="m-pos-th-axis"></div>
        <div class="m-pos-th">{{ t("machine.posMach") }}</div>
        <div class="m-pos-th">{{ t("machine.posWork") }}</div>
        <div class="m-pos-th">{{ t("machine.posDeltaCmd") }}</div>
        <template v-for="ax in axes" :key="ax">
          <div class="m-pos-axis">{{ ax }}</div>
          <div class="m-pos-val">{{ fmtAxis(parsed(`pos_${ax.toLowerCase()}`)) }}</div>
          <div class="m-pos-val">{{ fmtAxis(parsed(`work_${ax.toLowerCase()}`)) }}</div>
          <div class="m-pos-val" :class="deltaClass(ax)">{{ fmtAxis(deltaCmd(ax)) }}</div>
        </template>
      </div>
    </div>

    <div class="m-camera">
      <span v-if="cameraConfigured" class="m-camera__live">● {{ t("machine.cameraLive") }}</span>
      <video
        v-if="cameraKind === 'hls'"
        :src="cameraURL"
        controls
        autoplay
        muted
        playsinline
        class="m-camera__frame"
      />
      <img
        v-else-if="cameraKind === 'snapshot'"
        :src="snapshotSrc"
        class="m-camera__frame"
        alt=""
      />
      <iframe
        v-else-if="cameraKind === 'iframe'"
        :src="cameraURL"
        class="m-camera__frame m-camera__frame--iframe"
        allow="autoplay; fullscreen; encrypted-media"
        referrerpolicy="no-referrer"
      />
      <div v-else-if="cameraKind === 'rtsp'" class="m-camera__hint">
        {{ t("machine.rtspNotSupported") }}
      </div>
      <div v-else class="m-camera__hint">{{ t("machine.cameraNone") }}</div>
      <button v-if="cameraConfigured" class="m-camera__expand" @click="$emit('expand-camera')" :title="t('machine.cameraExpand')">⛶</button>
    </div>
  </aside>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { useCncStore } from "@/stores/cnc";

const { t } = useI18n();
const cnc = useCncStore();

const props = defineProps<{
  axes: string[];
  positionTolerance: number; // inches
  cameraURL: string;
  cameraType: string;
  lineCurrent: number;
  lineTotal: number;
  etaMs: number | null;
  // Modal motion at the current line, parsed from the NC content by
  // Machine.vue. "rapid" colours a red RAPID badge, "feed" surfaces the
  // F value, "unknown" hides the chip entirely (idle or pre-program).
  motionMode?: "rapid" | "feed" | "unknown";
  motionFeed?: number | null;
}>();

defineEmits<{
  (e: "expand-camera"): void;
}>();

const metric = (key: string) => cnc.metrics[key];
const parsed = (key: string): unknown => metric(key)?.parsed ?? null;

// Current N-block reported by the controller via macro #3030 (any
// program source — MEM, DNC, SD card). Rendered next to the
// "1234 / 5678" line counter so an operator running from machine
// memory sees "I'm on N240" while the streamer's line count stays
// at 0. Empty when the metric is stale or unset, so an idle machine
// doesn't show a misleading N0.
const currentBlock = computed<string>(() => {
  const m = cnc.metrics.current_block;
  if (!m || !m.value || m.stale) return "";
  const n = parseInt(m.value, 10);
  return Number.isFinite(n) && n > 0 ? String(n) : "";
});

const fmtAxis = (v: unknown): string => {
  if (typeof v === "number" && Number.isFinite(v)) return v.toFixed(4);
  if (typeof v === "string" && v !== "") return v;
  return "—";
};

const deltaCmd = (ax: string): number | null => {
  const mp = parsed(`pos_${ax.toLowerCase()}`);
  // No commanded-position metric on the wire today; the field is
  // reserved for when Q-code spec lands. For now display 0.0000 when
  // the machine position itself reads cleanly, "—" otherwise.
  if (typeof mp !== "number" || !Number.isFinite(mp)) return null;
  return 0;
};

const deltaClass = (ax: string) => {
  const v = deltaCmd(ax);
  if (v === null) return "m-pos-val--unknown";
  if (Math.abs(v) > props.positionTolerance) return "m-pos-val--warn";
  return "m-pos-val--ok";
};

const pctWidth = computed(() => {
  if (props.lineTotal <= 0) return "0%";
  const pct = Math.min(100, (props.lineCurrent / props.lineTotal) * 100);
  return `${pct}%`;
});

const fmtDuration = (ms: number) => {
  const s = Math.floor(ms / 1000);
  const m = Math.floor(s / 60);
  const sec = s % 60;
  if (m >= 60) {
    const h = Math.floor(m / 60);
    return `${h}h ${String(m % 60).padStart(2, "0")}m`;
  }
  return `${m}:${String(sec).padStart(2, "0")} ${t("machine.etaLeft")}`;
};
const etaLabel = computed(() => (props.etaMs && props.etaMs > 0 ? fmtDuration(props.etaMs) : ""));

// ── Camera ──
const cameraConfigured = computed(() => !!props.cameraURL && props.cameraType !== "none");
const snapshotTick = ref(0);
let snapshotTimer: ReturnType<typeof setInterval> | null = null;

const cameraKind = computed<"none" | "hls" | "snapshot" | "iframe" | "rtsp">(() => {
  const u = props.cameraURL;
  if (!u) return "none";
  switch (props.cameraType) {
    case "none": return "none";
    case "hls": return "hls";
    case "mjpeg": return "snapshot";
    case "iframe": return "iframe";
  }
  if (u.startsWith("rtsp://") || u.startsWith("rtsps://")) return "rtsp";
  if (u.endsWith(".m3u8")) return "hls";
  return "snapshot";
});

const snapshotSrc = computed(() => {
  if (!props.cameraURL) return "";
  const sep = props.cameraURL.includes("?") ? "&" : "?";
  return `${props.cameraURL}${sep}_t=${snapshotTick.value}`;
});

watch(cameraKind, (kind) => {
  if (snapshotTimer) { clearInterval(snapshotTimer); snapshotTimer = null; }
  if (kind === "snapshot") snapshotTimer = setInterval(() => snapshotTick.value++, 200);
}, { immediate: true });

onBeforeUnmount(() => { if (snapshotTimer) clearInterval(snapshotTimer); });
</script>

<style scoped>
.m-rail {
  display: flex;
  flex-direction: column;
  gap: 5px;
  min-height: 0;
  min-width: 0;
}
.m-card {
  background: var(--alt-background, #fafafa);
  border-radius: 6px;
  padding: 10px 12px;
  flex-shrink: 0;
  border: 1px solid var(--border-color, #eee);
}
.m-card__label {
  font-size: 10px;
  color: var(--fg-muted, #888);
  letter-spacing: 0.5px;
  font-weight: 500;
  text-transform: uppercase;
}
.m-progress-row {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  margin-top: 4px;
}
.m-progress-num {
  font-size: 16px;
  color: var(--textPrimary, #222);
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.m-progress-eta { font-size: 11px; color: var(--fg-muted, #888); }
/* N-block badge next to the line counter. Marker for "the controller
   thinks it's on N240 right now" so operators running from machine
   memory (no streamer line count) still have an authoritative pointer. */
.m-progress-nblock {
  display: inline-block;
  margin-left: 6px;
  padding: 0 6px;
  border-radius: 3px;
  font-size: 10px;
  font-weight: 500;
  background: rgba(24, 95, 165, 0.12);
  color: #185FA5;
  vertical-align: middle;
}
.m-progress-bar {
  height: 4px;
  background: var(--border-color, #e2e2e2);
  border-radius: 2px;
  overflow: hidden;
  margin-top: 6px;
}
.m-progress-fill {
  height: 100%;
  background: #185FA5;
  transition: width 0.25s ease;
}

/* RAPID vs F chip — surfaces the modal motion at the current line so
   the operator knows whether the machine is hauling at G00 or cutting
   at G01 + the active feed. Pulled from the NC content client-side so
   it works for both streamed and machine-memory jobs. */
.m-motion-row {
  display: flex;
  margin-top: 6px;
}
.m-motion {
  font-size: 11px;
  font-weight: 600;
  padding: 2px 8px;
  border-radius: 3px;
  letter-spacing: 0.05em;
  font-variant-numeric: tabular-nums;
}
.m-motion--rapid {
  background: rgba(198, 40, 40, 0.14);
  color: #c62828;
}
.m-motion--feed {
  background: rgba(99, 153, 34, 0.14);
  color: #639922;
  display: inline-flex;
  gap: 4px;
  align-items: baseline;
}
.m-motion__label { font-weight: 400; opacity: 0.8; }
.m-motion__val { font-weight: 600; }

.m-pos-table {
  display: grid;
  /* Position is the operator's primary read while standing at the
     machine — make it large enough to glance from a few feet away. */
  grid-template-columns: 22px 1fr 1fr 1fr;
  gap: 4px 8px;
  font-variant-numeric: tabular-nums;
  margin-top: 6px;
}
.m-pos-th {
  font-size: 10px;
  color: var(--fg-muted, #888);
  font-weight: 500;
  text-align: right;
  letter-spacing: 0.3px;
}
.m-pos-th-axis { font-size: 10px; }
.m-pos-axis { font-size: 18px; color: #185FA5; font-weight: 600; }
.m-pos-val { font-size: 18px; color: var(--textPrimary, #222); text-align: right; font-weight: 500; }
.m-pos-val--ok { color: #639922; }
.m-pos-val--warn { color: #BA7517; }
.m-pos-val--unknown { color: var(--fg-muted, #888); }

.m-camera {
  background: #2C2C2A;
  border-radius: 6px;
  flex: 1 1 0;
  min-height: 60px;
  display: flex;
  align-items: center;
  justify-content: center;
  color: #888780;
  font-size: 10px;
  position: relative;
  overflow: hidden;
}
.m-camera__frame {
  width: 100%;
  height: 100%;
  object-fit: contain;
  background: #000;
}
.m-camera__frame--iframe { border: 0; display: block; }
.m-camera__live {
  position: absolute;
  top: 4px;
  right: 4px;
  font-size: 9px;
  color: #C0DD97;
  z-index: 2;
}
.m-camera__expand {
  position: absolute;
  bottom: 4px;
  right: 4px;
  font-size: 12px;
  color: #888780;
  background: transparent;
  border: 0;
  cursor: pointer;
  z-index: 2;
}
.m-camera__hint {
  padding: 8px;
  text-align: center;
  font-size: 10px;
  color: #888780;
}
</style>
