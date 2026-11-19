// Forge launcher.
//
// The process the grader starts is this file, under Node. The launcher
// itself serves nothing; it babysits the compiled Go core that lives
// beside it in the repository:
//
//   1. start ./forge with FORGE_PORT=0, so the kernel assigns the port;
//   2. read the chosen port out of the banner the core prints;
//   3. announce the port to the trial environment
//      (POST $MAKEFUN_ENV_URL/register with a {"port": ...} body);
//   4. poll $MAKEFUN_ENV_URL/done until the environment reports the
//      trial finished, then stop the core and exit 0.
//
// Every other concern — git smart HTTP, the REST surface, GraphQL and
// MCP — belongs to the Go core. This process hands its environment
// down to the child untouched, so MAKEFUN_SHOP_TOKEN and its siblings
// arrive exactly as the harness set them, and it mirrors the core's
// stdout into its own so the trial log shows one combined stream.

import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoDir = path.dirname(fileURLToPath(import.meta.url));
const coreBinary = path.join(repoDir, "forge");
const envOrigin = (process.env.MAKEFUN_ENV_URL ?? "").replace(/\/+$/, "");

// Pick the runtime the trial expects. The pod starts this file with
// Node 22, but a workstation's default node may be a newer major whose
// type stripping behaves a little differently, so on the wrong major
// we look for a Node 22 install and re-exec this file under it. The
// FORGE_RUNTIME_PICKED flag is set on the re-exec so it happens once.
const majorOf = (version: string): number => Number(version.split(".")[0]);
if (majorOf(process.versions.node) !== 22 && process.env.FORGE_RUNTIME_PICKED !== "1") {
  const homes: string[] = [];
  if (process.env.FORGE_NODE22 !== undefined && process.env.FORGE_NODE22 !== "") {
    homes.push(process.env.FORGE_NODE22);
  }
  homes.push("node22", "/usr/local/bin/node22", "/opt/node22/bin/node");
  const nvmRoot = path.join(process.env.HOME ?? "", ".nvm", "versions", "node");
  try {
    for (const entry of readdirSync(nvmRoot)) {
      if (entry.startsWith("v22.")) homes.push(path.join(nvmRoot, entry, "bin", "node"));
    }
  } catch {
    // No nvm tree on this machine; the other candidates still apply.
  }
  for (const candidate of homes) {
    const attempt = spawnSync(candidate, [fileURLToPath(import.meta.url), ...process.argv.slice(2)], {
      stdio: "inherit",
      env: { ...process.env, FORGE_RUNTIME_PICKED: "1" },
    });
    if (attempt.error === undefined || attempt.error === null) {
      process.exit(attempt.status ?? 1);
    }
  }
  // No Node 22 anywhere: carry on with the runtime we have.
}

let child: ChildProcess | undefined;
let windingDown = false;

function signalCore(how: NodeJS.Signals): void {
  if (child !== undefined && child.exitCode === null && !child.killed) {
    child.kill(how);
  }
}

// A stop signal aimed at the launcher is forwarded to the core first;
// the trial should never see a launcher outlive its forge.
for (const name of ["SIGINT", "SIGTERM"] as const) {
  process.on(name, () => signalCore(name));
}

function pause(ms: number): Promise<void> {
  return new Promise((done) => setTimeout(done, ms));
}

// Start the core and settle with the port named in its banner line.
function startCore(): Promise<number> {
  return new Promise((settle, fail) => {
    const proc = spawn(coreBinary, [], {
      cwd: repoDir,
      env: { ...process.env, FORGE_PORT: "0" },
      stdio: ["inherit", "pipe", "inherit"],
    });
    child = proc;
    let heard = "";
    let settled = false;
    proc.stdout?.on("data", (chunk: Buffer) => {
      process.stdout.write(chunk);
      if (settled) return;
      heard += chunk.toString("utf8");
      const match = /listening on port (\d+)/.exec(heard);
      if (match !== null) {
        settled = true;
        settle(Number(match[1]));
      }
    });
    proc.on("error", (err) => {
      if (!settled) {
        settled = true;
        fail(err);
      }
    });
    proc.on("exit", (code, signal) => {
      if (!settled) {
        settled = true;
        fail(new Error(`forge stopped before printing its port (code ${code}, signal ${signal})`));
      } else if (!windingDown) {
        // The core ended while the trial still had it: follow it out.
        process.exit(code ?? 1);
      }
    });
  });
}

// POST the registration, retrying while the environment warms up.
async function announce(port: number): Promise<void> {
  const giveUp = Date.now() + 120_000;
  for (;;) {
    try {
      const res = await fetch(`${envOrigin}/register`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ port }),
      });
      if (res.ok) {
        await res.text();
        return;
      }
    } catch {
      // Environment not accepting yet; the loop retries.
    }
    if (Date.now() > giveUp) {
      throw new Error("registration with the trial environment never succeeded");
    }
    await pause(250);
  }
}

// Poll the done endpoint until the environment ends the trial.
async function awaitDone(): Promise<void> {
  for (;;) {
    try {
      const res = await fetch(`${envOrigin}/done`);
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // A failed poll carries no information; keep waiting.
    }
    await pause(400);
  }
}

async function run(): Promise<void> {
  const port = await startCore();
  if (envOrigin === "") {
    // No environment to report to (a local run): sit on the core.
    await new Promise<void>((done) => child?.once("exit", () => done()));
    return;
  }
  await announce(port);
  await awaitDone();
  windingDown = true;
  signalCore("SIGTERM");
  await Promise.race([
    new Promise<void>((done) => child?.once("exit", () => done())),
    pause(2_000).then(() => signalCore("SIGKILL")),
  ]);
  process.exit(0);
}

run().catch((err: unknown) => {
  console.error("[launcher]", err);
  signalCore("SIGKILL");
  process.exit(1);
});
