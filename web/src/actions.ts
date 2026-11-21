import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import { api, type ActionBody } from "./api";
import { useDialogs } from "./components/Dialog";
import { useToast } from "./components/Toasts";
import { keys } from "./queries";
import type { ActionResult, Conversation, ListResult, ListView, MessageAction } from "./types";
import { plural } from "./util";

export type Target =
  | { threaded: true; threadIds: number[]; view: ListView }
  | { threaded: false; messageIds: number[] };

export function targetCount(t: Target) {
  return t.threaded ? t.threadIds.length : t.messageIds.length;
}

function matches(t: Target, c: Conversation) {
  return t.threaded ? t.threadIds.includes(c.thread_id) : t.messageIds.includes(c.latest_id);
}

const moves: MessageAction[] = ["archive", "trash", "spam", "notspam", "inbox", "move", "delete"];

/** Applies the expected result of an action to cached lists right away, before the server confirms it. */
function optimistic(qc: QueryClient, t: Target, body: ActionBody) {
  const patch: Partial<Conversation> | null =
    body.action === "read"
      ? { unread: false }
      : body.action === "unread"
        ? { unread: true }
        : body.action === "star"
          ? { starred: true }
          : body.action === "unstar"
            ? { starred: false }
            : null;
  const remove = moves.includes(body.action);
  if (!patch && !remove) return;
  for (const [key, data] of qc.getQueriesData<ListResult>({ queryKey: keys.conversations })) {
    if (!data?.conversations) continue;
    const params = (key[1] ?? {}) as Record<string, string>;
    let next = data.conversations;
    if (patch) {
      next = next.map((c) => (matches(t, c) ? { ...c, ...patch } : c));
    } else if (remove) {
      const leavesView =
        body.action === "trash" || body.action === "spam" || body.action === "delete"
          ? true
          : params.view === "mailbox" && params.mailbox !== String(body.mailbox_id ?? "");
      if (!leavesView) continue;
      next = next.filter((c) => !matches(t, c));
    }
    const removed = data.conversations.length - next.length;
    qc.setQueryData<ListResult>(key, { ...data, conversations: next, total: data.total - removed });
  }
}

function describe(action: MessageAction, n: number, noun: string, name?: string): string | null {
  const what = n === 1 ? noun[0].toUpperCase() + noun.slice(1) : plural(n, noun);
  switch (action) {
    case "archive":
      return `${what} archived.`;
    case "trash":
      return `${what} moved to Trash.`;
    case "spam":
      return `${what} marked as spam.`;
    case "notspam":
    case "inbox":
      return `${what} moved to Inbox.`;
    case "move":
      return `${what} moved to “${name ?? "folder"}”.`;
    case "delete":
      return `${what} deleted forever.`;
    case "label":
      return `${what} added to “${name ?? "label"}”.`;
    case "unlabel":
      return `${what} removed from “${name ?? "label"}”.`;
    case "read":
      return n > 1 ? `${what} marked as read.` : null;
    case "unread":
      return n > 1 ? `${what} marked as unread.` : null;
    default:
      return null;
  }
}

export function useMailActions() {
  const qc = useQueryClient();
  const toast = useToast();
  const dialogs = useDialogs();

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: keys.conversations });
    void qc.invalidateQueries({ queryKey: keys.thread });
    void qc.invalidateQueries({ queryKey: keys.message });
    void qc.invalidateQueries({ queryKey: keys.mailboxes });
    void qc.invalidateQueries({ queryKey: keys.labels });
  };

  const undoMoves = async (res: ActionResult) => {
    try {
      for (const g of res.undo ?? []) await api.messageAction(g.ids, { action: "move", mailbox_id: g.mailbox_id });
      toast.show({ message: "Action undone." });
    } catch (err) {
      toast.error(err);
    }
    refresh();
  };

  /**
   * Runs an action on conversations or messages, shows a confirmation with
   * an Undo button where possible, and returns whether it succeeded.
   */
  const perform = async (t: Target, body: ActionBody, opts: { name?: string; quiet?: boolean } = {}): Promise<boolean> => {
    const n = targetCount(t);
    if (n === 0) return false;
    const noun = t.threaded ? "conversation" : "message";
    if (body.action === "delete") {
      const ok = await dialogs.confirm({
        title: "Delete forever?",
        message: `${n === 1 ? `This ${noun}` : plural(n, noun)} will be permanently deleted. This can't be undone.`,
        confirmLabel: "Delete forever",
        danger: true,
      });
      if (!ok) return false;
    }
    optimistic(qc, t, body);
    let res: ActionResult;
    try {
      res = t.threaded ? await api.threadAction(t.threadIds, t.view, body) : await api.messageAction(t.messageIds, body);
    } catch (err) {
      toast.error(err);
      refresh();
      return false;
    }
    refresh();
    const msg = opts.quiet ? null : describe(body.action, n, noun, opts.name);
    if (msg) {
      let undo: (() => void) | null = null;
      if (res.undo?.length) undo = () => void undoMoves(res);
      else if (body.action === "label" || body.action === "unlabel") {
        const inverse: ActionBody = { ...body, action: body.action === "label" ? "unlabel" : "label" };
        undo = () => void perform(t, inverse, { quiet: true }).then((ok) => ok && toast.show({ message: "Action undone." }));
      }
      toast.show({ message: msg, actions: undo ? [{ label: "Undo", run: undo }] : undefined });
    }
    return true;
  };

  return { perform, refresh };
}
