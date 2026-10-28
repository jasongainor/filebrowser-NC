// Setup-sheet pages for the loaded program (payload "setup", built by cncd's
// cncd/setup_sheet.go). Included by main.cpp after fitToWidth(); it relies on
// main.cpp's globals (display, cfg, payload_doc, current_page, logf).
//
// Pages, in order. When the payload has "groups" (a main program that calls
// subprograms) the order is A, G..., T and the flat I/L pages are not built;
// with no "groups" it is I, L, T exactly as before.
//   'A'  ALERTS - cross-program alerts (absent when there are none)
//   'G'  one program of the call tree: its tools, with state and call count,
//        the running program marked
//   'I'  FIX BEFORE RUN - only the pockets that are not ok (absent when all ok)
//   'L'  the plain list: one line per pocket, run order, each once
//        (I and L carry a QR to the program's setup sheet at the foot when
//        cncd sends one)
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
  char kind;   // 'A', 'G', 'I', 'L' or 'T'
  int id;      // chunk number for A/I/L, group*100+chunk for G, pocket for T
  int first;   // A/G/I/L: first row of the chunk; T: tool index
  int count;   // rows on the page
  int group;   // G: index into setup.groups; otherwise 0
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

inline JsonArrayConst setupGroups() {
  return payload_doc["setup"]["groups"].as<JsonArrayConst>();
}
inline JsonArrayConst setupAlerts() {
  return payload_doc["setup"]["alerts"].as<JsonArrayConst>();
}
inline bool hasGroups() {
  JsonArrayConst g = setupGroups();
  return !g.isNull() && g.size() > 0;
}
int setupAlertCount() {
  JsonArrayConst a = setupAlerts();
  return a.isNull() ? 0 : (int) a.size();
}

bool setupNoJson() {
  const char *st = payload_doc["setup"]["state"] | "none";
  return strcmp(st, "nojson") == 0;
}

// Extra 16 px lines under a list row: "want" (not ok, something expected
// is known) and "WEAR" (a wear offset is not zero).
constexpr int SHEET_SUB_H = 16;
inline bool rowHasWant(JsonObjectConst t) {
  const char *l = t["expected_length"] | "";
  const char *d = t["expected_diameter"] | "";
  return !statusIsOk(t) && (l[0] || d[0]);
}
inline bool rowHasWear(JsonObjectConst t) {
  const char *l = t["wear_length"] | "";
  const char *d = t["wear_diameter"] | "";
  return l[0] || d[0];
}
inline int rowHeight(JsonObjectConst t) {
  return SHEET_ROW_H + SHEET_SUB_H * ((rowHasWant(t) ? 1 : 0) + (rowHasWear(t) ? 1 : 0));
}

// The QR to the setup sheet: rows of hex from cncd, one bit per module,
// most significant bit first. Module size fits it in ~160 px portrait,
// ~110 px landscape; the strip is the QR plus a white margin around it.
constexpr int SHEET_QR_PAD = 14;
inline int qrModules() {
  JsonArrayConst rows = payload_doc["setup"]["qr"]["rows"];
  return rows.isNull() ? 0 : (int) rows.size();
}
inline int qrScale() {
  int n = qrModules();
  if (n == 0) return 0;
  int s = (cfg.res_y >= cfg.res_x ? 160 : 110) / n;
  return s < 2 ? 2 : s;
}
inline int qrStripH() {
  int n = qrModules();
  return n ? n * qrScale() + 2 * SHEET_QR_PAD : 0;
}

void drawSheetQR() {
  int n = qrModules();
  if (n == 0) return;
  int sc = qrScale();
  int x0 = 10 + SHEET_QR_PAD, y0 = cfg.res_y - qrStripH() + SHEET_QR_PAD;
  JsonArrayConst rows = payload_doc["setup"]["qr"]["rows"];
  for (int r = 0; r < n; r++) {
    const char *hex = rows[r] | "";
    for (int c = 0; c < n; c++) {
      char ch = hex[c / 4];
      if (!ch) break;
      int v = (ch >= 'a') ? ch - 'a' + 10 : (ch >= 'A') ? ch - 'A' + 10 : ch - '0';
      if (v & (8 >> (c % 4))) display.fillRect(x0 + c * sc, y0 + r * sc, sc, sc, GxEPD_BLACK);
    }
  }
  int tx = x0 + n * sc + SHEET_QR_PAD;
  display.setFont(&FreeMonoBold12pt7b);
  display.setCursor(tx, y0 + 24);
  display.print("Setup sheet");
  display.setFont(&FreeMono9pt7b);
  display.setCursor(tx, y0 + 50);
  display.print(fitToWidth(String((const char *) (payload_doc["setup"]["qr"]["url"] | "")),
                           cfg.res_x - tx - 10));
}

// Pack rows (only the not-ok ones when onlyBad) into pages by height.
void packSheetPages(char kind, bool onlyBad, JsonArrayConst tools, int group = 0, int idBase = 0) {
  int avail = cfg.res_y - SHEET_ROWS_TOP - 6 - qrStripH();
  int pg = 0, first = 0, count = 0, used = 0, seen = 0;
  int n = tools.isNull() ? 0 : (int) tools.size();
  for (int i = 0; i < n; i++) {
    JsonObjectConst t = tools[i].as<JsonObjectConst>();
    if (onlyBad && statusIsOk(t)) continue;
    int h = rowHeight(t);
    if (count > 0 && used + h > avail) {
      sheet_pages.push_back({kind, idBase + pg++, first, count, group});
      first = seen;
      count = 0;
      used = 0;
    }
    used += h;
    count++;
    seen++;
  }
  if (count > 0 || (!onlyBad && pg == 0)) sheet_pages.push_back({kind, idBase + pg, first, count, group});
}

// An alert takes three lines: kind/pocket/programs, then the detail wrapped
// to two. Fixed height so paging needs no text measuring.
constexpr int ALERT_LINE_H = 18;
constexpr int ALERT_ROW_H = 24 + 2 * ALERT_LINE_H + 10;

void packAlertPages() {
  int n = setupAlertCount();
  if (n == 0) return;
  int avail = cfg.res_y - SHEET_ROWS_TOP - 6 - qrStripH();
  int per = avail / ALERT_ROW_H;
  if (per < 1) per = 1;
  for (int first = 0, pg = 0; first < n; first += per, pg++) {
    sheet_pages.push_back({'A', pg, first, min(per, n - first), 0});
  }
}

int sheetTotalPages() {
  return sheet_pages.empty() ? 1 : (int) sheet_pages.size();
}

void buildSheetPages() {
  sheet_pages.clear();
  JsonArrayConst tools = payload_doc["setup"]["tools"];
  int n = tools.isNull() ? 0 : (int) tools.size();
  packAlertPages();
  if (hasGroups()) {
    JsonArrayConst groups = setupGroups();
    for (int g = 0; g < (int) groups.size(); g++) {
      packSheetPages('G', false, groups[g]["tools"].as<JsonArrayConst>(), g, g * 100);
    }
  } else {
    packSheetPages('I', true, tools);
    packSheetPages('L', false, tools);
  }

  for (int i = 0; i < n; i++) {
    int pocket = tools[i]["pocket"] | 0;
    sheet_pages.push_back({'T', pocket, i, 1, 0});
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

// Status box for a list row: white on black, small bold font.
void drawStatusSmall(const char *status, int x, int y, int w, int h) {
  display.setFont(&FreeMonoBold9pt7b);
  int16_t bx, by;
  uint16_t bw, bh;
  display.getTextBounds(status, 0, 0, &bx, &by, &bw, &bh);
  display.fillRect(x, y, w, h, GxEPD_BLACK);
  display.setTextColor(GxEPD_WHITE);
  display.setCursor(x + (w - (int) bw) / 2, y + (h + (int) bh) / 2 - 1);
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

// A drawn check mark in a w x h cell (the mono fonts have no glyph for it).
void drawCheck(int x, int y, int w, int h) {
  int s = min(w, h) - 8;
  int x0 = x + (w - s) / 2, y0 = y + (h - s) / 2;
  for (int d = 0; d < 3; d++) {
    display.drawLine(x0, y0 + s / 2 + d, x0 + s / 3, y0 + s - 2 + d, GxEPD_BLACK);
    display.drawLine(x0 + s / 3, y0 + s - 2 + d, x0 + s, y0 + d, GxEPD_BLACK);
  }
}

// 'I' and 'L' pages: "T12  1/2 drill   4.2527  0.0604  [check]", the status
// boxed instead of the check when not ok. For 'I' only the rows that are
// not ok, counted from p.first.
void renderSheetList(const SheetPage &p) {
  const int margin = 10;
  JsonArrayConst tools = payload_doc["setup"]["tools"];
  const char *program = payload_doc["setup"]["program"] | "";
  const char *title = payload_doc["setup"]["title"] | "";
  bool isGroup = (p.kind == 'G');
  bool running = false, next = false;
  String meta = "";
  if (isGroup) {
    JsonObjectConst g = setupGroups()[p.group].as<JsonObjectConst>();
    tools = g["tools"].as<JsonArrayConst>();
    program = g["program"] | "";
    title = g["title"] | "";
    running = g["running"] | false;
    next = g["next"] | false;
    meta = " " + String((const char *) (g["state"] | "")) + " x" + String((int) (g["calls"] | 0));
  }

  display.setFont(&FreeMonoBold9pt7b);
  String head = (p.kind == 'I') ? "FIX BEFORE RUN   " : "";
  head += "O" + String(program) + meta;
  if (title[0]) head += "  " + String(title);
  // Column heads over the offsets, right of the title.
  const int status_w = 76;
  const int dia_w = 80, len_w = 80;
  int dia_x = cfg.res_x - margin - status_w - dia_w;
  int len_x = dia_x - len_w;
  int head_x = margin;
  if (running || next) {
    // White on black tag ahead of the title: RUN (loaded and running) or NEXT.
    const char *tag = running ? "RUN" : "NEXT";
    int tw = (int) strlen(tag) * 11 + 8;
    display.fillRect(margin, 50 + 4 + 2, tw, 18, GxEPD_BLACK);
    display.setTextColor(GxEPD_WHITE);
    display.setCursor(margin + 4, 50 + 4 + 16);
    display.print(tag);
    display.setTextColor(GxEPD_BLACK);
    head_x = margin + tw + 6;
  }
  display.setCursor(head_x, 50 + 4 + 16);
  display.print(fitToWidth(head, len_x - head_x - 8));
  display.setCursor(len_x, 50 + 4 + 16);
  display.print("LEN");
  display.setCursor(dia_x, 50 + 4 + 16);
  display.print("DIA");
  drawSheetQR();

  if (tools.isNull() || tools.size() == 0) {
    display.setFont(&FreeMono12pt7b);
    display.setCursor(margin, SHEET_ROWS_TOP + 24);
    display.print("No tools.");
    return;
  }

  int seen = 0;      // rows passed that count for this page kind
  int drawn = 0;
  int y = SHEET_ROWS_TOP;
  for (JsonVariantConst v : tools) {
    JsonObjectConst t = v.as<JsonObjectConst>();
    bool ok = statusIsOk(t);
    if (p.kind == 'I' && ok) continue;
    if (seen++ < p.first) continue;
    if (drawn >= p.count) break;

    int row_h = rowHeight(t);
    char pocket[8];
    snprintf(pocket, sizeof pocket, "T%d", (int) (t["pocket"] | 0));
    display.setFont(&FreeMonoBold12pt7b);
    display.setCursor(margin, y + 24);
    display.print(pocket);

    display.setFont(&FreeMono12pt7b);
    display.setCursor(margin + 56, y + 24);
    display.print(fitToWidth(String((const char *) (t["label"] | "")),
                             len_x - margin - 56 - 8));

    display.setFont(&FreeMono9pt7b);
    display.setCursor(len_x, y + 22);
    display.print((const char *) (t["offset_length"] | "-"));
    display.setCursor(dia_x, y + 22);
    display.print((const char *) (t["offset_diameter"] | "-"));

    bool wear = rowHasWear(t);
    int sub_y = y + 24;
    if (rowHasWant(t)) {
      sub_y += SHEET_SUB_H;
      display.setFont(&FreeMono9pt7b);
      display.setCursor(margin + 56, sub_y);
      display.print("want");
      display.setCursor(len_x, sub_y);
      display.print((const char *) (t["expected_length"] | ""));
      display.setCursor(dia_x, sub_y);
      display.print((const char *) (t["expected_diameter"] | ""));
    }
    if (wear) {
      sub_y += SHEET_SUB_H;
      display.setFont(&FreeMonoBold9pt7b);
      display.setCursor(margin + 56, sub_y);
      display.print("WEAR");
      display.setFont(&FreeMono9pt7b);
      const char *wl = t["wear_length"] | "";
      const char *wd = t["wear_diameter"] | "";
      display.setCursor(len_x, sub_y);
      display.print(wl[0] ? wl : "0");
      display.setCursor(dia_x, sub_y);
      display.print(wd[0] ? wd : "0");
    }

    int sx = cfg.res_x - margin - status_w;
    if (!ok) drawStatusSmall(t["status"] | "", sx, y + 3, status_w, SHEET_ROW_H - 6);
    else if (wear) drawStatusSmall("WEAR", sx, y + 3, status_w, SHEET_ROW_H - 6);
    else drawCheck(sx + status_w - SHEET_ROW_H, y, SHEET_ROW_H, SHEET_ROW_H);
    display.drawLine(margin, y + row_h - 1, cfg.res_x - margin, y + row_h - 1, GxEPD_BLACK);
    y += row_h;
    drawn++;
  }
}

// 'A' pages: cross-program alerts, three lines each.
void renderSheetAlerts(const SheetPage &p) {
  const int margin = 10;
  JsonArrayConst alerts = setupAlerts();
  display.setFont(&FreeMonoBold9pt7b);
  display.setCursor(margin, 50 + 4 + 16);
  display.print("ALERTS  " + String(setupAlertCount()));
  drawSheetQR();
  int y = SHEET_ROWS_TOP;
  for (int i = p.first; i < p.first + p.count && i < (int) alerts.size(); i++) {
    JsonObjectConst a = alerts[i].as<JsonObjectConst>();
    String line = String((const char *) (a["kind"] | ""));
    int pocket = a["pocket"] | 0;
    if (pocket > 0) line += "  T" + String(pocket);
    JsonArrayConst progs = a["programs"].as<JsonArrayConst>();
    if (!progs.isNull()) {
      String ps = "";
      for (JsonVariantConst pv : progs) {
        if (ps.length()) ps += ",";
        ps += "O" + String((const char *) (pv | ""));
      }
      if (ps.length()) line += "  " + ps;
    }
    display.setFont(&FreeMonoBold12pt7b);
    display.setCursor(margin, y + 20);
    display.print(fitToWidth(line, cfg.res_x - 2 * margin));
    display.setFont(&FreeMono9pt7b);
    drawWrapped(String((const char *) (a["detail"] | "")), margin, y + 20 + ALERT_LINE_H,
                cfg.res_x - 2 * margin, ALERT_LINE_H, 2);
    display.drawLine(margin, y + ALERT_ROW_H - 1, cfg.res_x - margin, y + ALERT_ROW_H - 1, GxEPD_BLACK);
    y += ALERT_ROW_H;
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
  else if (p.kind == 'A') renderSheetAlerts(p);
  else renderSheetList(p);
}
