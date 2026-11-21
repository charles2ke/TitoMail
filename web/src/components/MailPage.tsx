import { Fragment, useEffect, useMemo, useRef, useState, type MouseEvent } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import {
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  EllipsisVertical,
  FileText,
  Inbox,
  Mail,
  MailOpen,
  OctagonAlert,
  Paperclip,
  RotateCw,
  Search,
  Send,
  Star,
  Trash2,
} from "lucide-react";
import { api } from "../api";
import { useMailActions, type Target } from "../actions";
import { useHotkeys, useMediaQuery } from "../hooks";
import { keys, useLabels, useMailboxes } from "../queries";
import { Link, navigate } from "../router";
import { useSession } from "../session";
import type { Conversation, ReadingPane } from "../types";
import { fullDate, listDate, participantSummary, plural } from "../util";
import { mailHref, resolveView, viewPath, type MailRoute, type ResolvedView } from "../views";
import { ActionBar, viewActions, type ActionDef } from "./ActionBar";
import { Chip, EmptyState, Spinner } from "./common";
import { useComposer } from "./Composer";
import { useDialogs } from "./Dialog";
import { Menu } from "./Menu";
import { Outbox } from "./Outbox";
import { ThreadView, type ThreadNav } from "./ThreadView";
import { useToast } from "./Toasts";

export function MailPage({ route }: { route: MailRoute }) {
  const { settings } = useSession();
  const mbq = useMailboxes();
  const lbq = useLabels();
  const narrow = useMediaQuery("(max-width: 900px)");
  const resolved = useMemo(
    () => resolveView(route.view, route.q, mbq.mailboxes, lbq.labels),
    [route.view, route.q, mbq.mailboxes, lbq.labels],
  );

  if (route.view.name === "scheduled") return <Outbox />;
  if (!resolved) {
    if (mbq.isPending || lbq.isPending) return <Spinner />;
    return (
      <EmptyState title="This folder or label doesn't exist anymore.">
        <Link to="/mail/inbox" className="btn">
          Go to Inbox
        </Link>
      </EmptyState>
    );
  }
  const pane: ReadingPane = narrow ? "none" : settings.reading_pane;
  return <MailView key={viewPath(route.view) + "?" + route.q} route={route} view={resolved} pane={pane} />;
}

const noConversations: Conversation[] = [];

const specialChipNames: Record<string, string> = { inbox: "Inbox", junk: "Spam", trash: "Trash" };

function MailView({ route, view, pane }: { route: MailRoute; view: ResolvedView; pane: ReadingPane }) {
  const { settings, mine } = useSession();
  const composer = useComposer();
  const { perform, refresh } = useMailActions();
  const { mailboxes } = useMailboxes();
  const { labels } = useLabels();
  const dialogs = useDialogs();
  const toast = useToast();

  const threaded = settings.conversation_view;
  const pageSize = settings.page_size > 0 ? settings.page_size : 50;
  const offset = (route.page - 1) * pageSize;
  const isSearch = view.ref.name === "search";
  const params = useMemo(
    () => ({ ...view.params, threaded: threaded ? "1" : "0", offset: String(offset), limit: String(pageSize) }),
    [view.params, threaded, offset, pageSize],
  );
  const list = useQuery({
    queryKey: [...keys.conversations, params],
    queryFn: ({ signal }) => api.conversations(new URLSearchParams(params), signal),
    placeholderData: keepPreviousData,
    enabled: !isSearch || route.q.trim() !== "",
  });
  const convs = list.data?.conversations ?? noConversations;
  const total = list.data?.total ?? 0;

  const keyOf = (c: Conversation) => (threaded ? c.thread_id : c.latest_id);
  const [selected, setSelected] = useState<ReadonlySet<number>>(new Set());
  const [cursor, setCursor] = useState(0);
  const anchor = useRef(-1);
  const listRef = useRef<HTMLDivElement>(null);

  // Forget selected conversations that left the list.
  useEffect(() => {
    setSelected((prev) => {
      if (prev.size === 0) return prev;
      const present = new Set(convs.map((c) => (threaded ? c.thread_id : c.latest_id)));
      const next = new Set([...prev].filter((k) => present.has(k)));
      return next.size === prev.size ? prev : next;
    });
    setCursor((c) => Math.min(c, Math.max(0, convs.length - 1)));
  }, [convs, threaded]);

  // Go back a page when the current one became empty, e.g. after archiving everything on it.
  useEffect(() => {
    if (list.isPlaceholderData || !list.data) return;
    if (convs.length === 0 && route.page > 1) {
      const last = Math.max(1, Math.ceil(total / pageSize));
      navigate(mailHref(view.ref, { q: route.q, page: Math.min(route.page - 1, last) }), { replace: true });
    }
  }, [list.data, list.isPlaceholderData, convs.length, route.page, route.q, total, pageSize, view.ref]);

  useEffect(() => {
    listRef.current?.scrollTo(0, 0);
  }, [route.page]);

  useEffect(() => {
    listRef.current?.querySelector(`[data-index="${cursor}"]`)?.scrollIntoView({ block: "nearest" });
  }, [cursor]);

  const open = route.open;
  const openIndex = open
    ? convs.findIndex((c) => (open.type === "thread" ? c.thread_id === open.id : c.latest_id === open.id || (c.message_ids ?? []).includes(open.id)))
    : -1;
  useEffect(() => {
    if (openIndex >= 0) setCursor(openIndex);
  }, [openIndex]);

  const selectedConvs = convs.filter((c) => selected.has(keyOf(c)));
  const targetOf = (cs: Conversation[]): Target =>
    threaded
      ? { threaded: true, threadIds: cs.map((c) => c.thread_id), view: view.actionView }
      : { threaded: false, messageIds: cs.map((c) => c.latest_id) };
  const openTarget: Target | null = open
    ? open.type === "thread"
      ? { threaded: true, threadIds: [open.id], view: view.actionView }
      : { threaded: false, messageIds: [open.id] }
    : null;

  const hrefFor = (c: Conversation) =>
    mailHref(view.ref, {
      open: threaded ? { type: "thread", id: c.thread_id } : { type: "message", id: c.latest_id },
      q: route.q,
      page: route.page,
    });
  const openConv = (c: Conversation, i: number) => {
    setCursor(i);
    if (view.special === "drafts") {
      void composer.openDraft(c.latest_id);
      return;
    }
    navigate(hrefFor(c));
  };
  const closeOpen = () => navigate(mailHref(view.ref, { q: route.q, page: route.page }));

  const toggleSelect = (i: number, shift: boolean) => {
    const c = convs[i];
    if (!c) return;
    const on = !selected.has(keyOf(c));
    const next = new Set(selected);
    if (shift && anchor.current >= 0 && anchor.current < convs.length) {
      const [a, b] = anchor.current < i ? [anchor.current, i] : [i, anchor.current];
      for (let j = a; j <= b; j++) {
        if (on) next.add(keyOf(convs[j]));
        else next.delete(keyOf(convs[j]));
      }
    } else if (on) next.add(keyOf(c));
    else next.delete(keyOf(c));
    anchor.current = i;
    setSelected(next);
  };
  const selectWhere = (pred: (c: Conversation) => boolean) => setSelected(new Set(convs.filter(pred).map(keyOf)));

  const actions = viewActions(view);
  const neighbor = (delta: number) => {
    const i = (openIndex >= 0 ? openIndex : cursor) + delta;
    return i >= 0 && i < convs.length ? i : -1;
  };
  const goTo = (i: number) => {
    if (i < 0) return;
    const c = convs[i];
    setCursor(i);
    if (view.special === "drafts") return;
    navigate(hrefFor(c), { replace: true });
  };
  const nav: ThreadNav | null =
    open && openIndex >= 0
      ? {
          position: offset + openIndex + 1,
          total,
          newer: neighbor(-1) >= 0 ? () => goTo(neighbor(-1)) : undefined,
          older: neighbor(1) >= 0 ? () => goTo(neighbor(1)) : undefined,
        }
      : null;

  /** What keyboard shortcuts act on: the selection, else the open conversation, else the one under the cursor. */
  const hotTarget = (): { target: Target; convs: Conversation[]; isOpen: boolean } | null => {
    if (selectedConvs.length) return { target: targetOf(selectedConvs), convs: selectedConvs, isOpen: false };
    if (openTarget) return { target: openTarget, convs: openIndex >= 0 ? [convs[openIndex]] : [], isOpen: true };
    const c = convs[cursor];
    return c ? { target: targetOf([c]), convs: [c], isOpen: false } : null;
  };
  const runOnHot = async (def: ActionDef | undefined) => {
    const t = hotTarget();
    if (!def || !t) return;
    if ((await perform(t.target, def.body)) && t.isOpen) closeOpen();
  };
  const byKey = (k: string) => actions.find((a) => a.key === k);

  useHotkeys({
    j: () => (open ? goTo(neighbor(1)) : setCursor((c) => Math.min(c + 1, convs.length - 1))),
    k: () => (open ? goTo(neighbor(-1)) : setCursor((c) => Math.max(c - 1, 0))),
    o: () => !open && convs[cursor] && openConv(convs[cursor], cursor),
    Enter: (e) => {
      if ((e.target as HTMLElement).closest?.("button, a, [tabindex]")) return;
      if (!open && convs[cursor]) openConv(convs[cursor], cursor);
    },
    u: () => open && closeOpen(),
    Escape: () => (selected.size ? setSelected(new Set()) : undefined),
    x: () => !open && toggleSelect(cursor, false),
    s: () => {
      const t = hotTarget();
      if (!t) return;
      const starred = t.convs.length > 0 && t.convs.every((c) => c.starred);
      void perform(t.target, { action: starred ? "unstar" : "star" }, { quiet: true });
    },
    e: () => void runOnHot(byKey("e")),
    "#": () => void runOnHot(byKey("#")),
    "!": () => void runOnHot(byKey("!")),
    "shift+i": () => {
      const t = hotTarget();
      if (t) void perform(t.target, { action: "read" });
    },
    "shift+u": () => {
      const t = hotTarget();
      if (t) void perform(t.target, { action: "unread" }).then((ok) => ok && t.isOpen && closeOpen());
    },
    "* a": () => selectWhere(() => true),
    "* n": () => setSelected(new Set()),
    "* r": () => selectWhere((c) => !c.unread),
    "* u": () => selectWhere((c) => c.unread),
    "* s": () => selectWhere((c) => c.starred),
    "* t": () => selectWhere((c) => !c.starred),
  });

  const labelState = (id: number) => {
    const n = selectedConvs.filter((c) => c.labels?.includes(id)).length;
    return n === 0 ? "none" : n === selectedConvs.length ? "all" : "some";
  };

  const chipsFor = (c: Conversation) => {
    const out: { key: string; name: string; color: string }[] = [];
    for (const id of c.mailboxes ?? []) {
      if (id === view.mailbox?.id) continue;
      const mb = mailboxes.find((m) => m.id === id);
      if (!mb) continue;
      if (mb.special_use) {
        const name = specialChipNames[mb.special_use];
        if (name) out.push({ key: `m${id}`, name, color: "" });
      } else out.push({ key: `m${id}`, name: mb.name, color: "" });
    }
    for (const id of c.labels ?? []) {
      if (id === view.label?.id) continue;
      const l = labels.find((x) => x.id === id);
      if (l) out.push({ key: `l${id}`, name: l.name, color: l.color });
    }
    return out;
  };

  const emptyMailbox = async () => {
    const mb = view.mailbox;
    if (!mb) return;
    const ok = await dialogs.confirm({
      title: `Empty ${view.title}?`,
      message: `All messages in ${view.title} will be permanently deleted.`,
      confirmLabel: `Empty ${view.title}`,
      danger: true,
    });
    if (!ok) return;
    try {
      const res = await api.emptyMailbox(mb.id);
      toast.show({ message: `${plural(res.deleted, "message")} deleted forever.` });
    } catch (err) {
      toast.error(err);
    }
    refresh();
  };

  const allSelected = convs.length > 0 && selectedConvs.length === convs.length;
  const someSelected = selectedConvs.length > 0 && !allSelected;
  const hoverActions = actions.filter((a) => a.body.action !== "spam" && a.body.action !== "notspam");
  const showList = pane !== "none" || !open;
  const unreadOnPage = convs.filter((c) => c.unread);

  const rowClick = (e: MouseEvent, c: Conversation, i: number) => {
    if ((e.target as HTMLElement).closest("button, input, a")) return;
    if ((e.ctrlKey || e.metaKey) && view.special !== "drafts") {
      window.open(hrefFor(c), "_blank");
      return;
    }
    openConv(c, i);
  };

  const listToolbar = (
    <div className="toolbar list-toolbar">
      <div className="select-all">
        <input
          type="checkbox"
          aria-label={allSelected || someSelected ? "Deselect all" : "Select all"}
          checked={allSelected}
          ref={(el) => {
            if (el) el.indeterminate = someSelected;
          }}
          disabled={convs.length === 0}
          onChange={() => setSelected(allSelected || someSelected ? new Set() : new Set(convs.map(keyOf)))}
        />
        <Menu
          trigger={(p) => (
            <button className="icon-btn tiny" aria-label="Select conversations" title="Select" {...p}>
              <ChevronDown size={14} />
            </button>
          )}
          items={[
            { label: "All", onSelect: () => selectWhere(() => true) },
            { label: "None", onSelect: () => setSelected(new Set()) },
            { label: "Read", onSelect: () => selectWhere((c) => !c.unread) },
            { label: "Unread", onSelect: () => selectWhere((c) => c.unread) },
            { label: "Starred", onSelect: () => selectWhere((c) => c.starred) },
            { label: "Unstarred", onSelect: () => selectWhere((c) => !c.starred) },
          ]}
        />
      </div>
      {selectedConvs.length > 0 ? (
        <ActionBar
          view={view}
          target={targetOf(selectedConvs)}
          anyUnread={selectedConvs.some((c) => c.unread)}
          anyStarred={selectedConvs.some((c) => c.starred)}
          labelState={labelState}
          onMoved={() => {
            if (openIndex >= 0 && selected.has(keyOf(convs[openIndex]))) closeOpen();
            setSelected(new Set());
          }}
        />
      ) : (
        <>
          <button className="icon-btn" title="Refresh" aria-label="Refresh" onClick={() => refresh()}>
            <RotateCw size={18} className={list.isFetching ? "spin" : ""} />
          </button>
          <Menu
            trigger={(p) => (
              <button className="icon-btn" title="More" aria-label="More" {...p}>
                <EllipsisVertical size={18} />
              </button>
            )}
            items={[
              {
                label: "Mark all as read",
                icon: <MailOpen size={16} />,
                disabled: unreadOnPage.length === 0,
                onSelect: () => void perform(targetOf(unreadOnPage), { action: "read" }),
              },
            ]}
          />
        </>
      )}
      <span className="spacer" />
      {selectedConvs.length > 0 && <span className="selection-count">{selectedConvs.length} selected</span>}
      {total > 0 && (
        <div className="pager">
          <span className="pager-text">
            {(offset + 1).toLocaleString()}–{(offset + convs.length).toLocaleString()} of {total.toLocaleString()}
          </span>
          <button
            className="icon-btn"
            title="Newer"
            aria-label="Newer page"
            disabled={route.page <= 1}
            onClick={() => navigate(mailHref(view.ref, { q: route.q, page: route.page - 1 }))}
          >
            <ChevronLeft size={20} />
          </button>
          <button
            className="icon-btn"
            title="Older"
            aria-label="Older page"
            disabled={offset + convs.length >= total}
            onClick={() => navigate(mailHref(view.ref, { q: route.q, page: route.page + 1 }))}
          >
            <ChevronRight size={20} />
          </button>
        </div>
      )}
    </div>
  );

  let body;
  if (isSearch && !route.q.trim()) {
    body = (
      <EmptyState icon={<Search size={48} />} title="Search your mail">
        <p>
          Type in the search box above. You can use operators like <code>from:</code>, <code>to:</code>, <code>subject:</code>,{" "}
          <code>has:attachment</code>, <code>is:unread</code>, <code>newer_than:7d</code> and <code>in:anywhere</code>.
        </p>
      </EmptyState>
    );
  } else if (list.isPending) {
    body = <Spinner />;
  } else if (list.error && !list.data) {
    body = (
      <EmptyState title="Your mail couldn't be loaded.">
        <button className="btn" onClick={() => void list.refetch()}>
          Try again
        </button>
      </EmptyState>
    );
  } else if (convs.length === 0) {
    body = <ViewEmpty view={view} />;
  } else {
    body = (
      <div className="conv-list" role="list" aria-label={view.title}>
        {convs.map((c, i) => {
          const k = keyOf(c);
          const isSel = selected.has(k);
          const parts = participantSummary(c, mine, view.special === "sent");
          const draftOnly = c.has_draft && !(c.participants ?? []).length;
          const chips = chipsFor(c);
          return (
            <div
              key={k}
              role="listitem"
              data-index={i}
              className={
                "conv-row" +
                (c.unread ? " unread" : "") +
                (isSel ? " selected" : "") +
                (i === cursor ? " cursor" : "") +
                (i === openIndex ? " active" : "")
              }
              onClick={(e) => rowClick(e, c, i)}
            >
              <span className="row-check">
                <input
                  type="checkbox"
                  aria-label={`Select “${c.subject || "(no subject)"}”`}
                  checked={isSel}
                  onChange={() => {}}
                  onClick={(e) => {
                    e.stopPropagation();
                    toggleSelect(i, e.shiftKey);
                  }}
                />
              </span>
              <button
                className={"row-star" + (c.starred ? " on" : "")}
                aria-label={c.starred ? "Starred" : "Not starred"}
                aria-pressed={c.starred}
                title={c.starred ? "Starred" : "Not starred"}
                onClick={() => void perform(targetOf([c]), { action: c.starred ? "unstar" : "star" }, { quiet: true })}
              >
                <Star size={18} fill={c.starred ? "currentColor" : "none"} />
              </button>
              <span className="row-from">
                {!draftOnly &&
                  parts.map((p, j) => (
                    <Fragment key={j}>
                      {j > 0 && ", "}
                      <span className={p.bold ? "b" : undefined}>{p.text}</span>
                    </Fragment>
                  ))}
                {c.has_draft && (
                  <>
                    {!draftOnly && ", "}
                    <span className="row-draft">Draft</span>
                  </>
                )}
                {threaded && c.count > 1 && <span className="row-count">{c.count}</span>}
              </span>
              <span className="row-main">
                {chips.map((ch) => (
                  <Chip key={ch.key} name={ch.name} color={ch.color} />
                ))}
                <span className="row-subject">{c.subject || "(no subject)"}</span>
                {c.snippet && <span className="row-snippet"> – {c.snippet}</span>}
              </span>
              <span className="row-end">
                {c.has_attachments && <Paperclip size={15} className="row-att" aria-label="Has attachments" />}
                <span className="row-date" title={fullDate(c.date)}>
                  {listDate(c.date)}
                </span>
                <span className="row-hover">
                  {hoverActions.map((a) => (
                    <button
                      key={a.body.action}
                      className="icon-btn small"
                      title={a.label}
                      aria-label={a.label}
                      onClick={() => void perform(targetOf([c]), a.body).then((ok) => ok && i === openIndex && closeOpen())}
                    >
                      {a.icon}
                    </button>
                  ))}
                  <button
                    className="icon-btn small"
                    title={c.unread ? "Mark as read" : "Mark as unread"}
                    aria-label={c.unread ? "Mark as read" : "Mark as unread"}
                    onClick={() => void perform(targetOf([c]), { action: c.unread ? "read" : "unread" }, { quiet: true })}
                  >
                    {c.unread ? <MailOpen size={18} /> : <Mail size={18} />}
                  </button>
                </span>
              </span>
            </div>
          );
        })}
      </div>
    );
  }

  return (
    <div className={`mail-view pane-${pane}${open ? " has-open" : ""}`}>
      {showList && (
        <section className="list-pane" aria-label={view.title}>
          {listToolbar}
          {(view.special === "trash" || view.special === "junk") && convs.length > 0 && (
            <div className="list-banner">
              {view.special === "trash"
                ? "Messages that have been in Trash for more than 30 days are deleted automatically."
                : "Messages that have been in Spam for more than 30 days are deleted automatically."}{" "}
              <button className="link-btn" onClick={() => void emptyMailbox()}>
                Empty {view.title} now
              </button>
            </div>
          )}
          <div className="list-scroll" ref={listRef}>
            {body}
          </div>
        </section>
      )}
      {open ? (
        <ThreadView key={`${open.type}:${open.id}`} view={view} open={open} nav={nav} onClose={closeOpen} showBack={pane === "none"} />
      ) : (
        pane !== "none" && (
          <section className="thread-pane empty">
            <EmptyState icon={<Mail size={56} strokeWidth={1} />} title="No conversation selected" />
          </section>
        )
      )}
    </div>
  );
}

function ViewEmpty({ view }: { view: ResolvedView }) {
  const composer = useComposer();
  switch (view.special) {
    case "inbox":
      return <EmptyState icon={<Inbox size={56} strokeWidth={1} />} title="Your inbox is empty" />;
    case "sent":
      return (
        <EmptyState icon={<Send size={56} strokeWidth={1} />} title="No sent messages">
          <button className="link-btn" onClick={() => composer.compose()}>
            Send one now
          </button>
        </EmptyState>
      );
    case "drafts":
      return (
        <EmptyState icon={<FileText size={56} strokeWidth={1} />} title="You don't have any saved drafts">
          <p>Saving a draft allows you to keep a message you aren't ready to send yet.</p>
        </EmptyState>
      );
    case "junk":
      return <EmptyState icon={<OctagonAlert size={56} strokeWidth={1} />} title="Hooray, no spam here!" />;
    case "trash":
      return <EmptyState icon={<Trash2 size={56} strokeWidth={1} />} title="No conversations in Trash" />;
  }
  if (view.ref.name === "starred")
    return (
      <EmptyState icon={<Star size={56} strokeWidth={1} />} title="No starred messages">
        <p>Stars let you give messages a special status to make them easier to find.</p>
      </EmptyState>
    );
  if (view.ref.name === "search")
    return (
      <EmptyState icon={<Search size={56} strokeWidth={1} />} title="No messages matched your search">
        <p>
          Try different words, or search everywhere including Spam and Trash with <code>in:anywhere</code>.
        </p>
      </EmptyState>
    );
  return <EmptyState title={`There are no conversations in “${view.title}”.`} />;
}
