<template>
  <errors v-if="error" :errorCode="error.status" />
  <div class="row" v-else-if="!layoutStore.loading">
    <div class="column">
      <div class="card">
        <div class="card-title">
          <h2>{{ t("displays.title") }}</h2>
        </div>
        <div class="card-content">
          <p class="small">{{ t("displays.intro") }}</p>

          <div v-if="errMsg" class="m-err">{{ errMsg }}</div>

          <div v-if="displays.length === 0" class="m-empty">
            {{ t("displays.empty") }}
          </div>

          <!-- One editable row per display. Add a new row at the bottom
               with the + button. Save persists the whole row to the
               backend via PUT (existing) or POST (new). -->
          <div
            v-for="(d, idx) in displays"
            :key="d.id || `new-${idx}`"
            class="display-row"
          >
            <div class="display-row__header">
              <h3>
                <input
                  class="input display-row__name"
                  type="text"
                  v-model="d.name"
                  :placeholder="t('displays.namePlaceholder')"
                />
              </h3>
              <button
                type="button"
                class="button button--flat display-row__delete"
                @click="onDelete(idx)"
              >
                <i class="material-icons">delete</i>
                {{ t("displays.delete") }}
              </button>
            </div>

            <p v-if="d.id">
              <label class="small">{{ t("displays.idLabel") }}</label>
              <code class="display-row__id">{{ d.id }}</code>
              <span class="small display-row__hint">
                {{ t("displays.idHint") }}
              </span>
            </p>

            <p>
              <label class="small">{{ t("displays.machineLabel") }}</label>
              <select class="input input--block" v-model="d.machineId">
                <option value="" disabled>{{ t("displays.machinePick") }}</option>
                <option v-for="m in machines" :key="m.id" :value="m.id">
                  {{ m.name || m.id }}
                </option>
              </select>
            </p>

            <p>
              <label class="small">{{ t("displays.tokenLabel") }}</label>
              <input
                class="input input--block"
                type="text"
                v-model="d.token"
                :placeholder="t('displays.tokenPlaceholder')"
              />
              <span class="small display-row__hint">
                {{ t("displays.tokenHint") }}
              </span>
            </p>

            <div class="display-row__grid">
              <p>
                <label class="small">{{ t("displays.resolution") }}</label>
                <span class="display-row__pair">
                  <input class="input" type="number" v-model.number="d.resolution[0]" min="1" :placeholder="String(defaults.resX)" />
                  ×
                  <input class="input" type="number" v-model.number="d.resolution[1]" min="1" :placeholder="String(defaults.resY)" />
                </span>
              </p>
              <p>
                <label class="small">{{ t("displays.pocketGrid") }}</label>
                <span class="display-row__pair">
                  <input class="input" type="number" v-model.number="d.pocketGrid[0]" min="1" :placeholder="String(defaults.cols)" />
                  ×
                  <input class="input" type="number" v-model.number="d.pocketGrid[1]" min="1" :placeholder="String(defaults.rows)" />
                </span>
              </p>
              <p>
                <label class="small">{{ t("displays.libraryPageSize") }}</label>
                <input class="input" type="number" v-model.number="d.libraryPageSize" min="1" :placeholder="String(defaults.pageSize)" />
              </p>
              <p>
                <label class="small">{{ t("displays.units") }}</label>
                <select class="input input--block" v-model="d.units">
                  <option value="">{{ t("displays.unitsFromMachine") }}</option>
                  <option value="in">in</option>
                  <option value="mm">mm</option>
                </select>
              </p>
              <p>
                <label class="small">{{ t("displays.pollPowered") }}</label>
                <input class="input" type="number" v-model.number="d.pollIntervalPoweredS" min="10" :placeholder="String(defaults.pollPowered)" />
              </p>
              <p>
                <label class="small">{{ t("displays.pollBattery") }}</label>
                <input class="input" type="number" v-model.number="d.pollIntervalBatteryS" min="60" :placeholder="String(defaults.pollBattery)" />
              </p>
            </div>

            <p>
              <label class="small">{{ t("displays.fields") }}</label>
              <input
                class="input input--block"
                type="text"
                v-model="d.fieldsCsv"
                :placeholder="defaults.fieldsCsv"
              />
              <span class="small display-row__hint">
                {{ t("displays.fieldsHint") }}
              </span>
            </p>
          </div>

          <p>
            <button type="button" class="button button--flat" @click="onAdd">
              <i class="material-icons">add</i>
              {{ t("displays.add") }}
            </button>
            <button type="button" class="button" @click="onSaveAll" :disabled="saving">
              {{ saving ? t("displays.saving") : t("displays.saveAll") }}
            </button>
          </p>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { useLayoutStore } from "@/stores/layout";
import { cnc as cncApi } from "@/api";
import type { Display } from "@/api/cnc";
import Errors from "@/views/Errors.vue";

const { t } = useI18n();
const layoutStore = useLayoutStore();

// We extend each row with `fieldsCsv` for the comma-separated input.
// On save we split it back to string[].
type Row = Display & { fieldsCsv: string; _isNew?: boolean };

const defaults = {
  resX: 800,
  resY: 480,
  cols: 2,
  rows: 10,
  pageSize: 20,
  pollPowered: 60,
  pollBattery: 900,
  fieldsCsv: "pocket, tool_number, description, diameter, length, wear",
};

const displays = reactive<Row[]>([]);
const machines = ref<{ id: string; name: string }[]>([]);
const errMsg = ref("");
const error = ref<{ status: number } | null>(null);
const saving = ref(false);

const refresh = async () => {
  errMsg.value = "";
  try {
    const [list, ms] = await Promise.all([
      cncApi.listDisplays(),
      cncApi.listMachines(),
    ]);
    displays.splice(0, displays.length);
    for (const d of list.displays || []) {
      displays.push(rowFromApi(d));
    }
    machines.value = (ms.machines || []).map((m: any) => ({
      id: m.id,
      name: m.name,
    }));
  } catch (e: any) {
    if (e?.status) {
      error.value = { status: e.status };
    } else {
      errMsg.value = e?.message || String(e);
    }
  }
};

const rowFromApi = (d: Display): Row => ({
  ...d,
  resolution: d.resolution || [0, 0],
  pocketGrid: d.pocketGrid || [0, 0],
  fields: d.fields || [],
  fieldsCsv: (d.fields || []).join(", "),
});

const onAdd = () => {
  displays.push({
    id: "",
    name: "",
    machineId: machines.value[0]?.id || "",
    token: "",
    resolution: [0, 0],
    pocketGrid: [0, 0],
    libraryPageSize: 0,
    fields: [],
    fieldsCsv: "",
    units: "",
    pollIntervalPoweredS: 0,
    pollIntervalBatteryS: 0,
    _isNew: true,
  });
};

const onDelete = async (idx: number) => {
  const d = displays[idx];
  if (!confirm(t("displays.confirmDelete", { name: d.name || d.id || "?" }) as string)) {
    return;
  }
  if (d.id && !d._isNew) {
    try {
      await cncApi.deleteDisplay(d.id);
    } catch (e: any) {
      errMsg.value = e?.message || String(e);
      return;
    }
  }
  displays.splice(idx, 1);
};

const fromRow = (r: Row): Display => ({
  id: r.id,
  name: r.name,
  machineId: r.machineId,
  token: r.token,
  resolution: r.resolution,
  pocketGrid: r.pocketGrid,
  libraryPageSize: r.libraryPageSize,
  fields: r.fieldsCsv
    ? r.fieldsCsv.split(",").map((s) => s.trim()).filter(Boolean)
    : [],
  units: r.units,
  pollIntervalPoweredS: r.pollIntervalPoweredS,
  pollIntervalBatteryS: r.pollIntervalBatteryS,
});

const onSaveAll = async () => {
  saving.value = true;
  errMsg.value = "";
  try {
    for (let i = 0; i < displays.length; i++) {
      const r = displays[i];
      if (!r.machineId) continue;
      const payload = fromRow(r);
      const saved = r._isNew
        ? await cncApi.createDisplay(payload)
        : await cncApi.updateDisplay(r.id, payload);
      displays[i] = rowFromApi(saved);
    }
  } catch (e: any) {
    errMsg.value = e?.message || String(e);
  } finally {
    saving.value = false;
  }
};

onMounted(refresh);
</script>

<style scoped>
.display-row {
  border: 1px solid var(--border-color, #ddd);
  border-radius: 6px;
  padding: 12px 14px;
  margin-bottom: 12px;
  background: var(--alt-background, #fafafa);
}
.display-row__header {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.display-row__header h3 {
  flex: 1;
  margin: 0;
  display: flex;
  gap: 8px;
}
.display-row__name {
  flex: 1;
  font-size: 14px;
  font-weight: 500;
}
.display-row__delete {
  flex-shrink: 0;
}
.display-row__id {
  background: var(--surface, #fff);
  padding: 2px 8px;
  border-radius: 3px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
}
.display-row__hint {
  display: block;
  color: var(--fg-muted, #888);
  margin-top: 2px;
}
.display-row__grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 8px;
}
.display-row__pair {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.display-row__pair .input {
  width: 70px;
}
.m-err {
  padding: 6px 10px;
  margin-bottom: 8px;
  background: rgba(198, 40, 40, 0.1);
  color: #c62828;
  border-radius: 4px;
  font-size: 12px;
}
.m-empty {
  padding: 16px;
  text-align: center;
  color: var(--fg-muted, #888);
  border: 1px dashed var(--border-color, #ddd);
  border-radius: 4px;
  margin-bottom: 12px;
}
</style>
