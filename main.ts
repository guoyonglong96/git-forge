import { spawn, type ChildProcess } from "node:child_process";
import { chmodSync, copyFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const rawEnv = process.env.MAKEFUN_ENV_URL;
let envOrigin = rawEnv === undefined ? "" : rawEnv;
while (envOrigin.length > 0 && envOrigin.charAt(envOrigin.length - 1) === "/") {
  envOrigin = envOrigin.slice(0, -1);
}
if (envOrigin === "") {
  process.stderr.write("[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n");
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
};

const retryDelay = (attempt: number): number => {
  const capped = attempt > 8 ? 8 : attempt;
  const delay = RETRY_INITIAL_MS * 2 ** capped;
  return delay > RETRY_MAX_MS ? RETRY_MAX_MS : delay;
};

const register = async (port: number): Promise<void> => {
  let attempt = 0;
  while (true) {
    let done = false;
    try {
      const res = await fetch(`${envOrigin}/register`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) {
        done = true;
      } else if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.indexOf("register answered HTTP ") === 0) throw err;
    }
    if (done) return;
    await sleep(retryDelay(attempt));
    attempt += 1;
  }
};

const waitForDone = async (): Promise<void> => {
  let attempt = 0;
  while (true) {
    let finished = false;
    try {
      const res = await fetch(`${envOrigin}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        finished = body.done === true;
      }
    } catch {
      finished = false;
    }
    if (finished) return;
    await sleep(retryDelay(attempt));
    attempt += 1;
  }
};

// Try to start the Go server from one binary path. Resolves once the child
// prints "[entry] listening on port <N>". Rejects on error, early exit, or
// if the line does not appear within the boot budget.
// The child's stdio is forwarded so the trial's entry.log keeps its logs.
const trySpawn = (binPath: string): Promise<{ child: ChildProcess; port: number }> => {
  return new Promise((resolve, reject) => {
    const child = spawn(binPath, [], { stdio: ["ignore", "pipe", "pipe"], env: process.env });
    let settled = false;
    let stdoutBuf = "";
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      child.kill("SIGKILL");
      reject(new Error("the Go server did not print its listening port in time"));
    }, 15_000);
    child.stdout?.on("data", (chunk: Buffer) => {
      process.stdout.write(chunk);
      if (settled) return;
      stdoutBuf += chunk.toString("utf8");
      const m = stdoutBuf.match(/\[entry\] listening on port (\d+)/);
      if (m) {
        settled = true;
        clearTimeout(timer);
        resolve({ child, port: Number(m[1]) });
      }
    });
    child.stderr?.on("data", (chunk: Buffer) => {
      process.stderr.write(chunk);
    });
    child.once("error", (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(err);
    });
    child.once("exit", (code, signal) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(new Error(`the Go server exited early with code ${code} signal ${signal}`));
    });
  });
};

// Spawn ./forge (resolved relative to this file, never via $PATH or cwd)
// and wait for its "[entry] listening on port <N>" line on stdout.
// The vendored binary may lack the executable bit (GitHub web upload stores
// it as 100644) and the repo may be read-only on the grading host, so:
//  1. try chmod + spawn in place;
//  2. if that fails, copy the binary to the OS temp dir, chmod the copy,
//     and spawn the copy (the repo is only read, never written, there).
// Rejects if neither attempt starts the server.
const spawnForge = async (): Promise<{ child: ChildProcess; port: number }> => {
  const forgePath = fileURLToPath(new URL("./forge", import.meta.url));
  // GitHub web upload drops the executable bit (mode 100644), so make the
  // vendored binary executable here before spawning it. Best-effort: if
  // this fails, spawn below surfaces the real error.
  try {
    chmodSync(forgePath, 0o755);
  } catch {
    // fall through; spawn will fail with the underlying reason
  }
  try {
    return await trySpawn(forgePath);
  } catch (err) {
    process.stderr.write(
      `[entry] direct spawn failed (${err instanceof Error ? err.message : String(err)}); trying a copy in the temp dir\n`,
    );
  }
  const tmpPath = join(tmpdir(), `forge-${process.pid}`);
  copyFileSync(forgePath, tmpPath);
  chmodSync(tmpPath, 0o755);
  return await trySpawn(tmpPath);
};

const main = async (): Promise<void> => {
  const { child, port } = await spawnForge();
  // If the child dies before /done, exit 1 promptly instead of polling
  // /done forever.
  let finished = false;
  child.once("exit", (code, signal) => {
    if (finished) return;
    process.stderr.write(`[entry] Go server exited early with code ${code} signal ${signal}\n`);
    process.exit(1);
  });
  await register(port);
  process.stdout.write("[entry] registered; waiting for the env to say done\n");
  await waitForDone();
  finished = true;
  child.kill("SIGTERM");
  await Promise.race([
    new Promise<void>((resolve) => child.once("exit", () => resolve())),
    sleep(2000).then(() => {
      child.kill("SIGKILL");
    }),
  ]);
  process.stdout.write("[entry] done\n");
};

main().then(
  () => process.exit(0),
  (err: unknown) => {
    process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  },
);
