#!/usr/bin/env node
// `npx workmux` — and this time it's honest: the npm package *contains* the binary.
//
// The pattern is esbuild's. This package depends on one small package per platform,
// each declaring its own os/cpu, so npm downloads exactly one of them and skips the
// rest. There is no postinstall script, nothing is fetched at install time, and no
// other runtime is required: this file's whole job is to exec the binary that npm
// already put on disk.
"use strict";

const { spawnSync } = require("child_process");

const PLATFORMS = {
  "darwin arm64": "workmux-darwin-arm64",
  "darwin x64": "workmux-darwin-amd64",
  "linux arm64": "workmux-linux-arm64",
  "linux x64": "workmux-linux-amd64",
};

const key = process.platform + " " + process.arch;
const pkg = PLATFORMS[key];
if (!pkg) {
  console.error(
    "workmux has no build for " + key + ".\n" +
    "It runs on macOS and Linux, on arm64 and x64. Build from source instead:\n" +
    "  go install github.com/trivial-corp/workmux/cmd/workmux@latest");
  process.exit(1);
}

let binary;
try {
  binary = require(pkg + "/bin/workmux");
} catch (err) {
  console.error(
    "workmux's binary for " + key + " isn't installed.\n" +
    "That package is an optional dependency, so this happens with --no-optional,\n" +
    "--omit=optional, or an npm that skipped it. Try:\n" +
    "  npm install " + pkg);
  process.exit(1);
}

// spawnSync, not exec: the binary is long-lived and interactive, and this keeps the
// terminal, the exit code and the signals behaving as if you had run it directly.
const child = spawnSync(binary, process.argv.slice(2), { stdio: "inherit" });
if (child.error) {
  console.error("workmux: could not run " + binary + ": " + child.error.message);
  process.exit(1);
}
if (child.signal) {
  process.kill(process.pid, child.signal);
}
process.exit(child.status === null ? 1 : child.status);
