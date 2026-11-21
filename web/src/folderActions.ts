import { useQueryClient } from "@tanstack/react-query";
import { api } from "./api";
import { useDialogs } from "./components/Dialog";
import { useToast } from "./components/Toasts";
import { keys } from "./queries";
import type { Label, Mailbox } from "./types";

/** Folder and label management shared by the sidebar and the settings page. */
export function useFolderActions() {
  const dialogs = useDialogs();
  const toast = useToast();
  const qc = useQueryClient();

  const refresh = () => {
    void qc.invalidateQueries({ queryKey: keys.mailboxes });
    void qc.invalidateQueries({ queryKey: keys.labels });
    void qc.invalidateQueries({ queryKey: keys.conversations });
  };

  /** Asks for a name and creates a folder; returns it, or null if cancelled or failed. */
  const createFolder = async (parent?: Mailbox): Promise<Mailbox | null> => {
    const name = await dialogs.prompt({
      title: parent ? `New folder in “${parent.name}”` : "New folder",
      label: "Folder name",
      confirmLabel: "Create",
    });
    if (!name) return null;
    try {
      const mb = await api.createMailbox(parent ? `${parent.name}/${name}` : name);
      refresh();
      return mb;
    } catch (err) {
      toast.error(err);
      return null;
    }
  };

  const renameFolder = async (mb: Mailbox): Promise<boolean> => {
    const name = await dialogs.prompt({ title: "Rename folder", label: "Folder name", initial: mb.name, confirmLabel: "Rename" });
    if (!name || name === mb.name) return false;
    try {
      await api.updateMailbox(mb.id, { name });
      refresh();
      return true;
    } catch (err) {
      toast.error(err);
      return false;
    }
  };

  const deleteFolder = async (mb: Mailbox): Promise<boolean> => {
    const ok = await dialogs.confirm({
      title: `Delete “${mb.name}”?`,
      message: "The folder and its subfolders are removed. Their messages are moved to Trash.",
      confirmLabel: "Delete folder",
      danger: true,
    });
    if (!ok) return false;
    try {
      await api.deleteMailbox(mb.id);
      refresh();
      toast.show({ message: `Folder “${mb.name}” deleted.` });
      return true;
    } catch (err) {
      toast.error(err);
      return false;
    }
  };

  const deleteLabel = async (l: Label): Promise<boolean> => {
    const ok = await dialogs.confirm({
      title: `Delete label “${l.name}”?`,
      message: "The label is removed from all conversations. The messages themselves are kept.",
      confirmLabel: "Delete label",
      danger: true,
    });
    if (!ok) return false;
    try {
      await api.deleteLabel(l.id);
      refresh();
      return true;
    } catch (err) {
      toast.error(err);
      return false;
    }
  };

  const emptyFolder = async (mb: Mailbox, title: string): Promise<boolean> => {
    const ok = await dialogs.confirm({
      title: `Empty ${title}?`,
      message: `All ${mb.total} messages in ${title} will be permanently deleted.`,
      confirmLabel: "Empty",
      danger: true,
    });
    if (!ok) return false;
    try {
      const res = await api.emptyMailbox(mb.id);
      refresh();
      toast.show({ message: `${res.deleted} ${res.deleted === 1 ? "message" : "messages"} deleted forever.` });
      return true;
    } catch (err) {
      toast.error(err);
      return false;
    }
  };

  return { createFolder, renameFolder, deleteFolder, deleteLabel, emptyFolder, refresh };
}
