/* bevin — hand-rolled chat-markdown renderer. No dependencies, no
 * third-party JS.
 *
 * Security: the DOM is built ONLY with createElement/createTextNode —
 * never innerHTML — so script injection is impossible by construction.
 * Links are restricted to http(s) (javascript: and friends are inert).
 *
 * Streaming (the anti-flicker contract): parseBlocks() marks every block
 * complete/incomplete against the FULL text. A block is "complete" only
 * when a terminator is already present (blank line, next block, closed
 * fence, terminating newline) — a complete block can never change as more
 * text arrives, so the caller moves it into the settled DOM once and
 * never touches it again. Only the trailing incomplete block (the tail)
 * is re-rendered per frame. Unclosed inline markers (a `**bol` mid-
 * stream) render LITERALLY until they close — no half-bold jitter.
 *
 * Subset (deliberate, chat-first): fenced code (``` / ~~~, optional lang),
 * ATX headings (#..######), hr (three or more -, * or _ chars), blockquote
 * (>), unordered + ordered lists (nested, loose), paragraphs (single \n =
 * <br>, chat style), and inline: code spans, **bold** / __bold__, *em* /
 * _em_ (intraword _ guards), ~~strike~~, [text](url) (http(s) only), bare
 * http(s) URLs. Not supported (documented deviations, v1): setext
 * headings, tables, image syntax (renders literal), lazy blockquote
 * continuations, 4-space code indents inside list items.
 *
 * Works in the browser (global BevinMD) and in node (module.exports) for
 * testing the DOM-free parts (parseBlocks, stableCut).
 */
(function (root, factory) {
  var api = factory();
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.BevinMD = api;
})(typeof globalThis !== "undefined" ? globalThis : self, function () {
  "use strict";

  var FENCE = /^\s{0,3}(`{3,}|~{3,})\s*(\S{0,30})\s*$/;
  var FENCE_CLOSE = /^\s{0,3}(`{3,}|~{3,})\s*$/;
  var HEADING = /^\s{0,3}(#{1,6})\s+(.*)$/;
  var HR = /^\s{0,3}((\*\s*){3,}|(-\s*){3,}|(_\s*){3,})$/;
  var QUOTE = /^\s{0,3}>\s?(.*)$/;
  var LIST = /^\s{0,3}([-*+]|\d{1,9}[.)])\s+(.*)$/;

  function indentOf(line) {
    var m = line.match(/^\s*/);
    return m ? m[0].length : 0;
  }

  // A line that OPENS a new block (used to terminate paragraphs).
  function isBlockStart(line) {
    return FENCE.test(line) || HEADING.test(line) || HR.test(line) ||
      QUOTE.test(line) || LIST.test(line);
  }

  // parseBlocks(text) -> [{start, end, type, complete, ...}]
  // start/end are character offsets into text (end exclusive). `complete`
  // is true only when the block is provably final — the terminator is
  // already in `text` — so it can be rendered once and never revisited.
  function parseBlocks(text) {
    var blocks = [];
    var n = text.length;
    var pos = 0;
    while (pos < n) {
      var lineEnd = text.indexOf("\n", pos);
      if (lineEnd < 0) lineEnd = n;
      var line = text.slice(pos, lineEnd);

      if (line.trim() === "") { pos = lineEnd + 1; continue; } // separators

      var m = line.match(FENCE);
      if (m) {
        var fenceCh = m[1][0], minLen = m[1].length, lang = m[2] || "";
        var bodyStart = lineEnd + 1;
        var p = bodyStart, closeLine = -1;
        while (p < n) {
          var le = text.indexOf("\n", p); if (le < 0) le = n;
          var cm = text.slice(p, le).match(FENCE_CLOSE);
          if (cm && cm[1][0] === fenceCh && cm[1].length >= minLen) { closeLine = p; break; }
          p = le + 1;
        }
        if (closeLine >= 0) {
          var bodyEnd = closeLine; // body excludes the closing fence line
          if (text.charAt(closeLine - 1) === "\n") bodyEnd = closeLine - 1;
          blocks.push({ start: pos, end: closeLine + 1, type: "code", lang: lang,
            complete: true, bodyStart: bodyStart, bodyEnd: bodyEnd });
          pos = (text.indexOf("\n", closeLine) < 0 ? n : text.indexOf("\n", closeLine) + 1);
        } else {
          blocks.push({ start: pos, end: n, type: "code", lang: lang,
            complete: false, bodyStart: bodyStart, bodyEnd: n });
          pos = n;
        }
        continue;
      }

      m = line.match(HEADING);
      if (m) {
        // Complete only once the line itself is terminated (a heading line
        // can still grow mid-stream: "# He" -> "# Hello").
        blocks.push({ start: pos, end: lineEnd + 1, type: "heading",
          level: m[1].length, text: m[2], complete: lineEnd < n });
        pos = lineEnd + 1;
        continue;
      }

      if (HR.test(line)) {
        blocks.push({ start: pos, end: lineEnd + 1, type: "hr",
          complete: lineEnd < n });
        pos = lineEnd + 1;
        continue;
      }

      if (QUOTE.test(line)) {
        var qStart = pos, q = lineEnd + 1;
        while (q < n) {
          var qle = text.indexOf("\n", q); if (qle < 0) qle = n;
          if (!QUOTE.test(text.slice(q, qle))) break;
          q = qle + 1;
        }
        // A blank line (or any non-> line) decisively ends a quote run: a
        // later `>` line is a NEW quote. Incomplete only at EOF.
        blocks.push({ start: qStart, end: q, type: "quote",
          text: text.slice(qStart, q), complete: q < n });
        pos = q;
        continue;
      }

      m = line.match(LIST);
      if (m) {
        var base = indentOf(line);
        var lStart = pos, l = lineEnd + 1;
        while (l < n) {
          var lle = text.indexOf("\n", l); if (lle < 0) lle = n;
          var ll = text.slice(l, lle);
          if (ll.trim() === "") {
            // Blank line: the list continues only if the NEXT line is a
            // sibling item, a nested item, or an indented continuation
            // (loose list). Otherwise it is the list's terminator.
            var l5 = lle + 1;
            if (l5 >= n) break;
            var l5e = text.indexOf("\n", l5); if (l5e < 0) l5e = n;
            var l5l = text.slice(l5, l5e);
            var l5m = l5l.match(LIST);
            if (!((l5m && indentOf(l5l) >= base) || /^\s{2,}\S/.test(l5l))) break;
            l = l5; // keep the list alive across the blank line
          } else {
            var lm = ll.match(LIST);
            if (lm) {
              var ind = indentOf(ll);
              if (ind < base) break;         // shallower indent: new block
              l = lle + 1; continue;         // sibling or nested item
            }
            if (/^\s{2,}\S/.test(ll)) { l = lle + 1; continue; } // continuation
            break;                           // new paragraph/block
          }
        }
        var complete = false;
        if (l < n && text.charAt(l) === "\n") {
          // Terminated by a blank line: decisive only if the line after it
          // is newline-terminated AND cannot extend the list (an
          // unterminated line could still become a list item).
          var after = l + 1;
          if (after < n) {
            var ae = text.indexOf("\n", after);
            if (ae >= 0) {
              var al = text.slice(after, ae);
              var alm = al.match(LIST);
              complete = !((alm && indentOf(al) >= base) || /^\s{2,}\S/.test(al));
            }
          }
        } else if (l < n) {
          // Terminated by a non-blank non-list line: decisive only once
          // that line itself is newline-terminated (mid-stream "2" could
          // still become "2. second").
          complete = text.indexOf("\n", l) >= 0;
        }
        blocks.push({ start: lStart, end: l, type: "list",
          ordered: !/^[-*+]/.test(m[1]), complete: complete,
          text: text.slice(lStart, l) });
        pos = l;
        continue;
      }

      // Paragraph: runs until a blank line or a line that opens a block.
      var pStart = pos, p2 = lineEnd + 1;
      while (p2 < n) {
        var ple = text.indexOf("\n", p2); if (ple < 0) ple = n;
        var pl = text.slice(p2, ple);
        if (pl.trim() === "" || isBlockStart(pl)) break;
        p2 = ple + 1;
      }
      blocks.push({ start: pStart, end: p2, type: "paragraph",
        complete: p2 < n, text: text.slice(pStart, p2) });
      pos = p2;
    }
    return blocks;
  }

  // stableCut(text, from) -> the farthest block-boundary offset >= from at
  // which everything before is settled (provably complete). The caller
  // renders [from, newCut) into the settled DOM once and re-renders only
  // [newCut, end) per frame.
  function stableCut(text, from) {
    if (from == null || from < 0) from = 0;
    if (from >= text.length) return from;
    var tail = text.slice(from);
    if (tail.trim() === "") return text.length; // whitespace settles
    var blocks = parseBlocks(tail);
    if (!blocks.length) return text.length;
    for (var i = 0; i < blocks.length; i++) {
      if (!blocks[i].complete) return from + blocks[i].start;
    }
    return text.length;
  }

  /* ---------------- DOM building (browser only) ---------------- */

  function parseInline(text, parent, doc) {
    var i = 0, n = text.length, plain = [];
    function flush() {
      if (plain.length) {
        parent.appendChild(doc.createTextNode(plain.join("")));
        plain = [];
      }
    }
    function runOf(c, j) {
      var k = j;
      while (k < n && text[k] === c) k++;
      return k - j;
    }
    function findRun(c, len, from) {
      var k = from;
      while (k < n) {
        if (text[k] === c) {
          var r = runOf(c, k);
          if (r === len) return k;
          k += r;
        } else k++;
      }
      return -1;
    }
    while (i < n) {
      var c = text[i];

      if (c === "`") {
        var rl = runOf("`", i);
        var close = findRun("`", rl, i + rl);
        if (close >= i + rl) {
          flush();
          var inner = text.slice(i + rl, close);
          if (inner.length >= 2 && inner.charAt(0) === " " &&
              inner.charAt(inner.length - 1) === " " &&
              inner.replace(/\s/g, "").length > 0) {
            inner = inner.slice(1, -1);
          }
          var code = doc.createElement("code");
          code.textContent = inner; // code spans are literal (no nesting)
          parent.appendChild(code);
          i = close + rl;
          continue;
        }
        plain.push(c); i++; continue; // unterminated mid-stream: literal
      }

      if (c === "*" || c === "_" || c === "~") {
        var rl2 = runOf(c, i);
        var isDouble = rl2 >= 2;
        var close2 = findRun(c, isDouble ? rl2 : 1, i + rl2);
        if (close2 >= i + (isDouble ? rl2 : 1)) {
          var inner2 = text.slice(i + (isDouble ? rl2 : 1), close2);
          var ok = inner2.length > 0 && inner2.replace(/\s/g, "").length > 0;
          if (ok && c === "_") {
            // intraword underscore is not emphasis (snake_case stays put)
            var before = i > 0 ? text.charAt(i - 1) : " ";
            var after = close2 + 1 < n ? text.charAt(close2 + 1) : " ";
            if (/[A-Za-z0-9]/.test(before) || /[A-Za-z0-9]/.test(after)) ok = false;
          }
          if (ok) {
            flush();
            var el = doc.createElement(c === "~" ? "del" : (isDouble ? "strong" : "em"));
            parseInline(inner2, el, doc);
            parent.appendChild(el);
            i = close2 + (isDouble ? rl2 : 1);
            continue;
          }
        }
        plain.push(c); i++; continue; // unterminated mid-stream: literal
      }

      if (c === "[") {
        var j = text.indexOf("]", i + 1);
        if (j >= 0 && text.charAt(j + 1) === "(") {
          var k = text.indexOf(")", j + 2);
          if (k > j + 1) {
            var url = text.slice(j + 2, k).replace(/^\s+|\s+$/g, "");
            if (/^https?:\/\/\S+$/i.test(url)) {
              flush();
              var a = doc.createElement("a");
              a.href = url;
              a.target = "_blank";
              a.rel = "noopener noreferrer";
              parseInline(text.slice(i + 1, j), a, doc);
              parent.appendChild(a);
              i = k + 1;
              continue;
            }
          }
        }
        plain.push(c); i++; continue;
      }

      if (c === "h" &&
          (text.slice(i, i + 8) === "https://" || text.slice(i, i + 7) === "http://") &&
          (i === 0 || /[\s(\[,>]/.test(text.charAt(i - 1)))) {
        var e = i;
        while (e < n && !/[\s<>"]/.test(text.charAt(e))) e++;
        var u = text.slice(i, e);
        for (;;) {
          var t = u.replace(/[)\]]*[.,;:!?'"]+$/, "");
          if (t === u || t.length <= 8) break;
          u = t;
        }
        if (u.length > 8) {
          flush();
          var a2 = doc.createElement("a");
          a2.href = u;
          a2.target = "_blank";
          a2.rel = "noopener noreferrer";
          a2.textContent = u;
          parent.appendChild(a2);
          i += u.length;
          continue;
        }
      }

      plain.push(c); i++;
    }
    flush();
  }

  function buildList(text, ordered, depth, frag, doc) {
    var el = doc.createElement(ordered ? "ol" : "ul");
    var lines = text.split("\n");
    var base = -1;
    for (var s = 0; s < lines.length; s++) {
      if (LIST.test(lines[s])) {
        base = indentOf(lines[s]);
        break;
      }
    }
    if (base < 0) base = 0;
    var items = [];
    var cur = null;
    for (var li2 = 0; li2 < lines.length; li2++) {
      var ln = lines[li2];
      var lm = ln.match(LIST);
      if (lm) {
        var ind = indentOf(ln);
        if (ind > base) {
          if (cur) cur.sub.push(ln);
        } else {
          cur = { content: [lm[2]], sub: [] };
          items.push(cur);
        }
      } else if (ln.trim() !== "") {
        if (!cur) cur = { content: [], sub: [] }, items.push(cur);
        if (/^\s{2,}\S/.test(ln)) cur.sub.push(ln);
        else cur.content.push(ln);
      }
    }
    for (var it = 0; it < items.length; it++) {
      var node = doc.createElement("li");
      var item = items[it];
      var first = true;
      for (var c2 = 0; c2 < item.content.length; c2++) {
        if (!first) node.appendChild(doc.createElement("br"));
        first = false;
        parseInline(item.content[c2], node, doc);
      }
      if (item.sub.length) {
        var subText = item.sub.join("\n");
        var subFirst = subText.match(LIST);
        if (depth < 4 && subFirst) {
          buildList(subText, !/^[-*+]/.test(subFirst[1]), depth + 1, node, doc);
        } else {
          // indented non-list continuation: plain lines inside the item
          var pEl = doc.createElement("p");
          var pl = subText.split("\n"), pf = true;
          for (var q2 = 0; q2 < pl.length; q2++) {
            if (pl[q2].trim() === "") continue;
            if (!pf) pEl.appendChild(doc.createElement("br"));
            pf = false;
            parseInline(pl[q2].replace(/^\s{0,3}/, ""), pEl, doc);
          }
          node.appendChild(pEl);
        }
      }
      el.appendChild(node);
    }
    frag.appendChild(el);
  }

  function buildBlock(b, frag, doc, text) {
    var el;
    switch (b.type) {
      case "code":
        el = doc.createElement("pre");
        var code = doc.createElement("code");
        if (b.lang) {
          var safe = b.lang.toLowerCase().replace(/[^a-z0-9+.#-]/g, "").slice(0, 24);
          if (safe) code.className = "lang-" + safe;
        }
        var body = text.slice(b.bodyStart, b.bodyEnd);
        if (body.charAt(body.length - 1) === "\n") body = body.slice(0, -1);
        code.textContent = body;
        el.appendChild(code);
        frag.appendChild(el);
        break;
      case "heading":
        el = doc.createElement("h" + b.level);
        parseInline(b.text, el, doc);
        frag.appendChild(el);
        break;
      case "hr":
        frag.appendChild(doc.createElement("hr"));
        break;
      case "quote":
        el = doc.createElement("blockquote");
        var ql = b.text.split("\n").map(function (l) {
          var qm = l.match(QUOTE);
          return qm ? qm[1] : "";
        });
        var qf = true;
        for (var qi = 0; qi < ql.length; qi++) {
          if (ql[qi].trim() === "") continue;
          if (!qf) el.appendChild(doc.createElement("br"));
          qf = false;
          parseInline(ql[qi], el, doc);
        }
        frag.appendChild(el);
        break;
      case "list":
        buildList(b.text, b.ordered, 0, frag, doc);
        break;
      case "paragraph":
      default:
        el = doc.createElement("p");
        var pl = b.text.split("\n");
        var pf = true;
        for (var pi = 0; pi < pl.length; pi++) {
          if (pl[pi].trim() === "") continue;
          if (!pf) el.appendChild(doc.createElement("br"));
          pf = false;
          parseInline(pl[pi], el, doc);
        }
        frag.appendChild(el);
        break;
    }
  }

  // render(text, container, beforeNode, sentinel) — parse `text` into
  // block elements and insert them into container before beforeNode
  // (null = append). `text` must begin at a block boundary (the caller's
  // contract). With sentinel=true a trailing newline is appended before
  // parsing: a SETTLED chunk's completeness was decided by text that may
  // sit just past the chunk's end (a list's terminating line), so the
  // re-parse needs a final newline to stay complete; a trailing blank
  // line creates no block, so it is harmless.
  function render(text, container, beforeNode, sentinel) {
    if (!text) return;
    if (sentinel) text = text + "\n";
    var doc = container.ownerDocument;
    var frag = doc.createDocumentFragment();
    var blocks = parseBlocks(text);
    for (var i = 0; i < blocks.length; i++) buildBlock(blocks[i], frag, doc, text);
    container.insertBefore(frag, beforeNode || null);
  }

  return {
    version: 1,
    parseBlocks: parseBlocks,
    stableCut: stableCut,
    render: render
  };
});
