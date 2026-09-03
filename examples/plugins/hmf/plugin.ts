// hmf-protection — opencode plugin (TypeScript)
//
// Enforces hmf protection rules by intercepting tool calls:
//   1. Edit tool: blocks edits to registered project dirs when cwd
//      is not inside that project. Agent must delegate with post_message.
//   2. Bash tool: checks commands against mouse.yaml deny/ask lists, and
//      blocks anything but reads from touching another registered project.
//   3. All tools: checks target paths against the fs deny/ask lists of the
//      mouse.yaml that owns that path, so secrets stay unreadable. For
//      grep/glob that means the search root up front, plus a filter over the
//      results so a directory search can't surface a denied file.
//
// Zero context cost — runs as code, AI never sees instructions.

import type { Plugin } from "@opencode-ai/plugin";
import { readFileSync, existsSync, statSync } from "node:fs";
import { join, resolve, relative, dirname } from "node:path";
import { homedir } from "node:os";
import { execSync } from "node:child_process";

// ============================================
// Types
// ============================================

interface ProjectInfo {
  workspace: string;
  name: string;
  path: string;
}

interface PermList {
  deny: string[];
  ask: string[];
}

interface MousePerms {
  fs: PermList;
  commands: PermList;
}

// Where the mouse.yaml was found — fs globs resolve relative to it.
interface MouseScope {
  perms: MousePerms;
  root: string;
}

// ============================================
// HMF daemon client (unix socket via python one-shot)
// ============================================

function hmfCall(method: string, params: Record<string, unknown>): unknown {
  const sock = join(homedir(), ".hmf", "daemon.sock");
  if (!existsSync(sock)) return null;
  try {
    const req = JSON.stringify({ method, params, id: 1 });
    const pyScript = `
import socket, json
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(${JSON.stringify(sock)})
s.sendall((${JSON.stringify(req)} + '\\n').encode())
buf = b''
while True:
    c = s.recv(4096)
    if not c: break
    buf += c
    if b'\\n' in buf: break
print(buf.decode().strip())
s.close()
`;
    const out = execSync("python3 -c " + JSON.stringify(pyScript), {
      timeout: 3000,
      encoding: "utf8",
    });
    const resp = JSON.parse(out);
    if (resp.error) return null;
    return resp.result;
  } catch {
    return null;
  }
}

// ============================================
// Registry lookup (init-time load — DB direct, daemon fallback)
// ============================================

function loadProjectsFromDB(): ProjectInfo[] {
  // Read the hmf SQLite DB directly via the sqlite3 CLI — no daemon socket,
  // no python one-shot, no contention. Fast + reliable.
  const dbPath = join(homedir(), ".hmf", "hmf.db");
  if (!existsSync(dbPath)) return [];
  try {
    const sql = "SELECT w.name, p.name, p.path FROM projects p JOIN workspaces w ON p.workspace_id=w.id";
    const out = execSync(`sqlite3 "${dbPath}" "${sql}"`, {
      timeout: 2000,
      encoding: "utf8",
    }).trim();
    if (!out) return [];
    const items: ProjectInfo[] = [];
    for (const line of out.split("\n")) {
      const parts = line.split("|");
      if (parts.length === 3) {
        items.push({ workspace: parts[0], name: parts[1], path: parts[2] });
      }
    }
    return items;
  } catch {
    return [];
  }
}

function loadProjects(): ProjectInfo[] {
  // Primary: read the DB directly (no daemon dependency).
  let items = loadProjectsFromDB();
  // Fallback: daemon socket call (if sqlite3 CLI missing or DB unreadable).
  if (items.length === 0) {
    const result = hmfCall("project_list", {});
    if (result) {
      try {
        items = JSON.parse(result as string) as ProjectInfo[];
      } catch {
        items = [];
      }
    }
  }
  return items;
}

// Session-lifetime cache with self-heal: an empty list means the daemon was
// down or the DB unreadable, which would silently disable cross-project
// protection for the whole session. Retry it — but at most once per retryMs,
// so a genuinely empty registry doesn't shell out on every tool call. A
// populated list is never re-queried.
export function makeRegistry(
  load: () => ProjectInfo[],
  retryMs = 30_000,
): () => ProjectInfo[] {
  let items = load();
  let last = Date.now();
  return () => {
    if (items.length === 0 && Date.now() - last >= retryMs) {
      last = Date.now();
      items = load();
    }
    return items;
  };
}

// ============================================
// Mouse.yaml loader (minimal parse — no YAML dep)
// ============================================

// scopeForPath resolves the rules that govern a file: the mouse.yaml of the
// project the file lives in. A parent reading a child's file is judged by the
// child's yaml, not its own.
export function scopeForPath(abs: string): MouseScope | null {
  return loadMouseYaml(dirname(abs));
}

function loadMouseYaml(dir: string): MouseScope | null {
  // Walk up: a session opened in A/config must still see A/mouse.yaml.
  let cur = dir;
  const stop = homedir();
  for (;;) {
    const perms = readMouseYaml(cur);
    if (perms) return { perms, root: cur };
    if (cur === stop || cur === dirname(cur)) break;
    cur = dirname(cur);
  }
  // Unregistered dir: fall back to the global default, same as Go's
  // config.ResolveMouse. Its fs rules are scoped to the session's own dir.
  const global = readMouseYaml(join(homedir(), ".hmf"));
  return global ? { perms: global, root: dir } : null;
}

export function readMouseYaml(dir: string): MousePerms | null {
  const path = join(dir, "mouse.yaml");
  if (!existsSync(path)) return null;
  try {
    const text = readFileSync(path, "utf8");
    // Supported shapes (minimal parse — no YAML dep):
    //   block:  "deny:" + "  - pattern" lines   |   inline:  deny: ["a", "b"]
    // Unquoted/quoted scalars both accepted; other YAML forms are NOT parsed.
    const out: MousePerms = { fs: { deny: [], ask: [] }, commands: { deny: [], ask: [] } };
    // Which parent the deny/ask block sits under. Untagged blocks fall back to
    // commands — an fs pattern leaking into the command list both fails to
    // protect the file and false-positives on unrelated commands.
    let group: "fs" | "commands" = "commands";
    const push = (section: "deny" | "ask", raw: string) => {
      const v = raw.replace(/^["']|["']$/g, "").trim();
      if (v) out[group][section].push(v);
    };
    let section: "deny" | "ask" | null = null;
    for (const line of text.split("\n")) {
      const parent = line.match(/^\s+(fs|commands):/);
      if (parent) { group = parent[1] as "fs" | "commands"; section = null; continue; }
      const inline = line.match(/^\s+(deny|ask):\s*\[(.*)\]\s*$/);
      if (inline) {
        for (const item of inline[2].split(",")) push(inline[1] as "deny" | "ask", item.trim());
        section = null;
        continue;
      }
      if (line.match(/^\s+deny:\s*$/)) { section = "deny"; continue; }
      if (line.match(/^\s+ask:\s*$/)) { section = "ask"; continue; }
      if (line.match(/^\s+\w/)) { section = null; continue; }
      if (section) {
        const m = line.match(/^\s+-\s+(.+?)\s*$/);
        if (m) push(section, m[1]);
      }
    }
    return out;
  } catch {
    return null;
  }
}

// gitignore-style glob against the path relative to the mouse.yaml dir.
// A pattern with no "/" matches at any depth; "*" stops at a path separator,
// "**" does not. A trailing "/" marks a directory and matches everything under it.
export function pathMatchesPattern(abs: string, root: string, pattern: string): boolean {
  const rel = relative(root, abs);
  if (!rel || rel.startsWith("..")) return false; // outside this config's scope
  const pat = pattern.replace(/\/+$/, "");
  const rx = new RegExp(
    "^" +
      pat
        .replace(/[.+^${}()|[\]\\]/g, "\\$&")
        .replace(/\*\*/g, "\u0000")
        .replace(/\*/g, "[^/]*")
        .replace(/\u0000/g, ".*") +
      "$",
  );
  if (!pat.includes("/")) return rel.split("/").some((seg) => rx.test(seg));
  return rx.test(rel);
}

export function commandMatchesPattern(cmd: string, pattern: string): boolean {
  if (pattern.includes("*")) {
    const re = "^" + pattern.replace(/\*/g, "\\S+").replace(/\s+/g, "\\s+") + "(\\s|$)";
    return new RegExp(re).test(cmd);
  }
  return cmd.startsWith(pattern);
}

// ============================================
// FS guard (shared by edit + bash)
// ============================================

function fsRule(
  abs: string,
  scope: MouseScope | null,
): { kind: "deny" | "ask"; pattern: string } | null {
  if (!scope) return null;
  for (const pattern of scope.perms.fs.deny) {
    if (pathMatchesPattern(abs, scope.root, pattern)) return { kind: "deny", pattern };
  }
  for (const pattern of scope.perms.fs.ask) {
    if (pathMatchesPattern(abs, scope.root, pattern)) return { kind: "ask", pattern };
  }
  return null;
}

// True when the file's own project forbids reading it.
export function fsBlocked(abs: string): boolean {
  return fsRule(abs, scopeForPath(abs)) !== null;
}

// Throws if abs is covered by an fs deny/ask pattern. `how` names the caller
// so the agent's BLOCKED reply says which tool it was.
function enforceFS(abs: string, scope: MouseScope | null, how: string): void {
  const hit = fsRule(abs, scope);
  if (!hit) return;
  if (hit.kind === "deny") {
    throw new Error(`hmf: blocked ${how} of ${abs} (denied in mouse.yaml fs: "${hit.pattern}").`);
  }
  throw new Error(`hmf: ${how} of ${abs} requires approval (ask in mouse.yaml fs: "${hit.pattern}").`);
}

// ============================================
// Search guard (grep / glob)
// ============================================

function isDir(p: string): boolean {
  try {
    return statSync(p).isDirectory();
  } catch {
    return false;
  }
}

// grep/glob take a search root (`path`, default cwd); grep's may be a single
// file. The root itself is judged exactly like a read — by the mouse.yaml of
// the project it lives in. A directory root can still *contain* denied files;
// those are stripped from the results by filterSearchOutput, because a
// before-hook can only allow or block the call as a whole.
export function enforceSearchPath(abs: string, how: string): void {
  enforceFS(abs, loadMouseYaml(isDir(abs) ? abs : dirname(abs)), how);
}

// Drops result groups whose path its own project denies. grep prints
// "<abs path>:" followed by indented "  Line N:" rows and a blank separator;
// glob prints one absolute path per line. Both are absolute, so a line
// starting with "/" opens a new group.
export function filterSearchOutput(
  text: string,
  denied: (abs: string) => boolean,
): { output: string; hidden: number } {
  const kept: string[] = [];
  let dropping = false;
  let hidden = 0;
  for (const line of text.split("\n")) {
    if (line.startsWith("/")) {
      dropping = denied(line.endsWith(":") ? line.slice(0, -1) : line);
      if (dropping) {
        hidden++;
        continue;
      }
    } else if (dropping) {
      if (line.trim() === "") dropping = false; // blank line closes the group
      continue;
    }
    kept.push(line);
  }
  if (hidden > 0) {
    kept.push("", `(${hidden} result(s) hidden by hmf — denied in the owning project's mouse.yaml fs rules)`);
  }
  return { output: kept.join("\n"), hidden };
}

// ============================================
// Cross-project path detection (shared by edit + bash)
// ============================================

function findProjectFor(abs: string, projects: ProjectInfo[]): ProjectInfo | undefined {
  return projects.find((p) => abs === p.path || abs.startsWith(p.path + "/"));
}

// Pulls path-looking args out of a shell command.
// ponytail: whitespace/quote split, not a real shell parser.
//
// mustExist=true (cross-project check): only tokens present on disk count, so
// an unrelated bare word can't be mistaken for a path into another project.
// mustExist=false (fs rules): a bare filename counts too — otherwise a write
// that creates the file (`touch new.key`, `echo x >.env`) slips past, since
// the target does not exist yet at check time.
export function extractPathCandidates(cmd: string, cwd: string, mustExist = true): string[] {
  const tokens = cmd.match(/(?:[^\s"']+|"[^"]*"|'[^']*')+/g) ?? [];
  const out: string[] = [];
  for (const raw of tokens) {
    let t = raw.replace(/^["']|["']$/g, "");
    // Strip leading shell operators so a glued redirect (">.env") still
    // yields its target.
    t = t.replace(/^[0-9]*[<>]+&?|^[|;&(]+/, "").replace(/^["']|["']$/g, "");
    if (t.startsWith("-")) {
      const eq = t.indexOf("=");
      if (eq === -1) continue; // bare flag, e.g. "-f" — value is the next token
      t = t.slice(eq + 1);
    }
    if (t.startsWith("@")) t = t.slice(1); // curl -d @path, etc.
    if (!t) continue;
    if (t.includes("://")) continue; // URL
    if (!t.includes("/") && /^[\w.-]+:[\w.-]+$/.test(t)) continue; // image ref, host:port
    const looksLikePath = t.includes("/") || t === "." || t.startsWith(".");
    if (mustExist) {
      if (!looksLikePath) continue;
      if (existsSync(resolve(cwd, t))) out.push(resolve(cwd, t));
    } else {
      out.push(resolve(cwd, t));
    }
  }
  return out;
}

// ============================================
// Read-only command detection
// ============================================

// A parent may LOOK inside a registered project to verify a child's work; it
// may never change it. Everything here only reads.
// Excludes anything with a shell escape or scripting engine: no awk (system(),
// getline "cmd"|), no sed (`e`, `w`), no pagers (less/more take `!cmd`).
const READ_ONLY_BINARIES = new Set([
  "cat", "head", "tail", "ls", "tree", "find", "fd", "stat",
  "file", "wc", "du", "diff", "cmp", "sort", "uniq", "cut", "column",
  "grep", "egrep", "fgrep", "rg", "ag", "ack", "jq", "yq",
  "basename", "dirname", "realpath", "readlink", "echo", "pwd", "date",
  "git", "go", "npm", "bun",
]);

// Flags that make an otherwise read-only binary write a file or run a command.
const FORBIDDEN_FLAGS: Record<string, RegExp> = {
  find: /^-(delete|exec|execdir|ok|okdir|fprint|fprintf|fprint0|fls)$/,
  fd: /^(-x|-X|--exec|--exec-batch)$/,
  rg: /^(--pre|--hostname-bin|--pre-glob)$/,
  sort: /^(-o|--output)/,
  // `-c core.pager=…` / `-c alias.…` / `-c core.sshCommand=…` all run commands;
  // `-O` hands git grep a pager command.
  git: /^(-c|--config-env|--exec-path|--upload-pack|--receive-pack|-O|--open-files-in-pager)/,
  go: /^-w$/, // `go env -w` writes the go env config
};

// Subcommand allowlists for tools whose other subcommands write or execute.
const READ_ONLY_SUBCOMMANDS: Record<string, Set<string>> = {
  git: new Set(["log", "status", "diff", "show", "blame", "ls-files", "ls-tree",
    "rev-parse", "describe", "shortlog", "cat-file", "grep", "remote"]),
  go: new Set(["list", "version", "env", "doc"]),
  npm: new Set(["ls", "list", "view", "outdated"]),
  bun: new Set(["pm"]),
};

// True when every segment of the command only reads. Conservative by design:
// anything unrecognised is a write.
export function isReadOnlyCommand(cmd: string): boolean {
  // Output redirection writes wherever it points; `-i` edits in place.
  if (/(^|[^0-9<>])>{1,2}[^>]/.test(cmd) || />\s*$/.test(cmd)) return false;
  if (/(^|\s)-i(\s|$)|(^|\s)--in-place(\s|$)/.test(cmd)) return false;
  // Command substitution and process substitution hide the real command.
  if (cmd.includes("$(") || cmd.includes("`") || cmd.includes("<(")) return false;
  const segments = cmd.split(/\|\||&&|[|;]/);
  for (const seg of segments) {
    const words = seg.trim().split(/\s+/).filter(Boolean);
    if (words.length === 0) continue;
    const bin = words[0].split("/").pop() ?? "";
    if (bin === "sudo" || bin === "env" || bin === "command" || bin === "xargs") return false;
    if (!READ_ONLY_BINARIES.has(bin)) return false;
    const forbidden = FORBIDDEN_FLAGS[bin];
    if (forbidden && words.slice(1).some((w) => forbidden.test(w))) return false;
    // `uniq in out` writes its second positional; one input only.
    if (bin === "uniq" && words.slice(1).filter((w) => !w.startsWith("-")).length > 1) return false;
    const subs = READ_ONLY_SUBCOMMANDS[bin];
    if (subs) {
      // First non-flag word after the binary is the subcommand. `git -C <dir>`
      // takes a value, so skip that pair too.
      let i = 1;
      while (i < words.length && words[i].startsWith("-")) {
        i += words[i] === "-C" || words[i] === "--git-dir" ? 2 : 1;
      }
      if (i >= words.length || !subs.has(words[i])) return false;
    }
  }
  return true;
}

// ============================================
// Plugin
// ============================================

export const HmfProtection: Plugin = async ({ directory }) => {
  const registry = makeRegistry(loadProjects); // loads now, retries only if empty
  return {
    "tool.execute.before": async (input, output) => {
      // ============================================
      // Read protection
      // ============================================
      // Reading another registered project is allowed (a parent verifying a
      // child's work), but that project's own fs deny/ask list still applies.
      if (input.tool === "read") {
        const filePath = (output.args as { filePath?: string; path?: string })?.filePath
          ?? (output.args as { filePath?: string; path?: string })?.path
          ?? "";
        if (!filePath) return;
        const abs = resolve(filePath);
        enforceFS(abs, scopeForPath(abs), "read of");
        return;
      }

      // ============================================
      // Search protection
      // ============================================
      // Same fs rules as read, applied to the search root. Matches under a
      // directory root are filtered in tool.execute.after.
      if (input.tool === "grep" || input.tool === "glob") {
        const search = (output.args as { path?: string })?.path;
        enforceSearchPath(resolve(process.cwd(), search ?? "."), input.tool);
        return;
      }

      // ============================================
      // Edit protection
      // ============================================
      if (input.tool === "edit") {
        const filePath = (output.args as { filePath?: string; path?: string })?.filePath
          ?? (output.args as { filePath?: string; path?: string })?.path
          ?? "";
        if (!filePath) return;

        const abs = resolve(filePath);
        const cwd = process.cwd();

        // fs rules first: they apply inside your own repo too, so they must
        // not sit behind the cross-project early-returns below.
        enforceFS(abs, loadMouseYaml(dirname(abs)), "edit");

        // Only guard edits INTO a registered project — unregistered targets
        // (scratch files, /tmp, unrelated clones) are always allowed.
        const projects = registry();
        const targetProject = findProjectFor(abs, projects);
        if (!targetProject) return;

        const myProject = findProjectFor(cwd, projects);
        if (myProject && myProject.path === targetProject.path) return;

        throw new Error(
          `hmf: blocked edit to ${targetProject.workspace}/${targetProject.name} — ` +
          `this is a registered project` +
          (myProject ? ` (you are ${myProject.workspace}/${myProject.name})` : "") +
          `. Use engage_project_agent to delegate. Target was ${abs}.`,
        );
      }

      // ============================================
      // Bash command protection
      // ============================================
      if (input.tool === "bash") {
        const cmd = (output.args as { command?: string })?.command ?? "";
        if (!cmd) return;

        const cwd = process.cwd();

        // Same rule as edit, with one exception: reading another registered
        // project is allowed, so a parent can verify a child's work. Anything
        // that could change or run it is still the owning agent's job.
        const projects = registry();
        const myProject = findProjectFor(cwd, projects);
        const readOnly = isReadOnlyCommand(cmd);
        for (const abs of extractPathCandidates(cmd, cwd)) {
          const targetProject = findProjectFor(abs, projects);
          if (!targetProject) continue;
          if (myProject && myProject.path === targetProject.path) continue;
          if (readOnly) {
            // The target's own fs rules still hold — its secrets are not
            // readable just because the caller is a parent.
            enforceFS(abs, scopeForPath(abs), "read of");
            continue;
          }
          throw new Error(
            `hmf: blocked command touching ${targetProject.workspace}/${targetProject.name} — ` +
            `this is a registered project` +
            (myProject ? ` (you are ${myProject.workspace}/${myProject.name})` : "") +
            `. Reading it is allowed; changing or running it is not — delegate with ` +
            `post_message. Command: "${cmd}", path: ${abs}.`,
          );
        }

        const mouse = loadMouseYaml(cwd);
        if (!mouse) return;

        // mustExist=false: also covers a write that creates the file.
        for (const abs of extractPathCandidates(cmd, cwd, false)) {
          enforceFS(abs, mouse, "command access to");
        }

        for (const pattern of mouse.perms.commands.deny) {
          if (commandMatchesPattern(cmd, pattern)) {
            throw new Error(
              `hmf: blocked command (denied in mouse.yaml): "${cmd}" matches "${pattern}".`,
            );
          }
        }
        for (const pattern of mouse.perms.commands.ask) {
          if (commandMatchesPattern(cmd, pattern)) {
            throw new Error(
              `hmf: command requires approval (ask in mouse.yaml): "${cmd}" matches "${pattern}".`,
            );
          }
        }
      }
    },

    // A directory search legitimately spans files the owning project denies.
    // Blocking the whole call would make grep/glob useless inside any project
    // with fs rules (the global fallback denies ".env"/"*.key" everywhere), so
    // the denied paths are stripped from the results instead.
    "tool.execute.after": async (input, output) => {
      if (input.tool !== "grep" && input.tool !== "glob") return;
      const filtered = filterSearchOutput(output.output ?? "", fsBlocked);
      if (filtered.hidden > 0) output.output = filtered.output;
    },
  };
};

export default HmfProtection;
