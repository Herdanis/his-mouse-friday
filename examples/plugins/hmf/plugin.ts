// hmf-protection — opencode plugin (TypeScript)
//
// Enforces hmf protection rules by intercepting tool calls:
//   1. Edit tool: blocks edits to registered project dirs when cwd
//      is not inside that project. Agent must use engage_project_agent.
//   2. Bash tool: checks commands against mouse.yaml deny/ask lists.
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

interface MousePerms {
  deny: string[];
  ask: string[];
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

function loadMouseYaml(dir: string): MousePerms | null {
  // Walk up: a session opened in A/config must still see A/mouse.yaml.
  let cur = dir;
  const stop = homedir();
  for (;;) {
    const perms = readMouseYaml(cur);
    if (perms) return perms;
    if (cur === stop || cur === dirname(cur)) return null;
    cur = dirname(cur);
  }
}

function readMouseYaml(dir: string): MousePerms | null {
  const path = join(dir, "mouse.yaml");
  if (!existsSync(path)) return null;
  try {
    const text = readFileSync(path, "utf8");
    // Supported shapes (minimal parse — no YAML dep):
    //   block:  "deny:" + "  - pattern" lines   |   inline:  deny: ["a", "b"]
    // Unquoted/quoted scalars both accepted; other YAML forms are NOT parsed.
    const deny: string[] = [];
    const ask: string[] = [];
    const push = (section: "deny" | "ask", raw: string) => {
      const v = raw.replace(/^["']|["']$/g, "").trim();
      if (v) (section === "deny" ? deny : ask).push(v);
    };
    let section: "deny" | "ask" | null = null;
    for (const line of text.split("\n")) {
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
    return { deny, ask };
  } catch {
    return null;
  }
}

function commandMatchesPattern(cmd: string, pattern: string): boolean {
  if (pattern.includes("*")) {
    const re = "^" + pattern.replace(/\*/g, "\\S+").replace(/\s+/g, "\\s+") + "(\\s|$)";
    return new RegExp(re).test(cmd);
  }
  return cmd.startsWith(pattern);
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

        // Case 1: am I a project's own agent (cwd inside a registered project)?
        // Then confine edits to that project. Blocks edits to a sibling clone /
        // stow-deployed copy / home dir — the agent must edit its registered
        // repo, not a different clone of the same files.
        const myProject = projects.find(
          (p) => cwd === p.path || cwd.startsWith(p.path + "/"),
        );
        if (myProject) {
          const inside = abs === myProject.path || abs.startsWith(myProject.path + "/");
          if (!inside) {
            throw new Error(
              `hmf: blocked edit OUTSIDE your project ${myProject.workspace}/${myProject.name}. ` +
              `You are operating in ${myProject.path}; edit only within it. ` +
              `Target was ${abs}. (If this is a deployed/stow copy or a different clone, ` +
              `edit the registered repo path instead.)`,
            );
          }
          return; // inside my own project → allow
        }

        // Case 2: open mode (cwd not in any registered project). Block edits
        // INTO registered projects — must delegate via engage_project_agent.
        const targetProject = projects.find(
          (p) => abs === p.path || abs.startsWith(p.path + "/"),
        );
        if (targetProject) {
          throw new Error(
            `hmf: blocked edit to ${targetProject.workspace}/${targetProject.name} — ` +
            `this is a registered project. Use engage_project_agent to delegate.`,
          );
        }
        // open mode, unregistered target → allow
        return;
      }

      // ============================================
      // Bash command protection
      // ============================================
      if (input.tool === "bash") {
        const cmd = (output.args as { command?: string })?.command ?? "";
        if (!cmd) return;

        const cwd = process.cwd();
        const mouse = loadMouseYaml(cwd);
        if (!mouse) return;

        for (const pattern of mouse.deny) {
          if (commandMatchesPattern(cmd, pattern)) {
            throw new Error(
              `hmf: blocked command (denied in mouse.yaml): "${cmd}" matches "${pattern}".`,
            );
          }
        }
        for (const pattern of mouse.ask) {
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
