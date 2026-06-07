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
constexpr int8_t PIN_EPD_CS    = D1;
constexpr int8_t PIN_EPD_DC    = D3;
constexpr int8_t PIN_EPD_RST   = D0;
constexpr int8_t PIN_EPD_BUSY  = D2;
constexpr int8_t PIN_SD_CS     = D5;
// The reTerminal E1001 carries a single user-facing button wired to
// D6 on the XIAO baseboard. Active LOW with the internal pull-up.
constexpr int8_t PIN_BUTTON    = D6;

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
unsigned long last_good_fetch_ms = 0;
bool serving_cache = false;
String last_updated_display = "";
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
  if (!SD.begin(PIN_SD_CS)) {
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
  WiFi.begin(cfg.wifi_ssid.c_str(), cfg.wifi_pass.c_str());
  unsigned long start = millis();
  while (WiFi.status() != WL_CONNECTED && millis() - start < 15000) {
    delay(250);
  }
  if (WiFi.status() != WL_CONNECTED) {
    logf("WiFi: connect timeout for SSID %s", cfg.wifi_ssid.c_str());
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

int computeTotalPages() {
  JsonArrayConst lib = payload_doc["data"]["library"];
  int lib_pages = 0;
  if (!lib.isNull()) {
    int n = lib.size();
    lib_pages = (n + cfg.library_page_size - 1) / cfg.library_page_size;
  }
  // Page 0 is always the pocket map; library pages follow.
  return 1 + lib_pages;
}

void renderPocketMap() {
  // 2×10 default grid: cell is roughly cfg.res_x / cols wide and
  // (cfg.res_y - status) / rows tall.
  const int status_h = 50;
  int grid_top = status_h + 4;
  int grid_h = cfg.res_y - grid_top;
  int cell_w = cfg.res_x / cfg.pocket_cols;
  int cell_h = grid_h / cfg.pocket_rows;

  JsonArrayConst pockets = payload_doc["data"]["pockets"];
  int idx = 0;
  display.setFont(&FreeMono9pt7b);

  for (int r = 0; r < cfg.pocket_rows; r++) {
    for (int c = 0; c < cfg.pocket_cols; c++) {
      int x = c * cell_w;
      int y = grid_top + r * cell_h;
      display.drawRect(x, y, cell_w, cell_h, GxEPD_BLACK);
      if (idx >= (int) pockets.size()) {
        idx++;
        continue;
      }
      JsonObjectConst p = pockets[idx];
      idx++;

      // Pocket number — large, bold, top-left of the cell.
      int pocket_num = p["pocket"] | 0;
      display.setFont(&FreeMonoBold9pt7b);
      display.setCursor(x + 6, y + 16);
      display.print("P");
      display.print(pocket_num);

      if (p["tool_number"].isNull()) {
        display.setFont(&FreeMono9pt7b);
        display.setCursor(x + 6, y + 32);
        display.print("empty");
        continue;
      }
      int tool_num = p["tool_number"] | 0;
      display.setFont(&FreeMonoBold9pt7b);
      display.setCursor(x + 60, y + 16);
      display.print("T");
      display.print(tool_num);

      // Description (truncated to fit).
      const char *desc = p["description"] | "";
      if (desc[0]) {
        display.setFont(&FreeMono9pt7b);
        display.setCursor(x + 6, y + 32);
        // ~14 chars fit in an 80px wide cell at 9pt mono.
        int max_chars = (cell_w - 12) / 7;
        String d = String(desc);
        if ((int) d.length() > max_chars) d = d.substring(0, max_chars);
        display.print(d);
      }

      // Diameter/length — small line at the bottom.
      display.setFont(&FreeMono9pt7b);
      display.setCursor(x + 6, y + cell_h - 6);
      if (!p["diameter"].isNull()) {
        display.print("D");
        display.print((float) p["diameter"], 3);
        display.print(" ");
      }
      if (!p["length"].isNull()) {
        display.print("L");
        display.print((float) p["length"], 3);
      }
    }
  }
}

void renderLibraryPage(int page_index_zero_based) {
  // page_index_zero_based is 0..N-1 across the library_page_size rows.
  JsonArrayConst lib = payload_doc["data"]["library"];
  if (lib.isNull() || lib.size() == 0) {
    display.setFont(&FreeMono9pt7b);
    display.setCursor(20, 100);
    display.print("Library is empty.");
    return;
  }
  int start = page_index_zero_based * cfg.library_page_size;
  if (start >= (int) lib.size()) return;
  int end = start + cfg.library_page_size;
  if (end > (int) lib.size()) end = lib.size();

  const int status_h = 50;
  int table_top = status_h + 4;
  int row_h = (cfg.res_y - table_top) / cfg.library_page_size;
  if (row_h < 14) row_h = 14;

  display.setFont(&FreeMonoBold9pt7b);
  display.setCursor(8, table_top + 14);
  // Compact header line — column names from cfg.fields in order.
  for (int i = 0; i < cfg.fields_count; i++) {
    display.print(cfg.fields[i].c_str());
    if (i < cfg.fields_count - 1) display.print(" | ");
  }

  display.setFont(&FreeMono9pt7b);
  int y = table_top + row_h;
  for (int i = start; i < end; i++) {
    JsonObjectConst row = lib[i];
    display.setCursor(8, y + 12);
    for (int f = 0; f < cfg.fields_count; f++) {
      const String &name = cfg.fields[f];
      if (name == "pocket") display.print((int) (row["pocket"] | 0));
      else if (name == "tool_number") display.print((int) (row["tool_number"] | 0));
      else if (name == "description") display.print((const char *) (row["description"] | ""));
      else if (name == "diameter") {
        if (row["diameter"].isNull()) display.print("—");
        else display.print((float) row["diameter"], 3);
      } else if (name == "length") {
        if (row["length"].isNull()) display.print("—");
        else display.print((float) row["length"], 3);
      } else if (name == "wear") {
        if (row["wear"].isNull()) display.print("0.000");
        else display.print((float) row["wear"], 3);
      }
      if (f < cfg.fields_count - 1) display.print(" | ");
    }
    y += row_h;
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

  display.setRotation(0);
  display.setTextColor(GxEPD_BLACK);

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
    if (current_page == 0) {
      renderPocketMap();
    } else {
      renderLibraryPage(current_page - 1);
    }
  } while (display.nextPage());
}

// ─── Button: wraparound paging ───────────────────────────────────────
void handleButton() {
  static unsigned long last_press_ms = 0;
  static int last_state = HIGH;
  int s = digitalRead(PIN_BUTTON);
  if (s == LOW && last_state == HIGH && millis() - last_press_ms > 200) {
    last_press_ms = millis();
    int total = computeTotalPages();
    current_page = (current_page + 1) % total;
    logf("BTN: page -> %d/%d", current_page + 1, total);
    renderAll();
  }
  last_state = s;
}

// ─── Setup + loop ────────────────────────────────────────────────────
void setup() {
  Serial.begin(115200);
  delay(500);
  logf("eterminal-display %s booting", ETERMINAL_FIRMWARE_VERSION);

  pinMode(PIN_BUTTON, INPUT_PULLUP);

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
  handleButton();

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
