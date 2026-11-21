import type { Location } from "./router";
import type { Label, ListView, Mailbox, SpecialUse } from "./types";

export type SystemView = "inbox" | "starred" | "sent" | "drafts" | "archive" | "all" | "spam" | "trash" | "scheduled" | "search";

export type ViewRef = { name: SystemView } | { name: "folder"; id: number } | { name: "label"; id: number };

export interface OpenItem {
  type: "thread" | "message";
  id: number;
}

export interface MailRoute {
  section: "mail";
  view: ViewRef;
  open?: OpenItem;
  q: string;
  page: number;
}

export type Route =
  | MailRoute
  | { section: "settings"; tab: string }
  | { section: "contacts" }
  | { section: "admin"; tab: string }
  | { section: "redirect"; to: string };

const systemViews: SystemView[] = ["inbox", "starred", "sent", "drafts", "archive", "all", "spam", "trash", "scheduled", "search"];

export function parseRoute(loc: Location): Route {
  const [section, ...rest] = loc.segments;
  if (section === "settings") return { section: "settings", tab: rest[0] ?? "general" };
  if (section === "contacts") return { section: "contacts" };
  if (section === "admin") return { section: "admin", tab: rest[0] ?? "overview" };
  if (section !== "mail") return { section: "redirect", to: "/mail/inbox" };

  let view: ViewRef | null = null;
  let i = 1;
  const first = rest[0];
  if (first === "folder" || first === "label") {
    const id = Number(rest[1]);
    if (Number.isInteger(id) && id > 0) view = { name: first, id };
    i = 2;
  } else if (systemViews.includes(first as SystemView)) {
    view = { name: first as SystemView };
  }
  if (!view) return { section: "redirect", to: "/mail/inbox" };

  let open: OpenItem | undefined;
  const type = rest[i];
  const id = Number(rest[i + 1]);
  if ((type === "thread" || type === "message") && Number.isInteger(id) && id > 0) open = { type, id };
  const page = Math.max(1, Number(loc.query.get("page")) || 1);
  return { section: "mail", view, open, q: loc.query.get("q") ?? "", page };
}

export function viewPath(v: ViewRef): string {
  return v.name === "folder" || v.name === "label" ? `/mail/${v.name}/${v.id}` : `/mail/${v.name}`;
}

export function mailHref(v: ViewRef, opts: { open?: OpenItem; q?: string; page?: number } = {}): string {
  let p = viewPath(v);
  if (opts.open) p += `/${opts.open.type}/${opts.open.id}`;
  const params = new URLSearchParams();
  if (opts.q) params.set("q", opts.q);
  if (opts.page && opts.page > 1) params.set("page", String(opts.page));
  const qs = params.toString();
  return qs ? `${p}?${qs}` : p;
}

export function sameView(a: ViewRef, b: ViewRef): boolean {
  return a.name === b.name && ("id" in a ? a.id : 0) === ("id" in b ? b.id : 0);
}

const specialFor: Partial<Record<SystemView, SpecialUse>> = {
  inbox: "inbox",
  sent: "sent",
  drafts: "drafts",
  archive: "archive",
  spam: "junk",
  trash: "trash",
};

export const viewTitles: Record<SystemView, string> = {
  inbox: "Inbox",
  starred: "Starred",
  sent: "Sent",
  drafts: "Drafts",
  archive: "Archive",
  all: "All mail",
  spam: "Spam",
  trash: "Trash",
  scheduled: "Scheduled",
  search: "Search results",
};

export interface ResolvedView {
  ref: ViewRef;
  title: string;
  /** Query parameters for GET /api/conversations (without paging). */
  params: Record<string, string>;
  actionView: ListView;
  special?: SpecialUse;
  mailbox?: Mailbox;
  label?: Label;
  /** Whether messages in Trash and Spam belong to the view. */
  includeTrash: boolean;
}

const trashSearch = /(^|\s)in:(trash|bin|spam|junk|anywhere)(\s|$)/i;

/** Maps a route view to API parameters; returns null while mailboxes are loading or when the view no longer exists. */
export function resolveView(ref: ViewRef, q: string, mailboxes: Mailbox[], labels: Label[]): ResolvedView | null {
  if (ref.name === "label") {
    const label = labels.find((l) => l.id === ref.id);
    if (!label) return null;
    return {
      ref,
      title: label.name,
      params: { view: "label", label: String(label.id) },
      actionView: { kind: "label", label_id: label.id },
      label,
      includeTrash: false,
    };
  }
  if (ref.name === "folder") {
    const mailbox = mailboxes.find((m) => m.id === ref.id);
    if (!mailbox) return null;
    return {
      ref,
      title: mailbox.name,
      params: { view: "mailbox", mailbox: String(mailbox.id) },
      actionView: { kind: "mailbox", mailbox_id: mailbox.id },
      mailbox,
      includeTrash: false,
    };
  }
  const special = specialFor[ref.name];
  if (special) {
    const mailbox = mailboxes.find((m) => m.special_use === special);
    if (!mailbox) return null;
    return {
      ref,
      title: viewTitles[ref.name],
      params: { view: "mailbox", mailbox: String(mailbox.id) },
      actionView: { kind: "mailbox", mailbox_id: mailbox.id },
      special,
      mailbox,
      includeTrash: special === "trash" || special === "junk",
    };
  }
  if (ref.name === "starred") {
    return { ref, title: "Starred", params: { view: "starred" }, actionView: { kind: "starred" }, includeTrash: false };
  }
  if (ref.name === "search") {
    return {
      ref,
      title: q ? `Search results for “${q}”` : "Search",
      params: { view: "all", q },
      actionView: { kind: "all" },
      includeTrash: trashSearch.test(q),
    };
  }
  return { ref, title: viewTitles[ref.name], params: { view: "all" }, actionView: { kind: "all" }, includeTrash: false };
}

/** Sorts user folders by name and computes their nesting depth (folders use "/" as the hierarchy separator). */
export function folderTree(mailboxes: Mailbox[]): { mailbox: Mailbox; depth: number; leaf: string }[] {
  return mailboxes
    .filter((m) => !m.special_use)
    .sort((a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: "base" }))
    .map((m) => {
      const parts = m.name.split("/");
      return { mailbox: m, depth: parts.length - 1, leaf: parts[parts.length - 1] };
    });
}
