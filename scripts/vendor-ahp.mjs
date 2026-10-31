// Build a reviewable upstream revision of the official SDK until 1.1 is on npm.
// Usage: node scripts/vendor-ahp.mjs /path/to/agent-host-protocol
// The source checkout is read-only; all generation happens in a temporary tree.
import { execFileSync } from "node:child_process";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const revision = "d7a5579b1733fb2e1e4a2c653442e26c7b7226dd";
const repo = resolve(process.argv[2] ?? "../agent-host-protocol");
const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const ui = join(root, "server/ui");
const temporary = mkdtempSync(join(tmpdir(), "wingman-ahp-sdk-"));
try {
  const archive = execFileSync("git", ["-C", repo, "archive", revision], { maxBuffer: 64 << 20 });
  execFileSync("tar", ["-xf", "-", "-C", temporary], { input: archive });
  const sdk = join(temporary, "clients/typescript");
  // Upstream's TypeScript generator mirrors canonical sources and excludes tests.
  // Its derived action-origin/window-exempt sources are committed at this revision.
  function copyTypes(from, to) {
    mkdirSync(to, { recursive: true });
    for (const entry of readdirSync(from, { withFileTypes: true })) {
      if (entry.name === "test-cases" || entry.name.endsWith(".test.ts")) continue;
      if (entry.isDirectory()) copyTypes(join(from, entry.name), join(to, entry.name));
      else if (entry.name.endsWith(".ts")) cpSync(join(from, entry.name), join(to, entry.name));
    }
  }
  copyTypes(join(temporary, "types"), join(sdk, "src/types"));
  cpSync(join(temporary, "LICENSE"), join(sdk, "LICENSE"));
  const manifest = JSON.parse(readFileSync(join(sdk, "package.json"), "utf8"));
  manifest.version = `1.1.0-dev.${revision.slice(0, 12)}`;
  manifest.wingmanUpstreamRevision = revision;
  writeFileSync(join(sdk, "package.json"), JSON.stringify(manifest, null, 2) + "\n");
  execFileSync(join(ui, "node_modules/.bin/tsc"), ["-p", join(sdk, "tsconfig.json")], { stdio: "inherit" });
  const destination = join(ui, "vendor");
  mkdirSync(destination, { recursive: true });
  execFileSync("npm", ["pack", "--ignore-scripts", "--cache", join(temporary, "npm-cache"), "--pack-destination", destination, "--silent"], { cwd: sdk, stdio: "inherit" });
} finally {
  rmSync(temporary, { recursive: true, force: true });
}
