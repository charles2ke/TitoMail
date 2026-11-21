import { expect, request, type APIRequestContext, type Page, type TestInfo } from "@playwright/test";
import net from "node:net";
import path from "node:path";
import zlib from "node:zlib";
import { admin, baseURL, domain, ports } from "./env";

export { admin, domain };

export interface Account {
  email: string;
  password: string;
  name: string;
}

// ---------------------------------------------------------------------------
// Accounts and sessions

/** Signs in through the JSON API and returns a request context holding the session. */
export async function apiSession(account: Account): Promise<{ api: APIRequestContext; csrf: string }> {
  const api = await request.newContext({ baseURL });
  const res = await api.post("/api/login", { data: { email: account.email, password: account.password } });
  expect(res.status(), await res.text()).toBe(200);
  const { csrf_token } = (await res.json()) as { csrf_token: string };
  return { api, csrf: csrf_token };
}

/** Creates a user as the administrator. An existing user with the same address is reused. */
export async function createUser(local: string, name: string, password = "s3cret-passw0rd"): Promise<Account> {
  const account = { email: `${local}@${domain}`, password, name };
  const { api, csrf } = await apiSession(admin);
  try {
    const res = await api.post("/api/admin/users", {
      data: { email: account.email, password, display_name: name },
      headers: { "X-CSRF-Token": csrf },
    });
    if (res.status() !== 201 && res.status() !== 409) {
      throw new Error(`creating ${account.email} failed: ${res.status()} ${await res.text()}`);
    }
  } finally {
    await api.dispose();
  }
  return account;
}

/** Signs the page in without going through the login form, then opens the inbox. */
export async function signIn(page: Page, account: Account) {
  const res = await page.request.post("/api/login", { data: { email: account.email, password: account.password } });
  expect(res.status(), await res.text()).toBe(200);
  await page.goto("/mail/inbox");
  await expect(page.getByRole("button", { name: "Compose" })).toBeVisible();
}

// ---------------------------------------------------------------------------
// Screenshots

const shotsDir = path.join(__dirname, "screenshots");

/** Saves a full-window screenshot to e2e/screenshots and attaches it to the report. */
export async function snap(page: Page, testInfo: TestInfo, name: string) {
  // Let pending layout and paint finish so the image isn't a stale frame.
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  await page.evaluate(() => document.fonts.ready.then(() => undefined));
  const body = await page.screenshot({ path: path.join(shotsDir, `${name}.png`), animations: "disabled", caret: "hide" });
  await testInfo.attach(name, { body, contentType: "image/png" });
}

// ---------------------------------------------------------------------------
// Building messages

export type Addr = string | { name: string; address: string };

export interface Attachment {
  filename: string;
  contentType: string;
  content: Buffer;
}

export interface Mail {
  from: Addr;
  to: Addr[];
  cc?: Addr[];
  subject: string;
  text?: string;
  html?: string;
  date?: Date;
  messageId?: string;
  inReplyTo?: string;
  references?: string[];
  headers?: Record<string, string>;
  attachments?: Attachment[];
}

const ascii = (s: string) => /^[\x20-\x7e]*$/.test(s);
const encodeWord = (s: string) => (ascii(s) ? s : `=?UTF-8?B?${Buffer.from(s).toString("base64")}?=`);
const addressOf = (a: Addr) => (typeof a === "string" ? a : a.address);

function formatAddr(a: Addr): string {
  if (typeof a === "string") return a;
  const name = ascii(a.name) ? `"${a.name.replace(/["\\]/g, "\\$&")}"` : encodeWord(a.name);
  return `${name} <${a.address}>`;
}

function rfc5322Date(d: Date): string {
  return d.toUTCString().replace(/GMT$/, "+0000");
}

interface Entity {
  headers: string[];
  body: string;
}

const base64Lines = (b: Buffer) => b.toString("base64").replace(/.{76}/g, "$&\r\n");
const render = (e: Entity) => `${e.headers.join("\r\n")}\r\n\r\n${e.body}`;

function leaf(contentType: string, content: Buffer, extra: string[] = []): Entity {
  return { headers: [`Content-Type: ${contentType}`, "Content-Transfer-Encoding: base64", ...extra], body: base64Lines(content) };
}

function multipart(subtype: string, parts: Entity[]): Entity {
  const boundary = `=_${subtype}_${Math.random().toString(36).slice(2)}`;
  return {
    headers: [`Content-Type: multipart/${subtype}; boundary="${boundary}"`],
    body: parts.map((p) => `--${boundary}\r\n${render(p)}\r\n`).join("") + `--${boundary}--\r\n`,
  };
}

let counter = 0;

/** Returns a new unique Message-ID. */
export function messageId(): string {
  return `<e2e.${Date.now().toString(36)}.${(counter++).toString(36)}@e2e.test>`;
}

/** Renders a message as RFC 5322 text with a MIME body. */
export function buildMessage(m: Mail): string {
  let content = leaf("text/plain; charset=utf-8", Buffer.from(m.text ?? ""));
  if (m.html) content = multipart("alternative", [content, leaf("text/html; charset=utf-8", Buffer.from(m.html))]);
  if (m.attachments?.length) {
    content = multipart("mixed", [
      content,
      ...m.attachments.map((a) =>
        leaf(`${a.contentType}; name="${a.filename}"`, a.content, [`Content-Disposition: attachment; filename="${a.filename}"`]),
      ),
    ]);
  }
  const headers = [
    `From: ${formatAddr(m.from)}`,
    `To: ${m.to.map(formatAddr).join(", ")}`,
    ...(m.cc?.length ? [`Cc: ${m.cc.map(formatAddr).join(", ")}`] : []),
    `Subject: ${encodeWord(m.subject)}`,
    `Date: ${rfc5322Date(m.date ?? new Date())}`,
    `Message-ID: ${m.messageId ?? messageId()}`,
    ...(m.inReplyTo ? [`In-Reply-To: ${m.inReplyTo}`] : []),
    ...(m.references?.length ? [`References: ${m.references.join(" ")}`] : []),
    ...Object.entries(m.headers ?? {}).map(([k, v]) => `${k}: ${v}`),
    "MIME-Version: 1.0",
    ...content.headers,
  ];
  return `${headers.join("\r\n")}\r\n\r\n${content.body}`;
}

// ---------------------------------------------------------------------------
// SMTP

/** A minimal line-oriented SMTP client, enough to hand messages to the MX listener. */
class SMTPConnection {
  private buf = "";
  private lines: string[] = [];
  private wake: (() => void) | null = null;
  private failure: Error | null = null;

  constructor(private sock: net.Socket) {
    sock.setEncoding("utf8");
    sock.on("data", (chunk: string) => {
      this.buf += chunk;
      for (let i = this.buf.indexOf("\r\n"); i >= 0; i = this.buf.indexOf("\r\n")) {
        this.lines.push(this.buf.slice(0, i));
        this.buf = this.buf.slice(i + 2);
      }
      this.wake?.();
    });
    const fail = (err: Error) => {
      this.failure ??= err;
      this.wake?.();
    };
    sock.on("error", fail);
    sock.on("close", () => fail(new Error("SMTP connection closed")));
  }

  private async line(): Promise<string> {
    while (this.lines.length === 0) {
      if (this.failure) throw this.failure;
      await new Promise<void>((resolve) => (this.wake = resolve));
      this.wake = null;
    }
    return this.lines.shift()!;
  }

  async reply(expected: number): Promise<string> {
    const text: string[] = [];
    for (;;) {
      const l = await this.line();
      text.push(l);
      if (l[3] !== "-") break;
    }
    if (text[0][0] !== String(expected)[0]) throw new Error(`SMTP: expected ${expected}, got "${text.join(" / ")}"`);
    return text.join("\n");
  }

  async command(line: string, expected: number): Promise<string> {
    this.sock.write(`${line}\r\n`);
    return this.reply(expected);
  }
}

/** Delivers raw message data to the server's MX listener, as another mail server would. */
export async function deliver(mailFrom: string, rcpts: string[], data: string): Promise<void> {
  const sock = net.connect(ports.smtp, "127.0.0.1");
  sock.setTimeout(20_000, () => sock.destroy(new Error("SMTP timeout")));
  const c = new SMTPConnection(sock);
  try {
    await c.reply(220);
    await c.command("EHLO e2e.test", 250);
    await c.command(`MAIL FROM:<${mailFrom}>`, 250);
    for (const r of rcpts) await c.command(`RCPT TO:<${r}>`, 250);
    await c.command("DATA", 354);
    const body = data.replace(/\r?\n/g, "\r\n").replace(/^\./gm, "..");
    await c.command(`${body}${body.endsWith("\r\n") ? "" : "\r\n"}.`, 250);
    await c.command("QUIT", 221);
  } finally {
    sock.destroy();
  }
}

/** Builds a message and delivers it to its To and Cc addresses, which must be local. */
export async function sendMail(m: Mail): Promise<string> {
  return sendTo(m, [...m.to, ...(m.cc ?? [])].map(addressOf));
}

/** Builds a message and delivers it to the given envelope recipients. Returns its Message-ID. */
export async function sendTo(m: Mail, rcpts: string[]): Promise<string> {
  const id = m.messageId ?? messageId();
  await deliver(addressOf(m.from), rcpts, buildMessage({ ...m, messageId: id }));
  return id;
}

// ---------------------------------------------------------------------------
// Fixtures

function pngChunk(type: string, data: Buffer): Buffer {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, "latin1"), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(zlib.crc32(body));
  return Buffer.concat([len, body, crc]);
}

/** Draws a simple landscape picture: a sky gradient, a sun and green hills. */
export function landscapePNG(width = 480, height = 300): Buffer {
  const rows: Buffer[] = [];
  for (let y = 0; y < height; y++) {
    const row = Buffer.alloc(1 + width * 3);
    for (let x = 0; x < width; x++) {
      const t = y / height;
      let rgb = [Math.round(70 + 120 * t), Math.round(130 + 90 * t), 235];
      if ((x - width * 0.72) ** 2 + (y - height * 0.3) ** 2 < (height * 0.11) ** 2) rgb = [255, 204, 77];
      const hill = height * 0.68 + Math.sin(x / 45) * 18 + Math.cos(x / 23) * 6;
      if (y > hill) rgb = [Math.round(52 + 30 * Math.sin(x / 60)), 140 - Math.round((y - hill) / 4), 72];
      row.set(rgb, 1 + x * 3);
    }
    rows.push(row);
  }
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr.set([8, 2, 0, 0, 0], 8); // 8-bit RGB, no interlace
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    pngChunk("IHDR", ihdr),
    pngChunk("IDAT", zlib.deflateSync(Buffer.concat(rows))),
    pngChunk("IEND", Buffer.alloc(0)),
  ]);
}

/** Returns a small, valid one-page PDF showing the given lines of text. */
export function simplePDF(lines: string[]): Buffer {
  const text = lines.map((l, i) => `BT /F1 14 Tf 72 ${720 - i * 24} Td (${l.replace(/[()\\]/g, "\\$&")}) Tj ET`).join("\n");
  const objects = [
    "<< /Type /Catalog /Pages 2 0 R >>",
    "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
    "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
    `<< /Length ${Buffer.byteLength(text)} >>\nstream\n${text}\nendstream`,
    "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
  ];
  let out = "%PDF-1.4\n";
  const offsets: number[] = [];
  objects.forEach((o, i) => {
    offsets.push(Buffer.byteLength(out));
    out += `${i + 1} 0 obj\n${o}\nendobj\n`;
  });
  const xref = Buffer.byteLength(out);
  out += `xref\n0 ${objects.length + 1}\n0000000000 65535 f \n`;
  out += offsets.map((o) => `${String(o).padStart(10, "0")} 00000 n \n`).join("");
  out += `trailer\n<< /Size ${objects.length + 1} /Root 1 0 R >>\nstartxref\n${xref}\n%%EOF\n`;
  return Buffer.from(out, "latin1");
}

// ---------------------------------------------------------------------------
// Sample mail

const team = { name: "TitoMail Team", address: "team@titomail.test" };
const grace = { name: "Grace Hopper", address: "grace@navy.test" };
const alan = { name: "Alan Turing", address: "alan@bletchley.test" };
const ada = { name: "Ada Lovelace", address: "ada@engine.test" };

/** Fills a mailbox with a realistic mix of messages, oldest first. */
export async function seedMailbox(to: Account) {
  const me = { name: to.name, address: to.email };
  // Only the local recipient gets a copy: the MX listener refuses to relay to other domains.
  const sendMail = (m: Mail) => sendTo(m, [to.email]);
  await sendMail({
    from: { name: "The Weekly Byte", address: "hello@weeklybyte.test" },
    to: [me],
    subject: "This week: Go 1.27, SQLite tips and a tiny SMTP server",
    text: "Your weekly digest of programming news.\n\nGo 1.27 ships range-over-func improvements...\n",
    html:
      '<div style="font-family:Georgia,serif;max-width:560px"><h2 style="color:#b5452a">The Weekly Byte</h2>' +
      "<p>Your weekly digest of programming news.</p><ul><li><b>Go 1.27</b> ships iterator improvements</li>" +
      "<li>Five SQLite pragmas worth knowing</li><li>Writing a tiny SMTP server</li></ul>" +
      '<p style="color:#777;font-size:12px">You are receiving this because you subscribed.</p></div>',
    headers: { "List-Unsubscribe": "<mailto:unsubscribe@weeklybyte.test>", "List-Id": "<weekly.weeklybyte.test>" },
  });
  await sendMail({
    from: { name: "Acme Billing", address: "billing@acme.test" },
    to: [me],
    subject: "Your invoice #4821 for October",
    text: `Hi ${to.name.split(" ")[0]},\n\nYour invoice for October is attached. The total of $42.00 will be charged on November 15.\n\nThanks,\nAcme Billing`,
    attachments: [{ filename: "invoice-4821.pdf", contentType: "application/pdf", content: simplePDF(["Acme Inc.", "Invoice #4821", "Total: $42.00"]) }],
  });
  const lunch = await sendMail({
    from: alan,
    to: [me, ada],
    subject: "Lunch on Friday?",
    text: "Shall we try the new noodle place on Friday at 12:30?\n\nAlan",
  });
  await sendMail({
    from: ada,
    to: [alan],
    cc: [me],
    subject: "Re: Lunch on Friday?",
    inReplyTo: lunch,
    references: [lunch],
    text: "Count me in! I'll book a table for three.\n\nAda\n\nOn Friday, Alan Turing wrote:\n> Shall we try the new noodle place on Friday at 12:30?\n>\n> Alan\n",
  });
  await sendMail({
    from: { name: "GitHub", address: "notifications@github.test" },
    to: [me],
    subject: "[titomail] CI passed on main",
    html:
      '<table style="font-family:-apple-system,Segoe UI,sans-serif;border:1px solid #d0d7de;border-radius:6px;padding:16px">' +
      '<tr><td><h3 style="margin:0 0 8px">✅ All checks have passed</h3>' +
      "<p>3 successful checks on <code>main</code>: go test, web build, e2e.</p>" +
      '<a href="https://github.test/titomail/actions" style="background:#1f883d;color:#fff;padding:6px 12px;border-radius:6px;text-decoration:none">View workflow run</a>' +
      "</td></tr></table>",
    text: "All checks have passed.",
    headers: { "List-Unsubscribe": "<https://github.test/unsubscribe>" },
  });
  await sendMail({
    from: grace,
    to: [me],
    subject: "Photos from the offsite",
    text: "Here's my favourite shot from the offsite. The view from the hill was worth the climb!\n\nGrace",
    attachments: [{ filename: "offsite.png", contentType: "image/png", content: landscapePNG() }],
  });
  await sendMail({
    from: team,
    to: [me],
    subject: "Welcome to TitoMail",
    html:
      '<div style="font-family:system-ui,sans-serif;max-width:560px;line-height:1.5">' +
      `<h1 style="color:#1a73e8;font-weight:500">Welcome, ${to.name.split(" ")[0]}!</h1>` +
      "<p>Your mailbox is ready. A few things to try:</p><ol>" +
      "<li>Press <b>c</b> to write a message and <b>?</b> to see every keyboard shortcut.</li>" +
      "<li>Add labels to keep related mail together.</li>" +
      "<li>Connect a desktop or phone app over IMAP from <i>Settings → Mail apps</i>.</li></ol></div>",
    text: "Your mailbox is ready.",
  });
}
