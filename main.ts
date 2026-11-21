// Launcher for the forge: spawns the vendored Go binary, registers its
// port with the make.fun environment, waits for the trial to finish, then
// shuts the binary down. The hot path lives entirely in the binary; this
// file only owns the lifecycle.
import { spawn, type ChildProcess } from "node:child_process";
import { copyFileSync, chmodSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const envUrl = (process.env.MAKEFUN_ENV_URL ?? "").replace(/\/+$/, "");

if (!envUrl) {
  console.error("forge launcher: MAKEFUN_ENV_URL is not set");
  process.exit(1);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// Stage the binary somewhere executable. /bot is read-only and the exec bit
// on the committed file is not guaranteed to survive, so copy it to a temp
// directory and chmod it there. Fall back to running it in place.
function stageBinary(): string {
  const committed = join(here, "forge");
  try {
    const dir = mkdtempSync(join(tmpdir(), "forge-"));
    const staged = join(dir, "forge");
    copyFileSync(committed, staged);
    chmodSync(staged, 0o755);
    return staged;
  } catch (error) {
    console.error(`forge launcher: could not stage the binary (${error}); trying it in place`);
    try {
      chmodSync(committed, 0o755);
    } catch {
      // The read-only mount may refuse; the exec bit may already be set.
    }
    return committed;
  }
}

async function readPort(child: ChildProcess): Promise<number> {
  return new Promise((resolve, reject) => {
    let buffer = "";
    const timer = setTimeout(() => {
      reject(new Error("timed out waiting for FORGE_PORT"));
    }, 15000);
    child.stdout?.on("data", (chunk: Buffer) => {
      buffer += chunk.toString("utf8");
      const match = buffer.match(/FORGE_PORT=(\d+)/);
      if (match) {
        clearTimeout(timer);
        resolve(Number(match[1]));
      }
    });
    child.on("exit", (code) => {
      clearTimeout(timer);
      reject(new Error(`forge exited early with code ${code}`));
    });
    child.on("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
  });
}

async function register(port: number): Promise<void> {
  let delay = 250;
  for (;;) {
    let res: Response;
    try {
      res = await fetch(`${envUrl}/register`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ port }),
      });
    } catch (error) {
      console.error(`forge launcher: register failed (${error}); retrying in ${delay}ms`);
      await sleep(delay);
      delay = Math.min(delay * 2, 2000);
      continue;
    }
    if (res.status === 200) return;
    if (res.status === 409) {
      // Another entry is registered; keep waiting.
      await sleep(delay);
      delay = Math.min(delay * 2, 2000);
      continue;
    }
    if (res.status === 400 || res.status === 405) {
      throw new Error(`register rejected with ${res.status}`);
    }
    console.error(`forge launcher: register returned ${res.status}; retrying in ${delay}ms`);
    await sleep(delay);
    delay = Math.min(delay * 2, 2000);
  }
}

async function waitForDone(): Promise<void> {
  for (;;) {
    try {
      const res = await fetch(`${envUrl}/done`);
      if (res.ok) {
        const body = (await res.json()) as { done?: boolean };
        if (body.done) return;
      }
    } catch {
      // The env may be briefly unreachable; keep polling.
    }
    await sleep(250);
  }
}

async function main(): Promise<void> {
  const binary = stageBinary();
  const child = spawn(binary, [], {
    env: process.env,
    stdio: ["ignore", "pipe", "inherit"],
  });
  let port: number;
  try {
    port = await readPort(child);
  } catch (error) {
    console.error(`forge launcher: ${error}`);
    child.kill("SIGKILL");
    process.exit(1);
  }
  console.error(`forge launcher: binary listening on port ${port}`);
  try {
    await register(port);
  } catch (error) {
    console.error(`forge launcher: ${error}`);
    child.kill("SIGKILL");
    process.exit(1);
  }
  await waitForDone();
  child.kill("SIGTERM");
  const exited = await Promise.race([
    new Promise<boolean>((resolve) => child.on("exit", () => resolve(true))),
    sleep(2000).then(() => false),
  ]);
  if (!exited) child.kill("SIGKILL");
  // Clean up the staged copy when it lives in the temp directory.
  if (binary.startsWith(tmpdir())) {
    try {
      rmSync(dirname(binary), { recursive: true, force: true });
    } catch {
      // Best effort only.
    }
  }
  process.exit(0);
}

main().catch((error) => {
  console.error(`forge launcher: ${error}`);
  process.exit(1);
});
