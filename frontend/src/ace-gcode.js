// frontend/src/ace-gcode.js
// Custom G-code mode for ACE with per-axis tokens.

ace.define("ace/mode/gcode_highlight_rules", [
  "require","exports","module",
  "ace/lib/oop",
  "ace/mode/text_highlight_rules"
], function(require, exports, module) {
  "use strict";

  var oop = require("ace/lib/oop");
  var TextHighlightRules = require("ace/mode/text_highlight_rules").TextHighlightRules;

  var GcodeHighlightRules = function() {
  this.$rules = {
    start: [
      // (comment) and ;comment
      {
        token: "gcode.comment",
        regex: "\\(.*?\\)"
      },
      {
        token: "gcode.comment",
        regex: ";.*$"
      },

      // Nxx block numbers  -> white via CSS
      {
        token: "gcode.block",
        regex: "\\bN[0-9]+\\b"
      },
    // G codes: Gxx -> purple
    {
    token: "gcode.gword",
    regex: "\\bG[0-9]+(?:\\.[0-9]+)?\\b"
    },
      // M codes: Mxx -> yellow
      {
        token: "gcode.mcode",
        regex: "\\bM[0-9]+(?:\\.[0-9]+)?\\b"
      },

      // X / I parameters (orange)
    // A / X / I parameters (orange)
    {
    token: "gcode.xparam",
    regex: "\\b[AXiI][+-]?[0-9]+(?:\\.[0-9]+)?\\b"
    },

      // Y / J parameters (green)
      {
        token: "gcode.yparam",
        regex: "\\b[YjJ][+-]?[0-9]+(?:\\.[0-9]+)?\\b"
      },

      // Z / K parameters (blue)
      {
        token: "gcode.zparam",
        regex: "\\b[ZkK][+-]?[0-9]+(?:\\.[0-9]+)?\\b"
      },

      // Feed / speed / H, D, T tool/offset style codes -> teal
      {
        token: "gcode.feedspeed",
        // F123, S5000, H12, D3, T01, HCC/Hcc/hcc
        regex: "\\b(?:F|S|HCC|Hcc|hcc|H|D|T)[+-]?[0-9]*(?:\\.[0-9]+)?\\b"
      },

      // P subprogram numbers -> light blue
      {
        token: "gcode.subprog",
        regex: "\\bP[0-9]+(?:\\.[0-9]+)?\\b"
      },

      // Program markers: % and O1234
      {
        token: "gcode.marker",
        regex: "%|\\bO[0-9]+\\b"
      },

      // Fallback numeric
      {
        token: "constant.numeric",
        regex: "[+-]?[0-9]+(?:\\.[0-9]+)?\\b"
      }
    ]
  };

  this.normalizeRules();
};

  oop.inherits(GcodeHighlightRules, TextHighlightRules);
  exports.GcodeHighlightRules = GcodeHighlightRules;
});

ace.define("ace/mode/gcode", [
  "require","exports","module",
  "ace/lib/oop",
  "ace/mode/text",
  "ace/mode/gcode_highlight_rules"
], function(require, exports, module) {
  "use strict";

  var oop = require("ace/lib/oop");
  var TextMode = require("ace/mode/text").Mode;
  var GcodeHighlightRules = require("ace/mode/gcode_highlight_rules").GcodeHighlightRules;

  var Mode = function() {
    this.HighlightRules = GcodeHighlightRules;
    this.lineCommentStart = ";";
  };
  oop.inherits(Mode, TextMode);

  (function() {
    this.$id = "ace/mode/gcode";
  }).call(Mode.prototype);

  exports.Mode = Mode;
});