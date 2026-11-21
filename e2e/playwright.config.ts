import { defineConfig } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";
import { admin, baseURL, ports } from "./env";

const bin = path.resolve(process.env.TITOMAIL_BIN || path.join(__dirname, "..", "bin", "titomail"));
if (!fs.existsSync(bin)) {
  throw new Error(`TitoMail binary not found at ${bin}. Run "make build" first or set TITOMAIL_BIN.`);
}

// Every run starts from an empty data directory with a bootstrapped administrator.
// Remote delivery is disabled so nothing leaves the machine; mail to other
// domains stays in the queue.
const serve = [
  `"${bin}" serve`,
  "-data-dir .data",
  "-hostname mail.example.test",
  `-public-url ${baseURL}`,
  `-http-addr 127.0.0.1:${ports.http}`,
  `-smtp-addr 127.0.0.1:${ports.smtp}`,
  `-submission-addr 127.0.0.1:${ports.submission}`,
  `-imap-addr 127.0.0.1:${ports.imap}`,
  `-admin-email ${admin.email}`,
  `-admin-password ${admin.password}`,
  "-allow-insecure-auth",
  "-disable-outbound",
  "-inbound-auth-checks=false",
  "-log-level warn",
].join(" ");

export default defineConfig({
  testDir: "./tests",
  outputDir: "./test-results",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  // The tests share one server, so run them one at a time and don't retry:
  // a retry would replay setup against data the first attempt already changed.
  workers: 1,
  fullyParallel: false,
  retries: 0,
  forbidOnly: !!process.env.CI,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  use: {
    baseURL,
    // Set PW_CHANNEL=chrome to use an installed Google Chrome instead of
    // Playwright's bundled Chromium.
    channel: process.env.PW_CHANNEL || undefined,
    viewport: { width: 1440, height: 900 },
    colorScheme: "light",
    locale: "en-US",
    timezoneId: "UTC",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  webServer: {
    command: `rm -rf .data && ${serve}`,
    cwd: __dirname,
    url: `${baseURL}/healthz`,
    reuseExistingServer: false,
    timeout: 30_000,
    stdout: "ignore",
    stderr: "pipe",
    gracefulShutdown: { signal: "SIGTERM", timeout: 5_000 },
  },
});
