<template>
  <div class="m-playback" :class="{ 'm-playback--disabled': disabled }">
    <button
      class="m-playback__btn"
      :disabled="disabled || !canStepBack"
      :title="t('playback.stepBack')"
      @click="stepBack"
    >⏮</button>
    <button
      class="m-playback__btn m-playback__btn--primary"
      :disabled="disabled"
      :title="playing ? t('playback.pause') : t('playback.play')"
      @click="togglePlay"
    >{{ playing ? "⏸" : "▶" }}</button>
    <button
      class="m-playback__btn"
      :disabled="disabled || !canStepForward"
      :title="t('playback.stepForward')"
      @click="stepForward"
    >⏭</button>
    <button
      class="m-playback__btn"
      :disabled="disabled || currentLine <= 1"
      :title="t('playback.reset')"
      @click="reset"
    >↺</button>
    <input
      class="m-playback__scrub"
      type="range"
      :min="1"
      :max="Math.max(1, totalLines)"
      :value="currentLine"
      :disabled="disabled || totalLines === 0"
      :title="t('playback.scrub')"
      @input="onScrub(($event.target as HTMLInputElement).value)"
    />
    <span class="m-playback__pos" :title="t('playback.posTitle')">
      {{ currentLine.toLocaleString() }} / {{ totalLines.toLocaleString() }}
    </span>
    <label class="m-playback__speed" :title="t('playback.speedTitle')">
      <span>{{ speed }}</span>
      <span class="m-playback__speed-unit">{{ t("playback.linesPerSec") }}</span>
      <input
        class="m-playback__speed-range"
        type="range"
        :min="1"
        :max="500"
        :step="1"
        :value="speed"
        :disabled="disabled"
        @input="onSpeed(($event.target as HTMLInputElement).value)"
      />
    </label>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from "vue";
import { useI18n } from "vue-i18n";

const { t } = useI18n();

const props = withDefaults(
  defineProps<{
    currentLine: number;
    totalLines: number;
    // True when a real streaming job is driving the cursor — playback
    // would fight the live line, so we lock the bar out and grey it.
    disabled?: boolean;
    initialSpeed?: number;
  }>(),
  { initialSpeed: 10, disabled: false }
);

const emit = defineEmits<{
  (e: "update:line", line: number): void;
  (e: "play"): void;
  (e: "pause"): void;
}>();

const playing = ref(false);
// Speed lives in localStorage so an operator who likes 30 l/s doesn't
// have to drag the slider every time they open a file. Bounded same as
// the input range below.
const STORAGE_KEY = "gcodePlaybackSpeed";
const initial = (() => {
  const v = parseFloat(localStorage.getItem(STORAGE_KEY) || "");
  return Number.isFinite(v) && v >= 1 && v <= 500 ? v : props.initialSpeed;
})();
const speed = ref<number>(initial);

const canStepForward = computed(() => props.currentLine < props.totalLines);
const canStepBack = computed(() => props.currentLine > 1);

let timer: ReturnType<typeof setInterval> | null = null;

const stopTimer = () => {
  if (timer) {
    clearInterval(timer);
    timer = null;
  }
};

// Interval based on the current speed. Re-armed when speed changes.
// 1000 / speed gives ms per line; floor at 2ms so a 500-l/s sweep
// doesn't pin a CPU core on a slow Pi.
const armTimer = () => {
  stopTimer();
  if (!playing.value) return;
  const ms = Math.max(2, Math.floor(1000 / Math.max(1, speed.value)));
  timer = setInterval(() => {
    const next = props.currentLine + 1;
    if (next > props.totalLines) {
      // Auto-pause at EOF — the operator can hit play again to loop
      // manually, but we don't loop on our own.
      stop();
      return;
    }
    emit("update:line", next);
  }, ms);
};

const togglePlay = () => {
  if (props.disabled) return;
  if (playing.value) {
    stop();
  } else {
    play();
  }
};

const play = () => {
  if (props.disabled) return;
  // If we're sitting on EOF, rewind first so play does something.
  if (props.currentLine >= props.totalLines) {
    emit("update:line", 1);
  }
  playing.value = true;
  emit("play");
  armTimer();
};

const stop = () => {
  playing.value = false;
  emit("pause");
  stopTimer();
};

const stepForward = () => {
  if (props.disabled || !canStepForward.value) return;
  stop();
  emit("update:line", props.currentLine + 1);
};

const stepBack = () => {
  if (props.disabled || !canStepBack.value) return;
  stop();
  emit("update:line", props.currentLine - 1);
};

const reset = () => {
  if (props.disabled) return;
  stop();
  emit("update:line", 1);
};

const onScrub = (raw: string) => {
  const n = parseInt(raw, 10);
  if (!Number.isFinite(n)) return;
  stop();
  emit("update:line", n);
};

const onSpeed = (raw: string) => {
  const n = parseFloat(raw);
  if (!Number.isFinite(n) || n < 1) return;
  speed.value = n;
  localStorage.setItem(STORAGE_KEY, String(n));
  // If a tick is currently in flight, re-arm it at the new rate so the
  // change feels immediate.
  if (playing.value) armTimer();
};

// Pause whenever the parent disables us (a real job started streaming
// this file). The cursor will then track the live machineLine instead.
watch(
  () => props.disabled,
  (off) => {
    if (off && playing.value) stop();
  }
);

onBeforeUnmount(() => {
  stopTimer();
});
</script>

<style scoped>
.m-playback {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 6px 10px;
  background: #1f1f1d;
  border-top: 1px solid #333;
  color: #B4B2A9;
  font-size: 11px;
  font-variant-numeric: tabular-nums;
}
.m-playback--disabled {
  opacity: 0.55;
}
.m-playback__btn {
  background: #2C2C2A;
  color: #D3D1C7;
  border: 1px solid #444;
  border-radius: 3px;
  width: 26px;
  height: 24px;
  font-size: 12px;
  cursor: pointer;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}
.m-playback__btn:hover:not(:disabled) { background: #3a3a37; color: #fff; }
.m-playback__btn:disabled { opacity: 0.35; cursor: not-allowed; }
.m-playback__btn--primary {
  background: #185FA5;
  border-color: #14507f;
  color: #fff;
}
.m-playback__btn--primary:hover:not(:disabled) { background: #1d6dbd; }

.m-playback__scrub {
  flex: 1 1 0;
  min-width: 80px;
  accent-color: #185FA5;
}
.m-playback__pos {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 11px;
  white-space: nowrap;
  min-width: 100px;
  text-align: right;
}
.m-playback__speed {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  white-space: nowrap;
}
.m-playback__speed-unit { color: #888780; font-size: 10px; }
.m-playback__speed-range {
  width: 70px;
  accent-color: #639922;
}
</style>
