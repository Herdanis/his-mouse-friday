// Run with: bun test examples/plugins/hmf/

import { test, expect } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { readMouseYaml, pathMatchesPattern, commandMatchesPattern } from "./plugin";

function fixture(yaml: string): string {
  const dir = mkdtempSync(join(tmpdir(), "hmf-plugin-"));
  writeFileSync(join(dir, "mouse.yaml"), yaml);
  return dir;
}

const MOUSE = `permissions:
  fs:
    deny:
      - ".env"
      - "*.key"
    ask:
      - "secrets/"
  commands:
    deny:
      - "sudo"
      - "rm -rf /"
    ask:
      - "git push"
a2a:
  allow_inbound: true
`;

test("fs patterns do not leak into the command list", () => {
  const p = readMouseYaml(fixture(MOUSE))!;
  expect(p.commands.deny).toEqual(["sudo", "rm -rf /"]);
  expect(p.commands.ask).toEqual(["git push"]);
  expect(p.fs.deny).toEqual([".env", "*.key"]);
  expect(p.fs.ask).toEqual(["secrets/"]);
});

test("an fs pattern no longer false-positives on a command", () => {
  const p = readMouseYaml(fixture(MOUSE))!;
  // "*.key" used to land in commands.deny and block this.
  expect(p.commands.deny.some((x) => commandMatchesPattern("myfile.key", x))).toBe(false);
  expect(p.commands.deny.some((x) => commandMatchesPattern("sudo ls", x))).toBe(true);
});

test("fs globs match at any depth, and stay inside the config's scope", () => {
  const root = "/repo";
  expect(pathMatchesPattern("/repo/.env", root, ".env")).toBe(true);
  expect(pathMatchesPattern("/repo/deep/nested/.env", root, ".env")).toBe(true);
  expect(pathMatchesPattern("/repo/certs/tls.key", root, "*.key")).toBe(true);
  expect(pathMatchesPattern("/repo/secrets/db.txt", root, "secrets/")).toBe(true);
  expect(pathMatchesPattern("/repo/src/main.go", root, "*.key")).toBe(false);
  // "*" must not cross a separator
  expect(pathMatchesPattern("/repo/a/b.key", root, "a*.key")).toBe(false);
  // outside the mouse.yaml's directory
  expect(pathMatchesPattern("/elsewhere/.env", root, ".env")).toBe(false);
});

test("commands is the fallback group for an untagged deny block", () => {
  const p = readMouseYaml(fixture("permissions:\n  deny:\n    - \"sudo\"\n"))!;
  expect(p.commands.deny).toEqual(["sudo"]);
  expect(p.fs.deny).toEqual([]);
});

// ============================================
// Path extraction
// ============================================

import { extractPathCandidates } from "./plugin";

const FS_DENY = [".env", "*.key"];
const blocked = (cmd: string, cwd: string) =>
  extractPathCandidates(cmd, cwd, false).some((abs) =>
    FS_DENY.some((pat) => pathMatchesPattern(abs, cwd, pat)),
  );

test("fs rules catch writes that create the protected file", () => {
  const cwd = "/work";
  expect(blocked("touch new.key", cwd)).toBe(true);      // bare name, does not exist
  expect(blocked("echo x >.env", cwd)).toBe(true);       // glued redirect operator
  expect(blocked("echo x > .env", cwd)).toBe(true);
  expect(blocked("cp secrets.key /tmp", cwd)).toBe(true); // bare name as source
  expect(blocked("cat .env", cwd)).toBe(true);
  expect(blocked("cat ./nested/deep/.env", cwd)).toBe(true);
});

test("fs rules leave unrelated commands alone", () => {
  const cwd = "/work";
  expect(blocked("go test ./...", cwd)).toBe(false);
  expect(blocked("git status", cwd)).toBe(false);
  expect(blocked("docker run alpine:3.19 sh", cwd)).toBe(false);
  expect(blocked("curl https://example.com/thing.key", cwd)).toBe(false);
  expect(blocked("rm -rf build", cwd)).toBe(false);
});

test("cross-project extraction still requires the path to exist", () => {
  // mustExist=true is the default and must stay strict, or an unrelated bare
  // word would read as a path into another registered project.
  expect(extractPathCandidates("touch new.key", "/work")).toEqual([]);
});
