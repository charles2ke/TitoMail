import type { Address, Conversation, QueueEntry } from "./types";

const timeFmt = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });
const monthDayFmt = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" });
const shortDateFmt = new Intl.DateTimeFormat(undefined, { year: "2-digit", month: "numeric", day: "numeric" });
const fullFmt = new Intl.DateTimeFormat(undefined, {
  weekday: "short",
  year: "numeric",
  month: "short",
  day: "numeric",
  hour: "numeric",
  minute: "2-digit",
});
const relFmt = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });

function validDate(iso: string | undefined): Date | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) || d.getFullYear() < 1971 ? null : d;
}

/** Formats a date for the message list, like Gmail: time today, "Mar 4" this year, else a short date. */
export function listDate(iso: string): string {
  const d = validDate(iso);
  if (!d) return "";
  const now = new Date();
  if (d.toDateString() === now.toDateString()) return timeFmt.format(d);
  if (d.getFullYear() === now.getFullYear()) return monthDayFmt.format(d);
  return shortDateFmt.format(d);
}

export function fullDate(iso: string): string {
  const d = validDate(iso);
  return d ? fullFmt.format(d) : "";
}

export function relativeTime(iso: string): string {
  const d = validDate(iso);
  if (!d) return "";
  const diff = (d.getTime() - Date.now()) / 1000;
  const abs = Math.abs(diff);
  if (abs < 60) return relFmt.format(Math.round(diff), "second");
  if (abs < 3600) return relFmt.format(Math.round(diff / 60), "minute");
  if (abs < 86400) return relFmt.format(Math.round(diff / 3600), "hour");
  if (abs < 86400 * 30) return relFmt.format(Math.round(diff / 86400), "day");
  return "";
}

export function messageDate(iso: string): string {
  const rel = relativeTime(iso);
  const full = fullDate(iso);
  return rel ? `${full} (${rel})` : full;
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

export function displayName(a: Address | undefined | null): string {
  if (!a) return "";
  return a.name || a.address;
}

export function formatAddress(a: Address): string {
  return a.name ? `${a.name} <${a.address}>` : a.address;
}

export function firstName(a: Address): string {
  if (!a.name) return a.address.split("@")[0];
  const n = a.name.replace(/["']/g, "").trim();
  if (n.includes(",")) return n.split(",")[1]?.trim().split(/\s+/)[0] || n;
  return n.split(/\s+/)[0] || n;
}

export function initials(a: Address): string {
  const src = (a.name || a.address).replace(/["'(]/g, "").trim();
  const parts = src.split(/[\s.@_-]+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (!a.name || parts.length === 1) return parts[0][0].toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

const avatarColors = ["#1a73e8", "#d93025", "#188038", "#e37400", "#9334e6", "#007b83", "#c5221f", "#b06000", "#3949ab", "#00897b"];

export function avatarColor(seed: string): string {
  let h = 0;
  for (let i = 0; i < seed.length; i++) h = (h * 31 + seed.charCodeAt(i)) | 0;
  return avatarColors[Math.abs(h) % avatarColors.length];
}

export function escapeHTML(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);
}

export function textToHTML(s: string): string {
  return escapeHTML(s).replace(/\r?\n/g, "<br>");
}

const emailRe = /^[^\s@<>()[\]\\,;:"]+@[^\s@<>()[\]\\,;:"]+\.[^\s@<>()[\]\\,;:"]+$/;

export function isValidEmail(addr: string): boolean {
  return emailRe.test(addr.trim());
}

/** Parses free text like `Alice <a@x.com>, b@y.org; "Bob, Jr." <bob@z.net>`. */
export function parseAddresses(input: string): Address[] {
  const out: Address[] = [];
  let buf = "";
  let quoted = false;
  let angle = false;
  const flush = () => {
    const t = buf.trim();
    buf = "";
    if (!t) return;
    const m = t.match(/^(.*?)<([^>]+)>\s*$/);
    if (m) {
      out.push({ name: m[1].trim().replace(/^"(.*)"$/, "$1").trim(), address: m[2].trim() });
    } else {
      out.push({ name: "", address: t.replace(/^mailto:/i, "") });
    }
  };
  for (const ch of input) {
    if (ch === '"') quoted = !quoted;
    else if (ch === "<" && !quoted) angle = true;
    else if (ch === ">" && !quoted) angle = false;
    if ((ch === "," || ch === ";" || ch === "\n") && !quoted && !angle) {
      flush();
      continue;
    }
    buf += ch;
  }
  flush();
  return out;
}

/** Builds the "From" column of the conversation list. */
export function participantSummary(c: Conversation, mine: Set<string>, showRecipients: boolean): { text: string; bold: boolean }[] {
  if (showRecipients) {
    const to = c.to ?? [];
    if (to.length === 0) return [{ text: "(no recipients)", bold: false }];
    const names = to.map((a) => (mine.has(a.address.toLowerCase()) ? "me" : to.length > 1 ? firstName(a) : displayName(a)));
    return [{ text: "To: " + names.slice(0, 3).join(", ") + (names.length > 3 ? ", …" : ""), bold: c.unread }];
  }
  const parts = c.participants ?? [];
  const label = (p: { name: string; address: string }) =>
    mine.has(p.address.toLowerCase()) ? "me" : parts.length > 1 ? firstName(p) : displayName(p);
  let shown = parts;
  if (parts.length > 3) shown = [parts[0], ...parts.slice(-2)];
  const out = shown.map((p, i) => ({
    text: (i === 1 && parts.length > 3 ? "… " : "") + label(p),
    bold: p.unread,
  }));
  if (out.length === 0) out.push({ text: "(unknown sender)", bold: c.unread });
  return out;
}

export function plural(n: number, one: string, many = one + "s"): string {
  return `${n} ${n === 1 ? one : many}`;
}

export function fileKind(contentType: string, filename: string): "image" | "pdf" | "doc" | "sheet" | "archive" | "audio" | "video" | "file" {
  const ct = contentType.toLowerCase();
  const ext = filename.toLowerCase().split(".").pop() ?? "";
  if (ct.startsWith("image/")) return "image";
  if (ct === "application/pdf" || ext === "pdf") return "pdf";
  if (ct.startsWith("audio/")) return "audio";
  if (ct.startsWith("video/")) return "video";
  if (["xls", "xlsx", "csv", "ods"].includes(ext)) return "sheet";
  if (["doc", "docx", "odt", "rtf", "txt", "md"].includes(ext)) return "doc";
  if (["zip", "gz", "tgz", "7z", "rar", "tar"].includes(ext)) return "archive";
  return "file";
}

/** Returns the datetime-local input value for a Date. */
export function toLocalInput(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** A queued message the user scheduled for later (not one inside its short undo-send window). */
export function isScheduled(e: QueueEntry): boolean {
  const recipients = e.recipients ?? [];
  return e.scheduled && recipients.length > 0 && recipients.every(
    (r) => r.status === "pending" && r.attempts === 0 && r.last_attempt_at === "0001-01-01T00:00:00Z",
  );
}

/** A queued message whose delivery failed temporarily and will be retried. */
export function isDelayed(e: QueueEntry): boolean {
  return (e.recipients ?? []).some((r) => r.status === "pending" && r.attempts > 0);
}
