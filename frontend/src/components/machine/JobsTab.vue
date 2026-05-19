<template>
  <section class="m-jobs">
    <header class="m-jobs__head">
      <h2 class="m-jobs__title">{{ t("jobs.title") }}</h2>
      <label class="m-jobs__window">
        {{ t("jobs.windowLabel") }}
        <select v-model.number="windowDays" @change="refresh">
          <option :value="1">{{ t("jobs.window24h") }}</option>
          <option :value="7">{{ t("jobs.window7d") }}</option>
          <option :value="30">{{ t("jobs.window30d") }}</option>
          <option :value="0">{{ t("jobs.windowAll") }}</option>
        </select>
      </label>
      <button class="m-jobs__refresh" @click="refresh" :disabled="loading">
        {{ loading ? "…" : t("jobs.refresh") }}
      </button>
    </header>

    <div v-if="errMsg" class="m-jobs__err">{{ errMsg }}</div>

    <!-- Aggregate cards. The four-up layout collapses to two-up on narrow
         viewports via auto-fit; on a phone or kiosk the cards stack. -->
    <div class="m-jobs__cards" v-if="stats">
      <div class="m-jobs__card">
        <div class="m-jobs__card-label">{{ t("jobs.totalJobs") }}</div>
        <div class="m-jobs__card-value">{{ stats.total_jobs }}</div>
        <div class="m-jobs__card-sub">
          <span class="m-jobs__chip m-jobs__chip--ok">{{ stats.completed_jobs }} {{ t("jobs.statusCompleted") }}</span>
          <span v-if="stats.stopped_jobs > 0" class="m-jobs__chip">{{ stats.stopped_jobs }} {{ t("jobs.statusStopped") }}</span>
          <span v-if="stats.errored_jobs > 0" class="m-jobs__chip m-jobs__chip--err">{{ stats.errored_jobs }} {{ t("jobs.statusError") }}</span>
        </div>
      </div>
      <div class="m-jobs__card">
        <div class="m-jobs__card-label">{{ t("jobs.runtime") }}</div>
        <div class="m-jobs__card-value">{{ fmtDuration(stats.total_run_seconds) }}</div>
        <div class="m-jobs__card-sub">
          {{ t("jobs.avgRuntime", { v: fmtDuration(stats.avg_run_seconds) }) }}
        </div>
      </div>
      <div class="m-jobs__card">
        <div class="m-jobs__card-label">{{ t("jobs.longest") }}</div>
        <div class="m-jobs__card-value">{{ fmtDuration(stats.longest_run_seconds) }}</div>
      </div>
      <div class="m-jobs__card">
        <div class="m-jobs__card-label">{{ t("jobs.lastJob") }}</div>
        <div class="m-jobs__card-value m-jobs__card-value--small">
          {{ stats.last_job ? fmtTs(stats.last_job.started_at) : "—" }}
        </div>
        <div class="m-jobs__card-sub" v-if="stats.last_job">
          <code>{{ fileName(stats.last_job.file_path) }}</code>
          —
          <span :class="statusClass(stats.last_job.status)">{{ stats.last_job.status }}</span>
        </div>
      </div>
    </div>

    <h3 v-if="stats?.top_files?.length" class="m-jobs__h">{{ t("jobs.topFiles") }}</h3>
    <table v-if="stats?.top_files?.length" class="m-jobs__top">
      <thead>
        <tr>
          <th>{{ t("jobs.file") }}</th>
          <th class="num">{{ t("jobs.runs") }}</th>
          <th class="num">{{ t("jobs.totalRuntime") }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="f in stats.top_files" :key="f.file_path">
          <td><code>{{ fileName(f.file_path) }}</code></td>
          <td class="num">{{ f.runs }}</td>
          <td class="num">{{ fmtDuration(f.run_seconds) }}</td>
        </tr>
      </tbody>
    </table>

    <h3 class="m-jobs__h">{{ t("jobs.recent") }}</h3>
    <div v-if="entries.length === 0 && !loading" class="m-jobs__empty">
      {{ t("jobs.emptyHint") }}
    </div>
    <table v-if="entries.length > 0" class="m-jobs__table">
      <thead>
        <tr>
          <th>{{ t("jobs.startedAt") }}</th>
          <th>{{ t("jobs.file") }}</th>
          <th class="num">{{ t("jobs.lines") }}</th>
          <th class="num">{{ t("jobs.duration") }}</th>
          <th>{{ t("jobs.statusCol") }}</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="e in entries" :key="e.job_id">
          <td>{{ fmtTs(e.started_at) }}</td>
          <td>
            <code>{{ fileName(e.file_path) }}</code>
            <span v-if="e.method" class="m-jobs__method">{{ e.method }}</span>
          </td>
          <td class="num">{{ e.line_final.toLocaleString() }} / {{ e.line_total.toLocaleString() }}</td>
          <td class="num">{{ fmtDuration(Math.round(e.duration_ms / 1000)) }}</td>
          <td>
            <span :class="statusClass(e.status)">{{ e.status }}</span>
            <span v-if="e.error_msg" class="m-jobs__err-text" :title="e.error_msg">⚠</span>
          </td>
        </tr>
      </tbody>
    </table>
  </section>
</template>

<script setup lang="ts">
import { onMounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { cnc as cncApi } from "@/api";
import type { JobHistoryEntry, JobStats } from "@/api/cnc";

const { t } = useI18n();

const props = defineProps<{
  machineId?: string;
}>();

const entries = ref<JobHistoryEntry[]>([]);
const stats = ref<JobStats | null>(null);
const windowDays = ref<number>(7);
const loading = ref(false);
const errMsg = ref("");

const refresh = async () => {
  loading.value = true;
  errMsg.value = "";
  try {
    const [list, st] = await Promise.all([
      cncApi.listJobs({ machineId: props.machineId, limit: 100 }),
      cncApi.getJobStats({ machineId: props.machineId, days: windowDays.value }),
    ]);
    entries.value = list.entries || [];
    stats.value = st;
  } catch (e: any) {
    errMsg.value = e?.message || String(e);
  } finally {
    loading.value = false;
  }
};

const fileName = (p: string) => {
  if (!p) return "—";
  const slash = p.lastIndexOf("/");
  return slash >= 0 ? p.slice(slash + 1) : p;
};

const fmtTs = (iso: string) => {
  if (!iso) return "—";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
};

const fmtDuration = (secs: number) => {
  if (!secs || secs <= 0) return "0s";
  if (secs < 60) return `${secs}s`;
  const m = Math.floor(secs / 60);
  const s = secs % 60;
  if (m < 60) return `${m}m ${s}s`;
  const h = Math.floor(m / 60);
  const rm = m % 60;
  if (h < 24) return `${h}h ${rm}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
};

const statusClass = (s: string) => {
  if (s === "completed") return "m-jobs__status m-jobs__status--ok";
  if (s === "stopped") return "m-jobs__status m-jobs__status--warn";
  if (s === "error") return "m-jobs__status m-jobs__status--err";
  return "m-jobs__status";
};

watch(() => props.machineId, refresh);
onMounted(refresh);
</script>

<style scoped>
.m-jobs {
  padding: 12px;
  overflow: auto;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.m-jobs__head {
  display: flex;
  align-items: center;
  gap: 12px;
}
.m-jobs__title {
  margin: 0;
  font-size: 14px;
  font-weight: 500;
  flex: 1;
}
.m-jobs__window {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 11px;
  color: var(--fg-muted, #888);
}
.m-jobs__window select {
  padding: 2px 4px;
  font-size: 11px;
  border: 1px solid var(--border-color, #ddd);
  border-radius: 3px;
  background: var(--surface, #fff);
}
.m-jobs__refresh {
  padding: 4px 10px;
  font-size: 11px;
  background: var(--alt-background, #f4f4f4);
  border: 1px solid var(--border-color, #ddd);
  border-radius: 3px;
  cursor: pointer;
}
.m-jobs__err {
  padding: 6px 10px;
  background: rgba(198, 40, 40, 0.1);
  color: #c62828;
  border-radius: 3px;
  font-size: 12px;
}
.m-jobs__cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 8px;
}
.m-jobs__card {
  background: var(--alt-background, #fafafa);
  border: 1px solid var(--border-color, #eee);
  border-radius: 6px;
  padding: 10px 12px;
}
.m-jobs__card-label {
  font-size: 10px;
  color: var(--fg-muted, #888);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  font-weight: 500;
}
.m-jobs__card-value {
  font-size: 22px;
  font-weight: 600;
  color: var(--textPrimary, #222);
  font-variant-numeric: tabular-nums;
  margin-top: 2px;
}
.m-jobs__card-value--small {
  font-size: 13px;
}
.m-jobs__card-sub {
  font-size: 10px;
  color: var(--fg-muted, #666);
  margin-top: 4px;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  align-items: center;
}
.m-jobs__chip {
  padding: 1px 6px;
  border-radius: 999px;
  background: var(--surface-hover, rgba(0,0,0,0.05));
  font-size: 10px;
}
.m-jobs__chip--ok { background: rgba(46, 125, 50, 0.12); color: #2e7d32; }
.m-jobs__chip--err { background: rgba(198, 40, 40, 0.12); color: #c62828; }

.m-jobs__h {
  margin: 0;
  font-size: 12px;
  font-weight: 500;
  color: var(--fg-muted, #555);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.m-jobs__empty {
  font-size: 12px;
  color: var(--fg-muted, #888);
  padding: 12px;
  text-align: center;
  border: 1px dashed var(--border-color, #ddd);
  border-radius: 4px;
}
.m-jobs__table,
.m-jobs__top {
  width: 100%;
  border-collapse: collapse;
  font-size: 12px;
}
.m-jobs__table th,
.m-jobs__table td,
.m-jobs__top th,
.m-jobs__top td {
  text-align: left;
  padding: 4px 6px;
  border-bottom: 1px solid var(--border-color, #eee);
}
.m-jobs__table th,
.m-jobs__top th {
  font-size: 10px;
  text-transform: uppercase;
  color: var(--fg-muted, #888);
  letter-spacing: 0.03em;
  background: var(--alt-background, #fafafa);
  position: sticky;
  top: 0;
}
.m-jobs__table .num,
.m-jobs__top .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
}
.m-jobs__method {
  margin-left: 6px;
  padding: 0 5px;
  border-radius: 2px;
  font-size: 9px;
  background: rgba(24, 95, 165, 0.12);
  color: #185FA5;
  text-transform: uppercase;
}
.m-jobs__status {
  font-size: 11px;
  font-weight: 500;
  text-transform: lowercase;
}
.m-jobs__status--ok { color: #2e7d32; }
.m-jobs__status--warn { color: #BA7517; }
.m-jobs__status--err { color: #c62828; }
.m-jobs__err-text {
  margin-left: 4px;
  color: #c62828;
  cursor: help;
}
</style>
