# Program identity — header + sidecar + reconciliation

Written 2026-09-07. Answers the failure in
`docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md` section A: an NC file today
carries no record of which Carbon job/operation it belongs to, or where its
tool data came from. A `T5` in the program is matched to a `T5` in whatever
tool table was last read from the control, on the bare integer alone.

This doc defines the fix: a small header comment block the post writes into
every program, a JSON sidecar next to the file with everything preflight
needs to check tools *before* the machine ever sees them, and the exact
`.cps` snippet to generate both. The Go implementation lives in
`cnc/program_identity.go` and hooks into `cnc/preflight.go`.

Because tool libraries now live in Fusion's cloud library rather than a
locally-uploaded `tool-library.json`, the sidecar is self-contained — it
carries the per-tool geometry needed for reconciliation itself, so no
library upload step is required at send time.

## 1. Header comment block

Written by the post as the very first lines of the file, before any other
preamble the post already emits (date/time, native tool list, etc.) — the
identity block owns line 1 unconditionally so parsing never has to guess
where it starts.

Rules (Haas-safe):

- Each line is a standalone `( ... )` comment — no G-code sharing the line.
- Uppercase only. Restrict content to `A-Z 0-9 . , : = + - _ /` — avoid
  characters some Haas comment/DPRNT paths choke on (`%`, unmatched `(`/`)`,
  control characters). None of the fields below need anything outside that
  set.
- Each line under 80 characters total, including the parens.
- The block is a **contiguous run starting at line 1**. The first line that
  is not `(GMW-...)` ends the block. Everything from that point to EOF,
  including any of the post's own preamble comments, is "body" for the
  sha256 in `GMW-SHA`.
- Field order is fixed so a human skimming the top of the file always sees
  the same shape. Only `GMW-ID` is strictly required for the file to be
  recognized as identity-bearing; the rest degrade gracefully (a missing or
  malformed field just doesn't populate, it doesn't invalidate the block).

```
(GMW-ID V1)
(GMW-JOB J000020 OP10)
(GMW-PART F-BRACKET-REV-C)
(GMW-POST HAAS-NGC V1.0.3)
(GMW-POSTED 2026-09-07T14:22:00Z)
(GMW-TOOLS 3)
(GMW-SHA 9F3A5B21C0D4E7F1A2B3C4D5E6F708192A3B4C5D6E7F8091A2B3C4D5E6F7081)
```

| Field | Meaning | Example |
|---|---|---|
| `GMW-ID` | Schema version of this header format | `V1` |
| `GMW-JOB` | Carbon `jobReadableId`, then the operation id/seq | `J000020 OP10` |
| `GMW-PART` | Part or item id | `F-BRACKET-REV-C` |
| `GMW-POST` | Post name, then post version | `HAAS-NGC V1.0.3` |
| `GMW-POSTED` | UTC timestamp, RFC 3339 | `2026-09-07T14:22:00Z` |
| `GMW-TOOLS` | Tool count, for a fast sanity check without reading the sidecar | `3` |
| `GMW-SHA` | sha256 (hex) of the file **body**, i.e. everything after the header block | 64 hex chars |

`GMW-SHA` is what makes "re-posting with the same body" detectable: the
hash covers only the body, so re-running the post at a different time (new
`GMW-POSTED`) against unchanged toolpaths produces the same `GMW-SHA`.
Changing so much as a feedrate changes it.

Parsing lives in `cnc/program_identity.go`: `ParseIdentity(nc []byte) (*Identity, bool)`.
The bool is false only when line 1 isn't a `GMW-ID` line at all — an
un-annotated program is not an error, it's just not identity-bearing, and
everything falls back to today's T-number-only preflight.

## 2. Sidecar: `<program>.gmw.json`

Written next to the NC file (same directory, filename + `.gmw.json`).
Resolved in Go as `path + ".gmw.json"` — see `SidecarPath` in
`cnc/program_identity.go`. Never crosses into `http/`: `cnc.BuildPreflight`
resolves it itself from the NC file's path.

Carries the same identity fields as the header (as real JSON, not
comment-parsed strings) plus everything preflight needs to check tools
against the machine *without* a separately-uploaded library: per-tool
geometry, reach, and the per-operation list.

### Shape

```json
{
  "schema_version": 1,
  "job": "J000020",
  "operation": "OP10",
  "part": "F-BRACKET-REV-C",
  "post_name": "HAAS-NGC",
  "post_version": "1.0.3",
  "posted_at": "2026-09-07T14:22:00Z",
  "tool_count": 2,
  "sha256": "9f3a5b21c0d4e7f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f7081",
  "tools": [
    {
      "t_number": 5,
      "guid": "3f9c2b7e-...-guid",
      "description": "1/4 4FL CARBIDE EM",
      "type": "flat end mill",
      "diameter": 0.25,
      "flute_length": 0.75,
      "overall_length": 2.5,
      "stickout_length": 1.5,
      "holder_id": "BT30-ER16",
      "holder_description": "ER16 COLLET CHUCK",
      "min_z": -1.87,
      "max_depth": 1.87,
      "feed": 25.0,
      "speed": 8000
    },
    {
      "t_number": 7,
      "guid": "8a1d4f22-...-guid",
      "description": "0.201 DRILL",
      "type": "drill",
      "diameter": 0.201,
      "flute_length": 1.25,
      "overall_length": 3.0,
      "stickout_length": 1.8,
      "min_z": -0.62,
      "max_depth": 0.62,
      "feed": 4.5,
      "speed": 3200
    }
  ],
  "operations": [
    {"name": "ROUGH XY", "tool": 5, "wcs": "G54", "stock_to_leave": 0.01},
    {"name": "FINISH XY", "tool": 5, "wcs": "G54", "stock_to_leave": 0.0},
    {"name": "DRILL 4X", "tool": 7, "wcs": "G54", "stock_to_leave": 0.0}
  ]
}
```

### Field notes

- `max_depth` is the deepest the toolpath reaches below stock top, for the
  tool, as a positive number of inches — the maximum across every operation
  that reuses the tool. `min_z` is the raw signed value the post read off
  `section.getGlobalZRange()`, kept for debugging; `max_depth` is what
  `ReconcileTools` actually uses (see §4). Whether `min_z == -max_depth`
  depends on the WCS Z=0 convention the post/CAM setup uses — **verify in
  Fusion** that Z=0 is stock top for this post's setups before trusting the
  sign flip blindly.
- `flute_length` limits how deep the tool can cut at all; `stickout_length`
  (tool exposed below the holder) limits how deep before the *holder*
  collides with the part or a fixture. Both are needed because a reach can
  clear the flute but still crash the holder into a clamp.
- `stock_to_leave` is per-operation because roughing and finishing passes
  on the same tool normally use different values.
- `guid` is the Fusion tool-library GUID when the post can obtain it. Per
  the open question already on record in
  `docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md` section A, the post
  kernel's exact accessor for this is unconfirmed — **verify in Fusion**.
  `ReconcileTools` does not require it; it is carried for a future
  GUID-keyed reconciliation and for humans reading the file.

### JSON Schema (draft-07)

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "GMW program identity sidecar",
  "type": "object",
  "required": ["schema_version", "job", "sha256", "tools"],
  "properties": {
    "schema_version": {"type": "integer", "minimum": 1},
    "job": {"type": "string"},
    "operation": {"type": "string"},
    "part": {"type": "string"},
    "post_name": {"type": "string"},
    "post_version": {"type": "string"},
    "posted_at": {"type": "string", "format": "date-time"},
    "tool_count": {"type": "integer", "minimum": 0},
    "sha256": {"type": "string", "pattern": "^[0-9a-fA-F]{64}$"},
    "tools": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["t_number", "diameter"],
        "properties": {
          "t_number": {"type": "integer", "minimum": 1},
          "guid": {"type": "string"},
          "description": {"type": "string"},
          "type": {"type": "string"},
          "diameter": {"type": "number", "minimum": 0},
          "flute_length": {"type": "number", "minimum": 0},
          "overall_length": {"type": "number", "minimum": 0},
          "stickout_length": {"type": "number", "minimum": 0},
          "holder_id": {"type": "string"},
          "holder_description": {"type": "string"},
          "min_z": {"type": "number"},
          "max_depth": {"type": "number", "minimum": 0},
          "feed": {"type": "number", "minimum": 0},
          "speed": {"type": "number", "minimum": 0}
        }
      }
    },
    "operations": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["name", "tool"],
        "properties": {
          "name": {"type": "string"},
          "tool": {"type": "integer"},
          "wcs": {"type": "string"},
          "stock_to_leave": {"type": "number"}
        }
      }
    }
  }
}
```

## 3. Fusion post-processor snippet

Paste into the Haas `.cps`. This is written to *merge into* whatever
`onOpen` / `onSection` / `onClose` already exist in Jason's post — do not
replace them; add the calls shown at the marked points.

**Confidence note up front:** the post kernel (the JS engine used by
Fusion's CAM post processors, formerly the HSMWorks post engine) is not
vendored in this repo and there is no live Fusion instance to test against
here, so every API name below is offline knowledge, not something this
change could execute and confirm. Where I have reasonable confidence
(commonly used in the public Autodesk sample posts) that's noted; where I
don't, it's marked **verify in Fusion** rather than asserted. Do not treat
anything in this section as confirmed until it's been run through the
post's own debugger/`writeln` output once. The full list is repeated as a
checklist at the end of this section.

### Properties (post configuration, set once per machine/post)

```js
properties.gmwJob  = "";   // Carbon jobReadableId, e.g. "J000020" -- set at post time
properties.gmwOp   = "";   // Carbon operation id/seq, e.g. "OP10"
properties.gmwPart = "";   // part or item id, e.g. "F-BRACKET-REV-C"

propertyDefinitions.gmwJob = {
  title: "GMW Job", description: "Carbon jobReadableId (e.g. J000020)", type: "string"
};
propertyDefinitions.gmwOp = {
  title: "GMW Operation", description: "Carbon operation id/seq (e.g. OP10)", type: "string"
};
propertyDefinitions.gmwPart = {
  title: "GMW Part/Item", description: "Part or item id", type: "string"
};
```

`properties` / `propertyDefinitions` are the standard post-property
mechanism used throughout every stock Fusion post — high confidence, no
flag needed.

### Collecting tools and operation depths

The whole toolpath plan is known before the first line of G-code is
written, so this pulls tool + depth data with one pass over every section
in `onOpen`, rather than accumulating incrementally in `onSection`. That
sidesteps ordering questions about when each per-tool field first becomes
available. Verify in Fusion that `getNumberOfSections()` / `getSection(i)`
are safe to call from `onOpen` on this post version — they are in every
standard Autodesk sample post, but confirm before relying on it here.

```js
var gmwTools = {};   // keyed by tool.number, deduped across operations
var gmwOps   = [];   // one entry per operation/section, in program order

function gmwCollectAll() {
  var n = getNumberOfSections();
  for (var i = 0; i < n; ++i) {
    var section = getSection(i);
    var tool = section.getTool();
    if (!tool) { continue; }

    // section.getGlobalZRange() -- reasonably well-attested in Autodesk
    // sample posts for "what Z does this operation's motion span".
    // VERIFY IN FUSION: confirm the Z frame of reference (this snippet
    // assumes Z=0 is stock top per the CAM setup's WCS, i.e. minimum is
    // negative and max_depth = -minimum). If this post's setups don't
    // follow that convention, max_depth needs a different derivation
    // (e.g. subtract the stock-top Z explicitly).
    var zRange = section.getGlobalZRange();
    var minZ = zRange ? zRange.getMinimum() : undefined;

    var key = tool.number;
    if (!gmwTools[key]) {
      gmwTools[key] = {
        t_number: tool.number,
        // VERIFY IN FUSION: no confirmed accessor for the tool-library
        // GUID from the live post Tool object. This is the same open
        // question already on record in
        // docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md section A.
        // Leave "" if nothing pans out rather than guessing a property.
        guid: (tool.guid !== undefined) ? tool.guid : "",
        // VERIFY IN FUSION: tool.comment is well-attested as the
        // human-readable text CAM posts already emit ("1/4 4FL Carbide
        // EM" style). tool.description is NOT confirmed to exist on the
        // live object -- try tool.comment first.
        description: tool.comment || tool.description || "",
        // getToolTypeName() is a helper commonly DEFINED BY THE POST
        // ITSELF (not guaranteed to be a kernel builtin) to turn the
        // tool.type enum into text. VERIFY IN FUSION that this post
        // already defines it; if not, either add it or fall back to
        // the raw enum value as done here.
        type: (typeof getToolTypeName === "function") ? getToolTypeName(tool.type) : String(tool.type),
        diameter: tool.diameter,
        // VERIFY IN FUSION: tool.fluteLength / tool.bodyLength are
        // plausible from post-processor cookbook usage but not
        // confirmed against this Fusion version's Tool object.
        flute_length: tool.fluteLength,
        overall_length: tool.bodyLength,
        // VERIFY IN FUSION: there is no single obviously-correct
        // "stickout" property; posts that do holder-collision checking
        // derive it from tool.holder geometry rather than reading it
        // directly. tool.fluteLength is used here ONLY as a
        // last-resort placeholder -- replace with the real derivation
        // once confirmed (likely gaugeLength minus holder engagement).
        stickout_length: tool.fluteLength,
        // VERIFY IN FUSION: tool.holder as a sub-object (with its own
        // .comment / .productId / geometry) was added to the post
        // kernel at some point for holder-collision checking, but the
        // exact property names on it are unconfirmed here.
        holder_id: (tool.holder && tool.holder.productId) ? tool.holder.productId : "",
        holder_description: (tool.holder && tool.holder.comment) ? tool.holder.comment : "",
        min_z: minZ,
        max_depth: (minZ !== undefined) ? -minZ : undefined,
        // VERIFY IN FUSION: feed/speed per section is normally reached
        // through section.getParameter(...) with a strategy-specific
        // parameter name, not a flat tool property. tool.spindleRPM
        // is a reasonable guess for speed; feed has no single stable
        // property at all -- this is the weakest-attested field here.
        feed: (typeof tool.feedCutting === "number") ? tool.feedCutting : undefined,
        speed: (typeof tool.spindleRPM === "number") ? tool.spindleRPM : undefined
      };
    } else if (minZ !== undefined) {
      // Reused tool -- widen the depth range across every operation.
      if (gmwTools[key].min_z === undefined || minZ < gmwTools[key].min_z) {
        gmwTools[key].min_z = minZ;
        gmwTools[key].max_depth = -minZ;
      }
    }

    gmwOps.push({
      // VERIFY IN FUSION: "operation-comment" is the commonly-used
      // parameter name for the operation's display name in Autodesk
      // sample posts, accessed via section.hasParameter/getParameter.
      name: section.hasParameter("operation-comment") ? section.getParameter("operation-comment") : "",
      tool: tool.number,
      // VERIFY IN FUSION: work offset -> WCS number mapping.
      // section.workOffset is commonly an integer (1=G54, 2=G55, ...);
      // this assumes that exact numbering.
      wcs: (typeof section.workOffset === "number" && section.workOffset > 0)
        ? "G" + (53 + section.workOffset) : "",
      // VERIFY IN FUSION: exact stock-to-leave parameter key varies by
      // milling strategy (2D vs 3D vs adaptive) in real Fusion posts.
      stock_to_leave: section.hasParameter("operation:stockToLeaveStock")
        ? section.getParameter("operation:stockToLeaveStock") : 0
    });
  }
}
```

### `onOpen` — call the collector, write the header (SHA pending)

The sha256 covers the body, which doesn't exist yet when `onOpen` runs, so
the header is written with a placeholder and patched in `onClose` (below).
There is no confirmed built-in `sha256()` in the post kernel's JS
environment (it's a plain ECMAScript sandbox with no `crypto` module) —
**verify in Fusion**, and if genuinely absent, a small pure-JS SHA-256
implementation (no external dependency, straightforward bitwise ES5 code)
has to be embedded directly in the `.cps` file. That implementation is not
included here since it's mechanical, not something this change can verify
against a real post kernel's JS dialect (ES5 vs. later) — confirm the
kernel's JS support level in Fusion before writing it.

```js
function onOpen() {
  // ... existing onOpen body stays first ...

  gmwCollectAll();

  writeComment("GMW-ID V1");
  writeComment("GMW-JOB " + properties.gmwJob + " " + properties.gmwOp);
  writeComment("GMW-PART " + properties.gmwPart);
  writeComment("GMW-POST HAAS-NGC V1.0.3");
  // VERIFY IN FUSION: exact Date -> RFC3339 formatting helper. Plain JS
  // Date exists in the kernel (used elsewhere for DATE/TIME comments in
  // stock posts); toISOString() is standard ES5 and should be fine, but
  // hasn't been run against this kernel.
  writeComment("GMW-POSTED " + new Date().toISOString());
  writeComment("GMW-TOOLS " + Object.keys(gmwTools).length);
  writeComment("GMW-SHA PENDING");
}
```

`writeComment` is the standard kernel function every stock post already
uses for `( ... )` output — high confidence, no flag needed. Note it must
already uppercase/sanitize per the post's `format` settings; if this post's
`format.comment` allows lowercase or punctuation outside the safe set,
tighten it so the header stays Haas-safe.

### `onClose` — write the sidecar, then patch the real SHA

```js
function onClose() {
  // ... existing onClose body stays first, so the NC file is fully
  // written before we touch it ...

  var ncPath = getOutputPath(); // VERIFY IN FUSION: exact accessor name/behavior
  gmwWriteSidecar(ncPath);
  gmwPatchSha(ncPath);
}

function gmwWriteSidecar(ncPath) {
  var toolList = [];
  for (var k in gmwTools) { toolList.push(gmwTools[k]); }

  var sidecar = {
    schema_version: 1,
    job: properties.gmwJob,
    operation: properties.gmwOp,
    part: properties.gmwPart,
    post_name: "HAAS-NGC",
    post_version: "1.0.3",
    posted_at: new Date().toISOString(),
    tool_count: toolList.length,
    sha256: gmwLastComputedSha || "",
    tools: toolList,
    operations: gmwOps
  };

  // TextFile + FileSystem are the standard kernel APIs stock posts use
  // to emit an extra file alongside the NC output (setup sheets, tool
  // lists in CSV, etc.) -- VERIFY IN FUSION the exact TextFile
  // constructor signature (argument order/meaning for append-vs-
  // overwrite, supported encoding names) against this Fusion version;
  // it has varied across releases in ways this offline pass cannot
  // confirm.
  var file = new TextFile(ncPath + ".gmw.json", false, "utf-8");
  file.write(JSON.stringify(sidecar, null, 2));
  file.close();
}

function gmwPatchSha(ncPath) {
  // Re-read the completed file, split header from body at the first
  // line that isn't a GMW- comment (mirrors cnc.ParseIdentity's rule),
  // hash the body, and replace the PENDING placeholder in place.
  //
  // VERIFY IN FUSION: TextFile read-mode signature and whether it
  // exposes a "read whole file" convenience or requires a per-line
  // read loop.
  var reader = new TextFile(ncPath, true, "utf-8");
  var lines = [];
  while (!reader.isEnd()) { lines.push(reader.readln()); }
  reader.close();

  var bodyStart = 0;
  while (bodyStart < lines.length && /^\(GMW-/i.test(lines[bodyStart])) {
    bodyStart++;
  }
  var body = lines.slice(bodyStart).join("\n");
  var sha = gmwSha256Hex(body).toUpperCase(); // pure-JS impl -- see note above

  for (var i = 0; i < bodyStart; ++i) {
    if (/^\(GMW-SHA /i.test(lines[i])) {
      lines[i] = "(GMW-SHA " + sha + ")";
    }
  }

  var writer = new TextFile(ncPath, false, "utf-8");
  writer.write(lines.join("\n"));
  writer.close();
}
```

### Verify-in-Fusion checklist

Everything in this table must be confirmed against a real Fusion post
session (the post editor's debugger, or a `writeln`/console dump during a
test post) before this is trusted on a production job. None of it could be
executed or confirmed from this repo.

| API / assumption | Where used | Confidence |
|---|---|---|
| `getNumberOfSections()` / `getSection(i)` callable from `onOpen` | `gmwCollectAll` | Medium-high — standard in Autodesk sample posts |
| `section.getTool()` | `gmwCollectAll` | High — ubiquitous |
| `section.getGlobalZRange()` + its Z frame of reference | depth/`max_depth` | Medium — function likely real; sign/frame convention unverified |
| `tool.guid` | sidecar `guid` | **Unconfirmed** — same open question as `PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md` §A |
| `tool.comment` / `tool.description` | sidecar `description` | Medium (`comment`) / low (`description`) |
| `getToolTypeName()` | sidecar `type` | Low — usually post-local, not a kernel builtin |
| `tool.fluteLength` / `tool.bodyLength` | flute/overall length | Medium — plausible, unconfirmed on this Fusion version |
| stickout-length derivation | `stickout_length` | **Unconfirmed** — placeholder only, needs a real holder-geometry derivation |
| `tool.holder.productId` / `tool.holder.comment` | holder id/description | Low |
| `tool.feedCutting` / `tool.spindleRPM` | feed/speed | Low — feed in particular has no single stable property |
| `section.hasParameter("operation-comment")` | operation name | Medium — common in sample posts |
| `section.workOffset` → `"G" + (53 + n)` mapping | `wcs` | Medium — standard Haas numbering, mapping itself unconfirmed |
| `section.getParameter("operation:stockToLeaveStock")` | `stock_to_leave` | Low — varies by strategy |
| SHA-256 availability in the post kernel's JS | `GMW-SHA` | **Unconfirmed** — likely needs an embedded pure-JS implementation |
| `new Date().toISOString()` | `GMW-POSTED` | Medium-high — plain ES5, `Date` itself is known to work in this kernel |
| `getOutputPath()` | sidecar/patch file path | Medium — common name, unconfirmed on this post |
| `TextFile` constructor signature + `FileSystem` helpers | sidecar write, SHA patch | Medium — pattern is real and commonly used for setup sheets; exact signature unconfirmed |

## 4. Go: parsing, sidecar loading, reconciliation

`cnc/program_identity.go`:

- `ParseIdentity(nc []byte) (*Identity, bool)` — the bool is false only when
  line 1 isn't `(GMW-ID ...)`. Malformed individual fields degrade to zero
  values rather than invalidating the whole block. Also computes the sha256
  of the body (everything after the contiguous `GMW-` run) and reports
  `SHAMatch`/`ComputedSHA256` against the header's `GMW-SHA`.
- `SidecarPath(ncPath string) string` — `ncPath + ".gmw.json"`, resolved
  entirely inside `cnc/` so `http/` needs no change to support this.
- `LoadSidecar(path string) (*Sidecar, error)` — reads + JSON-decodes; a
  missing file returns the underlying `os.ErrNotExist`-wrapped error so
  callers can distinguish "no sidecar" from "corrupt sidecar."
- `ReconcileTools(sidecar *Sidecar, table *ToolTable, cfg ReconcileConfig) *ToolReconcileReport`:
  for each sidecar tool —
  1. **Pocket status.** If the tool's expected pocket (`t_number`) is
     loaded (non-empty, no read errors) in `table`, status is `match`.
     Otherwise, search every *other* pocket for one whose effective
     diameter is within `cfg.DiameterTolerance` of the sidecar's expected
     diameter — the only physical signature a bare Haas tool-table read
     carries, since it has no GUID/description of its own. A hit is
     `moved` (with `ActualPocket` set); no hit is `missing` ("swap
     needed").
  2. **Diameter warning**, independent of pocket status: if the resolved
     pocket's actual diameter differs from the sidecar's expected diameter
     by more than `cfg.DiameterTolerance`, flag a warning. This catches
     "a similar-but-wrong tool is sitting in the right slot" — a case a
     pure presence check would call `match`.
  3. **Length check.** `RequiredReach = max_depth + cfg.ClearanceMargin`
     (default clearance `0.100"` — generous enough to cover fixture lips
     and touch-off error without being so tight it fires on every job).
     Flags `length_insufficient` when the required reach exceeds either
     the sidecar's `flute_length` or `stickout_length` (this check needs
     no machine data at all — it's a sanity check on the toolpath/tool
     pairing itself), and separately when the resolved pocket's machine
     geometry length offset is less than the required reach (this one
     *does* need the table — it catches "the physical tool loaded is
     shorter than what the program needs," e.g. a shorter regrind swapped
     in without updating the post).

`cnc/preflight.go`: `Preflight` gains one new field, `Identity
*IdentityReport `json:"identity,omitempty"``, populated only when the NC
file's header parses (`ParseIdentity` returns `ok`). No existing field
changes shape. When the header is absent, `Identity` is nil and preflight
behaves exactly as before — the T-number/tool-table-only path in `Tools` is
untouched either way. When the header is present but the sidecar file is
missing or fails to parse, `Identity.SidecarFound` is false and
`Identity.Tools` is empty; the legacy `Tools` list still carries the
T-number check, so the operator isn't left with nothing. The sidecar path
is resolved internally via `SidecarPath(absPath)` — `http/` passes the same
`absPath` it always has.
