// Run with: bun test examples/plugins/hmf/

import { test, expect } from "bun:test";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { readMouseYaml, pathMatchesPattern, commandMatchesPattern, isReadOnlyCommand, scopeForPath } from "./plugin";

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

// A parent may look inside a child project to verify its work, never change it.
test("read-only commands are recognised", () => {
  for (const cmd of [
    "cat internal/app.go",
    "rg 'func main' ../other/src",
    "git -C ../other log --oneline -5",
    "git status",
    "ls -la ../other/internal",
    "head -50 ../other/README.md | grep install",
    "go list ./...",
  ]) {
    expect(isReadOnlyCommand(cmd)).toBe(true);
  }
});

test("anything that writes or runs code is not read-only", () => {
  for (const cmd of [
    "echo x > ../other/file.go",
    "sed -n '1,5p' ../other/main.go",
    "awk '{print}' ../other/main.go",
    "less ../other/main.go",
    "rm ../other/main.go",
    "go test ./...",
    "npm run build",
    "git commit -am wip",
    "git checkout -b feature",
    "cat f.go && rm g.go",
    "cat $(rm -rf x)",
    "sudo cat /etc/passwd",
  ]) {
    expect(isReadOnlyCommand(cmd)).toBe(false);
  }
});

// A parent reading a child's file is judged by the CHILD's mouse.yaml.
test("read rules come from the project the file lives in", () => {
  const child = fixture(`permissions:
  fs:
    deny:
      - "secret.txt"
`);
  const scope = scopeForPath(join(child, "src", "app.go"))!;
  expect(scope.root).toBe(child);
  expect(scope.perms.fs.deny).toEqual(["secret.txt"]);
  expect(pathMatchesPattern(join(child, "secret.txt"), scope.root, "secret.txt")).toBe(true);
  // The parent's own yaml has no say over a path inside the child.
  const parent = fixture(`permissions:
  fs:
    deny:
      - "*.go"
`);
  expect(pathMatchesPattern(join(child, "src", "app.go"), parent, "*.go")).toBe(false);
});

// Escapes that a name-only allowlist would wave through.
test("shell escapes inside allowlisted tools are not read-only", () => {
  for (const cmd of [
    "find ../other -name '*.go' -delete",
    "find ../other -type f -exec rm {} ;",
    "fd -x rm {} . ../other",
    "rg --pre ./evil.sh foo ../other",
    "sort -o ../other/main.go ../other/main.go",
    "uniq ../other/a.txt ../other/b.txt",
    "git -c core.pager='rm -rf /' log",
    "git -c alias.x='!sh -c evil' log",
    "git grep -O./evil.sh foo",
    "go env -w GOFLAGS=-mod=mod",
    "xargs rm < list.txt",
    "cat `rm -rf x`",
    "diff <(rm x) y",
  ]) {
    expect(isReadOnlyCommand(cmd)).toBe(false);
  }
});

test("plain forms of the same tools still read", () => {
  for (const cmd of [
    "find ../other -name '*.go'",
    "git -C ../other grep -n 'func main'",
    "sort ../other/a.txt",
    "uniq -c",
    "go env GOPATH",
  ]) {
    expect(isReadOnlyCommand(cmd)).toBe(true);
  }
});
