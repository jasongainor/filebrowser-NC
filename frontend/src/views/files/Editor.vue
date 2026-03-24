<template>
  <div id="editor-container">
    <header-bar>
      <action icon="close" :label="t('buttons.close')" @action="close()" />
      <title>{{ fileStore.req?.name ?? "" }}</title>

      <action
        icon="add"
        @action="increaseFontSize"
        :label="t('buttons.increaseFontSize')"
      />
      <span class="editor-font-size">{{ fontSize }}px</span>
      <action
        icon="remove"
        @action="decreaseFontSize"
        :label="t('buttons.decreaseFontSize')"
      />

      <action
        v-if="authStore.user?.perm.modify"
        id="save-button"
        icon="save"
        :label="t('buttons.save')"
        @action="save()"
      />

      <action
        icon="preview"
        :label="t('buttons.preview')"
        @action="preview()"
        v-show="isMarkdownFile"
      />
    </header-bar>

    <!-- loading spinner -->
    <div class="loading delayed" v-if="layoutStore.loading">
      <div class="spinner">
        <div class="bounce1"></div>
        <div class="bounce2"></div>
        <div class="bounce3"></div>
      </div>
    </div>

    <template v-else>
      <Breadcrumbs base="/files" noLink />

      <!-- markdown preview -->
      <div
        v-show="isPreview && isMarkdownFile"
        id="preview-container"
        class="md_preview"
        v-html="previewContent"
      ></div>

      <!-- editor + (optional) 3D viewer -->
      <div
        v-show="!isPreview || !isMarkdownFile"
        class="editor-layout"
      >
        <!-- Ace editor -->
        <div class="editor-pane" id="editor"></div>

        <!-- 3D G-code viewer for NC/TAP/GCODE/CNC -->
        <div v-if="isGcodeFile" class="viewer-pane">
          <GCode3DViewer
            :gcode="debouncedGcode"
            :cursor-line="cursorLine"
            @select-line="handleViewerLineSelect"
          />
        </div>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import "@/ace-gcode.js";

import { files as api } from "@/api";
import buttons from "@/utils/buttons";
import url from "@/utils/url";

import ace, { Ace, version as ace_version } from "ace-builds";
import "ace-builds/src-noconflict/ext-language_tools";
import modelist from "ace-builds/src-noconflict/ext-modelist";
import DOMPurify from "dompurify";

import Breadcrumbs from "@/components/Breadcrumbs.vue";
import Action from "@/components/header/Action.vue";
import HeaderBar from "@/components/header/HeaderBar.vue";
import { useAuthStore } from "@/stores/auth";
import { useFileStore } from "@/stores/file";
import { useLayoutStore } from "@/stores/layout";
import { getEditorTheme } from "@/utils/theme";
import { marked } from "marked";
import {
  inject,
  onBeforeUnmount,
  onMounted,
  ref,
  watchEffect,
  computed,
} from "vue";
import { useI18n } from "vue-i18n";
import { onBeforeRouteUpdate, useRoute, useRouter } from "vue-router";

import GCode3DViewer from "@/components/GCode3DViewer.vue";

// ── debounce helper (avoids lodash dependency) ──────────────────────────────
function debounce<T extends (...args: any[]) => void>(fn: T, delay: number): T {
  let timer: ReturnType<typeof setTimeout> | null = null;
  return ((...args: any[]) => {
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => fn(...args), delay);
  }) as T;
}

const $showError = inject<IToastError>("$showError")!;

const fileStore = useFileStore();
const authStore = useAuthStore();
const layoutStore = useLayoutStore();

const { t } = useI18n();

const route = useRoute();
const router = useRouter();

const editor = ref<Ace.Editor | null>(null);
const cursorLine = ref<number | null>(null);
const fontSize = ref(parseInt(localStorage.getItem("editorFontSize") || "14"));

const isPreview = ref(false);
const previewContent = ref("");

// Debounced gcode string — only updates 600ms after the user stops typing.
// This is what gets passed to GCode3DViewer to prevent hammering the parser
// and Three.js geometry rebuild on every keystroke.
const debouncedGcode = ref<string>(fileStore.req?.content || "");

const updateDebouncedGcode = debounce((val: string) => {
  debouncedGcode.value = val;
}, 600);

// ── file type helpers ────────────────────────────────────────────────────────
const isMarkdownFile = computed(() => {
  const name = fileStore.req?.name || "";
  return name.endsWith(".md") || name.endsWith(".markdown");
});

const isGcodeFile = computed(() => {
  const name = (fileStore.req?.name || "").toLowerCase();
  return (
    name.endsWith(".nc") ||
    name.endsWith(".tap") ||
    name.endsWith(".gcode") ||
    name.endsWith(".cnc")
  );
});

// ── lifecycle ────────────────────────────────────────────────────────────────
// markdown preview — at setup level so Vue manages its lifecycle correctly
watchEffect(async () => {
  if (isMarkdownFile.value && isPreview.value) {
    const new_value = editor.value?.getValue() || "";
    try {
      previewContent.value = DOMPurify.sanitize(await marked(new_value));
    } catch (error) {
      console.error("Failed to convert content to HTML:", error);
      previewContent.value = "";
    }
  }
});

onMounted(() => {
  window.addEventListener("keydown", keyEvent);
  window.addEventListener("beforeunload", handlePageChange);

  const fileContent = fileStore.req?.content || "";

  // seed the debounced value immediately so the viewer has content on first render
  debouncedGcode.value = fileContent;

  ace.config.set(
    "basePath",
    `https://cdn.jsdelivr.net/npm/ace-builds@${ace_version}/src-min-noconflict/`
  );

  editor.value = ace.edit("editor", {
    value: fileContent,
    showPrintMargin: false,
    readOnly: fileStore.req?.type === "textImmutable",
    theme: getEditorTheme(authStore.user?.aceEditorTheme ?? ""),
    mode: isGcodeFile.value
      ? "ace/mode/gcode"
      : modelist.getModeForPath(fileStore.req?.name ?? "").mode,
    wrap: true,
    enableBasicAutocompletion: true,
    enableLiveAutocompletion: true,
    enableSnippets: true,
  });

  editor.value.setFontSize(fontSize.value);
  editor.value.focus();

  // track cursor line for 3D sphere highlight
  editor.value.getSession().selection.on("changeCursor", () => {
    const pos = editor.value!.getCursorPosition();
    cursorLine.value = pos.row;
  });

  // fire debounced gcode update on every edit — viewer only re-parses after
  // the user pauses typing for 600ms
  editor.value.session.on("change", () => {
    if (isGcodeFile.value) {
      updateDebouncedGcode(editor.value!.getValue());
    }
  });
});

onBeforeUnmount(() => {
  window.removeEventListener("keydown", keyEvent);
  window.removeEventListener("beforeunload", handlePageChange);
  editor.value?.destroy();
});

onBeforeRouteUpdate((to, from, next) => {
  if (editor.value?.session.getUndoManager().isClean()) {
    next();
    return;
  }

  layoutStore.showHover({
    prompt: "discardEditorChanges",
    confirm: (event: Event) => {
      event.preventDefault();
      next();
    },
    saveAction: async () => {
      await save();
      next();
    },
  });
});

// ── keyboard & page guards ───────────────────────────────────────────────────
const keyEvent = (event: KeyboardEvent) => {
  if (event.code === "Escape") {
    close();
  }
  if (!event.ctrlKey && !event.metaKey) return;
  if (event.key !== "s") return;
  event.preventDefault();
  save();
};

const handlePageChange = (event: BeforeUnloadEvent) => {
  if (!editor.value?.session.getUndoManager().isClean()) {
    event.preventDefault();
    event.returnValue = true;
  }
};

// ── actions ──────────────────────────────────────────────────────────────────
const save = async () => {
  const button = "save";
  buttons.loading("save");
  try {
    await api.put(route.path, editor.value?.getValue());
    editor.value?.session.getUndoManager().markClean();
    buttons.success(button);
  } catch (e: any) {
    buttons.done(button);
    $showError(e);
  }
};

const increaseFontSize = () => {
  fontSize.value += 1;
  editor.value?.setFontSize(fontSize.value);
  localStorage.setItem("editorFontSize", fontSize.value.toString());
};

const decreaseFontSize = () => {
  if (fontSize.value > 1) {
    fontSize.value -= 1;
    editor.value?.setFontSize(fontSize.value);
    localStorage.setItem("editorFontSize", fontSize.value.toString());
  }
};

const close = () => {
  if (!editor.value?.session.getUndoManager().isClean()) {
    layoutStore.showHover({
      prompt: "discardEditorChanges",
      confirm: (event: Event) => {
        event.preventDefault();
        finishClose();
      },
      saveAction: async () => {
        await save();
        finishClose();
      },
    });
    return;
  }
  finishClose();
};

const finishClose = () => {
  fileStore.updateRequest(null);
  const uri = url.removeLastDir(route.path) + "/";
  router.push({ path: uri });
};

const preview = () => {
  isPreview.value = !isPreview.value;
};

// when user clicks in 3D viewer, jump Ace editor to that line
const handleViewerLineSelect = (lineIndex: number) => {
  if (!editor.value) return;
  const session = editor.value.getSession();
  const maxRow = session.getLength() - 1;
  const row = Math.max(0, Math.min(lineIndex, maxRow));
  editor.value.gotoLine(row + 1, 0, true);
  editor.value.centerSelection();
};
</script>

<style scoped>
.editor-font-size {
  margin: 0 0.5em;
  color: var(--fg);
}
</style>

<style>
/*
 * G-code syntax highlight colors for ace-gcode.js
 *
 * Replace the <style> (non-scoped) block in Editor.vue with this.
 *
 * Design rules:
 *   - N-codes (block numbers) use `color: inherit` — they adapt to whatever
 *     Ace theme is active, so they're readable in both light and dark mode.
 *   - All other tokens use !important to beat Ace theme specificity.
 *   - Bare numbers use opacity so they recede without a hardcoded color.
 */

/* ── Comments ──────────────────────────────────────────────────────────────── */
.ace_gcode.ace_comment {
  color: #4b754b !important;
  font-style: italic;
}

/* ── Block numbers (N-codes) ───────────────────────────────────────────────── */
/* inherit = uses the active Ace theme's foreground color.                      */
/* Works correctly in light AND dark themes — no more white-on-white.           */
.ace_gcode.ace_block {
  color: inherit;
  opacity: 0.55;   /* dimmed so N-codes recede behind G/M words visually */
}

/* ── Program markers (% and Oxxxx) ────────────────────────────────────────── */
.ace_gcode.ace_marker {
  color: #e06c75 !important;  /* red — stands out as structural delimiters */
  font-weight: bold;
}

/* ── G-words ───────────────────────────────────────────────────────────────── */
.ace_gcode.ace_gword {
  color: #c586c0 !important;  /* purple */
  font-weight: bold;
}

/* ── M-codes ───────────────────────────────────────────────────────────────── */
.ace_gcode.ace_mcode {
  color: #dcdcaa !important;  /* yellow */
  font-weight: bold;
}

/* ── X / I / A  (orange) ───────────────────────────────────────────────────── */
.ace_gcode.ace_xparam {
  color: #ce9178 !important;
}

/* ── Y / J  (teal) ─────────────────────────────────────────────────────────── */
.ace_gcode.ace_yparam {
  color: #4ec9b0 !important;
}

/* ── Z / K / B  (blue) ─────────────────────────────────────────────────────── */
.ace_gcode.ace_zparam {
  color: #569cd6 !important;
}

/* ── F / S / H / D / T  (lighter teal) ────────────────────────────────────── */
.ace_gcode.ace_feedspeed {
  color: #9cdcfe !important;
}

/* ── P subprogram numbers (light blue) ─────────────────────────────────────── */
.ace_gcode.ace_subprog {
  color: #9cdcfe !important;
}

/* ── Bare numeric fallback ──────────────────────────────────────────────────── */
/* opacity-only — inherits theme color so it works in light and dark mode       */
.ace_constant.ace_numeric {
  opacity: 0.5;
}

/* ── Layout ─────────────────────────────────────────────────────────────────── */

#editor-container {
  display: flex;
  flex-direction: column;
  height: calc(100vh - 52px); /* 52px = header-bar height */
}

.editor-layout {
  display: flex;
  flex: 1;
  min-height: 0; /* lets flex children shrink below content size */
}

.editor-pane {
  flex: 1 1 50%;
  min-width: 0;
}

.viewer-pane {
  flex: 1 1 50%;
  min-width: 0;
  border-left: 1px solid var(--border-color, #333);
}
</style>