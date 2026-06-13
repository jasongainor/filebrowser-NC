// eterminal-display firmware
//
// Seeed reTerminal E1001 (XIAO ESP32-S3 + 7.5" 800×480 mono e-paper).
// Renders the reconciled tool list from filebrowser-NC's
// /api/displays/{id} endpoint. Caches the last good payload on SD so
// the screen stays useful when the server is unreachable.
//
// Design constraints (per CLAUDE.md prompt):
//  - Fully local. No cloud, no SenseCraft, no TRMNL.
//  - Display-agnostic JSON contract — same shape browser/kiosk read.
//  - Graceful degradation. Render the cache + an OFFLINE indicator
//    when the server is down rather than going blank.
//  - No NTP / RTC. Display payload's last_updated string verbatim;
//    track staleness with millis() elapsed since last good fetch.
//  - Power-aware polling — powered interval by default; battery
//    interval only when VBUS detection is trivial.
//  - One physical button for page advance. Paging reads from the
//    in-memory payload — no refetch on page change.
//  - OTA over WiFi AND SD-card .bin self-update. Cable only once.

#include <Arduino.h>
#include <ArduinoJson.h>
#include <ArduinoOTA.h>
#include <HTTPClient.h>
#include <SD.h>
#include <SPI.h>
#include <Update.h>
#include <WiFi.h>
#include <GxEPD2_BW.h>
#include <Fonts/FreeMonoBold9pt7b.h>
#include <Fonts/FreeMono9pt7b.h>

#ifndef ETERMINAL_FIRMWARE_VERSION
#define ETERMINAL_FIRMWARE_VERSION "0.1.0-dev"
#endif

// ─── Pin map ──────────────────────────────────────────────────────────
// Pulled from the Seeed reTerminal E1001 example for XIAO_ESP32S3 with
// EPD_SELECT = 0 (the mono panel option). DO NOT guess — these are the
// values Seeed publishes and they line up with the FFC ribbon traces.
// If you swap to the tri-color panel option set EPD_SELECT = 1 in
// the Seeed cookbook, the pin numbers change.
// CORRECTED 2026-06-12 against the official Seeed reTerminal E1001 V1.2
// schematic, the Seeed_GFX reTerminal_E1001_SDcard_BW example, the Seeed
// Arduino peripherals cookbook, AND the Zephyr board DTS — all four agree.
// The previous D1/D3/D0/D2/D5 values were guessed and every one was wrong
// (e.g. old SD CS = D5 = GPIO6 is actually the user LED). These are raw
// ESP32-S3 GPIO numbers, NOT XIAO Dx macros. SPI bus (SCK=GPIO7/MISO=GPIO8/
// MOSI=GPIO9) is shared with the SD card and is already correct via SPI.begin().
constexpr int8_t PIN_EPD_CS    = 10;  // SCREEN_CS#
constexpr int8_t PIN_EPD_DC    = 11;  // SCREEN_DC#
constexpr int8_t PIN_EPD_RST   = 12;  // SCREEN_RST#
constexpr int8_t PIN_EPD_BUSY  = 13;  // SCREEN_BUSY# (active LOW)
constexpr int8_t PIN_SD_CS     = 14;  // SD_CS
constexpr int8_t PIN_SD_EN     = 16;  // SD_EN — gates a TPS22916 load switch that
                                      // powers the microSD slot AND its on-board
                                      // pull-ups. MUST be driven HIGH before
                                      // SD.begin() or the card is dead (f_mount 3).
constexpr int8_t PIN_SD_DET    = 15;  // SD_DET — card-detect, active LOW (unused yet)
// Three front user buttons, verified against Seeed's Arduino cookbook +
// the Zephyr board DTS. All active-LOW with board pull-ups (pressed reads
// LOW). The old PIN_BUTTON=D6 was wrong — D6/GPIO43 is UART0 TX, not a
// button, which is why paging never worked and the pinMode was disabled.
// NOTE: KEY0/GPIO3 is an ESP32-S3 strapping pin (JTAG-sel); harmless on a
// stock unit, just don't hold KEY0 while resetting/flashing.
constexpr int8_t PIN_KEY_REFRESH = 3;  // KEY0 — re-fetch from server
constexpr int8_t PIN_KEY_PREV    = 5;  // left-physical button  — previous page
constexpr int8_t PIN_KEY_NEXT    = 4;  // right-physical button — next page

// VBUS detection on the XIAO ESP32-S3 is not trivially exposed — there
// is no broken-out ADC pin to VBUS, only a USB-Serial detect bit that
// fights the CDC stack. Per the prompt: when VBUS is uncertain, use
// the single powered interval and skip the battery logic.
constexpr bool USE_BATTERY_INTERVAL = false;

// ─── Files on SD ─────────────────────────────────────────────────────
constexpr const char *CFG_PATH    = "/config.json";
constexpr const char *CACHE_PATH  = "/cache.json";
constexpr const char *NEW_FW_PATH = "/firmware.bin";

// ─── Render layout (overridden by config.json + server) ──────────────
struct DisplayConfig {
  String wifi_ssid;
  String wifi_pass;
  String base_url;       // e.g. http://192.168.20.10
  String display_id;
  String token;          // optional
  // Layout (populated from /api/displays/{id} response.config) —
  // fall back to baked-in defaults when the server is unreachable.
  int res_x = 800;
  int res_y = 480;
  int pocket_cols = 2;
  int pocket_rows = 10;
  int library_page_size = 20;
  int poll_powered_s = 60;
  int poll_battery_s = 900;
  String units = "in";
  // pocketView: "table" (paginated magazine table — dia/len/wear always in
  // aligned columns, default) or "cells" (the 2×N magazine grid followed by
  // the library tables). rotate: 0 = landscape, 90 = portrait. Both come
  // from the server Display config; rotate can also be toggled on-device by
  // long-pressing Refresh.
  String pocket_view = "table";
  int rotate = 0;
  String fields[8];      // ordered library columns; size matches the
                         // server-side DefaultDisplayFields length
  int fields_count = 6;
};

// ─── Display surface ─────────────────────────────────────────────────
// 7.5" mono panel. GxEPD2_750_T7 is the GxEPD2 class for the 800×480
// mono Waveshare/Goodisplay panel — the same controller Seeed ships
// on the E1001 mono variant.
GxEPD2_BW<GxEPD2_750_T7, GxEPD2_750_T7::HEIGHT> display(
    GxEPD2_750_T7(PIN_EPD_CS, PIN_EPD_DC, PIN_EPD_RST, PIN_EPD_BUSY));

DisplayConfig cfg;

// Page state. PAGE_POCKETS = the 2×10 carousel map; pages ≥1 = library
// rows, paged by cfg.library_page_size.
int current_page = 0;
// Runtime rotation override: -1 = use cfg.rotate from server config; else 0
// or 90. Long-pressing Refresh flips it so portrait can be evaluated on-device.
int g_rotation = -1;
unsigned long last_good_fetch_ms = 0;
bool serving_cache = false;
String last_updated_display = "";

// Forward decls — renderStatusBar() calls computeTotalPages() which is
// defined further down; one-pass C++ parser needs the prototype.
int computeTotalPages();
bool machine_connected = false;
String machine_name = "";
int periodic_full_refresh_at_page = 0;   // for ghosting purge

// In-memory payload from the last good fetch (or SD cache fallback).
// We keep the full JSON tree so paging is zero-network.
JsonDocument payload_doc;

// ─── Logging helper ──────────────────────────────────────────────────
void logf(const char *fmt, ...) {
  char buf[256];
  va_list args;
  va_start(args, fmt);
  vsnprintf(buf, sizeof(buf), fmt, args);
  va_end(args);
  Serial.println(buf);
}

// ─── SD: load config.json ────────────────────────────────────────────
bool loadConfigFromSD() {
  // Cap the shared-bus clock to 4 MHz. SD and the e-paper share SCK/MOSI/MISO
  // (GPIO7/9/8); the default ~20 MHz ramp is unreliable on this long-trace bus.
  if (!SD.begin(PIN_SD_CS, SPI, 4000000)) {
    logf("SD: mount failed — using defaults (firmware will idle until SD is fixed)");
    return false;
  }
  File f = SD.open(CFG_PATH);
  if (!f) {
    logf("SD: %s missing — see README for config.json format", CFG_PATH);
    return false;
  }
  JsonDocument doc;
  DeserializationError err = deserializeJson(doc, f);
  f.close();
  if (err) {
    logf("SD: %s parse error: %s", CFG_PATH, err.c_str());
    return false;
  }
  cfg.wifi_ssid  = doc["wifi_ssid"]  | "";
  cfg.wifi_pass  = doc["wifi_pass"]  | "";
  cfg.base_url   = doc["base_url"]   | "";
  cfg.display_id = doc["display_id"] | "";
  cfg.token      = doc["token"]      | "";
  if (cfg.wifi_ssid.length() == 0 || cfg.base_url.length() == 0 ||
      cfg.display_id.length() == 0) {
    logf("SD: %s missing wifi_ssid / base_url / display_id", CFG_PATH);
    return false;
  }
  logf("SD: loaded config for display %s @ %s", cfg.display_id.c_str(),
       cfg.base_url.c_str());
  return true;
}

// ─── SD: cache read/write ────────────────────────────────────────────
bool loadCacheFromSD() {
  File f = SD.open(CACHE_PATH);
  if (!f) return false;
  DeserializationError err = deserializeJson(payload_doc, f);
  f.close();
  if (err) {
    logf("SD: cache parse error: %s", err.c_str());
    return false;
  }
  return true;
}

void writeCacheToSD(const String &raw) {
  File f = SD.open(CACHE_PATH, FILE_WRITE);
  if (!f) {
    logf("SD: cache write open failed");
    return;
  }
  f.print(raw);
  f.close();
}

// ─── SD self-update: check /firmware.bin on boot ─────────────────────
//
// Pattern: if /firmware.bin exists, stream it through Update.write(),
// then rename the file (so we don't loop on it next boot) and reboot.
void maybeFlashFromSD() {
  if (!SD.exists(NEW_FW_PATH)) return;
  File f = SD.open(NEW_FW_PATH);
  if (!f) {
    logf("SD: firmware.bin open failed");
    return;
  }
  size_t sz = f.size();
  if (sz < 1024) {
    logf("SD: firmware.bin too small (%u bytes) — aborting flash", (unsigned) sz);
    f.close();
    return;
  }
  if (!Update.begin(sz)) {
    logf("SD: Update.begin failed: %s", Update.errorString());
    f.close();
    return;
  }
  size_t written = Update.writeStream(f);
  f.close();
  if (written != sz) {
    logf("SD: Update.writeStream wrote %u of %u — aborting",
         (unsigned) written, (unsigned) sz);
    Update.abort();
    return;
  }
  if (!Update.end(true)) {
    logf("SD: Update.end failed: %s", Update.errorString());
    return;
  }
  // Rename so we don't try to flash the same .bin every boot. .applied
  // suffix lets a curious operator see "yes, this one was flashed."
  SD.rename(NEW_FW_PATH, "/firmware.bin.applied");
  logf("SD: flashed %u bytes — rebooting", (unsigned) sz);
  delay(500);
  ESP.restart();
}

// ─── WiFi ────────────────────────────────────────────────────────────
bool connectWiFi() {
  WiFi.mode(WIFI_STA);
  WiFi.disconnect(true);
  delay(100);

  // Diagnostic scan: log the SSID we want (quoted, so a trailing space shows)
  // and every 2.4 GHz AP actually in range, so a renamed/typo'd SSID in
  // /config.json is obvious from the serial log instead of a guess.
  logf("WiFi: looking for SSID \"%s\"", cfg.wifi_ssid.c_str());
  int n = WiFi.scanNetworks();
  logf("WiFi: scan found %d networks:", n);
  bool seen = false;
  for (int i = 0; i < n; i++) {
    bool match = (WiFi.SSID(i) == cfg.wifi_ssid);
    if (match) seen = true;
    logf("  [%d] \"%s\" rssi=%d ch=%d enc=%d%s", i, WiFi.SSID(i).c_str(),
         WiFi.RSSI(i), WiFi.channel(i), (int) WiFi.encryptionType(i),
         match ? "  <-- MATCH" : "");
  }
  WiFi.scanDelete();
  if (!seen) {
    logf("WiFi: \"%s\" not in scan — fix wifi_ssid in /config.json (exact name, "
         "2.4 GHz, in range)", cfg.wifi_ssid.c_str());
  }

  WiFi.begin(cfg.wifi_ssid.c_str(), cfg.wifi_pass.c_str());
  unsigned long start = millis();
  while (WiFi.status() != WL_CONNECTED && millis() - start < 15000) {
    delay(250);
  }
  if (WiFi.status() != WL_CONNECTED) {
    logf("WiFi: connect timeout for SSID \"%s\" (status=%d)",
         cfg.wifi_ssid.c_str(), WiFi.status());
    return false;
  }
  logf("WiFi: connected, IP %s", WiFi.localIP().toString().c_str());
  return true;
}

// ─── ArduinoOTA (WiFi push) ──────────────────────────────────────────
// Hostname matches the display ID so the operator can pick it from
// the Arduino IDE port menu unambiguously.
void setupOTA() {
  String host = "eterminal-" + cfg.display_id;
  ArduinoOTA.setHostname(host.c_str());
  // Token doubles as the OTA password when set; empty leaves OTA open
  // on the LAN, matching the LAN-permissive read path.
  if (cfg.token.length() > 0) {
    ArduinoOTA.setPassword(cfg.token.c_str());
  }
  ArduinoOTA.onStart([]() {
    logf("OTA: starting (%s)",
         ArduinoOTA.getCommand() == U_FLASH ? "firmware" : "fs");
  });
  ArduinoOTA.onError([](ota_error_t err) {
    logf("OTA: error %u", err);
  });
  ArduinoOTA.begin();
  logf("OTA: ready at %s.local", host.c_str());
}

// ─── HTTP fetch ──────────────────────────────────────────────────────
bool fetchPayload() {
  String url = cfg.base_url + "/api/displays/" + cfg.display_id;
  HTTPClient http;
  if (!http.begin(url)) {
    logf("HTTP: begin failed for %s", url.c_str());
    return false;
  }
  if (cfg.token.length() > 0) {
    http.addHeader("Authorization", "Bearer " + cfg.token);
  }
  http.setTimeout(10000);
  int code = http.GET();
  if (code != 200) {
    logf("HTTP: %s -> %d", url.c_str(), code);
    http.end();
    return false;
  }
  String body = http.getString();
  http.end();

  JsonDocument fresh;
  DeserializationError err = deserializeJson(fresh, body);
  if (err) {
    logf("HTTP: payload parse error: %s", err.c_str());
    return false;
  }
  // Adopt the new payload + persist to SD.
  payload_doc = fresh;
  writeCacheToSD(body);
  last_good_fetch_ms = millis();
  serving_cache = false;
  return true;
}

// ─── Display rendering ───────────────────────────────────────────────
//
// All rendering writes to GxEPD2's internal buffer; we then page+flush.
// Each render is a full-screen redraw so ghosting is bounded; every
// 12th render we force a full clear/refresh to scrub any residual.
void renderStatusBar(int top_y) {
  // Top row: machine name + connected dot · last_updated · page x/N
  display.setFont(&FreeMonoBold9pt7b);
  display.setCursor(8, top_y + 14);
  display.print(machine_name);
  display.print(serving_cache ? " [SERVER OFFLINE - cached]"
                : (machine_connected ? " [connected]"
                                     : " [disconnected]"));

  display.setFont(&FreeMono9pt7b);
  display.setCursor(8, top_y + 30);
  display.print("updated ");
  display.print(last_updated_display);

  int total_pages = computeTotalPages();
  String pager = "page " + String(current_page + 1) +
                 " / " + String(total_pages);
  // Right-align to the screen width with a 12px margin. Each char is
  // ~10px in the 9pt font; subtract a back-of-envelope width.
  int w = pager.length() * 10;
  display.setCursor(cfg.res_x - w - 12, top_y + 30);
  display.print(pager);
}

int pageSize() { return cfg.library_page_size > 0 ? cfg.library_page_size : 10; }

int computeTotalPages() {
  int per = pageSize();
  if (cfg.pocket_view == "table") {
    // Paginated magazine table: one page set over the pockets array.
    JsonArrayConst pk = payload_doc["data"]["pockets"];
    int n = pk.isNull() ? 0 : (int) pk.size();
    int p = (n + per - 1) / per;
    return p < 1 ? 1 : p;
  }
  // Cells mode: page 0 is the magazine grid; library tables follow.
  JsonArrayConst lib = payload_doc["data"]["library"];
  int n = lib.isNull() ? 0 : (int) lib.size();
  int lib_pages = (n + per - 1) / per;
  return 1 + lib_pages;
}

// Truncate s to fit within max_w pixels in the currently-selected font,
// appending ">" when clipped. Measures real glyph metrics via getTextBounds
// so it is correct for any font/cell width instead of guessing chars-per-px.
String fitToWidth(const String &s, int max_w) {
  int16_t bx, by;
  uint16_t bw, bh;
  display.getTextBounds(s, 0, 0, &bx, &by, &bw, &bh);
  if ((int) bw <= max_w) return s;
  String t = s;
  while (t.length() > 1) {
    t.remove(t.length() - 1);
    String probe = t + ">";
    display.getTextBounds(probe, 0, 0, &bx, &by, &bw, &bh);
    if ((int) bw <= max_w) return probe;
  }
  return t;
}

void renderPocketMap() {
  // 2×10 default grid. Each cell shows TWO tidy rows that never overlap:
  //   row 1 (bold): P<n>  T<tool>            D<dia> L<len>   (dims right-aligned)
  //   row 2:        <description, truncated to the cell width>
  // Previously three lines were crammed into ~42px and the description
  // collided with the dia/len line (baselines only ~4px apart).
  const int status_h = 50;
  const int pad = 6;
  int grid_top = status_h + 4;
  int grid_h = cfg.res_y - grid_top;
  int cell_w = cfg.res_x / cfg.pocket_cols;
  int cell_h = grid_h / cfg.pocket_rows;

  JsonArrayConst pockets = payload_doc["data"]["pockets"];
  int idx = 0;

  for (int r = 0; r < cfg.pocket_rows; r++) {
    for (int c = 0; c < cfg.pocket_cols; c++) {
      int x = c * cell_w;
      int y = grid_top + r * cell_h;
      display.drawRect(x, y, cell_w, cell_h, GxEPD_BLACK);

      const int base1 = y + 16;          // header row (upper third)
      const int base2 = y + cell_h - 8;  // description row (lower third)

      bool have = idx < (int) pockets.size();
      JsonObjectConst p;
      int pocket_num = idx + 1;
      if (have) {
        p = pockets[idx].as<JsonObjectConst>();
        pocket_num = p["pocket"] | (idx + 1);
      }
      idx++;

      // Row 1: pocket number (always shown).
      display.setFont(&FreeMonoBold9pt7b);
      display.setCursor(x + pad, base1);
      display.print("P");
      display.print(pocket_num);

      // Empty pocket: a single marker on the description row.
      if (!have || p["tool_number"].isNull()) {
        display.setFont(&FreeMono9pt7b);
        display.setCursor(x + pad, base2);
        display.print("(empty)");
        continue;
      }

      // Row 1 cont.: tool number, then dia/len right-aligned in the cell.
      display.setCursor(x + 52, base1);
      display.print("T");
      display.print((int) (p["tool_number"] | 0));

      String dims;
      if (!p["diameter"].isNull()) dims += "D" + String((float) p["diameter"], 3);
      if (!p["length"].isNull()) {
        if (dims.length()) dims += " ";
        dims += "L" + String((float) p["length"], 3);
      }
      if (dims.length()) {
        display.setFont(&FreeMono9pt7b);
        int16_t bx, by;
        uint16_t bw, bh;
        display.getTextBounds(dims, 0, 0, &bx, &by, &bw, &bh);
        display.setCursor(x + cell_w - pad - (int) bw, base1);
        display.print(dims);
      }

      // Row 2: description, truncated to the cell width.
      const char *desc = p["description"] | "";
      if (desc[0]) {
        display.setFont(&FreeMono9pt7b);
        String d = String(desc);
        d.trim();
        display.setCursor(x + pad, base2);
        display.print(fitToWidth(d, cell_w - 2 * pad));
      }
    }
  }
}

// Short header label for a library column. (Long names won't fit a narrow
// numeric column.)
String libHeaderLabel(const String &name) {
  if (name == "pocket") return "Pkt";
  if (name == "tool_number") return "Tool";
  if (name == "description") return "Description";
  if (name == "diameter") return "Dia";
  if (name == "length") return "Len";
  if (name == "wear") return "Wear";
  return name;
}

// Formatted cell text for a library column. Uses ASCII "-" for missing
// values — the FreeMono font has no em-dash glyph, so the old "—" rendered
// as a tofu box.
String libCellText(const String &name, JsonObjectConst row) {
  if (name == "pocket") return "P" + String((int) (row["pocket"] | 0));
  if (name == "tool_number") return "T" + String((int) (row["tool_number"] | 0));
  if (name == "description") {
    String d = String((const char *) (row["description"] | ""));
    d.trim();
    return d;
  }
  if (name == "diameter")
    return row["diameter"].isNull() ? String("-") : String((float) row["diameter"], 3);
  if (name == "length")
    return row["length"].isNull() ? String("-") : String((float) row["length"], 3);
  if (name == "wear")
    return row["wear"].isNull() ? String("0.000") : String((float) row["wear"], 3);
  return "";
}

// Render a slice [start,end) of a tool array (pockets OR library) as a
// bordered table with a caption. Shared by the magazine-table view and the
// library pages so both look identical. Columns come from cfg.fields; the
// description column flexes, id/numeric columns are fixed width (sized so all
// six fit even in portrait at 480px wide).
void renderToolTable(JsonArrayConst arr, int start, int end, const char *title) {
  const int margin = 6;
  const int status_h = 50;
  const int caption_h = 18;
  const int header_h = 22;
  int caption_y = status_h + 4;
  int table_top = caption_y + caption_h;

  display.setFont(&FreeMonoBold9pt7b);
  display.setCursor(margin, caption_y + 13);
  display.print(title);

  if (arr.isNull() || end <= start) {
    display.setFont(&FreeMono9pt7b);
    display.setCursor(margin, table_top + 24);
    display.print("Nothing to show.");
    return;
  }

  int nf = cfg.fields_count;
  if (nf < 1) nf = 1;
  if (nf > 8) nf = 8;

  // Column widths: id/numeric columns fixed; description flexes to fill.
  int colw[8];
  int fixed_sum = 0, desc_idx = -1;
  for (int i = 0; i < nf; i++) {
    const String &nm = cfg.fields[i];
    if (nm == "description") { desc_idx = i; colw[i] = 0; }
    else if (nm == "pocket" || nm == "tool_number") { colw[i] = 52; fixed_sum += 52; }
    else { colw[i] = 84; fixed_sum += 84; }
  }
  int avail = cfg.res_x - 2 * margin;
  if (desc_idx >= 0) {
    int dw = avail - fixed_sum;
    colw[desc_idx] = dw < 80 ? 80 : dw;
  }
  // Left edge of each column; colx[nf] is the table's right edge.
  int colx[9];
  colx[0] = margin;
  for (int i = 0; i < nf; i++) colx[i + 1] = colx[i] + colw[i];
  int x_right = colx[nf];
  if (x_right > cfg.res_x - margin) x_right = cfg.res_x - margin;

  int per = pageSize();
  int nrows = end - start;
  int header_y = table_top;
  int body_y = header_y + header_h;
  int row_h = (cfg.res_y - body_y) / per;  // fixed per page so rows align
  if (row_h < 16) row_h = 16;
  int table_bottom = body_y + nrows * row_h;

  // Header labels (bold).
  display.setFont(&FreeMonoBold9pt7b);
  for (int i = 0; i < nf; i++) {
    display.setCursor(colx[i] + 4, header_y + 16);
    display.print(fitToWidth(libHeaderLabel(cfg.fields[i]), colw[i] - 8));
  }

  // Cell values, one line per row, truncated to the column width.
  display.setFont(&FreeMono9pt7b);
  for (int r = 0; r < nrows; r++) {
    JsonObjectConst row = arr[start + r].as<JsonObjectConst>();
    int ry = body_y + r * row_h;
    for (int f = 0; f < nf; f++) {
      display.setCursor(colx[f] + 4, ry + 15);
      display.print(fitToWidth(libCellText(cfg.fields[f], row), colw[f] - 8));
    }
  }

  // Grid: outer box, header underline, row separators, column separators.
  display.drawRect(colx[0], header_y, x_right - colx[0], table_bottom - header_y, GxEPD_BLACK);
  display.drawLine(colx[0], body_y, x_right, body_y, GxEPD_BLACK);
  for (int r = 1; r < nrows; r++) {
    int ly = body_y + r * row_h;
    display.drawLine(colx[0], ly, x_right, ly, GxEPD_BLACK);
  }
  for (int i = 1; i < nf; i++) {
    display.drawLine(colx[i], header_y, colx[i], table_bottom, GxEPD_BLACK);
  }
}

// Dispatch the current page to the right renderer based on pocket_view.
//  - "table": every page is a slice of the magazine (pockets) table.
//  - "cells": page 0 is the magazine grid; later pages are library tables.
void renderCurrentPage() {
  int per = pageSize();
  char title[56];
  if (cfg.pocket_view == "table") {
    JsonArrayConst pk = payload_doc["data"]["pockets"];
    int n = pk.isNull() ? 0 : (int) pk.size();
    int start = current_page * per;
    int end = start + per;
    if (end > n) end = n;
    snprintf(title, sizeof title, "Magazine  P%d-%d / %d", start + 1, end, n);
    renderToolTable(pk, start, end, title);
  } else if (current_page == 0) {
    renderPocketMap();
  } else {
    JsonArrayConst lib = payload_doc["data"]["library"];
    int n = lib.isNull() ? 0 : (int) lib.size();
    int start = (current_page - 1) * per;
    int end = start + per;
    if (end > n) end = n;
    snprintf(title, sizeof title, "Library  T%d-%d / %d", start + 1, end, n);
    renderToolTable(lib, start, end, title);
  }
}

void renderAll() {
  // Pull layout + machine fields from the current payload before we
  // draw. Default values stay if a field is missing.
  if (!payload_doc.isNull()) {
    JsonObjectConst c = payload_doc["config"];
    if (!c.isNull()) {
      cfg.res_x = c["resolution"][0] | cfg.res_x;
      cfg.res_y = c["resolution"][1] | cfg.res_y;
      cfg.pocket_cols = c["pocketGrid"][0] | cfg.pocket_cols;
      cfg.pocket_rows = c["pocketGrid"][1] | cfg.pocket_rows;
      cfg.library_page_size = c["libraryPageSize"] | cfg.library_page_size;
      cfg.poll_powered_s = c["pollIntervalPoweredS"] | cfg.poll_powered_s;
      cfg.poll_battery_s = c["pollIntervalBatteryS"] | cfg.poll_battery_s;
      cfg.units = c["units"] | cfg.units;
      cfg.pocket_view = c["pocketView"] | cfg.pocket_view;
      cfg.rotate = c["rotate"] | cfg.rotate;
      JsonArrayConst f = c["fields"];
      if (!f.isNull()) {
        int n = (int) f.size();
        if (n > 8) n = 8;
        cfg.fields_count = n;
        for (int i = 0; i < n; i++) cfg.fields[i] = (const char *) f[i];
      }
    }
    JsonObjectConst data = payload_doc["data"];
    if (!data.isNull()) {
      machine_name = (const char *) (data["machine"]["name"] | "");
      machine_connected = data["machine"]["connected"] | false;
      last_updated_display = (const char *) (data["last_updated_display"] | "");
    }
  }

  // Rotation: runtime override (long-press Refresh) wins over the config
  // value. setRotation(1) is portrait; pull the logical width/height back
  // from the driver so all layout math reflows for the active orientation.
  int eff_rotate = (g_rotation >= 0) ? g_rotation : cfg.rotate;
  display.setRotation(eff_rotate == 90 ? 1 : 0);
  cfg.res_x = display.width();
  cfg.res_y = display.height();
  display.setTextColor(GxEPD_BLACK);

  // Page count can shrink (mode change, smaller payload) — keep in range.
  int total_pages = computeTotalPages();
  if (current_page < 0 || current_page >= total_pages) current_page = 0;

  // Periodic full clear every 12 renders to scrub e-paper ghosting.
  bool full_clear = (periodic_full_refresh_at_page++ % 12) == 0;
  if (full_clear) {
    display.setFullWindow();
    display.fillScreen(GxEPD_WHITE);
    display.display(false);
  }

  display.setFullWindow();
  display.firstPage();
  do {
    display.fillScreen(GxEPD_WHITE);
    renderStatusBar(0);
    renderCurrentPage();
  } while (display.nextPage());
}

// ─── Buttons: paging + refresh ───────────────────────────────────────
// Three active-LOW buttons on GPIO3/4/5, edge-triggered with a 200 ms
// debounce. Paging is zero-network (reads the in-memory payload); only
// Refresh hits the server. Each press logs its label so the operator can
// confirm which physical button maps to which action.
void handleButtons() {
  static unsigned long last_press_ms = 0;
  static int prev_refresh = HIGH, prev_prev = HIGH, prev_next = HIGH;
  static unsigned long refresh_down_ms = 0;
  static bool refresh_long_done = false;

  int r = digitalRead(PIN_KEY_REFRESH);
  int p = digitalRead(PIN_KEY_PREV);
  int n = digitalRead(PIN_KEY_NEXT);
  bool can = (millis() - last_press_ms) > 200;
  int total = computeTotalPages();
  if (total < 1) total = 1;

  // Prev / Next page on the press edge.
  if (p == LOW && prev_prev == HIGH && can) {
    last_press_ms = millis();
    current_page = (current_page - 1 + total) % total;
    logf("BTN: Left -> page %d/%d", current_page + 1, total);
    renderAll();
  } else if (n == LOW && prev_next == HIGH && can) {
    last_press_ms = millis();
    current_page = (current_page + 1) % total;
    logf("BTN: Right -> page %d/%d", current_page + 1, total);
    renderAll();
  }

  // Refresh: SHORT press (on release) re-fetches; LONG press (~800 ms held)
  // toggles landscape/portrait so rotation can be evaluated on-device.
  if (r == LOW) {
    if (prev_refresh == HIGH) {
      refresh_down_ms = millis();
      refresh_long_done = false;
    }
    if (!refresh_long_done && (millis() - refresh_down_ms) > 800) {
      refresh_long_done = true;
      last_press_ms = millis();
      int cur = (g_rotation >= 0) ? g_rotation : cfg.rotate;
      g_rotation = (cur == 90) ? 0 : 90;
      logf("BTN: Refresh(hold) -> rotate %d", g_rotation);
      renderAll();
    }
  } else if (prev_refresh == LOW && !refresh_long_done && can) {
    last_press_ms = millis();
    logf("BTN: Refresh -> re-fetching from server");
    if (WiFi.status() == WL_CONNECTED && fetchPayload()) {
      serving_cache = false;
    }
    renderAll();
  }

  prev_refresh = r;
  prev_prev = p;
  prev_next = n;
}

// ─── Setup + loop ────────────────────────────────────────────────────
void setup() {
  Serial.begin(115200);
  // USB CDC enumerates asynchronously. The host attaches the port a
  // beat after the firmware boots; printing in that gap drops bytes.
  // Wait up to ~3s for Serial to be attached so the operator's
  // serial monitor catches the boot logs from line 1.
  unsigned long t0 = millis();
  while (!Serial && (millis() - t0) < 3000) {
    delay(10);
  }
  delay(200);
  logf("eterminal-display %s booting", ETERMINAL_FIRMWARE_VERSION);

  // Three front buttons on GPIO3/4/5 (active-LOW, board pull-ups). Safe to
  // enable now that the e-paper/SD pins moved off GPIO1-6 — the old
  // D6/GPIO43 "button" was UART0 TX and claiming it killed Serial.
  pinMode(PIN_KEY_REFRESH, INPUT_PULLUP);
  pinMode(PIN_KEY_PREV, INPUT_PULLUP);
  pinMode(PIN_KEY_NEXT, INPUT_PULLUP);

  // Power the microSD slot BEFORE any SPI/SD activity. The slot sits behind a
  // TPS22916 load switch gated by SD_EN (GPIO16); until it is HIGH the card —
  // and its on-board pull-ups — have no power and never respond to chip-select,
  // which is exactly the f_mount(3) / "sdSelectCard(): Select Failed" we saw.
  pinMode(PIN_SD_EN, OUTPUT);
  digitalWrite(PIN_SD_EN, HIGH);
  delay(50);

  SPI.begin();
  display.init(115200, true, 2, false);

  if (!loadConfigFromSD()) {
    // Render an SD-failure splash so the operator knows what to fix.
    display.setFullWindow();
    display.firstPage();
    do {
      display.fillScreen(GxEPD_WHITE);
      display.setTextColor(GxEPD_BLACK);
      display.setFont(&FreeMonoBold9pt7b);
      display.setCursor(40, 60);
      display.print("eterminal-display");
      display.setFont(&FreeMono9pt7b);
      display.setCursor(40, 100);
      display.print("/config.json missing or invalid on SD.");
      display.setCursor(40, 130);
      display.print("See eterminal-display/README.md");
    } while (display.nextPage());
    while (true) delay(1000);
  }

  // SD self-update before WiFi: if there's a pending firmware.bin,
  // flash + reboot. Operator could be staging the new .bin via a
  // card-pull when the LAN is unavailable.
  maybeFlashFromSD();

  if (!connectWiFi()) {
    // WiFi failed: render cache + OFFLINE and keep trying in loop().
    if (loadCacheFromSD()) {
      serving_cache = true;
      renderAll();
    }
  } else {
    setupOTA();
    if (fetchPayload()) {
      renderAll();
    } else if (loadCacheFromSD()) {
      serving_cache = true;
      renderAll();
    }
  }
}

void loop() {
  ArduinoOTA.handle();
  handleButtons();

  // Heartbeat so we always have some signal in the serial monitor.
  // If the operator opens the monitor late and missed setup() logs,
  // this still tells them the firmware is alive and what state it's
  // in. Once we're done debugging the device-side serial path we can
  // drop the cadence to once a minute or remove entirely.
  static unsigned long last_hb_ms = 0;
  if (millis() - last_hb_ms > 5000) {
    last_hb_ms = millis();
    logf("[hb] up=%lus wifi=%d serving_cache=%d page=%d",
         millis() / 1000, WiFi.status(), (int) serving_cache, current_page);
  }

  static unsigned long last_poll_ms = 0;
  unsigned long now = millis();
  int poll_s = (USE_BATTERY_INTERVAL ? cfg.poll_battery_s : cfg.poll_powered_s);
  if (poll_s < 10) poll_s = 10;
  unsigned long poll_ms = (unsigned long) poll_s * 1000;

  if (WiFi.status() == WL_CONNECTED) {
    if (now - last_poll_ms > poll_ms || last_poll_ms == 0) {
      last_poll_ms = now;
      bool ok = fetchPayload();
      if (!ok && !serving_cache && loadCacheFromSD()) {
        serving_cache = true;
      } else if (!ok) {
        serving_cache = true;  // already on cache, mark stale
      }
      renderAll();
    }
  } else {
    // Try to reconnect every 30s while serving the cache.
    static unsigned long last_reconnect_ms = 0;
    if (now - last_reconnect_ms > 30000) {
      last_reconnect_ms = now;
      WiFi.disconnect();
      connectWiFi();
    }
  }

  delay(50);
}
