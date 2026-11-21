import { useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Globe, Pencil, Plus, RefreshCw, RotateCw, Send, ShieldCheck, Trash2, Users } from "lucide-react";
import { api } from "../api";
import { Link } from "../router";
import { useSession } from "../session";
import type { DNSRecord, Domain, QueueEntry, User } from "../types";
import { formatBytes, fullDate, plural, relativeTime } from "../util";
import { Avatar, CopyButton, EmptyState, Spinner } from "./common";
import { Modal, useDialogs } from "./Dialog";
import { useToast } from "./Toasts";

const tabs = [
  { id: "overview", label: "Overview" },
  { id: "domains", label: "Domains" },
  { id: "users", label: "Users" },
  { id: "aliases", label: "Aliases" },
  { id: "queue", label: "Mail queue" },
];

const adminKey = ["admin"] as const;
const GB = 1024 ** 3;
const minPassword = 8;

export function AdminPage({ tab }: { tab: string }) {
  const { user } = useSession();
  const current = tabs.some((t) => t.id === tab) ? tab : "overview";
  if (!user.is_admin) {
    return (
      <div className="page">
        <EmptyState icon={<ShieldCheck size={56} strokeWidth={1} />} title="Administrators only">
          <p>Ask an administrator of this server for access.</p>
        </EmptyState>
      </div>
    );
  }
  return (
    <div className="page">
      <div className="page-head">
        <h1>Administration</h1>
      </div>
      <nav className="tabs" aria-label="Administration sections">
        {tabs.map((t) => (
          <Link
            key={t.id}
            to={`/admin/${t.id}`}
            replace
            className={"tab" + (t.id === current ? " active" : "")}
            aria-current={t.id === current ? "page" : undefined}
          >
            {t.label}
          </Link>
        ))}
      </nav>
      <div className="page-body">
        {current === "overview" && <OverviewTab />}
        {current === "domains" && <DomainsTab />}
        {current === "users" && <UsersTab />}
        {current === "aliases" && <AliasesTab />}
        {current === "queue" && <QueueTab />}
      </div>
    </div>
  );
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/** Renders the loading and error states of a query, or its content. */
function Loaded<T>({ q, children }: { q: { isPending: boolean; isError: boolean; error: Error | null; data: T | undefined }; children: (data: T) => ReactNode }) {
  if (q.isPending) return <Spinner />;
  if (q.isError || q.data === undefined) return <div className="error-text">{q.error?.message ?? "Couldn't load this page."}</div>;
  return <>{children(q.data)}</>;
}

// --- Overview ---

const portInfo: { key: string; name: string; use: string }[] = [
  { key: "smtp", name: "SMTP", use: "Mail from other servers (MX)" },
  { key: "submission", name: "Submission", use: "Sending from mail apps (STARTTLS)" },
  { key: "submissions", name: "Submissions", use: "Sending from mail apps (TLS)" },
  { key: "imap", name: "IMAP", use: "Reading in mail apps (STARTTLS)" },
  { key: "imaps", name: "IMAPS", use: "Reading in mail apps (TLS)" },
];

function OverviewTab() {
  const q = useQuery({ queryKey: [...adminKey, "overview"], queryFn: api.admin.overview });
  return (
    <Loaded q={q}>
      {(o) => (
        <>
          {o.domains === 0 && (
            <div className="banner warn">
              This server doesn't host any domains yet. <Link to="/admin/domains">Add a domain</Link> to start receiving mail.
            </div>
          )}
          <div className="stat-grid">
            <Stat label={o.users === 1 ? "User" : "Users"} value={o.users} to="/admin/users" />
            <Stat label={o.domains === 1 ? "Domain" : "Domains"} value={o.domains} to="/admin/domains" />
            <Stat label="Storage used" value={formatBytes(o.storage_bytes)} />
            <Stat
              label="Waiting to send"
              value={o.queue_pending + o.queue_deferred}
              to="/admin/queue"
              note={o.queue_deferred > 0 ? `${plural(o.queue_deferred, "recipient")} retrying` : undefined}
            />
          </div>
          <section className="settings-section">
            <h2>Server</h2>
            <dl className="facts">
              <dt>Host name</dt>
              <dd>
                <code>{o.hostname}</code>
              </dd>
              <dt>Version</dt>
              <dd>{o.version}</dd>
            </dl>
          </section>
          <section className="settings-section">
            <h2>Ports</h2>
            <table className="table">
              <thead>
                <tr>
                  <th>Service</th>
                  <th className="num">Port</th>
                  <th>Used for</th>
                </tr>
              </thead>
              <tbody>
                {portInfo.map((p) => (
                  <tr key={p.key}>
                    <td>{p.name}</td>
                    <td className="num">{o.ports[p.key] ? o.ports[p.key] : <span className="muted">Off</span>}</td>
                    <td className="muted">{p.use}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        </>
      )}
    </Loaded>
  );
}

function Stat({ label, value, to, note }: { label: string; value: ReactNode; to?: string; note?: string }) {
  const body = (
    <>
      <div className="stat-value">{value}</div>
      <div className="stat-label">{label}</div>
      {note && <div className="stat-note warn">{note}</div>}
    </>
  );
  return to ? (
    <Link to={to} className="stat">
      {body}
    </Link>
  ) : (
    <div className="stat">{body}</div>
  );
}

// --- Domains ---

function DomainsTab() {
  const q = useQuery({ queryKey: [...adminKey, "domains"], queryFn: api.admin.domains });
  const qc = useQueryClient();
  const toast = useToast();
  const dialogs = useDialogs();
  const [name, setName] = useState("");
  const [adding, setAdding] = useState(false);
  const [dnsFor, setDnsFor] = useState<Domain | null>(null);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    const n = name.trim().toLowerCase();
    if (!n) return;
    setAdding(true);
    try {
      const d = await api.admin.createDomain(n);
      setName("");
      void qc.invalidateQueries({ queryKey: adminKey });
      setDnsFor(d);
    } catch (err) {
      toast.error(err);
    } finally {
      setAdding(false);
    }
  };

  const remove = async (d: Domain) => {
    const ok = await dialogs.confirm({
      title: `Delete ${d.name}?`,
      message: "Aliases at this domain are deleted too, and mail sent to it will be refused.",
      confirmLabel: "Delete domain",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.admin.deleteDomain(d.id);
      void qc.invalidateQueries({ queryKey: adminKey });
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <>
      <form className="inline-form" onSubmit={add}>
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="example.com"
          aria-label="Domain name"
          autoCapitalize="off"
          spellCheck={false}
        />
        <button className="btn primary" type="submit" disabled={adding || !name.trim()}>
          <Plus size={16} /> Add domain
        </button>
      </form>
      <Loaded q={q}>
        {(domains) =>
          domains.length === 0 ? (
            <EmptyState icon={<Globe size={56} strokeWidth={1} />} title="No domains yet">
              <p>Add the domain you want to receive mail for, then publish the DNS records it needs.</p>
            </EmptyState>
          ) : (
            <table className="table">
              <thead>
                <tr>
                  <th>Domain</th>
                  <th className="num">Users</th>
                  <th className="hide-narrow">DKIM selector</th>
                  <th className="hide-narrow">Added</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {domains.map((d) => (
                  <tr key={d.id}>
                    <td>{d.name}</td>
                    <td className="num">{d.users}</td>
                    <td className="hide-narrow">
                      <code>{d.dkim_selector}</code>
                    </td>
                    <td className="hide-narrow muted">{fullDate(d.created_at)}</td>
                    <td className="row-actions">
                      <button className="btn small" onClick={() => setDnsFor(d)}>
                        DNS records
                      </button>
                      <button
                        className="icon-btn small"
                        title={d.users > 0 ? "Delete or move this domain's users first" : "Delete domain"}
                        aria-label={`Delete ${d.name}`}
                        disabled={d.users > 0}
                        onClick={() => void remove(d)}
                      >
                        <Trash2 size={16} />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )
        }
      </Loaded>
      {dnsFor && <DNSModal domain={dnsFor} onClose={() => setDnsFor(null)} />}
    </>
  );
}

const statusText: Record<DNSRecord["status"], string> = {
  ok: "OK",
  missing: "Missing",
  mismatch: "Different value",
  error: "Lookup failed",
  unchecked: "",
};

function DNSModal({ domain, onClose }: { domain: Domain; onClose: () => void }) {
  const q = useQuery({ queryKey: [...adminKey, "dns", domain.id], queryFn: () => api.admin.dns(domain.id, false) });
  const [checked, setChecked] = useState<DNSRecord[] | null>(null);
  const [checking, setChecking] = useState(false);
  const toast = useToast();

  const check = async () => {
    setChecking(true);
    try {
      setChecked((await api.admin.dns(domain.id, true)).records);
    } catch (err) {
      toast.error(err);
    } finally {
      setChecking(false);
    }
  };

  const records = checked ?? q.data?.records ?? [];
  const problems = checked?.filter((r) => r.required && r.status !== "ok").length ?? 0;

  return (
    <Modal
      title={`DNS records for ${domain.name}`}
      onClose={onClose}
      wide
      footer={
        <>
          <button className="btn" onClick={() => void check()} disabled={checking || q.isPending}>
            <RefreshCw size={16} className={checking ? "spin" : undefined} /> {checking ? "Checking…" : "Check DNS"}
          </button>
          <button className="btn primary" onClick={onClose}>
            Done
          </button>
        </>
      }
    >
      <p className="muted">
        Add these records at your DNS provider. Changes can take a few hours to be visible everywhere. The PTR record is set by
        whoever provides the server's IP address, not in your domain's DNS.
      </p>
      {checked && (
        <div className={"banner" + (problems ? " warn" : " ok")} role="status">
          {problems ? `${plural(problems, "required record")} ${problems === 1 ? "needs" : "need"} attention.` : "All required records look right."}
        </div>
      )}
      {q.isPending ? (
        <Spinner />
      ) : q.isError ? (
        <div className="error-text">{q.error.message}</div>
      ) : (
        <table className="table dns-table">
          <thead>
            <tr>
              <th>Type</th>
              <th>Name</th>
              <th>Value</th>
              {checked && <th>Status</th>}
            </tr>
          </thead>
          <tbody>
            {records.map((r, i) => (
              <tr key={i}>
                <td>
                  <code>{r.type}</code>
                  {!r.required && <div className="muted small">Optional</div>}
                </td>
                <td>
                  <span className="dns-cell">
                    <code className="dns-name">{r.name}</code>
                    {!r.name.startsWith("<") && <CopyButton text={r.name} label={`${r.type} name`} />}
                  </span>
                </td>
                <td>
                  <span className="dns-cell">
                    <code className="dns-value">{r.value}</code>
                    {!r.value.startsWith("<") && <CopyButton text={r.value} label={`${r.type} value`} />}
                  </span>
                  <div className="muted small">{r.purpose}</div>
                </td>
                {checked && (
                  <td>
                    <span className={`badge status-${r.status}`}>{statusText[r.status]}</span>
                    {r.status === "mismatch" && r.found && r.found.length > 0 && (
                      <div className="muted small">
                        Found: <code className="dns-value">{r.found.join(", ")}</code>
                      </div>
                    )}
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Modal>
  );
}

// --- Users ---

function quotaText(u: User): string {
  return u.quota_bytes > 0 ? `${formatBytes(u.used_bytes)} of ${formatBytes(u.quota_bytes)}` : formatBytes(u.used_bytes);
}

function UsersTab() {
  const { user: me } = useSession();
  const q = useQuery({ queryKey: [...adminKey, "users"], queryFn: api.admin.users });
  const qc = useQueryClient();
  const toast = useToast();
  const dialogs = useDialogs();
  const [editing, setEditing] = useState<User | "new" | null>(null);

  const remove = async (u: User) => {
    const ok = await dialogs.confirm({
      title: `Delete ${u.email}?`,
      message: "All of this user's mail, contacts and settings are deleted permanently, along with aliases that deliver to them.",
      confirmLabel: "Delete user",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.admin.deleteUser(u.id);
      void qc.invalidateQueries({ queryKey: adminKey });
      toast.show({ message: `${u.email} was deleted.` });
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <>
      <div className="list-toolbar">
        <button className="btn primary" onClick={() => setEditing("new")}>
          <Plus size={16} /> Create user
        </button>
      </div>
      <Loaded q={q}>
        {(users) =>
          users.length === 0 ? (
            <EmptyState icon={<Users size={56} strokeWidth={1} />} title="No users" />
          ) : (
            <table className="table">
              <thead>
                <tr>
                  <th>User</th>
                  <th className="hide-narrow">Storage</th>
                  <th className="hide-narrow">Created</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.id} className={u.disabled ? "dimmed" : undefined}>
                    <td>
                      <span className="contact-name">
                        <Avatar address={{ name: u.display_name, address: u.email }} size={32} />
                        <span>
                          <span>{u.display_name || u.email}</span>
                          {u.is_admin && <span className="badge">Admin</span>}
                          {u.disabled && <span className="badge status-error">Disabled</span>}
                          {u.id === me.id && <span className="muted small"> (you)</span>}
                          {u.display_name && <div className="muted small">{u.email}</div>}
                        </span>
                      </span>
                    </td>
                    <td className="hide-narrow">
                      <div>{quotaText(u)}</div>
                      {u.quota_bytes > 0 && (
                        <meter className="quota-meter" min={0} max={u.quota_bytes} high={u.quota_bytes * 0.9} value={u.used_bytes} />
                      )}
                    </td>
                    <td className="hide-narrow muted">{fullDate(u.created_at)}</td>
                    <td className="row-actions">
                      <button className="icon-btn small" title="Edit" aria-label={`Edit ${u.email}`} onClick={() => setEditing(u)}>
                        <Pencil size={16} />
                      </button>
                      <button
                        className="icon-btn small"
                        title={u.id === me.id ? "You can't delete your own account" : "Delete"}
                        aria-label={`Delete ${u.email}`}
                        disabled={u.id === me.id}
                        onClick={() => void remove(u)}
                      >
                        <Trash2 size={16} />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )
        }
      </Loaded>
      {editing && (
        <UserEditor
          user={editing === "new" ? null : editing}
          self={editing !== "new" && editing.id === me.id}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            void qc.invalidateQueries({ queryKey: adminKey });
          }}
        />
      )}
    </>
  );
}

function UserEditor({ user, self, onClose, onSaved }: { user: User | null; self: boolean; onClose: () => void; onSaved: () => void }) {
  const { user: me, setUser } = useSession();
  const domains = useQuery({ queryKey: [...adminKey, "domains"], queryFn: api.admin.domains, enabled: !user });
  const [local, setLocal] = useState("");
  const [domain, setDomain] = useState("");
  const [name, setName] = useState(user?.display_name ?? "");
  const [password, setPassword] = useState("");
  const [isAdmin, setIsAdmin] = useState(user?.is_admin ?? false);
  const [disabled, setDisabled] = useState(user?.disabled ?? false);
  const [quota, setQuota] = useState(user && user.quota_bytes > 0 ? String(+(user.quota_bytes / GB).toFixed(2)) : "");
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const toast = useToast();

  const domainList = domains.data ?? [];
  const chosenDomain = domain || domainList[0]?.name || "";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError("");
    const quotaGB = quota.trim() === "" ? 0 : Number(quota);
    if (!Number.isFinite(quotaGB) || quotaGB < 0) {
      setError("Enter the storage limit in GB, or leave it empty for no limit.");
      return;
    }
    if ((!user || password) && password.length < minPassword) {
      setError(`The password must be at least ${minPassword} characters.`);
      return;
    }
    const quotaBytes = Math.round(quotaGB * GB);
    setSaving(true);
    try {
      if (!user) {
        if (!local.trim() || !chosenDomain) {
          setError("Enter the new email address.");
          setSaving(false);
          return;
        }
        const email = `${local.trim()}@${chosenDomain}`;
        await api.admin.createUser({ email, password, display_name: name.trim(), is_admin: isAdmin, quota_bytes: quotaBytes });
        toast.show({ message: `${email} was created. They can sign in now.` });
      } else {
        const updated = await api.admin.updateUser(user.id, {
          display_name: name.trim(),
          quota_bytes: quotaBytes,
          ...(self ? {} : { is_admin: isAdmin, disabled }),
          ...(password ? { password } : {}),
        });
        if (updated.id === me.id) setUser(updated);
        toast.show({ message: password ? `Saved. ${user.email} was signed out of the web app.` : "Saved." });
      }
      onSaved();
    } catch (err) {
      setError(errorMessage(err));
      setSaving(false);
    }
  };

  return (
    <Modal
      title={user ? `Edit ${user.email}` : "Create user"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" type="submit" form="user-form" disabled={saving}>
            {user ? "Save" : "Create"}
          </button>
        </>
      }
    >
      <form id="user-form" className="stack" onSubmit={submit}>
        {!user &&
          (domains.isPending ? (
            <Spinner />
          ) : domainList.length === 0 ? (
            <div className="banner warn">
              Add a domain first. <Link to="/admin/domains">Go to domains</Link>
            </div>
          ) : (
            <div className="field">
              <span id="new-user-email">Email address</span>
              <div className="email-field" role="group" aria-labelledby="new-user-email">
                <input
                  value={local}
                  onChange={(e) => setLocal(e.target.value.replace(/@.*/, ""))}
                  aria-label="User name"
                  autoCapitalize="off"
                  spellCheck={false}
                  autoFocus
                  required
                />
                <span>@</span>
                {domainList.length === 1 ? (
                  <span>{chosenDomain}</span>
                ) : (
                  <select value={chosenDomain} onChange={(e) => setDomain(e.target.value)} aria-label="Domain">
                    {domainList.map((d) => (
                      <option key={d.id} value={d.name}>
                        {d.name}
                      </option>
                    ))}
                  </select>
                )}
              </div>
            </div>
          ))}
        <label className="field">
          <span>Name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} maxLength={200} autoFocus={!!user} />
        </label>
        <label className="field">
          <span>{user ? "New password" : "Password"}</span>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
            minLength={password || !user ? minPassword : undefined}
            maxLength={72}
            placeholder={user ? "Leave empty to keep the current password" : undefined}
            required={!user}
          />
        </label>
        <label className="field">
          <span>Storage limit (GB)</span>
          <input type="number" min={0} step="any" value={quota} onChange={(e) => setQuota(e.target.value)} placeholder="No limit" />
        </label>
        <label className="check">
          <input type="checkbox" checked={isAdmin} disabled={self} onChange={(e) => setIsAdmin(e.target.checked)} />
          <span>Administrator</span>
        </label>
        {user && (
          <label className="check">
            <input type="checkbox" checked={disabled} disabled={self} onChange={(e) => setDisabled(e.target.checked)} />
            <span>Disabled (can't sign in, and mail sent to them is refused)</span>
          </label>
        )}
        {self && <p className="muted small">You can't remove your own administrator access or disable your own account.</p>}
        {error && (
          <div className="error-text" role="alert">
            {error}
          </div>
        )}
      </form>
    </Modal>
  );
}

// --- Aliases ---

function AliasesTab() {
  const aliases = useQuery({ queryKey: [...adminKey, "aliases"], queryFn: api.admin.aliases });
  const users = useQuery({ queryKey: [...adminKey, "users"], queryFn: api.admin.users });
  const qc = useQueryClient();
  const toast = useToast();
  const dialogs = useDialogs();
  const [address, setAddress] = useState("");
  const [userId, setUserId] = useState(0);
  const [adding, setAdding] = useState(false);

  const userList = users.data ?? [];
  const target = userId || userList[0]?.id || 0;

  const add = async (e: FormEvent) => {
    e.preventDefault();
    if (!address.trim() || !target) return;
    setAdding(true);
    try {
      await api.admin.createAlias(address.trim(), target);
      setAddress("");
      void qc.invalidateQueries({ queryKey: [...adminKey, "aliases"] });
    } catch (err) {
      toast.error(err);
    } finally {
      setAdding(false);
    }
  };

  const remove = async (id: number, addr: string) => {
    const ok = await dialogs.confirm({
      title: `Delete ${addr}?`,
      message: "Mail sent to this address will be refused.",
      confirmLabel: "Delete alias",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.admin.deleteAlias(id);
      void qc.invalidateQueries({ queryKey: [...adminKey, "aliases"] });
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <>
      <p className="muted">
        An alias delivers mail to an existing user, who can also send from it. Use <code>@example.com</code> as the address to
        catch mail for every address at that domain that doesn't belong to a user or another alias (catch-all addresses can't
        be sent from).
      </p>
      <form className="inline-form" onSubmit={add}>
        <input
          value={address}
          onChange={(e) => setAddress(e.target.value)}
          placeholder="sales@example.com"
          aria-label="Alias address"
          autoCapitalize="off"
          spellCheck={false}
        />
        <span className="muted">delivers to</span>
        <select value={target} onChange={(e) => setUserId(Number(e.target.value))} aria-label="Delivers to" disabled={!userList.length}>
          {userList.map((u) => (
            <option key={u.id} value={u.id}>
              {u.email}
            </option>
          ))}
        </select>
        <button className="btn primary" type="submit" disabled={adding || !address.trim() || !target}>
          <Plus size={16} /> Add alias
        </button>
      </form>
      <Loaded q={aliases}>
        {(list) =>
          list.length === 0 ? (
            <EmptyState title="No aliases" />
          ) : (
            <table className="table">
              <thead>
                <tr>
                  <th>Alias</th>
                  <th>Delivers to</th>
                  <th className="hide-narrow">Added</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {list.map((a) => (
                  <tr key={a.id}>
                    <td>{a.address.startsWith("@") ? <>Anything {a.address}</> : a.address}</td>
                    <td>{a.user_email}</td>
                    <td className="hide-narrow muted">{fullDate(a.created_at)}</td>
                    <td className="row-actions">
                      <button className="icon-btn small" title="Delete" aria-label={`Delete ${a.address}`} onClick={() => void remove(a.id, a.address)}>
                        <Trash2 size={16} />
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )
        }
      </Loaded>
    </>
  );
}

// --- Queue ---

function rcptStatus(r: NonNullable<QueueEntry["recipients"]>[number]): { text: string; cls: string } {
  if (r.status === "sent") return { text: "Delivered", cls: "status-ok" };
  if (r.status === "failed") return { text: "Failed", cls: "status-error" };
  const when = relativeTime(r.next_attempt_at);
  if (r.attempts > 0) return { text: when ? `Retrying ${when}` : "Retrying", cls: "status-mismatch" };
  return { text: when && new Date(r.next_attempt_at).getTime() > Date.now() + 60_000 ? `Scheduled ${when}` : "Sending", cls: "" };
}

function QueueTab() {
  const q = useQuery({ queryKey: [...adminKey, "queue"], queryFn: api.admin.queue, refetchInterval: 10_000 });
  const qc = useQueryClient();
  const toast = useToast();
  const dialogs = useDialogs();
  const refresh = () => void qc.invalidateQueries({ queryKey: adminKey });

  const retry = async (e: QueueEntry) => {
    try {
      await api.admin.retryQueue(e.id);
      toast.show({ message: "Retrying now." });
      refresh();
    } catch (err) {
      toast.error(err);
    }
  };

  const remove = async (e: QueueEntry) => {
    const ok = await dialogs.confirm({
      title: "Delete this message from the queue?",
      message: "Recipients who haven't received it yet won't get it, and the sender won't be told.",
      confirmLabel: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.admin.deleteQueue(e.id);
      refresh();
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <>
      <div className="list-toolbar">
        <span className="muted">Outgoing messages that haven't been delivered to every recipient yet.</span>
        <button className="btn" onClick={refresh} disabled={q.isFetching}>
          <RefreshCw size={16} className={q.isFetching ? "spin" : undefined} /> Refresh
        </button>
      </div>
      <Loaded q={q}>
        {(entries) =>
          entries.length === 0 ? (
            <EmptyState icon={<Send size={56} strokeWidth={1} />} title="The queue is empty">
              <p>Everything has been delivered.</p>
            </EmptyState>
          ) : (
            <table className="table queue-table">
              <thead>
                <tr>
                  <th>Message</th>
                  <th>Recipients</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {entries.map((e) => {
                  const rcpts = e.recipients ?? [];
                  const pending = rcpts.some((r) => r.status === "pending");
                  return (
                    <tr key={e.id}>
                      <td>
                        <div className="ellipsis">{e.subject || <span className="muted">(no subject)</span>}</div>
                        <div className="muted small">
                          From {e.mail_from || "<>"} · {formatBytes(e.size)} · queued {fullDate(e.created_at)}
                        </div>
                      </td>
                      <td>
                        <ul className="rcpt-list">
                          {rcpts.map((r) => {
                            const s = rcptStatus(r);
                            return (
                              <li key={r.id}>
                                <span>{r.rcpt}</span> <span className={`badge ${s.cls}`}>{s.text}</span>
                                {r.attempts > 0 && <span className="muted small"> {plural(r.attempts, "attempt")}</span>}
                                {r.last_error && <div className="muted small error-detail">{r.last_error}</div>}
                              </li>
                            );
                          })}
                        </ul>
                      </td>
                      <td className="row-actions">
                        {pending && (
                          <button className="icon-btn small" title="Retry now" aria-label="Retry now" onClick={() => void retry(e)}>
                            <RotateCw size={16} />
                          </button>
                        )}
                        <button className="icon-btn small" title="Delete" aria-label="Delete from queue" onClick={() => void remove(e)}>
                          <Trash2 size={16} />
                        </button>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )
        }
      </Loaded>
    </>
  );
}
