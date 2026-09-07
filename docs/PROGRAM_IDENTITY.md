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
| `GMW-SHA` | sha256 (hex) of the file **body**, i.e. everything after the header block, when the post can compute it — otherwise the literal string `PENDING` | 64 hex chars, or `PENDING` |

`GMW-SHA` is written as `PENDING` by the `.cps` snippet in §3, not a real hash — the
post kernel's JS sandbox has no hash/crypto primitive to compute it with (verified
against the Autodesk Post Processor Reference's full 57-class index: no `Hash`,
`Crc`, `Md5`, or `Sha` class exists). `cnc.ParseIdentity` already computes the real
sha256 of the body independently in Go (`ComputedSHA256`, unaffected by whatever the
header says) and is the source of truth; `SHAMatch` will read `false` whenever the
header says `PENDING`, which is expected, not a corruption signal. See §3's SHA-256
note for the full tradeoff.

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
  `ReconcileTools` actually uses (see §4). `getGlobalZRange()` is confirmed
  (Post Processor Reference, `classSection.html`: "Returns the Z-coordinate
  range of the toolpath of the section in the global coordinate system.")
  and its use to build a per-tool Z range is exactly what the stock Haas
  Next Generation post itself does in `writeProgramHeader()` (called from
  `onOpen`) to print `ZMIN=` in the tool-list comment. What the reference
  does **not** say is whether "global coordinate system" Z=0 lands on stock
  top for a given CAM setup's WCS — that's a property of how each job's
  Fusion setup is built, not of the kernel, so it genuinely can't be
  confirmed from documentation alone. **Verify in Fusion**: confirm Z=0 is
  stock top for the setups this post runs before trusting `max_depth =
  -min_z` blindly. This is the one item in this document that stays a
  verify-in-Fusion checkbox after cross-checking every other field against
  the reference and the shipped Haas post source (see §3).
- `flute_length` limits how deep the tool can cut at all; `stickout_length`
  (tool exposed below the holder) limits how deep before the *holder*
  collides with the part or a fixture. Both are needed because a reach can
  clear the flute but still crash the holder into a clamp. `stickout_length`
  is now sourced from the same parameter the stock Haas post itself reads
  for exactly this purpose — see the `getBodyLength()` note in §3.
- `stock_to_leave` is per-operation because roughing and finishing passes
  on the same tool normally use different values.
- `guid` is **not obtainable**. The Post Processor Reference's `Tool` class
  page lists every public member (78 methods, 60 attributes, confirmed by
  reading the full member table) and none of them is a GUID/unique-id
  accessor other than `getToolId()` — which is documented as "internal
  (unique) id of the tool **in a Fusion/Inventor document**," not a
  tool-library GUID, and is not the identifier
  `docs/PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md` section A is asking for.
  There is also zero use of any `guid`-like accessor anywhere in the
  current shipped Haas Next Generation post source. The field stays in the
  schema (as optional — see below) for forward compatibility, but the
  `.cps` snippet in §3 always writes it as `""`; this is resolved, not
  unconfirmed. `ReconcileTools` does not require it.
- `sha256` is written by the post as `PENDING`, not a real hash — see the
  header field table above and §3's SHA-256 note for why, and why that's
  safe. It is no longer `required` below for that reason, and its pattern
  accepts `PENDING` alongside a real hex digest.

### JSON Schema (draft-07)

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "GMW program identity sidecar",
  "type": "object",
  "required": ["schema_version", "job", "tools"],
  "properties": {
    "schema_version": {"type": "integer", "minimum": 1},
    "job": {"type": "string"},
    "operation": {"type": "string"},
    "part": {"type": "string"},
    "post_name": {"type": "string"},
    "post_version": {"type": "string"},
    "posted_at": {"type": "string", "format": "date-time"},
    "tool_count": {"type": "integer", "minimum": 0},
    "sha256": {"type": "string", "pattern": "^([0-9a-fA-F]{64}|PENDING)$"},
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

**Verification note:** every API name below has been checked against two
primary sources: Autodesk's Post Processor Reference
(`cam.autodesk.com/posts/reference/`, fetched class-by-class — `Tool`,
`Section`, `PostProcessor`, `Holder`, `TextFile`, `FileSystem`,
`ToolTable`, `Base64`, `Date`, plus the full 57-class index and the
`entry_functions.html` page) and the actual shipped Haas Next Generation
post source (`cam.autodesk.com/posts/download.php?name=haas next
generation`, r44241, dated 2026-09-02 — the same post Jason is pasting
this into). Where the two sources agreed, or the shipped post uses the
exact call, that's marked high confidence. The only item that could not be
resolved this way is the Z=0/stock-top convention noted under
`gmwCollectAll` below, because that's a property of each CAM setup, not of
the kernel or the post. The full list is repeated as a checklist at the
end of this section — it is now three lines long.

### Properties (post configuration, set once per machine/post)

The two-object `properties.x = value` / `propertyDefinitions.x = {...}`
pattern in an earlier draft of this doc is the **old** style. The current
shipped Haas post (r44241) defines every property as a single object —
e.g. `properties.writeTools = {title: ..., description: ..., group:
"formats", type: "boolean", value: true, scope: "post"};` — with zero uses
of `propertyDefinitions` anywhere in the file. Use that shape:

```js
properties.gmwJob = {
  title: "GMW Job", description: "Carbon jobReadableId (e.g. J000020)",
  group: "gmw", type: "string", value: "", scope: "post"
};
properties.gmwOp = {
  title: "GMW Operation", description: "Carbon operation id/seq (e.g. OP10)",
  group: "gmw", type: "string", value: "", scope: "post"
};
properties.gmwPart = {
  title: "GMW Part/Item", description: "Part or item id",
  group: "gmw", type: "string", value: "", scope: "post"
};
```

Verified against the shipped post's own `properties.writeMachine` /
`properties.writeTools` / `properties.useParametricFeed` definitions
(`type: "boolean"` confirmed live; `"string"` is the same mechanism with
the standard alternate type — every stock Autodesk post uses string-typed
properties this way, e.g. program-name/post-name fields).

### Collecting tools and operation depths

The whole toolpath plan is known before the first line of G-code is
written, so this pulls tool + depth data with one pass over every section
in `onOpen`, rather than accumulating incrementally in `onSection`. This
is not a guess: the shipped Haas post does exactly this. Its `onOpen`
(line 987 of the post source) calls `writeProgramHeader()`, which loops
`getNumberOfSections()` / `getSection(i)` and calls
`section.getGlobalZRange()` per section to build a per-tool Z range for
the `ZMIN=` field of its own tool-list comment — the identical pattern
used here for `gmwCollectAll`.

```js
var gmwTools = {};   // keyed by tool.number, deduped across operations
var gmwOps   = [];   // one entry per operation/section, in program order

function gmwCollectAll() {
  var n = getNumberOfSections();
  for (var i = 0; i < n; ++i) {
    var section = getSection(i);
    var tool = section.getTool();
    if (!tool) { continue; }

    // section.getGlobalZRange() -- confirmed: Post Processor Reference
    // (classSection.html) documents it, and the shipped Haas post calls
    // it exactly like this in writeProgramHeader(). VERIFY IN FUSION:
    // the reference only says the range is "in the global coordinate
    // system" -- it does not say Z=0 is stock top. That's a property of
    // this post's CAM setups, not of the kernel, so it genuinely can't
    // be confirmed from documentation. If a setup's WCS doesn't put Z=0
    // at stock top, max_depth needs a different derivation.
    var zRange = section.getGlobalZRange();
    var minZ = zRange ? zRange.getMinimum() : undefined;

    var key = tool.number;
    if (!gmwTools[key]) {
      gmwTools[key] = {
        t_number: tool.number,
        // Confirmed absent: the Tool class reference lists every public
        // member (78 methods, 60 attributes) and none is a tool-library
        // GUID accessor. getToolId() exists but is documented as the
        // "internal (unique) id of the tool in a Fusion/Inventor
        // document" -- a different id, not what
        // PROGRAM_DELIVERY_AND_LIBRARY_SYNC_TODO.md section A needs.
        // Zero use of any guid-like accessor in the shipped Haas post
        // either. Left "" -- this is resolved, not unconfirmed.
        guid: "",
        // Confirmed: tool.description is used directly (not through a
        // getter) in the shipped post's writeProgramHeader():
        // `tool.description.toUpperCase()`, on a Tool returned by
        // ToolTable.getTool() -- documented to return the same Tool
        // class as section.getTool(). tool.comment is also a documented
        // attribute (getComment()/comment both listed). Chain both.
        description: tool.description || tool.comment || "",
        // Confirmed: getToolTypeName() is a PostProcessor global method
        // (documented on classPostProcessor.html, not post-local), and
        // the shipped Haas post calls it exactly this way:
        // `getToolTypeName(tool.type)` (the integer type, not the tool).
        type: getToolTypeName(tool.type),
        diameter: tool.diameter,
        // Confirmed attributes on Tool (classTool.html): fluteLength,
        // overallLength. NOTE: an earlier draft used tool.bodyLength for
        // overall_length -- bodyLength ("the body length") and
        // overallLength ("the entire length of the tool") are two
        // different documented attributes; this was a bug, fixed here.
        flute_length: tool.fluteLength,
        overall_length: tool.overallLength,
        // Confirmed: this is not a Tool property at all -- it's the
        // exact reach the shipped Haas post itself computes for tool-
        // length-compensation purposes, in its own getBodyLength(tool)
        // helper: `section.getParameter("operation:tool_assemblyGaugeLength",
        // tool.bodyLength + tool.holderLength)` for Fusion, falling back
        // to `operation:tool_overallLength` for "legacy products". Reused
        // verbatim (tool.holderLength is a confirmed Tool attribute).
        stickout_length: section.getParameter("operation:tool_assemblyGaugeLength",
          section.getParameter("operation:tool_overallLength", tool.bodyLength + tool.holderLength)),
        // Confirmed: the Holder class itself (classHolder.html) has NO
        // productId/comment/description/vendor -- only dimensional
        // geometry (getMaximumDiameter, getTotalLength, getGaugeLength,
        // per-section getDiameter/getLength). Those string fields live
        // directly on Tool instead: getHolderProductId(),
        // getHolderComment(), getHolderDescription() -- all confirmed
        // Tool methods. No tool.holder.x sub-object access needed.
        holder_id: tool.getHolderProductId() || "",
        holder_description: tool.getHolderComment() || tool.getHolderDescription() || "",
        min_z: minZ,
        max_depth: (minZ !== undefined) ? -minZ : undefined,
        // Confirmed: tool.feedCutting does not exist anywhere in the Tool
        // class reference or the shipped post. Real cutting feed is a
        // per-section parameter, `operation:tool_feedCutting`, read via
        // section.getParameter() throughout the shipped post's feed-
        // context code (getFeed()/initializeParametricFeeds()).
        // tool.spindleRPM is a confirmed Tool attribute (also
        // getSpindleRPM()).
        feed: section.getParameter("operation:tool_feedCutting", 0),
        speed: tool.spindleRPM
      };
    } else if (minZ !== undefined) {
      // Reused tool -- widen the depth range across every operation.
      if (gmwTools[key].min_z === undefined || minZ < gmwTools[key].min_z) {
        gmwTools[key].min_z = minZ;
        gmwTools[key].max_depth = -minZ;
      }
    }

    gmwOps.push({
      // Confirmed: "operation-comment" is used verbatim in the shipped
      // post (`getParameter("operation-comment", "")`), and
      // hasParameter/getParameter are documented Section methods.
      name: section.getParameter("operation-comment", ""),
      tool: tool.number,
      // Confirmed, and better than the earlier "G" + (53 + n) guess:
      // section.wcs is itself a documented String attribute ("The WCS.")
      // that already carries the formatted G-code word -- the shipped
      // post writes it directly with `writeBlock(section.wcs)` in
      // writeWCS(). No numbering formula needed at all.
      wcs: section.wcs || "",
      // Confirmed: the real parameter key is "operation:stockToLeave"
      // (plus a separate "operation:verticalStockToLeave" for vertical
      // passes), used throughout the shipped post's smoothing-level
      // logic. The earlier draft's "operation:stockToLeaveStock" does
      // not appear anywhere in the reference or the shipped post -- it
      // was wrong.
      stock_to_leave: section.getParameter("operation:stockToLeave", 0)
    });
  }
}
```

### `onOpen` — call the collector, write the header

```js
function onOpen() {
  // ... existing onOpen body stays first ...

  gmwCollectAll();

  writeComment("GMW-ID V1");
  writeComment("GMW-JOB " + properties.gmwJob + " " + properties.gmwOp);
  writeComment("GMW-PART " + properties.gmwPart);
  writeComment("GMW-POST HAAS-NGC V1.0.3");
  // Date is a documented kernel class ("JavaScript date class" per
  // classDate.html) and JSON.stringify is confirmed in active use in the
  // shipped post (`JSON.parse(JSON.stringify(state))`), so this is a
  // reasonably modern ES5+ environment; toISOString() is standard ES5.
  // Medium-high confidence -- the one Date-specific method call that
  // isn't independently attested by name in the shipped post's source.
  writeComment("GMW-POSTED " + new Date().toISOString());
  writeComment("GMW-TOOLS " + Object.keys(gmwTools).length);
  // See the SHA-256 note below -- this is intentionally never patched to
  // a real hash by the post.
  writeComment("GMW-SHA PENDING");
}
```

`writeComment` is defined by the shipped Haas post itself (not a kernel
builtin — confirmed by reading its definition at line 2061 of the post
source), wrapping the kernel's `writeln`. It already uppercases and
filters to a permitted character set via `formatComment()`/
`settings.comments.permittedCommentChars` before output, so the header
stays Haas-safe as long as this post's `settings.comments` block hasn't
been narrowed below the field's `A-Z 0-9 . , : = + - _ /` character set —
worth a quick glance at `settings.comments.permittedCommentChars` in
Jason's post, but not a Fusion-runtime unknown.

### SHA-256: moved to the daemon, not computed by the post

The post kernel's JS sandbox has no hash primitive. This is not a "not
found in the docs I happened to fetch" gap: the Post Processor Reference's
full class index lists all 57 documented classes (`Array` through
`ZipFile`, including `Base64` for base64 — but no `Hash`, `Crc`, `Md5`, or
`Sha` class), and the shipped Haas post has zero uses of `sha`, `crc32`,
`md5`, or `hash` anywhere in ~5,100 lines. There is no built-in to lean on
and nothing to embed a fallback around.

The original design also planned to read the finished NC file back with
`TextFile` in `onClose`, hash the body, and patch the placeholder in
place. That compounds the problem: `TextFile`'s documented surface is
exactly `TextFile(path, write, encoding)`, `isOpen()`, `readln()`,
`write()`, `writeln()`, `close()` — there is no `isEnd()`/EOF-detection
method and no whole-file read. A `while (!reader.isEnd())` loop (as an
earlier draft had) calls a method that doesn't exist. Looping `readln()`
until it returns something falsy is not documented behavior either, so a
read-back-and-patch design would be stacking two unconfirmed assumptions
(a hash algorithm and a file-read-to-EOF idiom) rather than one.

**Decision: move hashing to the daemon side**, per the tradeoff this task
asked to make explicit:

- The post writes `GMW-SHA PENDING` in the header (never patched) and
  `"sha256": "PENDING"` in the sidecar (schema updated in §2 to allow this
  — the field is no longer `required` and its pattern accepts `PENDING`
  alongside a real digest).
- `cnc.ParseIdentity` already computes the real sha256 of the body
  independently in Go (`ComputedSHA256`, §4) — this doesn't depend on
  anything the post writes, so nothing about Go's ability to detect
  "same body, re-posted" is lost.
- The cost: a human glancing at the raw `.nc` file can no longer eyeball
  "was this re-posted unchanged" from the header alone — that check now
  requires running preflight (`ComputedSHA256`/`SHAMatch`), not reading a
  comment. If that visibility is ever worth restoring, the cleanest place
  is Go, not the post: `LoadSidecar`/`ParseIdentity` already parses the
  full byte content, so a future increment could have the daemon compute
  the hash on ingest and rewrite the sidecar's `sha256` field (ordinary
  Go file I/O, none of the `TextFile` EOF ambiguity above). That's future,
  optional work, not part of this change, and doesn't touch the Go code
  landed in PR #137.

```js
function onClose() {
  // ... existing onClose body stays first, so the NC file is fully
  // written before we touch it ...

  var ncPath = getOutputPath(); // confirmed PostProcessor global method
                                 // (classPostProcessor.html), and used
                                 // exactly this way (bare call) in the
                                 // shipped post, e.g.
                                 // FileSystem.getFolderPath(getOutputPath())
  gmwWriteSidecar(ncPath);
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
    sha256: "PENDING", // see the SHA-256 note above
    tools: toolList,
    operations: gmwOps
  };

  // TextFile(path, write, encoding) confirmed on classTextFile.html.
  // write=true means "open for writing" -- an earlier draft passed
  // `false` here, which per that same signature opens for *reading* and
  // would have silently produced an empty/unwritten sidecar. Fixed.
  var file = new TextFile(ncPath + ".gmw.json", true, "utf-8");
  file.write(JSON.stringify(sidecar, null, 2));
  file.close();
}
```

### Verify-in-Fusion checklist

Down to the items that genuinely can't be resolved from documentation or
the shipped post source alone — everything else above is confirmed
against one or both.

| API / assumption | Where used | Status |
|---|---|---|
| Z=0-is-stock-top convention for `section.getGlobalZRange()` | `max_depth` sign flip | **Verify in Fusion** — a property of each CAM setup's WCS, not of the kernel; confirm before trusting `max_depth = -min_z` |
| `new Date().toISOString()` | `GMW-POSTED` | Medium-high — `Date` is a documented kernel class and `JSON.stringify` is confirmed live in the shipped post, but `toISOString()` itself isn't independently attested by name in that source |
| `settings.comments.permittedCommentChars` in Jason's specific post build | Haas-safe header chars | Worth a one-line glance at his post's `settings.comments` block; not a kernel unknown, just a per-post config check |

Everything else in this section — `getNumberOfSections()`/`getSection(i)`
from `onOpen`, `section.getTool()`, `section.getGlobalZRange()` itself,
`tool.description`/`tool.comment`, `getToolTypeName()`,
`tool.fluteLength`/`tool.overallLength`/`tool.holderLength`, the
`operation:tool_assemblyGaugeLength`/`operation:tool_overallLength`
stickout derivation, `tool.getHolderProductId()`/`getHolderComment()`/
`getHolderDescription()`, `operation:tool_feedCutting`, `tool.spindleRPM`,
`operation-comment`, `section.wcs`, `operation:stockToLeave`,
`getOutputPath()`, and the `TextFile`/`FileSystem` signatures — is
confirmed either directly in the Post Processor Reference, by verbatim
use in the shipped Haas Next Generation post source, or both. `tool.guid`
and in-kernel SHA-256 are confirmed **absent** rather than unconfirmed
(see the notes above and in §2), which is why they're resolved rather
than listed here as open.

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
