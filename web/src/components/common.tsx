import type { CSSProperties, ReactNode } from "react";
import { Copy, X } from "lucide-react";
import type { Address } from "../types";
import { avatarColor, initials } from "../util";
import { useToast } from "./Toasts";

export function Avatar({ address, size = 40 }: { address: Address; size?: number }) {
  return (
    <span
      className="avatar"
      aria-hidden="true"
      style={{ width: size, height: size, fontSize: size * 0.42, background: avatarColor(address.address.toLowerCase()) }}
    >
      {initials(address)}
    </span>
  );
}

export function Spinner({ label = "Loading…" }: { label?: string }) {
  return (
    <div className="spinner-wrap" role="status">
      <span className="spinner" />
      <span className="sr-only">{label}</span>
    </div>
  );
}

/** Text color with enough contrast on a label's background color. */
function chipStyle(color: string): CSSProperties | undefined {
  const m = /^#([0-9a-f]{6})$/i.exec(color);
  if (!m) return undefined;
  const n = parseInt(m[1], 16);
  const lum = (0.299 * ((n >> 16) & 255) + 0.587 * ((n >> 8) & 255) + 0.114 * (n & 255)) / 255;
  return { background: color, color: lum > 0.6 ? "#202124" : "#fff" };
}

export function Chip({ name, color = "", onRemove, title }: { name: string; color?: string; onRemove?: () => void; title?: string }) {
  return (
    <span className="chip" style={chipStyle(color)} title={title ?? name}>
      <span className="chip-name">{name}</span>
      {onRemove && (
        <button
          type="button"
          className="chip-x"
          aria-label={`Remove ${name}`}
          onClick={(e) => {
            e.stopPropagation();
            onRemove();
          }}
        >
          <X size={12} />
        </button>
      )}
    </span>
  );
}

export function EmptyState({ icon, title, children }: { icon?: ReactNode; title: string; children?: ReactNode }) {
  return (
    <div className="empty-state">
      {icon && <div className="empty-icon">{icon}</div>}
      <p className="empty-title">{title}</p>
      {children && <div className="empty-body">{children}</div>}
    </div>
  );
}

export function CopyButton({ text, label }: { text: string; label?: string }) {
  const toast = useToast();
  const failed = () => toast.show({ message: "Couldn't copy. Select the text and copy it instead.", error: true });
  return (
    <button
      className="icon-btn tiny"
      type="button"
      title="Copy"
      aria-label={`Copy ${label ?? text}`}
      onClick={() => {
        // The Clipboard API is missing on plain-HTTP origins other than localhost.
        if (!navigator.clipboard) return failed();
        navigator.clipboard.writeText(text).then(() => toast.show({ message: "Copied to clipboard.", duration: 2000 }), failed);
      }}
    >
      <Copy size={14} />
    </button>
  );
}
