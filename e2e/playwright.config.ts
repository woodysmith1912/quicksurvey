import { defineConfig, devices } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, rmSync } from 'node:fs';
import path from 'node:path';
import { ADMIN, BASE_URL, EXTERNAL_URL, PORT } from './env';

// Two ways to run, and the difference is only where the server comes from.
//
//   make e2e         builds the binary and lets Playwright launch it here
//   make e2e-docker  points at a container that is already up (QS_E2E_BASE_URL)
//
// In the local mode the data directory is seeded below; in the container mode
// the compose service seeds itself before it starts serving.
const local = !EXTERNAL_URL;
const repo = path.resolve(__dirname, '..');
const binary = path.join(repo, 'quicksurvey');
const dataDir = path.join(__dirname, '.tmp', 'data');

if (local) {
  if (!existsSync(binary)) {
    throw new Error(`${binary} not found — run "make build", or use "make e2e" which does it for you`);
  }
  // Seed at config load rather than in globalSetup, so it is guaranteed to
  // happen before the web server launches and fails loudly if it cannot.
  rmSync(path.join(__dirname, '.tmp'), { recursive: true, force: true });
  mkdirSync(dataDir, { recursive: true });
  execFileSync(binary, ['user', 'add', '-data', dataDir, '-name', ADMIN.user, '-role', 'admin'], {
    env: { ...process.env, QS_PASSWORD: ADMIN.password },
    stdio: 'inherit',
  });
}

export default defineConfig({
  testDir: './tests',
  // One worker against one server and one data directory: these tests share
  // state on purpose, because that is how the application is deployed.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: [['list'], ['html', { open: 'never' }]],
  // Every interaction is capped at 3 seconds. This application does no work
  // that can legitimately take longer — no external calls, no queries, just a
  // map lookup and a template — so a slower response is a defect, not a wait.
  // The per-test budget is a backstop for a test that hangs between steps.
  timeout: 15_000,
  expect: { timeout: 3_000 },
  use: {
    baseURL: BASE_URL,
    actionTimeout: 3_000,
    navigationTimeout: 3_000,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: local
    ? {
        command: [
          binary, 'serve',
          '-data', dataDir,
          '-addr', `127.0.0.1:${PORT}`,
          '-base-url', BASE_URL,
          '-tz', 'UTC',
          // A Secure cookie would be dropped by the browser over plain http://.
          '-secure-cookies=false',
        ].join(' '),
        url: `${BASE_URL}/healthz`,
        reuseExistingServer: false,
        stdout: 'pipe',
        stderr: 'pipe',
        timeout: 20_000,
      }
    : undefined,
});
