// Parse the embedded frontend's script without running it.
//
// Go's build says nothing about the JS inside internal/web/dist, so a syntax error
// there ships happily and takes the whole page down — a duplicate `const` in one
// function did exactly that. This is the cheapest possible guard against it.
"use strict";
const fs = require("fs");
const path = process.argv[2] || "internal/web/dist/index.html";
const html = fs.readFileSync(path, "utf8");
const scripts = [...html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/g)];
if (!scripts.length) {
  console.error(path + ": no inline script found — did the page change shape?");
  process.exit(1);
}
for (const [i, m] of scripts.entries()) {
  try {
    new Function(m[1]);
  } catch (err) {
    // Point at the line in the file, not in the extracted fragment.
    const before = html.slice(0, m.index).split("\n").length;
    console.error(`${path}: inline script ${i + 1} (near line ${before}): ${err.message}`);
    process.exit(1);
  }
}
console.log(`${path}: ${scripts.length} inline script(s) parse`);
