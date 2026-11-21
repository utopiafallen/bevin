// Node test for the DOM-free core of web/static/markdown.js
// (parseBlocks + stableCut — the streaming "settled boundary" logic).
// Run: node web/markdown_test.js
"use strict";
var MD = require("./static/markdown.js");
var failures = 0;

function check(name, cond, detail) {
  if (cond) { console.log("  ok  " + name); }
  else { failures++; console.log("FAIL  " + name + (detail ? " — " + detail : "")); }
}
function blocksOf(text) {
  return MD.parseBlocks(text).map(function (b) {
    return b.type.charAt(0) + (b.complete ? "" : "*");
  }).join(",");
}
// cut(text) — the settled boundary of `text` from 0.
function cut(text) { return MD.stableCut(text, 0); }

console.log("parseBlocks: completeness");
check("paragraph at EOF is incomplete", blocksOf("hello") === "p*", blocksOf("hello"));
check("paragraph + blank line is complete", blocksOf("hello\n\n") === "p", blocksOf("hello\n\n"));
check("paragraph terminated by heading", blocksOf("hello\n# H\n\n") === "p,h", blocksOf("hello\n# H\n\n"));
check("heading un-newline-terminated is incomplete", blocksOf("# H") === "h*", blocksOf("# H"));
check("heading newline-terminated is complete", blocksOf("# H\n") === "h", blocksOf("# H\n"));
check("hr un-newline-terminated is incomplete", blocksOf("---") === "h*", blocksOf("---"));
check("closed fence is complete", blocksOf("```\ncode\n```\n") === "c", blocksOf("```\ncode\n```\n"));
check("open fence is incomplete", blocksOf("```\ncode") === "c*", blocksOf("```\ncode"));
check("open fence with partial close is incomplete", blocksOf("```\ncode\n``") === "c*", blocksOf("```\ncode\n``"));
check("fence close must match char", blocksOf("```\ncode\n~~~\n") === "c*", blocksOf("```\ncode\n~~~\n"));
check("fence close must be long enough", blocksOf("````\ncode\n```\n") === "c*", blocksOf("````\ncode\n```\n"));
check("quote at EOF is incomplete", blocksOf("> a") === "q*", blocksOf("> a"));
check("quote terminated by text is complete", blocksOf("> a\nb\n\n") === "q,p", blocksOf("> a\nb\n\n"));
check("blank-then-new-quote makes first complete", blocksOf("> a\n\n> b") === "q,q*", blocksOf("> a\n\n> b"));
check("list at EOF is incomplete", blocksOf("- a") === "l*", blocksOf("- a"));
check("list terminated by blank+text is complete", blocksOf("- a\n\ntext\n") === "l,p*", blocksOf("- a\n\ntext\n"));
check("list terminated by blank+EOF is incomplete (item may follow)", blocksOf("- a\n\n") === "l*", blocksOf("- a\n\n"));
check("list terminated by blank+same-level item stays one incomplete list", blocksOf("- a\n\n- b") === "l*", blocksOf("- a\n\n- b"));
check("list terminated by heading is complete", blocksOf("- a\n# H\n") === "l,h", blocksOf("- a\n# H\n"));
check("list terminated by less-indented item: two lists", blocksOf("  - a\n- b\n") === "l,l*", blocksOf("  - a\n- b\n"));
check("nested list at EOF is one incomplete list", blocksOf("- a\n  - sub") === "l*", blocksOf("- a\n  - sub"));
check("paragraph then list (no blank) = p + list", blocksOf("intro\n- a\n") === "p,l*", blocksOf("intro\n- a\n"));
check("empty text", MD.parseBlocks("").length === 0);
check("whitespace only", MD.parseBlocks(" \n \n").length === 0);

console.log("parseBlocks: shape");
(function () {
  var t = "```\nfoo\nbar\n```\ntext";
  var b = MD.parseBlocks(t);
  check("fence body bounds", b[0].type === "code" &&
    t.slice(b[0].bodyStart, b[0].bodyEnd) === "foo\nbar",
    JSON.stringify(t.slice(b[0].bodyStart, b[0].bodyEnd)));
  var t2 = "  - one\n  - two";
  var l = MD.parseBlocks(t2)[0];
  check("list indent preserved in text", l.ordered === false && l.text === "  - one\n  - two", l.text);
  var t3 = "1. a\n2. b";
  check("ordered list detected", MD.parseBlocks(t3)[0].ordered === true);
})();

console.log("parseBlocks: tables");
check("table terminated by blank line is complete",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |\n\n") === "t",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |\n\n"));
check("table terminated by newline-terminated text is complete",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |\nnext\n") === "t,p*",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |\nnext\n"));
check("table terminated by unterminated text is incomplete (the line may grow into a row)",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |\nnext") === "t*,p*",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |\nnext"));
check("table at EOF is incomplete",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |") === "t*",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |"));
check("table at EOF with trailing newline is still incomplete (rows may follow)",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |\n") === "t*",
  blocksOf("| a | b |\n| - | - |\n| 1 | 2 |\n"));
check("header + unterminated delimiter is an incomplete table (tail re-parses as it grows)",
  blocksOf("| a | b |\n| - | - |") === "t*",
  blocksOf("| a | b |\n| - | - |"));
check("pipe-less line ends the table",
  blocksOf("| a | b |\n| - | - |\nplain\n") === "t,p*",
  blocksOf("| a | b |\n| - | - |\nplain\n"));
check("paragraph breaks before a table header",
  blocksOf("intro\n| a | b |\n| - | - |\n| 1 | 2 |\n\n") === "p,t",
  blocksOf("intro\n| a | b |\n| - | - |\n| 1 | 2 |\n\n"));
check("single-column delimiter is NOT a table (paragraph + hr stays)",
  blocksOf("text\n---\n\n") === "p,h",
  blocksOf("text\n---\n\n"));
check("header without pipes is not a table",
  blocksOf("a b\n| - | - |\n") === "p*",
  blocksOf("a b\n| - | - |\n"));
check("table text keeps all rows", (function () {
  var b = MD.parseBlocks("| a | b |\n| - | - |\n| 1 | 2 |\n| 3 | 4 |\n\n")[0];
  return b.type === "table" &&
    b.text === "| a | b |\n| - | - |\n| 1 | 2 |\n| 3 | 4 |\n",
    JSON.stringify(b.text);
})());

console.log("stableCut: tables stream in the tail");
check("cut holds while a table streams (stays in the tail)",
  cut("| a | b |\n| - | - |\n| 1 | 2 |") === 0,
  cut("| a | b |\n| - | - |\n| 1 | 2 |"));
check("cut settles the table once terminated",
  cut("text\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\n") === "text\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\n".length,
  cut("text\n\n| a | b |\n| - | - |\n| 1 | 2 |\n\n"));

console.log("settled-chunk re-parse (sentinel)");
(function () {
  // A list whose completeness was decided by a line past the chunk end:
  // without the sentinel the chunk re-parses incomplete; with it (as
  // render() applies) every chunk block is complete.
  var chunk = "- a\n- b\n\n";
  var noSent = MD.parseBlocks(chunk);
  var withSent = MD.parseBlocks(chunk + "\n");
  check("list chunk alone is incomplete", noSent.length === 1 && !noSent[0].complete);
  check("list chunk + sentinel is complete", withSent.length === 1 && withSent[0].complete);
})();

console.log("stableCut: streaming boundaries");
check("cut of 'hello' = 0 (tail)", cut("hello") === 0, cut("hello"));
check("cut of 'hello\\n\\n' = end", cut("hello\n\n") === 7, cut("hello\n\n"));
check("cut advances only at block boundaries",
  cut("hello\n\nwor") === 7, cut("hello\n\nwor"));
check("cut holds while tail grows",
  cut("hello\n\nworld") === cut("hello\n\nworld x"),
  cut("hello\n\nworld") + " vs " + cut("hello\n\nworld x"));
check("cut of closed fence message = end",
  cut("```\ncode\n```\n") === "```\ncode\n```\n".length,
  cut("```\ncode\n```\n"));
check("open fence keeps whole message in tail",
  cut("text\n\n```\nline") === 6, cut("text\n\n```\nline"));
check("fence closing mid-tail settles everything",
  cut("text\n\n```\nline\n```\n") === "text\n\n```\nline\n```\n".length);
check("cut(from) scans only the tail",
  MD.stableCut("AAA\n\nBBB", 5) === 5, MD.stableCut("AAA\n\nBBB", 5));
check("cut(from) settles tail blocks",
  MD.stableCut("AAA\n\nBBB\n\nCCC", 5) === 10, MD.stableCut("AAA\n\nBBB\n\nCCC", 5));
check("list at EOF stays in tail", cut("- a\n- b") === 0, cut("- a\n- b"));
check("two blocks, first settled", cut("# H\nmore text") === 4, cut("# H\nmore text"));
check("monotonic: more text never moves cut backwards", (function () {
  var src = "# Title\n\npara one\n\n- a\n- b\n\npara two more text";
  var c0 = 0, ok = true;
  for (var i = 1; i <= src.length; i++) {
    var c = MD.stableCut(src.slice(0, i), c0);
    if (c < c0) { ok = false; break; }
    c0 = c;
  }
  return ok;
})());

console.log("stableCut: settle-then-render consistency");
// Simulate the streaming harness: feed the text one character at a time,
// advancing the cut via stableCut, and verify every settled chunk re-parses
// into COMPLETE blocks only (so settled DOM never needs revisiting).
(function () {
  var src = [
    "Here is a **plan**:\n",
    "\n",
    "1. First step with `inline code`\n",
    "2. Second step\n",
    "   - a nested note\n",
    "\n",
    "```python\n",
    "def f():\n",
    "    return 42\n",
    "```\n",
    "\n",
    "> a quote line\n",
    "> second line\n",
    "\n",
    "| col A | col B | col C |\n",
    "| :--- | ---: | :---: |\n",
    "| **one** | `two` | plain |\n",
    "| [link](https://example.com/c) | 4 | \\\\| escaped-ish |\n",
    "\n",
    "Final paragraph. See [docs](https://example.com/a?b=1). See also https://example.com/bare.\n",
    "\n",
    "---\n",
    "\n",
    "tail still streaming"
  ].join("");
  var cutPos = 0, ok = true, detail = "";
  for (var i = 1; i <= src.length; i++) {
    var prefix = src.slice(0, i);
    var c = MD.stableCut(prefix, cutPos);
    if (c < cutPos) { ok = false; detail = "cut went backwards at i=" + i; break; }
    if (c > cutPos) {
      var chunk = prefix.slice(cutPos, c);
      var bs = MD.parseBlocks(chunk + "\n"); // same sentinel render() uses
      for (var b = 0; b < bs.length; b++) {
        if (!bs[b].complete) {
          ok = false;
          detail = "incomplete block " + bs[b].type + " settled at i=" + i +
            " chunk=" + JSON.stringify(chunk);
          break;
        }
      }
      cutPos = c;
      if (!ok) break;
    }
  }
  check("every settled chunk is fully complete (char-by-char stream)", ok, detail);
  // And the final full text settles completely (it ends with a blank-line
  // terminated block, except the last paragraph which stays in the tail).
  var finalCut = cut(src);
  var tail = src.slice(finalCut);
  check("final tail is the last paragraph",
    tail === "tail still streaming", JSON.stringify(tail));
})();

// ---- inline rendering (the marker branches) against a minimal fake DOM.
// render() needs only createElement/createTextNode/createDocumentFragment +
// appendChild/insertBefore/textContent/className/style/href — enough to
// serialize the tree and assert on the produced tags.
(function () {
  console.log("parseInline: marker branches (fake DOM)");
  function makeDoc() {
    function mk(tag) {
      var el = {
        tag: tag, children: [], className: "", style: {},
        href: "", target: "", rel: "", open: false,
        appendChild: function (c) { this.children.push(c); return c; },
        insertBefore: function (c, ref) {
          if (c.isFrag) { // DOM semantics: a fragment spreads its children
            var i = ref ? this.children.indexOf(ref) : this.children.length;
            var arr = c.children; c.children = [];
            this.children.splice(i, 0, ...arr);
          } else {
            var j = ref ? this.children.indexOf(ref) : -1;
            if (j >= 0) this.children.splice(j, 0, c); else this.children.push(c);
          }
          return c;
        },
        get textContent() {
          return this.children.map(function (c) {
            return c.tag === "#text" ? c.text : c.textContent;
          }).join("");
        },
        set textContent(t) { this.children = [{ tag: "#text", text: String(t) }]; }
      };
      return el;
    }
    return {
      createElement: mk,
      createTextNode: function (t) { return { tag: "#text", text: String(t) }; },
      createDocumentFragment: function () { var f = mk("#frag"); f.isFrag = true; return f; }
    };
  }
  function ser(n) {
    if (n.tag === "#text") return n.text;
    var inner = n.children.map(ser).join("");
    if (n.tag === "#frag") return inner;
    return "<" + n.tag + ">" + inner + "</" + n.tag + ">";
  }
  function renderInline(text) {
    var doc = makeDoc();
    var container = doc.createElement("div");
    container.ownerDocument = doc;
    MD.render(text, container, null, false);
    return ser(container);
  }

  // The live bug (used-car research): the model writes approximate prices
  // as ~$21.7k. A lone ~ must NEVER pair (GFM strike is ~~ only) — before
  // the fix, consecutive ~$ prices struck out the models + prices between.
  var prices = "Hyundai Venue ~$21.7k (cheapest new car on sale)\n- Kia K4 ~$23.5k · Nissan Sentra ~$23.8k · Corolla ~$24.3k\n- Mazda3 ~$24.5–26k · Civic ~$24.7–25.9k";
  var rp = renderInline(prices);
  check("lone ~ prices stay literal", rp.indexOf("<del>") < 0, rp);
  check("all ~$ amounts survive verbatim",
    ["~$21.7k", "~$23.5k", "~$23.8k", "~$24.3k", "~$24.5–26k", "~$24.7–25.9k"]
      .every(function (s) { return rp.indexOf(s) >= 0; }), rp);

  check("~~strike~~ still renders <del>",
    renderInline("before ~~struck~~ after").indexOf("<del>struck</del>") >= 0,
    renderInline("before ~~struck~~ after"));
  check("~~ with intraword ~s still pairs on the double run",
    renderInline("a ~~b~~ c").indexOf("<del>b</del>") >= 0);
  check("unterminated ~~ stays literal mid-stream",
    renderInline("only ~~ open").indexOf("~~") >= 0);
  check("*em* and **bold** unaffected",
    renderInline("*em* and **bold**") === "<div><p><em>em</em> and <strong>bold</strong></p></div>",
    renderInline("*em* and **bold**"));
  check("lone ~ with no pair at all stays literal",
    renderInline("about ~$16k total") === "<div><p>about ~$16k total</p></div>",
    renderInline("about ~$16k total"));
})();

console.log(failures === 0 ? "ALL PASS" : failures + " FAILURES");
process.exit(failures === 0 ? 0 : 1);
