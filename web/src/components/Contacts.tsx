import { useState, type FormEvent } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";
import { BookUser, Mail, Pencil, Plus, Search, Trash2, UserPlus } from "lucide-react";
import { api } from "../api";
import { useDebounced } from "../hooks";
import { navigate } from "../router";
import type { Contact } from "../types";
import { fullDate, isValidEmail } from "../util";
import { Avatar, EmptyState, Spinner } from "./common";
import { useComposer } from "./Composer";
import { Modal, useDialogs } from "./Dialog";
import { useToast } from "./Toasts";

const contactsKey = ["contacts"] as const;

export function ContactsPage() {
  const [tab, setTab] = useState<"saved" | "other">("saved");
  const [text, setText] = useState("");
  const q = useDebounced(text.trim(), 250);
  const list = useQuery({
    queryKey: [...contactsKey, q],
    queryFn: () => api.contacts(q),
    placeholderData: keepPreviousData,
  });
  const qc = useQueryClient();
  const composer = useComposer();
  const dialogs = useDialogs();
  const toast = useToast();
  const [editing, setEditing] = useState<Partial<Contact> | null>(null);

  const all = list.data ?? [];
  const saved = all.filter((c) => c.saved);
  const other = all.filter((c) => !c.saved);
  const shown = tab === "saved" ? saved : other;

  const refresh = () => void qc.invalidateQueries({ queryKey: contactsKey });

  const addToContacts = async (c: Contact) => {
    try {
      await api.saveContact({ email: c.email, name: c.name, phone: c.phone, notes: c.notes });
      refresh();
      toast.show({ message: `${c.name || c.email} added to contacts.` });
    } catch (err) {
      toast.error(err);
    }
  };

  const remove = async (c: Contact) => {
    const ok = await dialogs.confirm({
      title: `Delete ${c.name || c.email}?`,
      message: c.saved
        ? "The contact is removed from your address book. Their mail isn't affected."
        : "The address is removed from your suggestions until you write to it again.",
      confirmLabel: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteContact(c.id);
      refresh();
    } catch (err) {
      toast.error(err);
    }
  };

  return (
    <div className="page">
      <div className="page-head">
        <h1>Contacts</h1>
        <form className="page-search" role="search" onSubmit={(e) => e.preventDefault()}>
          <Search size={18} />
          <input value={text} onChange={(e) => setText(e.target.value)} placeholder="Search contacts" aria-label="Search contacts" />
        </form>
        <button className="btn primary" onClick={() => setEditing({})}>
          <Plus size={16} /> Create contact
        </button>
      </div>
      <nav className="tabs" aria-label="Contact lists">
        <button className={"tab" + (tab === "saved" ? " active" : "")} aria-pressed={tab === "saved"} onClick={() => setTab("saved")}>
          Contacts ({saved.length})
        </button>
        <button className={"tab" + (tab === "other" ? " active" : "")} aria-pressed={tab === "other"} onClick={() => setTab("other")}>
          Other contacts ({other.length})
        </button>
      </nav>
      <div className="page-body">
        {tab === "other" && (
          <p className="muted">People you've written to are remembered here so they're suggested when you write a message.</p>
        )}
        {list.isPending ? (
          <Spinner />
        ) : list.isError ? (
          <div className="error-text">{list.error.message}</div>
        ) : shown.length === 0 ? (
          <EmptyState icon={<BookUser size={56} strokeWidth={1} />} title={q ? "No matching contacts" : "No contacts yet"}>
            {!q && tab === "saved" && <p>Create a contact, or add one from “Other contacts”.</p>}
          </EmptyState>
        ) : (
          <table className="table contacts-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Email</th>
                <th className="hide-narrow">Phone</th>
                <th className="hide-narrow">{tab === "other" ? "Last written to" : "Notes"}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {shown.map((c) => (
                <tr key={c.id}>
                  <td>
                    <span className="contact-name">
                      <Avatar address={{ name: c.name, address: c.email }} size={32} />
                      {c.name || <span className="muted">(no name)</span>}
                    </span>
                  </td>
                  <td>{c.email}</td>
                  <td className="hide-narrow">{c.phone}</td>
                  <td className="hide-narrow muted ellipsis">{tab === "other" ? fullDate(c.last_used_at) : c.notes}</td>
                  <td className="row-actions">
                    <button
                      className="icon-btn small"
                      title="Send email"
                      aria-label={`Email ${c.email}`}
                      onClick={() => composer.compose({ to: [{ name: c.name, address: c.email }] })}
                    >
                      <Mail size={16} />
                    </button>
                    <button
                      className="icon-btn small"
                      title="Find emails"
                      aria-label={`Find emails with ${c.email}`}
                      onClick={() => navigate(`/mail/search?q=${encodeURIComponent(c.email)}`)}
                    >
                      <Search size={16} />
                    </button>
                    {c.saved ? (
                      <button className="icon-btn small" title="Edit" aria-label={`Edit ${c.email}`} onClick={() => setEditing(c)}>
                        <Pencil size={16} />
                      </button>
                    ) : (
                      <button
                        className="icon-btn small"
                        title="Add to contacts"
                        aria-label={`Add ${c.email} to contacts`}
                        onClick={() => void addToContacts(c)}
                      >
                        <UserPlus size={16} />
                      </button>
                    )}
                    <button className="icon-btn small" title="Delete" aria-label={`Delete ${c.email}`} onClick={() => void remove(c)}>
                      <Trash2 size={16} />
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {editing && (
        <ContactEditor
          contact={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null);
            setTab("saved");
            refresh();
          }}
        />
      )}
    </div>
  );
}

function ContactEditor({ contact, onClose, onSaved }: { contact: Partial<Contact>; onClose: () => void; onSaved: () => void }) {
  const [c, setC] = useState({
    name: contact.name ?? "",
    email: contact.email ?? "",
    phone: contact.phone ?? "",
    notes: contact.notes ?? "",
  });
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const toast = useToast();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!isValidEmail(c.email)) {
      setError("Enter a valid email address.");
      return;
    }
    setSaving(true);
    try {
      await api.saveContact({ ...c, id: contact.id });
      toast.show({ message: contact.id ? "Contact saved." : "Contact created." });
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setSaving(false);
    }
  };

  return (
    <Modal
      title={contact.id ? "Edit contact" : "Create contact"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" type="button" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" type="submit" form="contact-form" disabled={saving}>
            Save
          </button>
        </>
      }
    >
      <form id="contact-form" className="stack" onSubmit={submit}>
        <label className="field">
          <span>Name</span>
          <input value={c.name} maxLength={200} onChange={(e) => setC({ ...c, name: e.target.value })} autoFocus />
        </label>
        <label className="field">
          <span>Email</span>
          <input type="email" value={c.email} onChange={(e) => setC({ ...c, email: e.target.value })} required />
        </label>
        <label className="field">
          <span>Phone</span>
          <input type="tel" value={c.phone} maxLength={50} onChange={(e) => setC({ ...c, phone: e.target.value })} />
        </label>
        <label className="field">
          <span>Notes</span>
          <textarea rows={3} value={c.notes} maxLength={5000} onChange={(e) => setC({ ...c, notes: e.target.value })} />
        </label>
        {error && (
          <div className="error-text" role="alert">
            {error}
          </div>
        )}
      </form>
    </Modal>
  );
}
