import {
  createContext,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type DragEvent,
  type FormEvent,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ChevronDown, Maximize2, Minimize2, Minus, Paperclip, Trash2, Type, X } from "lucide-react";
import { api, ApiError, errorMessage, refURL, uploadFile } from "../api";
import { keys } from "../queries";
import { navigate } from "../router";
import { useSession } from "../session";
import type { Address, AttachmentRef, ComposeState, SendResult } from "../types";
import { formatBytes, fullDate, isValidEmail, toLocalInput } from "../util";
import { useDialogs } from "./Dialog";
import { Editor, type EditorHandle } from "./Editor";
import { Popover } from "./Menu";
import { RecipientField } from "./Recipients";
import { useToast } from "./Toasts";

type ReplyMode = "reply" | "reply_all" | "forward";

interface ComposerAPI {
  compose: (prefill?: Partial<ComposeState>) => void;
  openDraft: (id: number) => Promise<void>;
  reply: (messageId: number, mode: ReplyMode) => Promise<void>;
}

const ComposerContext = createContext<ComposerAPI | null>(null);

export function useComposer(): ComposerAPI {
  const c = useContext(ComposerContext);
  if (!c) throw new Error("useComposer outside of ComposerProvider");
  return c;
}

interface Win {
  key: number;
  initial: ComposeState;
  body: string;
  quote: string;
  focusBody: boolean;
  minimized: boolean;
  maximized: boolean;
}

let nextKey = 1;

/** Splits a trailing quoted message off a draft so it can be hidden behind "…" like Gmail does. */
function splitQuote(html: string): { body: string; quote: string } {
  const doc = new DOMParser().parseFromString(`<body>${html}</body>`, "text/html");
  const last = doc.body.lastElementChild;
  if (last && last.classList.contains("tm_quote")) {
    const quote = last.outerHTML;
    last.remove();
    return { body: doc.body.innerHTML, quote };
  }
  return { body: html, quote: "" };
}

export function ComposerProvider({ children }: { children: ReactNode }) {
  const session = useSession();
  const toast = useToast();
  const [wins, setWins] = useState<Win[]>([]);
  const draftIds = useRef(new Map<number, number>());
  const sessionRef = useRef(session);
  sessionRef.current = session;

  const api_ = useMemo<ComposerAPI>(() => {
    const signature = () => {
      // The server stores the signature as sanitized HTML.
      const sig = sessionRef.current.settings.signature.trim();
      return sig ? `<div><br></div><div class="tm_signature">-- <br>${sig}</div>` : "";
    };
    const blank = (): ComposeState => ({
      from: sessionRef.current.user.email,
      to: [],
      cc: [],
      bcc: [],
      subject: "",
      html: "",
      attachments: [],
    });
    const open = (w: Omit<Win, "key" | "minimized" | "maximized">) => {
      setWins((list) => {
        // Keep at most two expanded windows next to each other.
        let expanded = list.filter((x) => !x.minimized).length;
        const next = list.map((x) => {
          if (expanded >= 2 && !x.minimized) {
            expanded--;
            return { ...x, minimized: true };
          }
          return x;
        });
        return [...next, { ...w, key: nextKey++, minimized: false, maximized: false }];
      });
    };
    return {
      compose: (prefill = {}) => {
        const initial = { ...blank(), ...prefill };
        open({ initial, body: prefill.html || "<div><br></div>" + signature(), quote: "", focusBody: false });
      },
      openDraft: async (id) => {
        for (const [key, did] of draftIds.current) {
          if (did === id) {
            setWins((list) => list.map((w) => (w.key === key ? { ...w, minimized: false } : w)));
            return;
          }
        }
        try {
          const st = await api.draft(id);
          const { body, quote } = splitQuote(st.html || "");
          open({ initial: st, body: body || "<div><br></div>", quote, focusBody: (st.to ?? []).length > 0 });
        } catch (err) {
          toast.error(err);
        }
      },
      reply: async (messageId, mode) => {
        try {
          const st = await api.composeFrom(messageId, mode);
          if (mode === "forward") {
            open({ initial: st, body: "<div><br></div>" + signature() + "<br>" + st.html, quote: "", focusBody: false });
          } else {
            open({ initial: st, body: "<div><br></div>" + signature(), quote: st.html, focusBody: true });
          }
        } catch (err) {
          toast.error(err);
        }
      },
    };
  }, [toast]);

  const update = (key: number, patch: Partial<Win>) => setWins((list) => list.map((w) => (w.key === key ? { ...w, ...patch } : w)));
  const remove = (key: number) => {
    draftIds.current.delete(key);
    setWins((list) => list.filter((w) => w.key !== key));
  };

  return (
    <ComposerContext.Provider value={api_}>
      {children}
      <div className="compose-dock">
        {wins.map((w) => (
          <ComposeWindow
            key={w.key}
            win={w}
            onClose={() => remove(w.key)}
            onMinimize={() => update(w.key, { minimized: !w.minimized, maximized: false })}
            onMaximize={() => update(w.key, { maximized: !w.maximized, minimized: false })}
            onDraftId={(id) => draftIds.current.set(w.key, id)}
          />
        ))}
      </div>
    </ComposerContext.Provider>
  );
}

interface Att extends AttachmentRef {
  key: string;
  progress?: number;
  error?: string;
  abort?: AbortController;
}

let attKey = 1;
const newAttKey = () => `a${attKey++}`;

function SchedulePicker({ onPick }: { onPick: (d: Date) => void }) {
  const now = new Date();
  const at = (days: number, hour: number) => {
    const d = new Date(now);
    d.setDate(d.getDate() + days);
    d.setHours(hour, 0, 0, 0);
    return d;
  };
  const monday = new Date(now);
  monday.setDate(monday.getDate() + ((8 - monday.getDay()) % 7 || 7));
  monday.setHours(8, 0, 0, 0);
  const options = [
    { label: "Tomorrow morning", date: at(1, 8) },
    { label: "Tomorrow afternoon", date: at(1, 13) },
    { label: "Monday morning", date: monday },
  ];
  const [custom, setCustom] = useState(toLocalInput(at(1, 8)));
  const fmt = new Intl.DateTimeFormat(undefined, { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const d = new Date(custom);
    if (!Number.isNaN(d.getTime()) && d.getTime() > Date.now()) onPick(d);
  };
  return (
    <div className="menu schedule">
      <div className="menu-heading">Schedule send</div>
      {options.map((o) => (
        <button key={o.label} type="button" role="menuitem" className="menu-item" onClick={() => onPick(o.date)}>
          <span className="menu-label">{o.label}</span>
          <span className="menu-hint">{fmt.format(o.date)}</span>
        </button>
      ))}
      <div className="menu-sep" />
      <form className="schedule-custom" onSubmit={submit}>
        <label className="field">
          <span>Pick date &amp; time</span>
          <input type="datetime-local" value={custom} min={toLocalInput(now)} onChange={(e) => setCustom(e.target.value)} />
        </label>
        <button className="btn primary" type="submit">
          Schedule send
        </button>
      </form>
    </div>
  );
}

interface WindowProps {
  win: Win;
  onClose: () => void;
  onMinimize: () => void;
  onMaximize: () => void;
  onDraftId: (id: number) => void;
}

function ComposeWindow({ win, onClose, onMinimize, onMaximize, onDraftId }: WindowProps) {
  const session = useSession();
  const composer = useComposer();
  const toast = useToast();
  const dialogs = useDialogs();
  const qc = useQueryClient();
  const init = win.initial;

  const [from, setFrom] = useState(init.from || session.user.email);
  const [to, setTo] = useState<Address[]>(init.to ?? []);
  const [cc, setCc] = useState<Address[]>(init.cc ?? []);
  const [bcc, setBcc] = useState<Address[]>(init.bcc ?? []);
  const [showCc, setShowCc] = useState((init.cc ?? []).length > 0);
  const [showBcc, setShowBcc] = useState((init.bcc ?? []).length > 0);
  const [subject, setSubject] = useState(init.subject ?? "");
  const [atts, setAttsState] = useState<Att[]>(() => (init.attachments ?? []).map((a) => ({ ...a, key: newAttKey() })));
  const [quote, setQuote] = useState(win.quote);
  const [formatting, setFormatting] = useState(false);
  const [status, setStatus] = useState<"" | "saving" | "saved" | "error">("");
  const [sending, setSending] = useState(false);
  const [dragging, setDragging] = useState(false);

  const editor = useRef<EditorHandle>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const attsRef = useRef(atts);
  const draftId = useRef<number | undefined>(init.draft_id);
  const touched = useRef(false);
  const dirty = useRef(false);
  const closed = useRef(false);
  const timer = useRef(0);
  const chain = useRef<Promise<unknown>>(Promise.resolve());
  const fields = useRef({ from, to, cc, bcc, subject, quote });
  fields.current = { from, to, cc, bcc, subject, quote };

  // Attachments also live in a ref so that saves see updates made by the previous save immediately.
  const setAtts = (fn: (prev: Att[]) => Att[]) => {
    attsRef.current = fn(attsRef.current);
    setAttsState(attsRef.current);
  };

  const snapshot = () => {
    const f = fields.current;
    const ready = attsRef.current.filter((a) => a.progress === undefined && !a.error);
    const state: ComposeState = {
      draft_id: draftId.current,
      from: f.from,
      to: f.to,
      cc: f.cc,
      bcc: f.bcc,
      subject: f.subject,
      html: (editor.current?.html() ?? "") + f.quote,
      attachments: ready.map((a) => ({
        upload_id: a.upload_id,
        message_id: a.message_id,
        part: a.part,
        filename: a.filename,
        content_type: a.content_type,
        size: a.size,
      })),
      in_reply_to_id: init.in_reply_to_id,
      forward_of_id: init.forward_of_id,
    };
    return { state, attKeys: ready.map((a) => a.key) };
  };

  /** Saves the draft. Saves run one at a time because each one replaces the previous draft. */
  const save = (): Promise<unknown> => {
    window.clearTimeout(timer.current);
    const step = chain.current
      .catch(() => {})
      .then(async () => {
        if (!dirty.current || closed.current) return;
        dirty.current = false;
        const { state, attKeys } = snapshot();
        setStatus("saving");
        try {
          const res = await api.saveDraft(state);
          draftId.current = res.draft_id;
          onDraftId(res.draft_id);
          const refs = res.attachments ?? [];
          setAtts((prev) =>
            prev.map((a) => {
              const i = attKeys.indexOf(a.key);
              return i >= 0 && refs[i] ? { ...refs[i], key: a.key } : a;
            }),
          );
          setStatus("saved");
        } catch (err) {
          dirty.current = true;
          setStatus("error");
          throw err;
        }
      });
    chain.current = step;
    return step;
  };

  const markChanged = () => {
    touched.current = true;
    dirty.current = true;
    setStatus("");
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => void save().catch(() => {}), 2500);
  };

  useEffect(() => {
    const onUnload = (e: BeforeUnloadEvent) => {
      if (!closed.current && (dirty.current || attsRef.current.some((a) => a.progress !== undefined))) e.preventDefault();
    };
    window.addEventListener("beforeunload", onUnload);
    return () => {
      window.removeEventListener("beforeunload", onUnload);
      window.clearTimeout(timer.current);
    };
  }, []);

  const addFiles = (files: File[]) => {
    const max = session.info.max_upload_bytes;
    for (const file of files) {
      if (max && file.size > max) {
        toast.error(new Error(`“${file.name}” is larger than the ${formatBytes(max)} limit.`));
        continue;
      }
      const key = newAttKey();
      const ctl = new AbortController();
      setAtts((prev) => [
        ...prev,
        { key, filename: file.name, content_type: file.type || "application/octet-stream", size: file.size, progress: 0, abort: ctl },
      ]);
      uploadFile(file, (p) => setAtts((prev) => prev.map((a) => (a.key === key ? { ...a, progress: p } : a))), ctl.signal)
        .then((up) => {
          setAtts((prev) =>
            prev.map((a) =>
              a.key === key ? { key, upload_id: up.id, filename: up.filename, content_type: up.content_type, size: up.size } : a,
            ),
          );
          markChanged();
        })
        .catch((err) => {
          if (err instanceof DOMException && err.name === "AbortError") return;
          setAtts((prev) => prev.map((a) => (a.key === key ? { ...a, progress: undefined, error: errorMessage(err) } : a)));
        });
    }
  };

  const removeAtt = (key: string) => {
    attsRef.current.find((a) => a.key === key)?.abort?.abort();
    setAtts((prev) => prev.filter((a) => a.key !== key));
    markChanged();
  };

  const report = (res: SendResult) => {
    const undo = async () => {
      try {
        const r = await api.cancelSend(res.queue_id);
        toast.show({ message: res.scheduled ? "Scheduled send canceled." : "Sending undone." });
        void composer.openDraft(r.draft_id);
      } catch (err) {
        toast.error(err instanceof ApiError && (err.status === 404 || err.status === 409) ? new Error("Too late: the message is already on its way.") : err);
      }
      void qc.invalidateQueries({ queryKey: keys.outbox });
    };
    const view = () =>
      navigate(session.settings.conversation_view ? `/mail/sent/thread/${res.thread_id}` : `/mail/sent/message/${res.message_id}`);
    void qc.invalidateQueries({ queryKey: keys.outbox });
    if (res.scheduled) {
      toast.show({
        message: `Message scheduled for ${fullDate(res.send_at)}.`,
        actions: [
          { label: "Undo", run: () => void undo() },
          { label: "View", run: () => navigate("/mail/scheduled") },
        ],
        duration: 10000,
      });
      return;
    }
    const secs = session.settings.undo_send_seconds;
    toast.show({
      message: "Message sent.",
      actions: secs > 0 ? [{ label: "Undo", run: () => void undo() }, { label: "View message", run: view }] : [{ label: "View message", run: view }],
      duration: secs > 0 ? secs * 1000 : 6000,
    });
  };

  const send = async (scheduleAt?: Date) => {
    if (sending) return;
    const f = fields.current;
    const all = [...f.to, ...f.cc, ...f.bcc];
    if (all.length === 0) {
      toast.error(new Error("Add at least one recipient."));
      return;
    }
    const bad = all.find((a) => !isValidEmail(a.address));
    if (bad) {
      toast.error(new Error(`The address “${bad.address}” is not valid.`));
      return;
    }
    if (attsRef.current.some((a) => a.progress !== undefined)) {
      toast.error(new Error("Wait for the attachments to finish uploading."));
      return;
    }
    if (attsRef.current.some((a) => a.error)) {
      toast.error(new Error("Remove the attachments that failed to upload."));
      return;
    }
    const text = new DOMParser().parseFromString(editor.current?.html() ?? "", "text/html").body.textContent?.trim();
    if (!f.subject.trim() && !text && attsRef.current.length === 0) {
      const ok = await dialogs.confirm({ title: "Send an empty message?", message: "This message has no subject or text.", confirmLabel: "Send" });
      if (!ok) return;
    }
    setSending(true);
    window.clearTimeout(timer.current);
    await chain.current.catch(() => {});
    const { state } = snapshot();
    if (scheduleAt) state.schedule_at = scheduleAt.toISOString();
    try {
      const res = await api.send(state);
      closed.current = true;
      onClose();
      report(res);
    } catch (err) {
      setSending(false);
      toast.error(err);
    }
  };

  const close = async () => {
    window.clearTimeout(timer.current);
    if (touched.current) {
      try {
        await save();
      } catch (err) {
        toast.error(new Error(`The draft could not be saved: ${errorMessage(err)}`));
        return;
      }
      if (draftId.current) toast.show({ message: "Draft saved." });
    }
    closed.current = true;
    for (const a of attsRef.current) a.abort?.abort();
    onClose();
  };

  const discard = async () => {
    window.clearTimeout(timer.current);
    dirty.current = false;
    closed.current = true;
    for (const a of attsRef.current) a.abort?.abort();
    await chain.current.catch(() => {});
    const id = draftId.current;
    onClose();
    if (id) {
      try {
        await api.messageAction([id], { action: "delete" });
      } catch (err) {
        toast.error(err);
        return;
      }
    }
    toast.show({ message: "Draft discarded." });
  };

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      // Let address fields commit typed text first.
      window.setTimeout(() => void send(), 0);
    } else if (e.key === "Escape" && !e.defaultPrevented) {
      e.preventDefault();
      void close();
    }
  };

  const onDragOver = (e: DragEvent) => {
    if (!e.dataTransfer.types.includes("Files")) return;
    e.preventDefault();
    setDragging(true);
  };
  const onDrop = (e: DragEvent) => {
    setDragging(false);
    if (!e.dataTransfer.files.length) return;
    e.preventDefault();
    addFiles(Array.from(e.dataTransfer.files));
  };

  const addresses = session.addresses.includes(from) ? session.addresses : [from, ...session.addresses];
  const title = subject.trim() || (init.in_reply_to_id ? "Reply" : init.forward_of_id ? "Forward" : "New Message");

  return (
    <>
      {win.maximized && <div className="compose-backdrop" onClick={onMaximize} />}
      <section
        className={"compose-win" + (win.minimized ? " min" : "") + (win.maximized ? " max" : "")}
        role="dialog"
        aria-label={title}
        onKeyDown={onKeyDown}
        onDragOver={onDragOver}
        onDragLeave={(e) => !e.currentTarget.contains(e.relatedTarget as Node) && setDragging(false)}
        onDrop={onDrop}
      >
        <header className="compose-head" onClick={onMinimize}>
          <span className="compose-title">{title}</span>
          <button
            className="icon-btn small"
            aria-label={win.minimized ? "Restore" : "Minimize"}
            title={win.minimized ? "Restore" : "Minimize"}
            onClick={(e) => {
              e.stopPropagation();
              onMinimize();
            }}
          >
            <Minus size={16} />
          </button>
          <button
            className="icon-btn small"
            aria-label={win.maximized ? "Exit full screen" : "Full screen"}
            title={win.maximized ? "Exit full screen" : "Full screen"}
            onClick={(e) => {
              e.stopPropagation();
              onMaximize();
            }}
          >
            {win.maximized ? <Minimize2 size={16} /> : <Maximize2 size={16} />}
          </button>
          <button
            className="icon-btn small"
            aria-label="Save & close"
            title="Save & close"
            onClick={(e) => {
              e.stopPropagation();
              void close();
            }}
          >
            <X size={16} />
          </button>
        </header>
        <div className="compose-body" hidden={win.minimized}>
          {addresses.length > 1 && (
            <label className="compose-row">
              <span className="rcpt-label">From</span>
              <select
                value={from}
                onChange={(e) => {
                  setFrom(e.target.value);
                  markChanged();
                }}
              >
                {addresses.map((a) => (
                  <option key={a} value={a}>
                    {session.user.display_name ? `${session.user.display_name} <${a}>` : a}
                  </option>
                ))}
              </select>
            </label>
          )}
          <RecipientField
            label="To"
            value={to}
            autoFocus={!win.focusBody}
            onChange={(v) => {
              setTo(v);
              markChanged();
            }}
            extra={
              <span className="cc-toggles">
                {!showCc && (
                  <button type="button" onClick={() => setShowCc(true)}>
                    Cc
                  </button>
                )}
                {!showBcc && (
                  <button type="button" onClick={() => setShowBcc(true)}>
                    Bcc
                  </button>
                )}
              </span>
            }
          />
          {showCc && (
            <RecipientField
              label="Cc"
              value={cc}
              autoFocus={cc.length === 0}
              onChange={(v) => {
                setCc(v);
                markChanged();
              }}
            />
          )}
          {showBcc && (
            <RecipientField
              label="Bcc"
              value={bcc}
              autoFocus={bcc.length === 0}
              onChange={(v) => {
                setBcc(v);
                markChanged();
              }}
            />
          )}
          <input
            className="compose-subject"
            placeholder="Subject"
            aria-label="Subject"
            value={subject}
            onChange={(e) => {
              setSubject(e.target.value);
              markChanged();
            }}
          />
          <Editor
            ref={editor}
            initialHTML={win.body}
            onChange={markChanged}
            onFiles={addFiles}
            showToolbar={formatting}
            autoFocus={win.focusBody}
          >
            {quote && (
              <button
                type="button"
                className="quote-toggle"
                title="Show trimmed content"
                aria-label="Show trimmed content"
                onClick={() => {
                  editor.current?.append(quote);
                  setQuote("");
                }}
              >
                •••
              </button>
            )}
            {atts.length > 0 && (
              <ul className="compose-atts">
                {atts.map((a) => {
                  const url = refURL(a);
                  return (
                    <li key={a.key} className={a.error ? "error" : ""} title={a.error}>
                      {url ? (
                        <a href={url} target="_blank" rel="noreferrer" className="att-name">
                          {a.filename}
                        </a>
                      ) : (
                        <span className="att-name">{a.filename}</span>
                      )}
                      <span className="att-size">{a.error ? "Upload failed" : formatBytes(a.size)}</span>
                      {a.progress !== undefined && (
                        <span className="att-progress">
                          <span style={{ width: `${Math.round(a.progress * 100)}%` }} />
                        </span>
                      )}
                      <button type="button" className="icon-btn small" aria-label={`Remove ${a.filename}`} onClick={() => removeAtt(a.key)}>
                        <X size={14} />
                      </button>
                    </li>
                  );
                })}
              </ul>
            )}
          </Editor>
          <footer className="compose-foot">
            <div className="send-group">
              <button className="btn primary send" disabled={sending} onClick={() => void send()} title="Send (Ctrl+Enter)">
                {sending ? "Sending…" : "Send"}
              </button>
              <Popover
                up
                role="menu"
                className="menu-popover"
                trigger={(p) => (
                  <button className="btn primary send-more" aria-label="More send options" title="More send options" disabled={sending} {...p}>
                    <ChevronDown size={18} />
                  </button>
                )}
              >
                {(closeMenu) => (
                  <SchedulePicker
                    onPick={(d) => {
                      closeMenu();
                      void send(d);
                    }}
                  />
                )}
              </Popover>
            </div>
            <button
              type="button"
              className={"icon-btn" + (formatting ? " on" : "")}
              aria-pressed={formatting}
              aria-label="Formatting options"
              title="Formatting options"
              onClick={() => setFormatting(!formatting)}
            >
              <Type size={20} />
            </button>
            <button type="button" className="icon-btn" aria-label="Attach files" title="Attach files" onClick={() => fileInput.current?.click()}>
              <Paperclip size={20} />
            </button>
            <input
              ref={fileInput}
              type="file"
              multiple
              hidden
              onChange={(e) => {
                addFiles(Array.from(e.target.files ?? []));
                e.target.value = "";
              }}
            />
            <span className="compose-status" aria-live="polite">
              {status === "saving" ? "Saving…" : status === "saved" ? "Draft saved" : status === "error" ? "Couldn't save draft" : ""}
            </span>
            <button type="button" className="icon-btn" aria-label="Discard draft" title="Discard draft" onClick={() => void discard()}>
              <Trash2 size={20} />
            </button>
          </footer>
          {dragging && <div className="drop-overlay">Drop files to attach them</div>}
        </div>
      </section>
    </>
  );
}
