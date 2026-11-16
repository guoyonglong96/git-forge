import { createApp } from "./app.ts";

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

const main = async (): Promise<void> => {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === "string") {
    throw new Error("the server did not bind an ephemeral TCP port");
  }
  process.stdout.write(`[entry] listening on port ${address.port}\n`);
  await register(address.port);
  process.stdout.write("[entry] registered; waiting for the env to say done\n");
  await waitForDone();
  server.close();
  process.stdout.write("[entry] done\n");
};

main().then(
  () => process.exit(0),
  (err: unknown) => {
    process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
    process.exit(1);
  },
);
