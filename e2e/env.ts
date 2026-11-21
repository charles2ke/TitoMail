/** Settings shared by the Playwright config and the tests. */

const port = (name: string, fallback: number) => Number(process.env[name] || fallback);

// Unusual defaults so the suite can run next to a development server.
export const ports = {
  http: port("E2E_HTTP_PORT", 18480),
  smtp: port("E2E_SMTP_PORT", 12625),
  submission: port("E2E_SUBMISSION_PORT", 11687),
  imap: port("E2E_IMAP_PORT", 11243),
};

export const baseURL = `http://127.0.0.1:${ports.http}`;
export const domain = "example.test";
export const admin = { email: `admin@${domain}`, password: "correct-horse-battery", name: "Admin" };
