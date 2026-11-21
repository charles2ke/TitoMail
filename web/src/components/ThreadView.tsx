import { Fragment, useEffect, useRef, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  Ban,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Code,
  Download,
  EllipsisVertical,
  File as FileIcon,
  FileArchive,
  FileAudio,
  FileImage,
  FileSpreadsheet,
  FileText,
  FileVideo,
  Forward,
  ImageOff,
  OctagonAlert,
  Paperclip,
  Reply,
  ReplyAll,
  ShieldAlert,
  Star,
  Trash2,
} from "lucide-react";
import { api, ApiError, attachmentURL } from "../api";
import { useMailActions, type Target } from "../actions";
import { useHotkeys } from "../hooks";
import { keys, useLabels, useMailboxes } from "../queries";
import { useSession } from "../session";
import type { Address, Attachment, Message, MessageView } from "../types";
import { displayName, fileKind, firstName, formatBytes, fullDate, listDate, messageDate, parseAddresses, plural } from "../util";
import type { OpenItem, ResolvedView } from "../views";
import { ActionBar } from "./ActionBar";
import { Avatar, Chip, EmptyState, Spinner } from "./common";
import { useComposer } from "./Composer";
import { useDialogs } from "./Dialog";
import { Menu, Popover } from "./Menu";
import { MessageBody } from "./MessageBody";
import { useToast } from "./Toasts";

export interface ThreadNav {
  position: number;
  total: number;
  newer?: () => void;
  older?: () => void;
}

interface Props {
  view: ResolvedView;
  open: OpenItem;
  nav: ThreadNav | null;
  onClose: () => void;
  showBack: boolean;
}

const notFound = (err: unknown) => err instanceof ApiError && err.status === 404;
const retry = (n: number, err: Error) => !notFound(err) && n < 2;

const specialNames: Record<string, string> = { inbox: "Inbox", junk: "Spam", trash: "Trash" };

export function ThreadView({ view, open, nav, onClose, showBack }: Props) {
  const session = useSession();
  const composer = useComposer();
  const { perform } = useMailActions();
  const { mailboxes } = useMailboxes();
  const { labels } = useLabels();

  const threadQ = useQuery({
    queryKey: [...keys.thread, open.id, view.includeTrash],
    queryFn: () => api.thread(open.id, view.includeTrash),
    enabled: open.type === "thread",
    retry,
  });
  const msgQ = useQuery({
    queryKey: [...keys.message, open.id, false],
    queryFn: () => api.message(open.id, false),
    enabled: open.type === "message",
    retry,
  });
  const q = open.type === "thread" ? threadQ : msgQ;
  const messages: Message[] = open.type === "thread" ? (threadQ.data?.messages ?? noMessages) : msgQ.data ? [msgQ.data] : noMessages;
  const subject = (open.type === "thread" ? threadQ.data?.subject : msgQ.data?.subject) || "(no subject)";
  const target: Target =
    open.type === "thread"
      ? { threaded: true, threadIds: [open.id], view: view.actionView }
      : { threaded: false, messageIds: [open.id] };

  const real = messages.filter((m) => !m.draft);
  const last = real[real.length - 1];
  const anyUnread = real.some((m) => !m.seen);
  const anyStarred = messages.some((m) => m.flagged);

  // Mark the conversation as read once it is shown, and again if new unread messages arrive.
  const marked = useRef("");
  useEffect(() => {
    const unread = messages.filter((m) => !m.seen && !m.draft).map((m) => m.id);
    const key = unread.join(",");
    if (!unread.length || marked.current === key) return;
    marked.current = key;
    void perform(target, { action: "read" }, { quiet: true });
  }, [messages]);

  // Expand unread messages and the latest one; newly arriving messages are expanded too.
  const [expanded, setExpanded] = useState<ReadonlySet<number>>(new Set());
  const [showAll, setShowAll] = useState(false);
  const known = useRef(new Set<number>());
  useEffect(() => {
    const fresh = messages.filter((m) => !known.current.has(m.id));
    if (!fresh.length) return;
    const firstLoad = known.current.size === 0;
    for (const m of fresh) known.current.add(m.id);
    setExpanded((prev) => {
      const next = new Set(prev);
      if (firstLoad) {
        if (last) next.add(last.id);
        for (const m of real) if (!m.seen) next.add(m.id);
      } else {
        for (const m of fresh) if (!m.draft) next.add(m.id);
      }
      return next;
    });
  }, [messages]);

  const toggle = (id: number) =>
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  useHotkeys({
    r: () => last && void composer.reply(last.id, "reply"),
    a: () => last && void composer.reply(last.id, "reply_all"),
    f: () => last && void composer.reply(last.id, "forward"),
    "shift+n": () => setShowAll(true),
    ";": () => setExpanded(new Set(messages.map((m) => m.id))),
    ":": () => setExpanded(new Set(last ? [last.id] : [])),
  });

  const labelIds = new Set(real.flatMap((m) => m.labels ?? []));
  const labelState = (id: number) => {
    const n = real.filter((m) => m.labels?.includes(id)).length;
    return n === 0 ? "none" : n === real.length ? "all" : "some";
  };
  const mailboxIds = new Set(real.map((m) => m.mailbox_id));

  const toolbar = (
    <div className="toolbar thread-toolbar">
      {showBack && (
        <button className="icon-btn" title="Back to list (u)" aria-label="Back to list" onClick={onClose}>
          <ArrowLeft size={20} />
        </button>
      )}
      {messages.length > 0 && (
        <ActionBar
          view={view}
          target={target}
          anyUnread={anyUnread}
          anyStarred={anyStarred}
          labelState={labelState}
          onMoved={onClose}
          onMarkedUnread={onClose}
        />
      )}
      <span className="spacer" />
      {nav && (
        <div className="pager">
          <span className="pager-text">
            {nav.position.toLocaleString()} of {nav.total.toLocaleString()}
          </span>
          <button className="icon-btn" title="Newer (k)" aria-label="Newer" disabled={!nav.newer} onClick={nav.newer}>
            <ChevronLeft size={20} />
          </button>
          <button className="icon-btn" title="Older (j)" aria-label="Older" disabled={!nav.older} onClick={nav.older}>
            <ChevronRight size={20} />
          </button>
        </div>
      )}
    </div>
  );

  if (q.isLoading) {
    return (
      <section className="thread-pane">
        {toolbar}
        <Spinner />
      </section>
    );
  }
  if (q.error || messages.length === 0) {
    return (
      <section className="thread-pane">
        {toolbar}
        <EmptyState title={notFound(q.error) || !q.error ? "This conversation is no longer here." : "The conversation couldn't be loaded."}>
          {q.error && !notFound(q.error) && (
            <button className="btn" onClick={() => void q.refetch()}>
              Try again
            </button>
          )}
          <button className="btn" onClick={onClose}>
            Back to {view.title}
          </button>
        </EmptyState>
      </section>
    );
  }

  // Collapse a long run of read messages into an "N more" pill like Gmail.
  const firstExp = messages.findIndex((m) => expanded.has(m.id));
  const hideTo = !showAll && firstExp >= 4 ? firstExp - 2 : -1;

  const chips: ReactNode[] = [];
  for (const id of mailboxIds) {
    const mb = mailboxes.find((m) => m.id === id);
    if (!mb || mb.id === view.mailbox?.id) continue;
    if (mb.special_use && !specialNames[mb.special_use]) continue;
    const name = mb.special_use ? specialNames[mb.special_use] : mb.name;
    chips.push(
      <Chip
        key={`m${id}`}
        name={name}
        onRemove={
          mb.special_use === "inbox"
            ? () => void perform(target, { action: "archive" }).then((ok) => ok && view.special === "inbox" && onClose())
            : undefined
        }
      />,
    );
  }
  for (const id of labelIds) {
    const l = labels.find((x) => x.id === id);
    if (!l) continue;
    chips.push(
      <Chip
        key={`l${id}`}
        name={l.name}
        color={l.color}
        onRemove={() =>
          void perform(target, { action: "unlabel", label_id: l.id }, { name: l.name }).then((ok) => ok && view.label?.id === l.id && onClose())
        }
      />,
    );
  }

  return (
    <section className="thread-pane" aria-label={subject}>
      {toolbar}
      <div className="thread-scroll">
        <div className="thread-head">
          <h1 className="thread-subject">{subject}</h1>
          {chips.length > 0 && <div className="thread-chips">{chips}</div>}
        </div>
        <div className="thread-messages">
          {messages.map((m, i) => {
            if (i >= 1 && i <= hideTo) {
              return i === 1 ? (
                <button key="more" className="more-pill" onClick={() => setShowAll(true)}>
                  <span className="more-count">{hideTo}</span>
                  <span className="sr-only">Show {plural(hideTo, "more message")}</span>
                </button>
              ) : null;
            }
            if (m.draft) return <DraftCard key={m.id} m={m} />;
            return expanded.has(m.id) ? (
              <ExpandedMessage
                key={m.id}
                m={m}
                isOnly={real.length === 1}
                onCollapse={() => toggle(m.id)}
                onGone={onClose}
                laterIds={messages.slice(i).filter((x) => !x.draft).map((x) => x.id)}
              />
            ) : (
              <CollapsedMessage key={m.id} m={m} mine={session.mine} onExpand={() => toggle(m.id)} />
            );
          })}
        </div>
        {last && (
          <div className="thread-replies">
            <button className="btn outline" onClick={() => void composer.reply(last.id, "reply")}>
              <Reply size={18} /> Reply
            </button>
            {replyAllUseful(last, session.mine) && (
              <button className="btn outline" onClick={() => void composer.reply(last.id, "reply_all")}>
                <ReplyAll size={18} /> Reply all
              </button>
            )}
            <button className="btn outline" onClick={() => void composer.reply(last.id, "forward")}>
              <Forward size={18} /> Forward
            </button>
          </div>
        )}
      </div>
    </section>
  );
}

const noMessages: Message[] = [];

function replyAllUseful(m: Message, mine: Set<string>): boolean {
  const others = new Set(
    [m.from, ...(m.to ?? []), ...(m.cc ?? [])].map((a) => a.address.toLowerCase()).filter((a) => a && !mine.has(a)),
  );
  return others.size > 1;
}

function recipientSummary(m: Message, mine: Set<string>): string {
  const all = [...(m.to ?? []), ...(m.cc ?? []), ...(m.bcc ?? [])];
  if (all.length === 0) return "to undisclosed recipients";
  const names = all.map((a) => (mine.has(a.address.toLowerCase()) ? "me" : firstName(a)));
  return "to " + names.join(", ");
}

function CollapsedMessage({ m, mine, onExpand }: { m: Message; mine: Set<string>; onExpand: () => void }) {
  return (
    <article
      className={"msg collapsed" + (m.seen ? "" : " unread")}
      tabIndex={0}
      onClick={onExpand}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onExpand();
        }
      }}
    >
      <Avatar address={m.from} size={32} />
      <span className="msg-from">{mine.has(m.from.address.toLowerCase()) ? "me" : displayName(m.from)}</span>
      <span className="msg-snippet">{m.snippet}</span>
      {m.has_attachments && <Paperclip size={14} className="msg-clip" />}
      {m.flagged && <Star size={14} className="msg-star" fill="currentColor" />}
      <span className="msg-date" title={fullDate(m.received_at)}>
        {listDate(m.received_at)}
      </span>
    </article>
  );
}

function DraftCard({ m }: { m: Message }) {
  const composer = useComposer();
  return (
    <article
      className="msg collapsed draft"
      tabIndex={0}
      onClick={() => void composer.openDraft(m.id)}
      onKeyDown={(e) => e.key === "Enter" && void composer.openDraft(m.id)}
      title="Edit draft"
    >
      <Avatar address={m.from} size={32} />
      <span className="msg-from">
        <span className="draft-label">Draft</span>
      </span>
      <span className="msg-snippet">{m.snippet || "(no content)"}</span>
      <span className="msg-date">{listDate(m.received_at)}</span>
    </article>
  );
}

function AddressList({ list }: { list: Address[] | null }) {
  return (
    <>
      {(list ?? []).map((a, i) => (
        <Fragment key={i}>
          {i > 0 && ", "}
          {a.name ? (
            <>
              <strong>{a.name}</strong> &lt;{a.address}&gt;
            </>
          ) : (
            a.address
          )}
        </Fragment>
      ))}
    </>
  );
}

function Details({ m, v }: { m: Message; v?: MessageView }) {
  return (
    <table className="msg-details">
      <tbody>
        <tr>
          <th>from:</th>
          <td>
            <AddressList list={[m.from]} />
            {v?.sender && <> (sent by {v.sender.address})</>}
          </td>
        </tr>
        {!!m.reply_to?.length && (
          <tr>
            <th>reply-to:</th>
            <td>
              <AddressList list={m.reply_to} />
            </td>
          </tr>
        )}
        <tr>
          <th>to:</th>
          <td>{m.to?.length ? <AddressList list={m.to} /> : "undisclosed recipients"}</td>
        </tr>
        {!!m.cc?.length && (
          <tr>
            <th>cc:</th>
            <td>
              <AddressList list={m.cc} />
            </td>
          </tr>
        )}
        {!!m.bcc?.length && (
          <tr>
            <th>bcc:</th>
            <td>
              <AddressList list={m.bcc} />
            </td>
          </tr>
        )}
        <tr>
          <th>date:</th>
          <td>{fullDate(m.received_at)}</td>
        </tr>
        <tr>
          <th>subject:</th>
          <td>{m.subject || "(no subject)"}</td>
        </tr>
        {v?.auth?.mailed_by && (
          <tr>
            <th>mailed-by:</th>
            <td>{v.auth.mailed_by}</td>
          </tr>
        )}
        {v?.auth?.signed_by && (
          <tr>
            <th>signed-by:</th>
            <td>{v.auth.signed_by}</td>
          </tr>
        )}
      </tbody>
    </table>
  );
}

function Banner({ tone, icon, children }: { tone: "warn" | "danger" | "info"; icon: ReactNode; children: ReactNode }) {
  return (
    <div className={`banner ${tone}`} role="note">
      <span className="banner-icon">{icon}</span>
      <div className="banner-text">{children}</div>
    </div>
  );
}

const attIcons = {
  image: FileImage,
  pdf: FileText,
  doc: FileText,
  sheet: FileSpreadsheet,
  archive: FileArchive,
  audio: FileAudio,
  video: FileVideo,
  file: FileIcon,
};
const previewable = /^image\/(png|jpe?g|gif|webp|bmp|avif)$/i;

function Attachments({ messageId, list }: { messageId: number; list: Attachment[] }) {
  return (
    <div className="atts">
      <div className="atts-head">{plural(list.length, "attachment")}</div>
      <div className="atts-grid">
        {list.map((a) => {
          const kind = fileKind(a.content_type, a.filename);
          const Icon = attIcons[kind];
          const name = a.filename || "attachment";
          return (
            <div key={a.part} className="att-card" title={name}>
              <a className="att-preview" href={attachmentURL(messageId, a.part)} target="_blank" rel="noopener noreferrer">
                {previewable.test(a.content_type) ? (
                  <img src={attachmentURL(messageId, a.part)} alt="" loading="lazy" />
                ) : (
                  <Icon size={40} strokeWidth={1.4} className={`att-icon ${kind}`} />
                )}
              </a>
              <div className="att-info">
                <span className="att-name">{name}</span>
                <span className="att-size">{formatBytes(a.size)}</span>
              </div>
              <a className="icon-btn small att-download" href={attachmentURL(messageId, a.part, true)} download={name} title={`Download ${name}`} aria-label={`Download ${name}`}>
                <Download size={16} />
              </a>
            </div>
          );
        })}
      </div>
    </div>
  );
}

interface ExpandedProps {
  m: Message;
  isOnly: boolean;
  laterIds: number[];
  onCollapse: () => void;
  onGone: () => void;
}

function ExpandedMessage({ m, isOnly, laterIds, onCollapse, onGone }: ExpandedProps) {
  const session = useSession();
  const composer = useComposer();
  const toast = useToast();
  const dialogs = useDialogs();
  const qc = useQueryClient();
  const { perform } = useMailActions();
  const [remote, setRemote] = useState(false);
  const [showQuotes, setShowQuotes] = useState(false);
  const q = useQuery({ queryKey: [...keys.message, m.id, remote], queryFn: () => api.message(m.id, remote), retry });
  const v = q.data;
  const target: Target = { threaded: false, messageIds: [m.id] };
  const from = m.from;
  const fromMe = session.mine.has(from.address.toLowerCase());
  const atts = (m.attachments ?? []).filter((a) => !a.inline || !a.content_id);
  const blocked = v?.sender_pref === "blocked";
  const inSpam = v?.mailbox_special === "junk";
  const authFailed = !!v?.auth && (v.auth.dmarc === "fail" || (v.auth.spf === "fail" && v.auth.dkim !== "pass"));

  const act = async (body: Parameters<typeof perform>[1]) => {
    if ((await perform(target, body)) && isOnly) onGone();
  };
  const trustSender = async () => {
    try {
      await api.setSender(from.address, "trusted");
      setRemote(true);
      void qc.invalidateQueries({ queryKey: keys.message });
      toast.show({ message: `Images from ${from.address} will always be shown.` });
    } catch (err) {
      toast.error(err);
    }
  };
  const setBlocked = async (block: boolean) => {
    if (block) {
      const ok = await dialogs.confirm({
        title: `Block ${from.address}?`,
        message: "Future messages from this sender will go to Spam. You can unblock them in Settings.",
        confirmLabel: "Block",
        danger: true,
      });
      if (!ok) return;
    }
    try {
      if (block) await api.setSender(from.address, "blocked");
      else await api.deleteSender(from.address);
      void qc.invalidateQueries({ queryKey: keys.message });
      toast.show({ message: block ? `${from.address} is blocked.` : `${from.address} is unblocked.` });
    } catch (err) {
      toast.error(err);
    }
  };
  const unsubscribe = async () => {
    const url = v?.unsubscribe;
    if (!url) return;
    const ok = await dialogs.confirm({
      title: `Unsubscribe from ${displayName(from)}?`,
      message: url.toLowerCase().startsWith("mailto:")
        ? "An unsubscribe request email will be prepared for you to send."
        : "The sender's unsubscribe page opens in a new tab.",
      confirmLabel: "Unsubscribe",
    });
    if (!ok) return;
    if (url.toLowerCase().startsWith("mailto:")) {
      try {
        const u = new URL(url);
        composer.compose({ to: parseAddresses(decodeURIComponent(u.pathname)), subject: u.searchParams.get("subject") || "unsubscribe", html: u.searchParams.get("body") ?? "" });
      } catch {
        toast.error(new Error("The unsubscribe address is not valid."));
      }
    } else {
      window.open(url, "_blank", "noopener,noreferrer");
    }
  };

  return (
    <article className="msg expanded">
      <header
        className="msg-head"
        onClick={(e) => {
          if (!isOnly && !(e.target as HTMLElement).closest("button, a, .popover")) onCollapse();
        }}
      >
        <Avatar address={from} />
        <div className="msg-meta">
          <div className="msg-from-line">
            <span className="msg-from-name">{fromMe ? (session.user.display_name || from.name || from.address) : displayName(from)}</span>
            {from.name && <span className="msg-from-addr">&lt;{from.address}&gt;</span>}
            {v?.sender && <span className="msg-via">via {v.sender.address.split("@")[1] ?? v.sender.address}</span>}
            {v?.unsubscribe && !fromMe && (
              <button className="link-btn unsubscribe" onClick={() => void unsubscribe()}>
                Unsubscribe
              </button>
            )}
          </div>
          <div className="msg-to-line">
            <span>{recipientSummary(m, session.mine)}</span>
            <Popover
              trigger={(p) => (
                <button className="icon-btn tiny" aria-label="Show details" title="Show details" {...p}>
                  <ChevronDown size={16} />
                </button>
              )}
              className="details-popover"
            >
              {() => <Details m={m} v={v} />}
            </Popover>
          </div>
        </div>
        <div className="msg-actions">
          {m.has_attachments && <Paperclip size={16} className="msg-clip" aria-label="Has attachments" />}
          <span className="msg-date" title={fullDate(m.received_at)}>
            <span className="date-long">{messageDate(m.received_at)}</span>
            <span className="date-short">{listDate(m.received_at)}</span>
          </span>
          <button
            className={"icon-btn star-btn" + (m.flagged ? " on" : "")}
            aria-pressed={m.flagged}
            aria-label={m.flagged ? "Starred" : "Not starred"}
            title={m.flagged ? "Starred" : "Not starred"}
            onClick={() => void perform(target, { action: m.flagged ? "unstar" : "star" }, { quiet: true })}
          >
            <Star size={18} fill={m.flagged ? "currentColor" : "none"} />
          </button>
          <button className="icon-btn" aria-label="Reply" title="Reply" onClick={() => void composer.reply(m.id, "reply")}>
            <Reply size={18} />
          </button>
          <Menu
            align="right"
            trigger={(p) => (
              <button className="icon-btn" aria-label="More message actions" title="More" {...p}>
                <EllipsisVertical size={18} />
              </button>
            )}
            items={[
              { label: "Reply", icon: <Reply size={16} />, onSelect: () => void composer.reply(m.id, "reply") },
              replyAllUseful(m, session.mine) && {
                label: "Reply all",
                icon: <ReplyAll size={16} />,
                onSelect: () => void composer.reply(m.id, "reply_all"),
              },
              { label: "Forward", icon: <Forward size={16} />, onSelect: () => void composer.reply(m.id, "forward") },
              "separator",
              v?.mailbox_special !== "trash" && { label: "Delete this message", icon: <Trash2 size={16} />, onSelect: () => void act({ action: "trash" }) },
              inSpam
                ? { label: "Report not spam", icon: <OctagonAlert size={16} />, onSelect: () => void act({ action: "notspam" }) }
                : !fromMe && { label: "Report spam", icon: <OctagonAlert size={16} />, onSelect: () => void act({ action: "spam" }) },
              !fromMe &&
                (blocked
                  ? { label: `Unblock “${displayName(from)}”`, icon: <Ban size={16} />, onSelect: () => void setBlocked(false) }
                  : { label: `Block “${displayName(from)}”`, icon: <Ban size={16} />, onSelect: () => void setBlocked(true) }),
              {
                label: "Mark unread from here",
                onSelect: () =>
                  void perform({ threaded: false, messageIds: laterIds }, { action: "unread" }, { quiet: true }).then((ok) => ok && onGone()),
              },
              "separator",
              { label: "Show original", icon: <Code size={16} />, onSelect: () => window.open(`/api/messages/${m.id}/raw`, "_blank", "noopener") },
              { label: "Download message", icon: <Download size={16} />, onSelect: () => (window.location.href = `/api/messages/${m.id}/raw?download=1`) },
            ]}
          />
        </div>
      </header>

      {inSpam && (
        <Banner tone="danger" icon={<OctagonAlert size={20} />}>
          <p>
            <strong>This message is in Spam.</strong> Be careful with links, attachments and requests for personal information.
          </p>
          <button className="btn small" onClick={() => void act({ action: "notspam" })}>
            Report not spam
          </button>
        </Banner>
      )}
      {authFailed && !inSpam && (
        <Banner tone="warn" icon={<ShieldAlert size={20} />}>
          <p>
            <strong>Be careful with this message.</strong> It couldn't be verified that it actually came from{" "}
            {from.address.split("@")[1] ?? from.address}. Avoid clicking links, downloading attachments or replying with personal information.
          </p>
        </Banner>
      )}
      {blocked && (
        <Banner tone="info" icon={<Ban size={20} />}>
          <p>You blocked {from.address}. New messages from this sender go to Spam.</p>
          <button className="btn small" onClick={() => void setBlocked(false)}>
            Unblock sender
          </button>
        </Banner>
      )}
      {v?.body.remote_blocked && !v.remote_allowed && (
        <Banner tone="info" icon={<ImageOff size={20} />}>
          <p>Images and other remote content in this message are hidden.</p>
          <button className="btn small" onClick={() => setRemote(true)}>
            Show images
          </button>
          {!inSpam && (
            <button className="btn small" onClick={() => void trustSender()}>
              Always show images from {from.address}
            </button>
          )}
        </Banner>
      )}

      <div className="msg-body">
        {q.isLoading ? (
          <Spinner />
        ) : q.error ? (
          <p className="error-text">
            This message couldn't be displayed.{" "}
            <button className="link-btn" onClick={() => void q.refetch()}>
              Try again
            </button>
          </p>
        ) : (
          v && (
            <MessageBody
              document={v.body.document}
              showQuotes={showQuotes}
              onMailto={(to, subject) => composer.compose({ to: parseAddresses(to), subject })}
            />
          )
        )}
        {v?.body.has_quote && (
          <button
            className="quote-toggle"
            title={showQuotes ? "Hide expanded content" : "Show trimmed content"}
            aria-label={showQuotes ? "Hide expanded content" : "Show trimmed content"}
            aria-expanded={showQuotes}
            onClick={() => setShowQuotes(!showQuotes)}
          >
            •••
          </button>
        )}
      </div>
      {atts.length > 0 && <Attachments messageId={m.id} list={atts} />}
    </article>
  );
}
