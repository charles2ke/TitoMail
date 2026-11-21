import { Modal } from "./Dialog";

const groups: { title: string; keys: [string, string][] }[] = [
  {
    title: "Anywhere",
    keys: [
      ["c", "Compose"],
      ["/", "Search mail"],
      ["?", "Show keyboard shortcuts"],
      ["g then i", "Go to Inbox"],
      ["g then s", "Go to Starred"],
      ["g then t", "Go to Sent"],
      ["g then d", "Go to Drafts"],
      ["g then a", "Go to All mail"],
      ["g then c", "Go to Contacts"],
    ],
  },
  {
    title: "Message list",
    keys: [
      ["j / k", "Older / newer conversation"],
      ["o or Enter", "Open conversation"],
      ["u", "Back to the list"],
      ["x", "Select conversation"],
      ["* then a", "Select all"],
      ["* then n", "Select none"],
      ["* then r / u", "Select read / unread"],
      ["* then s / t", "Select starred / unstarred"],
      ["Esc", "Clear selection"],
    ],
  },
  {
    title: "Actions",
    keys: [
      ["e", "Archive"],
      ["#", "Delete"],
      ["!", "Report spam"],
      ["s", "Star or unstar"],
      ["Shift + i", "Mark as read"],
      ["Shift + u", "Mark as unread"],
    ],
  },
  {
    title: "Conversation",
    keys: [
      ["r", "Reply"],
      ["a", "Reply all"],
      ["f", "Forward"],
      [";", "Expand all messages"],
      [":", "Collapse all messages"],
      ["Shift + n", "Show hidden messages"],
    ],
  },
  {
    title: "Writing",
    keys: [
      ["Ctrl + Enter", "Send"],
      ["Ctrl + k", "Insert link"],
      ["Esc", "Close (the draft is saved)"],
    ],
  },
];

export function ShortcutsHelp({ onClose }: { onClose: () => void }) {
  return (
    <Modal title="Keyboard shortcuts" onClose={onClose} wide>
      <div className="shortcut-groups">
        {groups.map((g) => (
          <section key={g.title}>
            <h3>{g.title}</h3>
            <dl className="shortcuts">
              {g.keys.map(([k, what]) => (
                <div key={k + what}>
                  <dt>
                    <kbd>{k}</kbd>
                  </dt>
                  <dd>{what}</dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </div>
      <p className="muted small">On a Mac, use ⌘ instead of Ctrl.</p>
    </Modal>
  );
}
