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
import { join, resolve, relative } from "node:path";
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
// Registry lookup
// ============================================

function getRegisteredProjects(): ProjectInfo[] {
  const result = hmfCall("project_list", {});
  if (!result) return [];
  try {
    const items = JSON.parse(result as string) as ProjectInfo[];
    return items;
  } catch {
    return [];
  }
}

function findRegisteredProject(filePath: string): ProjectInfo | null {
  const abs = resolve(filePath);
  const projects = getRegisteredProjects();
  for (const p of projects) {
    if (abs.startsWith(p.path + "/")) return p;
  }
  return null;
}

// ============================================
// Mouse.yaml loader (minimal parse — no YAML dep)
// ============================================

function loadMouseYaml(dir: string): MousePerms | null {
  const path = join(dir, "mouse.yaml");
  if (!existsSync(path)) return null;
  try {
    const text = readFileSync(path, "utf8");
    const deny: string[] = [];
    const ask: string[] = [];
    let section: "deny" | "ask" | null = null;
    for (const line of text.split("\n")) {
      if (line.match(/^\s+deny:\s*$/)) { section = "deny"; continue; }
      if (line.match(/^\s+ask:\s*$/)) { section = "ask"; continue; }
      if (line.match(/^\s+\w/)) { section = null; continue; }
      if (section) {
        const m = line.match(/^\s+-\s+["']?(.+?)["']?\s*$/);
        if (m) (section === "deny" ? deny : ask).push(m[1]);
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

        const project = findRegisteredProject(filePath);
        if (!project) return;

        const cwd = directory ?? process.cwd();
        const rel = relative(project.path, cwd);
        const cwdInside = rel && !rel.startsWith("..");
        if (cwdInside) return;

        throw new Error(
          `hmf: blocked edit to ${project.workspace}/${project.name} — ` +
          `this is a registered project. Use engage_project_agent to delegate.`,
        );
      }

      // ============================================
      // Bash command protection
      // ============================================
      if (input.tool === "bash") {
        const cmd = (output.args as { command?: string })?.command ?? "";
        if (!cmd) return;

        const cwd = directory ?? process.cwd();
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
