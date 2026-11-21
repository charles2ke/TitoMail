import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import { createPortal } from "react-dom";
import { X } from "lucide-react";

interface ModalProps {
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  className?: string;
}

/** An accessible modal dialog rendered on top of the page. */
export function Modal({ title, onClose, children, footer, wide, className }: ModalProps) {
  const ref = useRef<HTMLDivElement>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null;
    const el = ref.current;
    const first = el?.querySelector<HTMLElement>("[autofocus], input, select, textarea, button.primary");
    (first ?? el)?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        closeRef.current();
      } else if (e.key === "Tab" && el) {
        const items = Array.from(
          el.querySelectorAll<HTMLElement>("a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]"),
        ).filter((n) => n.tabIndex >= 0 && n.offsetParent !== null);
        if (items.length === 0) return;
        const firstItem = items[0];
        const last = items[items.length - 1];
        if (e.shiftKey && document.activeElement === firstItem) {
          e.preventDefault();
          last.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          firstItem.focus();
        }
      }
    };
    el?.addEventListener("keydown", onKey);
    return () => {
      el?.removeEventListener("keydown", onKey);
      prev?.focus?.();
    };
  }, []);
  return createPortal(
    <div className="modal-backdrop" data-modal-open onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        ref={ref}
        className={"modal" + (wide ? " wide" : "") + (className ? " " + className : "")}
        role="dialog"
        aria-modal="true"
        aria-label={typeof title === "string" ? title : undefined}
        tabIndex={-1}
      >
        <div className="modal-head">
          <h2>{title}</h2>
          <button className="icon-btn" aria-label="Close" onClick={onClose}>
            <X size={20} />
          </button>
        </div>
        <div className="modal-body">{children}</div>
        {footer && <div className="modal-foot">{footer}</div>}
      </div>
    </div>,
    document.body,
  );
}

interface ConfirmOptions {
  title: string;
  message?: ReactNode;
  confirmLabel?: string;
  danger?: boolean;
}

interface PromptOptions {
  title: string;
  label: string;
  initial?: string;
  placeholder?: string;
  confirmLabel?: string;
}

interface DialogAPI {
  confirm: (o: ConfirmOptions) => Promise<boolean>;
  prompt: (o: PromptOptions) => Promise<string | null>;
}

const DialogContext = createContext<DialogAPI | null>(null);

export function useDialogs(): DialogAPI {
  const d = useContext(DialogContext);
  if (!d) throw new Error("useDialogs outside of DialogProvider");
  return d;
}

type Pending =
  | { kind: "confirm"; opts: ConfirmOptions; resolve: (v: boolean) => void }
  | { kind: "prompt"; opts: PromptOptions; resolve: (v: string | null) => void };

export function DialogProvider({ children }: { children: ReactNode }) {
  const [pending, setPending] = useState<Pending | null>(null);
  const [value, setValue] = useState("");

  const confirm = useCallback(
    (opts: ConfirmOptions) => new Promise<boolean>((resolve) => setPending({ kind: "confirm", opts, resolve })),
    [],
  );
  const prompt = useCallback(
    (opts: PromptOptions) =>
      new Promise<string | null>((resolve) => {
        setValue(opts.initial ?? "");
        setPending({ kind: "prompt", opts, resolve });
      }),
    [],
  );
  const api = useRef({ confirm, prompt }).current;

  const close = (result: boolean) => {
    if (!pending) return;
    if (pending.kind === "confirm") pending.resolve(result);
    else pending.resolve(result ? value.trim() : null);
    setPending(null);
  };

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (pending?.kind === "prompt" && !value.trim()) return;
    close(true);
  };

  return (
    <DialogContext.Provider value={api}>
      {children}
      {pending && (
        <Modal title={pending.opts.title} onClose={() => close(false)}>
          <form onSubmit={submit} className="dialog-form">
            {pending.kind === "confirm" ? (
              pending.opts.message && <div className="dialog-message">{pending.opts.message}</div>
            ) : (
              <label className="field">
                <span>{pending.opts.label}</span>
                <input
                  autoFocus
                  value={value}
                  placeholder={pending.opts.placeholder}
                  onChange={(e) => setValue(e.target.value)}
                />
              </label>
            )}
            <div className="modal-foot inline">
              <button type="button" className="btn" onClick={() => close(false)}>
                Cancel
              </button>
              <button
                type="submit"
                className={"btn primary" + (pending.kind === "confirm" && pending.opts.danger ? " danger" : "")}
                autoFocus={pending.kind === "confirm"}
                disabled={pending.kind === "prompt" && !value.trim()}
              >
                {pending.opts.confirmLabel ?? "OK"}
              </button>
            </div>
          </form>
        </Modal>
      )}
    </DialogContext.Provider>
  );
}
