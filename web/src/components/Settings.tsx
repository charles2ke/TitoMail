import { useRef, useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Filter as FilterIcon, Folder, Pencil, Plus, Tag, Trash2 } from "lucide-react";
import { api } from "../api";
import { useFolderActions } from "../folderActions";
import { keys, useLabels, useMailboxes } from "../queries";
import { Link } from "../router";
import { useSession } from "../session";
import type { Filter, Label, Mailbox, Settings as SettingsT, Vacation } from "../types";
import { formatBytes, fullDate, toLocalInput } from "../util";
import { folderTree } from "../views";
import { CopyButton, EmptyState, Spinner } from "./common";
import { Modal, useDialogs } from "./Dialog";
import { Editor, type EditorHandle } from "./Editor";
import { LabelEditor, labelColors } from "./LabelEditor";
import { useToast } from "./Toasts";

const tabs = [
  { id: "general", label: "General" },
  { id: "vacation", label: "Out of office" },
  { id: "filters", label: "Filters" },
  { id: "labels", label: "Labels & folders" },
  { id: "senders", label: "Blocked & trusted" },
  { id: "account", label: "Account" },
  { id: "apps", label: "Mail apps" },
];

export function SettingsPage({ tab }: { tab: string }) {
  const current = tabs.some((t) => t.id === tab) ? tab : "general";
  return (
    <div className="page">
      <div className="page-head">
        <h1>Settings</h1>
      </div>
      <nav className="tabs" aria-label="Settings sections">
        {tabs.map((t) => (
          <Link
            key={t.id}
            to={`/settings/${t.id}`}
            replace
            className={"tab" + (t.id === current ? " active" : "")}
            aria-current={t.id === current ? "page" : undefined}
          >
            {t.label}
          </Link>
        ))}
      </nav>
      <div className="page-body">
        {current === "general" && <GeneralTab />}
        {current === "vacation" && <VacationTab />}
        {current === "filters" && <FiltersTab />}
        {current === "labels" && <LabelsTab />}
        {current === "senders" && <SendersTab />}
        {current === "account" && <AccountTab />}
        {current === "apps" && <AppsTab />}
      </div>
    </div>
  );
}

function Row({ title, hint, children }: { title: string; hint?: ReactNode; children: ReactNode }) {
  return (
    <div className="settings-row">
      <div className="settings-label">
        <div>{title}</div>
        {hint && <div className="muted small">{hint}</div>}
      </div>
      <div className="settings-control">{children}</div>
    </div>
  );
}

function Radios<T extends string | number | boolean>(props: {
  name: string;
  value: T;
  options: { value: T; label: ReactNode }[];
  onChange: (v: T) => void;
}) {
  return (
    <div className="radios" role="radiogroup">
      {props.options.map((o) => (
        <label key={String(o.value)} className="check">
          <input type="radio" name={props.name} checked={o.value === props.value} onChange={() => props.onChange(o.value)} />
          <span>{o.label}</span>
        </label>
      ))}
    </div>
  );
}

function SaveBar({ dirty, saving, onCancel }: { dirty: boolean; saving: boolean; onCancel: () => void }) {
  return (
    <div className="settings-foot">
      <button className="btn primary" type="submit" disabled={!dirty || saving}>
        {saving ? "Saving…" : "Save changes"}
      </button>
      <button className="btn" type="button" disabled={!dirty || saving} onClick={onCancel}>
        Cancel
      </button>
    </div>
  );
}

/** Reports whether editor HTML has no visible content. */
function isBlankHTML(html: string): boolean {
  if (/<img\b/i.test(html)) return false;
  return html.replace(/<[^>]*>|&nbsp;|\s/gi, "") === "";
}

const notificationsSupported = typeof Notification !== "undefined";

function GeneralTab() {
  const session = useSession();
  const toast = useToast();
  const [draft, setDraft] = useState<SettingsT>(session.settings);
  const [sigDirty, setSigDirty] = useState(false);
  const [sigKey, setSigKey] = useState(0);
  const [saving, setSaving] = useState(false);
  const sigRef = useRef<EditorHandle>(null);
  const strip = (s: SettingsT) => JSON.stringify({ ...s, signature: "" });
  const dirty = sigDirty || strip(draft) !== strip(session.settings);
  const set = <K extends keyof SettingsT>(k: K, v: SettingsT[K]) => setDraft((d) => ({ ...d, [k]: v }));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (draft.desktop_notifications && notificationsSupported && Notification.permission === "default") {
      // Must run in the click handler, before anything else is awaited.
      void Notification.requestPermission();
    }
    setSaving(true);
    try {
      const html = sigRef.current?.html() ?? draft.signature;
      const saved = await session.saveSettings({ ...draft, signature: isBlankHTML(html) ? "" : html });
      setDraft(saved);
      setSigDirty(false);
      toast.show({ message: "Settings saved." });
    } catch (err) {
      toast.error(err);
    } finally {
      setSaving(false);
    }
  };

  const cancel = () => {
    setDraft(session.settings);
    setSigDirty(false);
    setSigKey((k) => k + 1);
  };

  return (
    <form className="settings-form" onSubmit={save}>
      <Row title="Theme">
        <Radios
          name="theme"
          value={draft.theme}
          onChange={(v) => set("theme", v)}
          options={[
            { value: "system", label: "Device default" },
            { value: "light", label: "Light" },
            { value: "dark", label: "Dark" },
          ]}
        />
      </Row>
      <Row title="Density">
        <Radios
          name="density"
          value={draft.density}
          onChange={(v) => set("density", v)}
          options={[
            { value: "comfortable", label: "Comfortable" },
            { value: "compact", label: "Compact" },
          ]}
        />
      </Row>
      <Row title="Reading pane" hint="Where opened conversations are shown. Small windows always use the full width.">
        <Radios
          name="pane"
          value={draft.reading_pane}
          onChange={(v) => set("reading_pane", v)}
          options={[
            { value: "right", label: "Right of the list" },
            { value: "bottom", label: "Below the list" },
            { value: "none", label: "No split" },
          ]}
        />
      </Row>
      <Row title="Conversation view" hint="Group replies to the same message together.">
        <Radios
          name="conv"
          value={draft.conversation_view}
          onChange={(v) => set("conversation_view", v)}
          options={[
            { value: true, label: "Conversation view on" },
            { value: false, label: "Conversation view off" },
          ]}
        />
      </Row>
      <Row title="Page size">
        <label className="inline-field">
          Show
          <select value={draft.page_size} onChange={(e) => set("page_size", Number(e.target.value))}>
            {[10, 15, 20, 25, 50, 100].map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
          conversations per page
        </label>
      </Row>
      <Row title="Images" hint="Images loaded from the internet can tell senders that you opened their message.">
        <Radios
          name="images"
          value={draft.external_images}
          onChange={(v) => set("external_images", v)}
          options={[
            { value: "ask", label: "Ask before displaying external images" },
            { value: "always", label: "Always display external images" },
          ]}
        />
      </Row>
      <Row title="Undo send">
        <label className="inline-field">
          Send cancellation period:
          <select value={draft.undo_send_seconds} onChange={(e) => set("undo_send_seconds", Number(e.target.value))}>
            {[0, 5, 10, 20, 30].map((n) => (
              <option key={n} value={n}>
                {n === 0 ? "Off" : `${n} seconds`}
              </option>
            ))}
          </select>
        </label>
      </Row>
      <Row
        title="Desktop notifications"
        hint={
          !notificationsSupported
            ? "Your browser doesn't support notifications."
            : Notification.permission === "denied"
              ? "Notifications are blocked for this site in your browser settings."
              : "Shown for new messages in your Inbox while TitoMail is in the background."
        }
      >
        <Radios
          name="notify"
          value={draft.desktop_notifications}
          onChange={(v) => set("desktop_notifications", v)}
          options={[
            { value: true, label: "New mail notifications on" },
            { value: false, label: "Mail notifications off" },
          ]}
        />
      </Row>
      <Row title="Signature" hint="Added to the end of new messages, replies and forwards.">
        <div className="signature-editor">
          <Editor
            key={sigKey}
            ref={sigRef}
            initialHTML={draft.signature}
            showToolbar
            placeholder="No signature"
            onChange={() => setSigDirty(true)}
            onFiles={() => toast.show({ message: "Pictures can't be added to signatures." })}
          />
        </div>
      </Row>
      <SaveBar dirty={dirty} saving={saving} onCancel={cancel} />
    </form>
  );
}

function VacationTab() {
  const session = useSession();
  const toast = useToast();
  const [v, setV] = useState<Vacation>(session.settings.vacation);
  const [saving, setSaving] = useState(false);
  const dirty = JSON.stringify(v) !== JSON.stringify(session.settings.vacation);
  const set = <K extends keyof Vacation>(k: K, val: Vacation[K]) => setV((x) => ({ ...x, [k]: val }));
  const badRange = !!(v.start && v.end && v.end < v.start);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (badRange) return;
    setSaving(true);
    try {
      const saved = await session.saveSettings({ vacation: v });
      setV(saved.vacation);
      toast.show({ message: v.enabled ? "Automatic replies are on." : "Automatic replies are off." });
    } catch (err) {
      toast.error(err);
    } finally {
      setSaving(false);
    }
  };

  return (
    <form className="settings-form" onSubmit={save}>
      <Row
        title="Automatic replies"
        hint="Replies to incoming mail while you're away. Each sender gets the reply at most once every 4 days; mailing lists and automated mail never get one."
      >
        <Radios
          name="vac"
          value={v.enabled}
          onChange={(val) => {
            // Default the first day to today when turning replies on.
            setV((x) => ({ ...x, enabled: val, start: val && !x.start ? toLocalInput(new Date()).slice(0, 10) : x.start }));
          }}
          options={[
            { value: false, label: "Automatic replies off" },
            { value: true, label: "Automatic replies on" },
          ]}
        />
      </Row>
      <fieldset className="settings-fieldset" disabled={!v.enabled}>
        <Row title="Dates" hint="Leave the last day empty to keep replying until you turn this off.">
          <div className="date-range">
            <label className="field">
              <span>First day</span>
              <input type="date" value={v.start} onChange={(e) => set("start", e.target.value)} />
            </label>
            <label className="field">
              <span>Last day (optional)</span>
              <input type="date" value={v.end} min={v.start || undefined} onChange={(e) => set("end", e.target.value)} />
            </label>
          </div>
          {badRange && <div className="error-text">The last day can't be before the first day.</div>}
        </Row>
        <Row title="Message">
          <div className="stack">
            <label className="field">
              <span>Subject</span>
              <input value={v.subject} maxLength={200} placeholder="Re: (original subject)" onChange={(e) => set("subject", e.target.value)} />
            </label>
            <label className="field">
              <span>Message</span>
              <textarea
                rows={7}
                value={v.body}
                maxLength={10000}
                placeholder="I am currently away and will reply when I return."
                onChange={(e) => set("body", e.target.value)}
              />
            </label>
            <label className="check">
              <input type="checkbox" checked={v.contacts_only} onChange={(e) => set("contacts_only", e.target.checked)} />
              <span>Only send a response to people in my Contacts</span>
            </label>
          </div>
        </Row>
      </fieldset>
      <SaveBar dirty={dirty} saving={saving} onCancel={() => setV(session.settings.vacation)} />
    </form>
  );
}

const filtersKey = ["filters"] as const;

const emptyFilter: Filter = {
  id: 0,
  name: "",
  enabled: true,
  criteria: { from: "", to: "", subject: "", has_words: "", has_attachment: false },
  actions: { skip_inbox: false, mark_read: false, star: false, label_id: 0, move_to: 0, trash: false, never_spam: false },
};

function describeCriteria(f: Filter): string {
  const c = f.criteria;
  const parts: string[] = [];
  if (c.from.trim()) parts.push(`from:(${c.from.trim()})`);
  if (c.to.trim()) parts.push(`to:(${c.to.trim()})`);
  if (c.subject.trim()) parts.push(`subject:(${c.subject.trim()})`);
  if (c.has_words.trim()) parts.push(c.has_words.trim());
  if (c.has_attachment) parts.push("has:attachment");
  return parts.join(" ");
}

function describeActions(f: Filter, labels: Label[], mailboxes: Mailbox[]): string {
  const a = f.actions;
  const parts: string[] = [];
  if (a.trash) parts.push("Delete it");
  if (a.move_to) parts.push(`Move to “${mailboxes.find((m) => m.id === a.move_to)?.name ?? "deleted folder"}”`);
  if (a.skip_inbox) parts.push("Skip the Inbox");
  if (a.mark_read) parts.push("Mark as read");
  if (a.star) parts.push("Star it");
  if (a.label_id) parts.push(`Apply label “${labels.find((l) => l.id === a.label_id)?.name ?? "deleted label"}”`);
  if (a.never_spam) parts.push("Never send it to Spam");
  return parts.length ? parts.join(", ") : "Do nothing";
}

function FiltersTab() {
  const q = useQuery({ queryKey: filtersKey, queryFn: api.filters });
  const qc = useQueryClient();
  const { labels } = useLabels();
  const { mailboxes } = useMailboxes();
  const dialogs = useDialogs();
  const toast = useToast();
  const [editing, setEditing] = useState<Filter | null>(null);

  const save = async (f: Filter) => {
    try {
      await api.saveFilter(f);
      void qc.invalidateQueries({ queryKey: filtersKey });
      return true;
    } catch (err) {
      toast.error(err);
      return false;
    }
  };

  const remove = async (f: Filter) => {
    const ok = await dialogs.confirm({
      title: "Delete this filter?",
      message: describeCriteria(f),
      confirmLabel: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteFilter(f.id);
      void qc.invalidateQueries({ queryKey: filtersKey });
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <div className="settings-section">
      <p className="muted">Filters sort incoming mail as it arrives. They run in the order shown.</p>
      {q.isPending ? (
        <Spinner />
      ) : q.isError ? (
        <div className="error-text">{q.error.message}</div>
      ) : q.data.length === 0 ? (
        <EmptyState icon={<FilterIcon size={48} strokeWidth={1} />} title="No filters yet">
          <p>For example, label all mail from your bank, or skip the Inbox for newsletters.</p>
        </EmptyState>
      ) : (
        <ul className="settings-list">
          {q.data.map((f) => (
            <li key={f.id} className={"settings-item" + (f.enabled ? "" : " disabled")}>
              <label className="switch" title={f.enabled ? "Turn off" : "Turn on"}>
                <input
                  type="checkbox"
                  checked={f.enabled}
                  aria-label={`Filter ${f.name || describeCriteria(f)} enabled`}
                  onChange={(e) => void save({ ...f, enabled: e.target.checked })}
                />
                <span />
              </label>
              <div className="settings-item-main">
                {f.name && <div className="b">{f.name}</div>}
                <div>
                  <span className="muted">Matches: </span>
                  <code>{describeCriteria(f)}</code>
                </div>
                <div>
                  <span className="muted">Do this: </span>
                  {describeActions(f, labels, mailboxes)}
                </div>
              </div>
              <div className="settings-item-actions">
                <button className="btn small" onClick={() => setEditing(f)}>
                  Edit
                </button>
                <button className="btn small" onClick={() => void remove(f)}>
                  Delete
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      <div className="settings-foot">
        <button className="btn primary" onClick={() => setEditing(emptyFilter)}>
          <Plus size={16} /> Create a new filter
        </button>
      </div>
      {editing && (
        <FilterEditor
          filter={editing}
          labels={labels}
          mailboxes={mailboxes}
          onClose={() => setEditing(null)}
          onSave={async (f) => {
            if (await save(f)) {
              setEditing(null);
              toast.show({ message: f.id ? "Filter updated." : "Filter created." });
            }
          }}
        />
      )}
    </div>
  );
}

function FilterEditor(props: {
  filter: Filter;
  labels: Label[];
  mailboxes: Mailbox[];
  onClose: () => void;
  onSave: (f: Filter) => Promise<void>;
}) {
  const [f, setF] = useState<Filter>(props.filter);
  const [saving, setSaving] = useState(false);
  const folders = useFolderActions();
  const dialogs = useDialogs();
  const toast = useToast();
  const c = f.criteria;
  const a = f.actions;
  const setC = (k: keyof Filter["criteria"], v: string | boolean) => setF({ ...f, criteria: { ...c, [k]: v } });
  const setA = (k: keyof Filter["actions"], v: number | boolean) => setF({ ...f, actions: { ...a, [k]: v } });
  const empty = !c.from.trim() && !c.to.trim() && !c.subject.trim() && !c.has_words.trim() && !c.has_attachment;

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (empty) return;
    setSaving(true);
    await props.onSave(f);
    setSaving(false);
  };

  const newLabel = async () => {
    const name = await dialogs.prompt({ title: "New label", label: "Label name", confirmLabel: "Create" });
    if (!name) return;
    try {
      const l = await api.createLabel(name, labelColors[props.labels.length % labelColors.length]);
      folders.refresh();
      setA("label_id", l.id);
    } catch (err) {
      toast.error(err);
    }
  };

  const newFolder = async () => {
    const mb = await folders.createFolder();
    if (mb) setA("move_to", mb.id);
  };

  return (
    <Modal
      title={f.id ? "Edit filter" : "Create a filter"}
      onClose={props.onClose}
      wide
      footer={
        <>
          <button className="btn" type="button" onClick={props.onClose}>
            Cancel
          </button>
          <button className="btn primary" type="submit" form="filter-form" disabled={empty || saving}>
            {f.id ? "Save filter" : "Create filter"}
          </button>
        </>
      }
    >
      <form id="filter-form" className="filter-form" onSubmit={submit}>
        <fieldset>
          <legend>When a message arrives that matches</legend>
          <label className="field row">
            <span>From</span>
            <input value={c.from} onChange={(e) => setC("from", e.target.value)} placeholder="Name, address or domain" autoFocus />
          </label>
          <label className="field row">
            <span>To</span>
            <input value={c.to} onChange={(e) => setC("to", e.target.value)} />
          </label>
          <label className="field row">
            <span>Subject</span>
            <input value={c.subject} onChange={(e) => setC("subject", e.target.value)} />
          </label>
          <label className="field row">
            <span>Has the words</span>
            <input value={c.has_words} onChange={(e) => setC("has_words", e.target.value)} />
          </label>
          <label className="check">
            <input type="checkbox" checked={c.has_attachment} onChange={(e) => setC("has_attachment", e.target.checked)} />
            <span>Has attachment</span>
          </label>
          {empty && <div className="muted small">Fill in at least one of the fields above.</div>}
        </fieldset>
        <fieldset>
          <legend>Do this</legend>
          <label className="check">
            <input type="checkbox" checked={a.skip_inbox} onChange={(e) => setA("skip_inbox", e.target.checked)} />
            <span>Skip the Inbox (Archive it)</span>
          </label>
          <label className="check">
            <input type="checkbox" checked={a.mark_read} onChange={(e) => setA("mark_read", e.target.checked)} />
            <span>Mark as read</span>
          </label>
          <label className="check">
            <input type="checkbox" checked={a.star} onChange={(e) => setA("star", e.target.checked)} />
            <span>Star it</span>
          </label>
          <div className="check">
            <input
              type="checkbox"
              id="filter-label"
              checked={a.label_id > 0}
              onChange={(e) => setA("label_id", e.target.checked ? (props.labels[0]?.id ?? 0) : 0)}
              disabled={props.labels.length === 0 && a.label_id === 0}
            />
            <label htmlFor="filter-label">Apply the label:</label>
            <select
              value={a.label_id}
              aria-label="Label"
              onChange={(e) => (e.target.value === "new" ? void newLabel() : setA("label_id", Number(e.target.value)))}
            >
              <option value={0}>Choose label…</option>
              {props.labels.map((l) => (
                <option key={l.id} value={l.id}>
                  {l.name}
                </option>
              ))}
              {a.label_id > 0 && !props.labels.some((l) => l.id === a.label_id) && <option value={a.label_id}>(deleted label)</option>}
              <option value="new">New label…</option>
            </select>
          </div>
          <div className="check">
            <input
              type="checkbox"
              id="filter-move"
              checked={a.move_to > 0}
              onChange={(e) => {
                const first = folderTree(props.mailboxes)[0]?.mailbox.id ?? 0;
                setA("move_to", e.target.checked ? first : 0);
              }}
              disabled={folderTree(props.mailboxes).length === 0 && a.move_to === 0}
            />
            <label htmlFor="filter-move">Move to folder:</label>
            <select
              value={a.move_to}
              aria-label="Folder"
              onChange={(e) => (e.target.value === "new" ? void newFolder() : setA("move_to", Number(e.target.value)))}
            >
              <option value={0}>Choose folder…</option>
              {folderTree(props.mailboxes).map(({ mailbox: m }) => (
                <option key={m.id} value={m.id}>
                  {m.name}
                </option>
              ))}
              {a.move_to > 0 && !props.mailboxes.some((m) => m.id === a.move_to) && <option value={a.move_to}>(deleted folder)</option>}
              <option value="new">New folder…</option>
            </select>
          </div>
          <label className="check">
            <input type="checkbox" checked={a.trash} onChange={(e) => setA("trash", e.target.checked)} />
            <span>Delete it (move to Trash)</span>
          </label>
          <label className="check">
            <input type="checkbox" checked={a.never_spam} onChange={(e) => setA("never_spam", e.target.checked)} />
            <span>Never send it to Spam</span>
          </label>
        </fieldset>
        <fieldset>
          <legend>Options</legend>
          <label className="field row">
            <span>Name (optional)</span>
            <input value={f.name} maxLength={100} onChange={(e) => setF({ ...f, name: e.target.value })} />
          </label>
          <label className="check">
            <input type="checkbox" checked={f.enabled} onChange={(e) => setF({ ...f, enabled: e.target.checked })} />
            <span>Filter is on</span>
          </label>
        </fieldset>
      </form>
    </Modal>
  );
}

const systemFolderTitles: Record<string, string> = {
  inbox: "Inbox",
  sent: "Sent",
  drafts: "Drafts",
  archive: "Archive",
  junk: "Spam",
  trash: "Trash",
};

function LabelsTab() {
  const { labels, isPending: labelsPending } = useLabels();
  const { mailboxes, isPending: mailboxesPending } = useMailboxes();
  const folders = useFolderActions();
  const toast = useToast();
  const [editing, setEditing] = useState<Label | "new" | null>(null);
  const system = mailboxes.filter((m) => m.special_use);
  const user = folderTree(mailboxes);

  const toggleSubscribed = async (mb: Mailbox) => {
    try {
      await api.updateMailbox(mb.id, { subscribed: !mb.subscribed });
      folders.refresh();
    } catch (err) {
      toast.error(err);
    }
  };

  if (labelsPending || mailboxesPending) return <Spinner />;
  return (
    <div className="settings-section">
      <h2>Labels</h2>
      <p className="muted">A conversation can have several labels. Labels are only shown in TitoMail; mail apps see folders.</p>
      {labels.length === 0 ? (
        <p className="muted">You have no labels.</p>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Name</th>
              <th className="num">Conversations</th>
              <th className="num">Unread</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {labels.map((l) => (
              <tr key={l.id}>
                <td>
                  <Tag size={16} fill={l.color || "none"} color={l.color || "currentColor"} className="inline-icon" /> {l.name}
                </td>
                <td className="num">{l.total}</td>
                <td className="num">{l.unread}</td>
                <td className="row-actions">
                  <button className="icon-btn small" title="Edit" aria-label={`Edit label ${l.name}`} onClick={() => setEditing(l)}>
                    <Pencil size={16} />
                  </button>
                  <button
                    className="icon-btn small"
                    title="Remove"
                    aria-label={`Remove label ${l.name}`}
                    onClick={() => void folders.deleteLabel(l)}
                  >
                    <Trash2 size={16} />
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div className="settings-foot">
        <button className="btn" onClick={() => setEditing("new")}>
          <Plus size={16} /> Create new label
        </button>
      </div>

      <h2>Folders</h2>
      <p className="muted">
        Each message is in exactly one folder. Folders are also available in mail apps. Unticking “Show in mail apps” hides a folder
        from apps that only list subscribed folders.
      </p>
      <table className="table">
        <thead>
          <tr>
            <th>Name</th>
            <th className="num">Messages</th>
            <th className="num">Unread</th>
            <th className="num">Size</th>
            <th>Show in mail apps</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {system.map((m) => (
            <tr key={m.id}>
              <td>
                <Folder size={16} className="inline-icon" /> {systemFolderTitles[m.special_use!] ?? m.name}
              </td>
              <td className="num">{m.total}</td>
              <td className="num">{m.unread}</td>
              <td className="num">{formatBytes(m.size)}</td>
              <td>
                <input type="checkbox" checked disabled aria-label="System folders are always shown" />
              </td>
              <td className="row-actions">
                {(m.special_use === "trash" || m.special_use === "junk") && m.total > 0 && (
                  <button className="btn small" onClick={() => void folders.emptyFolder(m, systemFolderTitles[m.special_use!])}>
                    Empty
                  </button>
                )}
              </td>
            </tr>
          ))}
          {user.map(({ mailbox: m, depth, leaf }) => (
            <tr key={m.id}>
              <td style={{ paddingLeft: 12 + depth * 20 }}>
                <Folder size={16} className="inline-icon" /> {leaf}
              </td>
              <td className="num">{m.total}</td>
              <td className="num">{m.unread}</td>
              <td className="num">{formatBytes(m.size)}</td>
              <td>
                <input
                  type="checkbox"
                  checked={m.subscribed}
                  aria-label={`Show ${m.name} in mail apps`}
                  onChange={() => void toggleSubscribed(m)}
                />
              </td>
              <td className="row-actions">
                <button className="icon-btn small" title="Rename" aria-label={`Rename ${m.name}`} onClick={() => void folders.renameFolder(m)}>
                  <Pencil size={16} />
                </button>
                <button className="icon-btn small" title="Delete" aria-label={`Delete ${m.name}`} onClick={() => void folders.deleteFolder(m)}>
                  <Trash2 size={16} />
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="settings-foot">
        <button className="btn" onClick={() => void folders.createFolder()}>
          <Plus size={16} /> Create new folder
        </button>
      </div>
      {editing && <LabelEditor label={editing === "new" ? null : editing} onClose={() => setEditing(null)} />}
    </div>
  );
}

const sendersKey = ["senders"] as const;

function SendersTab() {
  const q = useQuery({ queryKey: sendersKey, queryFn: api.senders });
  const qc = useQueryClient();
  const toast = useToast();
  const [address, setAddress] = useState("");
  const [kind, setKind] = useState<"blocked" | "trusted">("blocked");

  const changed = () => {
    void qc.invalidateQueries({ queryKey: sendersKey });
    void qc.invalidateQueries({ queryKey: keys.message });
  };

  const add = async (e: FormEvent) => {
    e.preventDefault();
    if (!address.trim()) return;
    try {
      await api.setSender(address.trim(), kind);
      setAddress("");
      changed();
    } catch (err) {
      toast.error(err);
    }
  };

  const remove = async (addr: string) => {
    try {
      await api.deleteSender(addr);
      changed();
    } catch (err) {
      toast.error(err);
    }
  };

  const list = (k: "blocked" | "trusted") => {
    const items = (q.data ?? []).filter((s) => s.kind === k);
    if (items.length === 0) return <p className="muted">{k === "blocked" ? "You haven't blocked anyone." : "No trusted senders."}</p>;
    return (
      <table className="table">
        <tbody>
          {items.map((s) => (
            <tr key={s.address}>
              <td>{s.address.startsWith("@") ? `Everyone at ${s.address.slice(1)}` : s.address}</td>
              <td className="muted small">Added {fullDate(s.created_at)}</td>
              <td className="row-actions">
                <button className="btn small" onClick={() => void remove(s.address)}>
                  {k === "blocked" ? "Unblock" : "Remove"}
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    );
  };

  if (q.isPending) return <Spinner />;
  return (
    <div className="settings-section">
      <h2>Blocked senders</h2>
      <p className="muted">Mail from these addresses or domains goes straight to Spam.</p>
      {list("blocked")}
      <h2>Trusted senders</h2>
      <p className="muted">Images in mail from these senders are always shown, and their mail is never marked as spam.</p>
      {list("trusted")}
      <form className="inline-form" onSubmit={add}>
        <input
          value={address}
          onChange={(e) => setAddress(e.target.value)}
          placeholder="someone@example.com or @example.com"
          aria-label="Address or @domain"
        />
        <select value={kind} onChange={(e) => setKind(e.target.value as "blocked" | "trusted")} aria-label="List">
          <option value="blocked">Block</option>
          <option value="trusted">Trust</option>
        </select>
        <button className="btn primary" type="submit" disabled={!address.trim()}>
          Add
        </button>
      </form>
    </div>
  );
}

function AccountTab() {
  const session = useSession();
  const { user } = session;
  const toast = useToast();
  const aliases = useQuery({ queryKey: ["my-aliases"], queryFn: api.myAliases });
  const [name, setName] = useState(user.display_name);
  const [pw, setPw] = useState({ current: "", next: "", confirm: "" });
  const [pwError, setPwError] = useState("");
  const [busy, setBusy] = useState(false);

  const saveName = async (e: FormEvent) => {
    e.preventDefault();
    try {
      session.setUser(await api.updateAccount(name));
      toast.show({ message: "Name saved." });
    } catch (err) {
      toast.error(err);
    }
  };

  const changePassword = async (e: FormEvent) => {
    e.preventDefault();
    setPwError("");
    if (pw.next.length < 8) return setPwError("The new password must be at least 8 characters.");
    if (pw.next !== pw.confirm) return setPwError("The new passwords don't match.");
    setBusy(true);
    try {
      await api.changePassword(pw.current, pw.next);
      setPw({ current: "", next: "", confirm: "" });
      toast.show({ message: "Password changed. Other browsers were signed out. Update the password in your mail apps too." });
    } catch (err) {
      setPwError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  const quota = user.quota_bytes;
  return (
    <div className="settings-section">
      <form className="settings-form" onSubmit={saveName}>
        <Row title="Name" hint="Shown to the people you write to.">
          <div className="inline-form">
            <input value={name} maxLength={100} onChange={(e) => setName(e.target.value)} aria-label="Display name" />
            <button className="btn primary" type="submit" disabled={name.trim() === user.display_name}>
              Save
            </button>
          </div>
        </Row>
      </form>
      <Row title="Email address">
        <div>{user.email}</div>
        {(aliases.data?.length ?? 0) > 0 && (
          <div className="stack small-gap">
            <div className="muted small">Mail to these addresses also arrives in your Inbox, and you can send from them:</div>
            {aliases.data!.map((a) => (
              <div key={a.id}>{a.address}</div>
            ))}
          </div>
        )}
      </Row>
      <Row title="Storage">
        {quota > 0 ? `${formatBytes(user.used_bytes)} of ${formatBytes(quota)} used` : `${formatBytes(user.used_bytes)} used`}
      </Row>
      <form className="settings-form" onSubmit={changePassword}>
        <Row title="Password" hint="The same password is used in mail apps.">
          <div className="stack narrow">
            <label className="field">
              <span>Current password</span>
              <input
                type="password"
                autoComplete="current-password"
                value={pw.current}
                onChange={(e) => setPw({ ...pw, current: e.target.value })}
                required
              />
            </label>
            <label className="field">
              <span>New password</span>
              <input
                type="password"
                autoComplete="new-password"
                value={pw.next}
                minLength={8}
                onChange={(e) => setPw({ ...pw, next: e.target.value })}
                required
              />
            </label>
            <label className="field">
              <span>Confirm new password</span>
              <input
                type="password"
                autoComplete="new-password"
                value={pw.confirm}
                onChange={(e) => setPw({ ...pw, confirm: e.target.value })}
                required
              />
            </label>
            {pwError && (
              <div className="error-text" role="alert">
                {pwError}
              </div>
            )}
            <div>
              <button className="btn primary" type="submit" disabled={busy || !pw.current || !pw.next}>
                Change password
              </button>
            </div>
          </div>
        </Row>
      </form>
    </div>
  );
}

interface ServerInfo {
  kind: "imap" | "smtp";
  hostname: string;
  port: number;
  socket: string;
}

async function fetchClientConfig(address: string): Promise<ServerInfo[]> {
  const res = await fetch(`/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=${encodeURIComponent(address)}`);
  if (!res.ok) throw new Error(`Couldn't load the server settings (${res.status})`);
  const doc = new DOMParser().parseFromString(await res.text(), "application/xml");
  const out: ServerInfo[] = [];
  for (const el of Array.from(doc.querySelectorAll("incomingServer, outgoingServer"))) {
    out.push({
      kind: el.getAttribute("type") === "smtp" ? "smtp" : "imap",
      hostname: el.querySelector("hostname")?.textContent ?? "",
      port: Number(el.querySelector("port")?.textContent ?? 0),
      socket: el.querySelector("socketType")?.textContent ?? "",
    });
  }
  return out;
}

const securityNames: Record<string, string> = { SSL: "SSL/TLS", STARTTLS: "STARTTLS", plain: "None (unencrypted)" };

function AppsTab() {
  const { user } = useSession();
  const q = useQuery({ queryKey: ["client-config", user.email], queryFn: () => fetchClientConfig(user.email), staleTime: Infinity });
  const servers = q.data ?? [];
  const section = (kind: "imap" | "smtp", title: string) => {
    const list = servers.filter((s) => s.kind === kind);
    if (list.length === 0) return null;
    return (
      <>
        <h2>{title}</h2>
        <table className="table">
          <thead>
            <tr>
              <th>Server</th>
              <th className="num">Port</th>
              <th>Security</th>
            </tr>
          </thead>
          <tbody>
            {list.map((s) => (
              <tr key={s.port}>
                <td>
                  <code>{s.hostname}</code> <CopyButton text={s.hostname} />
                </td>
                <td className="num">{s.port}</td>
                <td>{securityNames[s.socket] ?? s.socket}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </>
    );
  };

  if (q.isPending) return <Spinner />;
  if (q.isError) return <div className="error-text">{q.error.message}</div>;
  return (
    <div className="settings-section">
      <p>
        Use TitoMail in Thunderbird, Apple Mail, Outlook, or the mail app on your phone. Many apps set themselves up automatically
        when you enter just your email address and password.
      </p>
      {servers.length === 0 ? (
        <EmptyState title="Mail apps aren't available">
          <p>This server doesn't offer IMAP or SMTP submission. Ask your administrator.</p>
        </EmptyState>
      ) : (
        <>
          <Row title="Username">
            <code>{user.email}</code> <CopyButton text={user.email} />
          </Row>
          <Row title="Password">Your TitoMail password (“normal password”)</Row>
          {section("imap", "Incoming mail (IMAP)")}
          {section("smtp", "Outgoing mail (SMTP)")}
          {servers.some((s) => s.socket === "plain") && (
            <p className="banner warn">
              This server has no TLS certificate, so passwords are sent unencrypted. Only use mail apps on a trusted network.
            </p>
          )}
        </>
      )}
      <p className="muted small">
        Apps that support automatic setup (such as Thunderbird) read{" "}
        <a href={`/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=${encodeURIComponent(user.email)}`}>this configuration file</a>.
      </p>
    </div>
  );
}
