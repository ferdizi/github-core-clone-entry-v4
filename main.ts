// Entry point: launch the committed static Go core (`./forge`), tell the
// trial env where it is, then wait until the env says the trial is over and
// exit 0. The Go binary binds an ephemeral port and prints `READY <port>`;
// this launcher reads that line and runs the MAKEFUN register/done protocol
// exactly as the Node entrypoint did.
//
// With MAKEFUN_ENV_URL set (a trial): POST {"port": N} to $MAKEFUN_ENV_URL/register
// (the first register wins; a 409 means another entry already registered and
// this process simply waits), then poll GET $MAKEFUN_ENV_URL/done until it
// answers {"done": true}. Never bind 8080: the env owns it.
// Without it (a local run): serve on the ephemeral port and keep running until
// stopped, printing the port so a local grader can dial it.

import { spawn, type ChildProcess } from 'node:child_process';
import { createInterface } from 'node:readline';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_START_MS = 250;
const RETRY_MAX_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function backoff(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_START_MS * 2 ** Math.min(attempt, 8));
}

/** POST the bound port to the env until it accepts (200) or reports a winner (409). */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(backoff(attempt));
  }
}

/** Poll GET /done until the env answers {"done": true}. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet: it is not done, so poll again.
    }
    await sleep(backoff(attempt));
  }
}

/** Spawn the Go core and wait for its `READY <port>` line. */
function spawnCore(): Promise<{ child: ChildProcess; port: number }> {
  const binary = join(dirname(fileURLToPath(import.meta.url)), 'forge');
  const child = spawn(binary, [], { stdio: ['ignore', 'pipe', 'inherit'] });
  return new Promise((resolve, reject) => {
    const lines = createInterface({ input: child.stdout! });
    const onLine = (line: string): void => {
      const match = /^READY (\d+)$/.exec(line.trim());
      if (match === null) return;
      lines.close();
      resolve({ child, port: Number(match[1]) });
    };
    lines.on('line', onLine);
    child.once('error', reject);
    child.once('exit', (code) => {
      reject(new Error(`forge exited before READY (code ${code ?? '?'})`));
    });
  });
}

async function main(): Promise<void> {
  const { child, port } = await spawnCore();
  process.stdout.write(`[entry] listening on port ${port}\n`);
  let stopping = false;
  const stopChild = (): void => {
    if (stopping) return;
    stopping = true;
    child.kill('SIGTERM');
  };
  process.once('SIGTERM', stopChild);
  process.once('SIGINT', stopChild);
  if (ENV_URL === '') {
    process.stdout.write('[entry] no MAKEFUN_ENV_URL: serving locally until stopped\n');
    await new Promise<void>((resolve) => {
      child.once('exit', () => resolve());
    });
    return;
  }
  await register(port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');
  await waitForDone();
  stopChild();
  await new Promise<void>((resolve) => {
    child.once('exit', () => resolve());
  });
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  if (ENV_URL !== '') process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
