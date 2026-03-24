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
            :gcode="currentGcode"
            :cursor-line="cursorLine"
            @select-line="handleViewerLineSelect"
          />
        </div>
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import "@/ace-gcode.js"; // your custom G-code mode

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

// markdown check
const isMarkdownFile = computed(() => {
  const name = fileStore.req?.name || "";
  return name.endsWith(".md") || name.endsWith(".markdown");
});

// which files get G-code treatment
const isGcodeFile = computed(() => {
  const name = (fileStore.req?.name || "").toLowerCase();
  return (
    name.endsWith(".nc") ||
    name.endsWith(".tap") ||
    name.endsWith(".gcode") ||
    name.endsWith(".cnc")
  );
});

// live G-code text for viewer (editor content if edited, else original file)
const currentGcode = computed(() => {
  return editor.value?.getValue() ?? (fileStore.req?.content || "");
});

onMounted(() => {
  window.addEventListener("keydown", keyEvent);
  window.addEventListener("beforeunload", handlePageChange);

  const fileContent = fileStore.req?.content || "";

  // markdown preview updates
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

  ace.config.set(
    "basePath",
    `https://cdn.jsdelivr.net/npm/ace-builds@${ace_version}/src-min-noconflict/`
  );

  editor.value = ace.edit("editor", {
    value: fileContent,
    showPrintMargin: false,
    readOnly: fileStore.req?.type === "textImmutable",
    theme: getEditorTheme(authStore.user?.aceEditorTheme ?? ""),
    mode: (() => {
      const name = (fileStore.req?.name || "").toLowerCase();

      if (
        name.endsWith(".nc") ||
        name.endsWith(".tap") ||
        name.endsWith(".gcode") ||
        name.endsWith(".cnc")
      ) {
        return "ace/mode/gcode";
      }

      return modelist.getModeForPath(fileStore.req!.name).mode;
    })(),
    wrap: true,
    enableBasicAutocompletion: true,
    enableLiveAutocompletion: true,
    enableSnippets: true,
  });

  editor.value.setFontSize(fontSize.value);
  editor.value.focus();

  // track cursor line for 3D highlight
  const session = editor.value.getSession();
  session.selection.on("changeCursor", () => {
    const pos = editor.value!.getCursorPosition();
    cursorLine.value = pos.row;
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

const keyEvent = (event: KeyboardEvent) => {
  if (event.code === "Escape") {
    close();
  }

  if (!event.ctrlKey && !event.metaKey) {
    return;
  }

  if (event.key !== "s") {
    return;
  }

  event.preventDefault();
  save();
};

const handlePageChange = (event: BeforeUnloadEvent) => {
  if (!editor.value?.session.getUndoManager().isClean()) {
    event.preventDefault();
    // returnValue is deprecated but kept for legacy browsers
    event.returnValue = true;
  }
};

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

// when user clicks in 3D, jump Ace to that line
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
/* G-code syntax colors */

/* Comments */
.ace_comment,
.ace_gcode.ace_comment {
  color: #4b754b !important;
}

/* N codes */
.ace_gcode.ace_block,
.ace_block {
  color: #ffffff !important;
}

/* Gxx (purple) */
.ace_gcode.ace_gword {
  color: #c586c0 !important;
}

/* X / I (orange) */
.ace_gcode.ace_xparam {
  color: #ce9178 !important;
}

/* Y / J / A (green-ish) */
.ace_gcode.ace_yparam {
  color: #4ec9b0 !important;
}

/* Z / K (blue) */
.ace_gcode.ace_zparam {
  color: #569cd6 !important;
}

/* F / S / H / D / T / HCC (teal) */
.ace_gcode.ace_feedspeed {
  color: #4ec9b0 !important;
}

/* M codes (yellow) */
.ace_gcode.ace_mcode {
  color: #dcdcaa !important;
}

/* P subprograms (light blue) */
.ace_gcode.ace_subprog {
  color: #9cdcfe !important;
}

/* Layout for editor + viewer */
.editor-layout {
  display: flex;
  height: 100%;
}

.editor-pane {
  flex: 1 1 50%;
}

.viewer-pane {
  flex: 1 1 50%;
  min-width: 0;
  border-left: 1px solid var(--border-color, #333);
}
</style>