// Setup-sheet pages for the loaded program (payload "setup", built by cncd's
// cncd/setup_sheet.go). Included by main.cpp after fitToWidth(); it relies on
// main.cpp's globals (display, cfg, payload_doc, current_page, logf).
//
// Pages, in order:
//   'I'  FIX BEFORE RUN - only the pockets that are not ok (absent when all ok)
//   'L'  the plain list: one line per pocket, run order, each once
//   'T'  one tool per page: side-view silhouette, pocket, type, holder, and
//        the diameter to enter (milling tools only)
// The page only changes on a button press or when the loaded program changes.
// A page is remembered by (kind, id) so it survives a re-fetch that adds or
// removes issue pages.

#pragma once

#include <Arduino.h>
#include <ArduinoJson.h>
#include <vector>
#include <Fonts/FreeMonoBold12pt7b.h>
#include <Fonts/FreeMono12pt7b.h>
#include <Fonts/FreeMonoBold18pt7b.h>
#include <Fonts/FreeMonoBold24pt7b.h>

struct SheetPage {
  char kind;   // 'I', 'L' or 'T'
  int id;      // chunk number for I/L, pocket number for T
  int first;   // I/L: first row of the chunk; T: tool index
  int count;   // rows on the page
};

static std::vector<SheetPage> sheet_pages;
static char cur_kind = 'L';
static int cur_id = 0;
static String g_setup_key = "";   // state + program; a change resets to page 1

constexpr int SHEET_ROW_H = 34;
constexpr int SHEET_ROWS_TOP = 78;

inline bool statusIsOk(JsonObjectConst t) {
  const char *s = t["status"] | "ok";
  return strcmp(s, "ok") == 0;
}

bool sheetActive() {
  const char *st = payload_doc["setup"]["state"] | "none";
  return strcmp(st, "sheet") == 0;
}

bool setupNoJson() {
  const char *st = payload_doc["setup"]["state"] | "none";
  return strcmp(st, "nojson") == 0;
}

int sheetRows() {
  int n = (cfg.res_y - SHEET_ROWS_TOP - 6) / SHEET_ROW_H;
  return n < 1 ? 1 : n;
}

int sheetTotalPages() {
  return sheet_pages.empty() ? 1 : (int) sheet_pages.size();
}

void buildSheetPages() {
  sheet_pages.clear();
  JsonArrayConst tools = payload_doc["setup"]["tools"];
  int n = tools.isNull() ? 0 : (int) tools.size();
  int per = sheetRows();

  int bad = 0;
  for (int i = 0; i < n; i++)
    if (!statusIsOk(tools[i].as<JsonObjectConst>())) bad++;
  for (int start = 0, pg = 0; start < bad; start += per, pg++)
    sheet_pages.push_back({'I', pg, start, min(per, bad - start)});

  int pg = 0;
  int start = 0;
  do {
    sheet_pages.push_back({'L', pg++, start, min(per, n - start)});
    start += per;
  } while (start < n);

  for (int i = 0; i < n; i++) {
    int pocket = tools[i]["pocket"] | 0;
    sheet_pages.push_back({'T', pocket, i, 1});
  }
}

void sheetRememberPage() {
  if (sheet_pages.empty()) return;
  if (current_page < 0 || current_page >= (int) sheet_pages.size()) current_page = 0;
  cur_kind = sheet_pages[current_page].kind;
  cur_id = sheet_pages[current_page].id;
}

// Re-find the remembered page in the freshly built list; page 1 on a reset
// (new program) or when that page no longer exists.
void sheetSyncPage(bool reset) {
  int found = -1;
  if (!reset) {
    for (int i = 0; i < (int) sheet_pages.size(); i++) {
      if (sheet_pages[i].kind == cur_kind && sheet_pages[i].id == cur_id) {
        found = i;
        break;
      }
    }
  }
  current_page = found >= 0 ? found : 0;
  sheetRememberPage();
}

// Status as text: plain when ok, white-on-black box otherwise. (x,y) is the
// top-left of a w x h cell; the current font is used.
void drawStatus(const char *status, int x, int y, int w, int h) {
  bool ok = strcmp(status, "ok") == 0;
  display.setFont(&FreeMonoBold12pt7b);
  int16_t bx, by;
  uint16_t bw, bh;
  display.getTextBounds(status, 0, 0, &bx, &by, &bw, &bh);
  int tx = x + (w - (int) bw) / 2;
  int ty = y + (h + (int) bh) / 2 - 2;
  if (!ok) {
    display.fillRect(x, y, w, h, GxEPD_BLACK);
    display.setTextColor(GxEPD_WHITE);
  }
  display.setCursor(tx, ty);
  display.print(status);
  display.setTextColor(GxEPD_BLACK);
}

// Word-wrapped text, up to max_lines lines. Returns the lines drawn.
int drawWrapped(String text, int x, int y, int max_w, int line_h, int max_lines) {
  text.trim();
  int lines = 0;
  while (text.length() > 0 && lines < max_lines) {
    int cut = text.length();
    int16_t bx, by;
    uint16_t bw, bh;
    while (cut > 1) {
      String probe = text.substring(0, cut);
      display.getTextBounds(probe, 0, 0, &bx, &by, &bw, &bh);
      if ((int) bw <= max_w) break;
      int sp = text.lastIndexOf(' ', cut - 1);
      cut = (sp > 0) ? sp : cut - 1;
    }
    String line = text.substring(0, cut);
    line.trim();
    display.setCursor(x, y + lines * line_h);
    display.print(line);
    text = text.substring(cut);
    text.trim();
    lines++;
  }
  return lines;
}

// 'I' and 'L' pages: "T12  1/2 drill        TOUCH". For 'I' only the rows
// that are not ok, counted from p.first.
void renderSheetList(const SheetPage &p) {
  const int margin = 10;
  JsonArrayConst tools = payload_doc["setup"]["tools"];
  const char *program = payload_doc["setup"]["program"] | "";
  const char *title = payload_doc["setup"]["title"] | "";

  display.setFont(&FreeMonoBold9pt7b);
  String head = (p.kind == 'I') ? "FIX BEFORE RUN   " : "";
  head += "O" + String(program);
  if (title[0]) head += "  " + String(title);
  display.setCursor(margin, 50 + 4 + 16);
  display.print(fitToWidth(head, cfg.res_x - 2 * margin));

  if (tools.isNull() || tools.size() == 0) {
    display.setFont(&FreeMono12pt7b);
    display.setCursor(margin, SHEET_ROWS_TOP + 24);
    display.print("No tools.");
    return;
  }

  int seen = 0;      // rows passed that count for this page kind
  int drawn = 0;
  const int status_w = 120;
  for (JsonVariantConst v : tools) {
    JsonObjectConst t = v.as<JsonObjectConst>();
    bool ok = statusIsOk(t);
    if (p.kind == 'I' && ok) continue;
    if (seen++ < p.first) continue;
    if (drawn >= p.count) break;

    int y = SHEET_ROWS_TOP + drawn * SHEET_ROW_H;
    char pocket[8];
    snprintf(pocket, sizeof pocket, "T%d", (int) (t["pocket"] | 0));
    display.setFont(&FreeMonoBold12pt7b);
    display.setCursor(margin, y + 24);
    display.print(pocket);

    display.setFont(&FreeMono12pt7b);
    display.setCursor(margin + 70, y + 24);
    display.print(fitToWidth(String((const char *) (t["label"] | "")),
                             cfg.res_x - margin - 70 - status_w - 20));

    drawStatus(t["status"] | "ok", cfg.res_x - margin - status_w, y + 2, status_w, SHEET_ROW_H - 4);
    display.drawLine(margin, y + SHEET_ROW_H - 1, cfg.res_x - margin, y + SHEET_ROW_H - 1, GxEPD_BLACK);
    drawn++;
  }
}

// Polylines [x0,y0,x1,y1,...] in mils, x from the centerline, y up from the
// tip. Scaled to fit the box; drawn twice (1 px apart) so e-paper shows them.
void drawShape(JsonObjectConst shape, int ax, int ay, int aw, int ah) {
  float hw = shape["hw"] | 1;
  float h = shape["h"] | 1;
  if (hw < 1) hw = 1;
  if (h < 1) h = 1;
  float scale = min((float) ah / h, (float) aw / (2.0f * hw));
  int cx = ax + aw / 2;
  int ybot = ay + ah;
  for (JsonVariantConst lv : shape["l"].as<JsonArrayConst>()) {
    JsonArrayConst l = lv.as<JsonArrayConst>();
    for (size_t i = 0; i + 3 < l.size(); i += 2) {
      int x0 = cx + (int) lroundf((float) (int) l[i] * scale);
      int y0 = ybot - (int) lroundf((float) (int) l[i + 1] * scale);
      int x1 = cx + (int) lroundf((float) (int) l[i + 2] * scale);
      int y1 = ybot - (int) lroundf((float) (int) l[i + 3] * scale);
      display.drawLine(x0, y0, x1, y1, GxEPD_BLACK);
      display.drawLine(x0 + 1, y0, x1 + 1, y1, GxEPD_BLACK);
    }
  }
}

// 'T' page: one tool.
void renderSheetTool(const SheetPage &p) {
  JsonArrayConst tools = payload_doc["setup"]["tools"];
  if (tools.isNull() || p.first >= (int) tools.size()) return;
  JsonObjectConst t = tools[p.first].as<JsonObjectConst>();

  bool landscape = cfg.res_x >= cfg.res_y;
  const int top = 58;
  int ax, ay, aw, ah, tx, ty, tw;
  if (landscape) {
    ax = 16; ay = top; aw = 390; ah = cfg.res_y - top - 14;
    tx = 430; ty = top + 40; tw = cfg.res_x - tx - 12;
  } else {
    ax = 16; ay = top; aw = cfg.res_x - 32; ah = (cfg.res_y - top) / 2 - 20;
    tx = 16; ty = top + ah + 60; tw = cfg.res_x - 32;
  }
  JsonObjectConst shape = t["shape"];
  if (!shape.isNull()) drawShape(shape, ax, ay, aw, ah);

  display.setFont(&FreeMonoBold24pt7b);
  display.setCursor(tx, ty);
  display.print("T");
  display.print((int) (t["pocket"] | 0));

  display.setFont(&FreeMonoBold12pt7b);
  display.setCursor(tx, ty + 36);
  display.print(fitToWidth(String((const char *) (t["label"] | "")), tw));

  drawStatus(t["status"] | "ok", tx, ty + 52, 120, 30);

  int y = ty + 112;
  const char *dia = t["diameter"] | "";
  if (dia[0]) {
    display.setFont(&FreeMonoBold18pt7b);
    display.setCursor(tx, y);
    display.print("DIA ");
    display.print(dia);
    y += 44;
  }

  display.setFont(&FreeMono9pt7b);
  const char *type = t["type"] | "";
  const char *desc = t["description"] | "";
  const char *holder = t["holder"] | "";
  if (type[0]) {
    display.setCursor(tx, y);
    display.print(fitToWidth(String(type), tw));
    y += 20;
  }
  if (desc[0]) y += 20 * drawWrapped(String(desc), tx, y, tw, 20, 2);
  if (holder[0]) {
    y += 10;
    display.setCursor(tx, y);
    display.print("Holder:");
    y += 20;
    drawWrapped(String(holder), tx, y, tw, 20, 3);
  }
}

void renderSheetPage() {
  if (sheet_pages.empty() || current_page < 0 || current_page >= (int) sheet_pages.size()) return;
  const SheetPage &p = sheet_pages[current_page];
  if (p.kind == 'T') renderSheetTool(p);
  else renderSheetList(p);
}
