// hmf-protection — opencode plugin (TypeScript)
//
// Enforces hmf protection rules by intercepting tool calls:
//   1. Edit tool: blocks edits to registered project dirs when cwd
//      is not inside that project. Agent must use engage_project_agent.
//   2. Bash tool: checks commands against mouse.yaml deny/ask lists.
//   3. Both tools: checks target paths against mouse.yaml's fs deny/ask
//      lists (gitignore-style globs), so secrets stay unreadable.
//
// Zero context cost — runs as code, AI never sees instructions.

import type { Plugin } from "@opencode-ai/plugin";
import { readFileSync, existsSync } from "node:fs";
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

// Run once at session start (plugin setup). Session-lifetime list.
// ponytail: no self-heal within session; restart opencode if daemon was down at start.
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

// ============================================
// Mouse.yaml loader (minimal parse — no YAML dep)
// ============================================

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

// Throws if abs is covered by an fs deny/ask pattern. `how` names the caller
// so the agent's BLOCKED reply says which tool it was.
function enforceFS(abs: string, scope: MouseScope | null, how: string): void {
  if (!scope) return;
  for (const pattern of scope.perms.fs.deny) {
    if (pathMatchesPattern(abs, scope.root, pattern)) {
      throw new Error(`hmf: blocked ${how} of ${abs} (denied in mouse.yaml fs: "${pattern}").`);
    }
  }
  for (const pattern of scope.perms.fs.ask) {
    if (pathMatchesPattern(abs, scope.root, pattern)) {
      throw new Error(`hmf: ${how} of ${abs} requires approval (ask in mouse.yaml fs: "${pattern}").`);
    }
  }
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
// Plugin
// ============================================

export const HmfProtection: Plugin = async ({ directory }) => {
  const projects = loadProjects();   // runs once, session-lifetime
  return {
    "tool.execute.before": async (input, output) => {
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

        // Same rule as edit: block a command touching another registered project.
        const myProject = findProjectFor(cwd, projects);
        for (const abs of extractPathCandidates(cmd, cwd)) {
          const targetProject = findProjectFor(abs, projects);
          if (!targetProject) continue;
          if (myProject && myProject.path === targetProject.path) continue;
          throw new Error(
            `hmf: blocked command touching ${targetProject.workspace}/${targetProject.name} — ` +
            `this is a registered project` +
            (myProject ? ` (you are ${myProject.workspace}/${myProject.name})` : "") +
            `. Use engage_project_agent to delegate. Command: "${cmd}", path: ${abs}.`,
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
  };
};

export default HmfProtection;
