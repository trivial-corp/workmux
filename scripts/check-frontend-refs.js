// Every top-level function the page calls must exist.
//
// A rewrite of one function silently deleted two others — send() and toast() — and the
// page kept parsing, so `make lint` was happy while every keystroke threw
// ReferenceError. Syntax is not enough; the names have to resolve.
"use strict";
const fs = require("fs");
const path = process.argv[2] || "internal/web/dist/index.html";
const html = fs.readFileSync(path, "utf8");
const script = [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)]
  .map((m) => m[1]).join("\n");

const defined = new Set();
// Top-level declarations…
for (const m of script.matchAll(/(?:async\s+)?function\s+([A-Za-z_$][\w$]*)/g)) defined.add(m[1]);
for (const m of script.matchAll(/(?:const|let|var)\s+([A-Za-z_$][\w$]*)/g)) defined.add(m[1]);
// …and every parameter name, since a call to a callback parameter is not a call to a
// missing global. Both `function (a, b)` and `(a, b) =>`.
for (const m of script.matchAll(/(?:function\s*[A-Za-z_$\w]*\s*|\)\s*=>|\(([^()]*)\)\s*=>)/g)) {
  if (m[1]) for (const part of m[1].split(",")) defined.add(part.trim().split(/[=\s:]/)[0]);
}
for (const m of script.matchAll(/function\s*[A-Za-z_$\w]*\s*\(([^()]*)\)/g)) {
  for (const part of m[1].split(",")) defined.add(part.trim().split(/[=\s:]/)[0]);
}
for (const m of script.matchAll(/([A-Za-z_$][\w$]*)\s*=>/g)) defined.add(m[1]);
// Destructured bindings: `const [a, b] = …`, `const {a, b} = …`, `for (const [k, v] of …)`.
for (const m of script.matchAll(/(?:const|let|var)\s*[[{]([^\]}]*)[\]}]/g)) {
  for (const part of m[1].split(",")) {
    const name = part.trim().split(/[:=\s]/)[0].replace(/^\.\.\./, "");
    if (name) defined.add(name);
  }
}
// Names that come from the platform, xterm, or a local binding rather than the top level.
const known = new Set(["Terminal", "FitAddon", "TextEncoder", "TextDecoder", "WebSocket",
  "DataTransfer", "File", "Function", "JSON", "Math", "Object", "Set", "Map", "Array",
  "Promise", "Error", "String", "Number", "Boolean", "Date", "RegExp", "parseInt",
  "parseFloat", "isNaN", "atob", "btoa", "fetch", "setTimeout", "setInterval",
  "clearTimeout", "clearInterval", "requestAnimationFrame", "confirm", "alert", "prompt",
  "encodeURIComponent", "decodeURIComponent", "ResizeObserver", "ClipboardEvent",
  "Uint8Array", "if", "for", "while", "switch", "catch", "return", "typeof", "function",
  "el", "$",
  // Keywords a call-shaped regex trips over.
  "await", "async", "of", "in", "new", "delete", "void", "yield", "do", "else", "try",
  "throw", "case", "let", "const", "var", "class", "extends", "super", "this", "import",
  "export", "default", "instanceof", "with", "debugger", "finally", "break", "continue"]);

// Comments mention things in call-ish shapes ("clipboard (a remote instance)"), so they
// are not code and must not be scanned. Naive but sufficient: this file has no regex
// literals containing comment markers.
const code = script
  .replace(/\/\*[\s\S]*?\*\//g, "")
  .split("\n").map((l) => l.replace(/(^|[^:\\])\/\/.*$/, "$1")).join("\n")
  // Text is not code either. "3 file(s)" in a sentence read as a call to file(), which
  // is a checker inventing a bug in a string it was never meant to look inside.
  .replace(/"(?:[^"\\\n]|\\.)*"/g, '""')
  .replace(/'(?:[^'\\\n]|\\.)*'/g, "''")
  // A template's prose goes, but what is interpolated into it is code and stays.
  .replace(/`(?:[^`\\]|\\.)*`/g, (t) => (t.match(/\$\{[\s\S]*?\}/g) || []).join(";"));

const missing = new Map();
for (const m of code.matchAll(/(?<![.\w$])([A-Za-z_$][\w$]*)\s*\(/g)) {
  const name = m[1];
  // A call chained onto the previous line — `.match(` after a newline — is a method,
  // not a global. The lookbehind above only sees the whitespace.
  const before = code.slice(0, m.index).replace(/\s+$/, "");
  if (before.endsWith(".")) continue;
  if (defined.has(name) || known.has(name)) continue;
  const line = script.slice(0, m.index).split("\n").length;
  if (!missing.has(name)) missing.set(name, line);
}
if (missing.size) {
  for (const [name, line] of missing) {
    console.error(`${path}: calls ${name}() which is never defined (script line ${line})`);
  }
  process.exit(1);
}
console.log(`${path}: ${defined.size} top-level names, every call resolves`);
