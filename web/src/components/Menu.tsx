import { useLayoutEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import { Check } from "lucide-react";
import { useDismiss } from "../hooks";

interface TriggerProps {
  onClick: () => void;
  "aria-expanded": boolean;
  "aria-haspopup": "menu" | "dialog";
}

interface PopoverProps {
  trigger: (p: TriggerProps) => ReactNode;
  children: (close: () => void) => ReactNode;
  align?: "left" | "right";
  up?: boolean;
  className?: string;
  role?: "menu" | "dialog";
  onOpen?: () => void;
}

/**
 * Whether content opening below the anchor would be cut off by the viewport or a
 * clipping ancestor while there is more room above it.
 */
function shouldOpenUp(anchor: HTMLElement, pop: HTMLElement): boolean {
  let top = 0;
  let bottom = window.innerHeight;
  for (let el = anchor.parentElement; el && el !== document.body; el = el.parentElement) {
    const s = getComputedStyle(el);
    if (s.overflowY !== "visible" || s.overflowX !== "visible") {
      const r = el.getBoundingClientRect();
      top = Math.max(top, r.top);
      bottom = Math.min(bottom, r.bottom);
    }
  }
  const a = anchor.getBoundingClientRect();
  const below = bottom - a.bottom;
  const above = a.top - top;
  return pop.offsetHeight + 8 > below && above > below;
}

/** A trigger with floating content that closes on outside clicks and Escape. */
export function Popover({ trigger, children, align = "left", up, className, role = "dialog", onOpen }: PopoverProps) {
  const [open, setOpen] = useState(false);
  const [flipUp, setFlipUp] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  const popRef = useRef<HTMLDivElement>(null);
  const close = () => setOpen(false);
  useDismiss(ref, open, close);
  // Measured before paint, so a flipped popover never flashes in the wrong place.
  useLayoutEffect(() => {
    if (open && !up && ref.current && popRef.current) setFlipUp(shouldOpenUp(ref.current, popRef.current));
  }, [open, up]);
  return (
    <div className="popover-anchor" ref={ref}>
      {trigger({
        onClick: () => {
          if (!open) onOpen?.();
          setOpen(!open);
        },
        "aria-expanded": open,
        "aria-haspopup": role,
      })}
      {open && (
        <div
          ref={popRef}
          className={`popover ${align}${up || flipUp ? " up" : ""}${className ? " " + className : ""}`}
          role={role}
        >
          {children(close)}
        </div>
      )}
    </div>
  );
}

export interface MenuItem {
  label: ReactNode;
  icon?: ReactNode;
  onSelect: () => void;
  danger?: boolean;
  checked?: boolean;
  disabled?: boolean;
  hint?: string;
}

export type MenuEntry = MenuItem | "separator" | { heading: string } | null | false;

/** Moves focus between menu items with the arrow keys. */
export function onMenuKey(e: KeyboardEvent<HTMLElement>) {
  if (e.key !== "ArrowDown" && e.key !== "ArrowUp" && e.key !== "Home" && e.key !== "End") return;
  e.preventDefault();
  const items = Array.from(e.currentTarget.querySelectorAll<HTMLButtonElement>("button[role=menuitem]:not([disabled])"));
  if (items.length === 0) return;
  const i = items.indexOf(document.activeElement as HTMLButtonElement);
  let next = 0;
  if (e.key === "ArrowDown") next = i < 0 ? 0 : (i + 1) % items.length;
  else if (e.key === "ArrowUp") next = i <= 0 ? items.length - 1 : i - 1;
  else if (e.key === "End") next = items.length - 1;
  items[next].focus();
}

export function MenuList({ items, close }: { items: MenuEntry[]; close: () => void }) {
  return (
    <div className="menu" onKeyDown={onMenuKey}>
      {items.map((it, i) => {
        if (!it) return null;
        if (it === "separator") return <div key={i} className="menu-sep" role="separator" />;
        if ("heading" in it)
          return (
            <div key={i} className="menu-heading">
              {it.heading}
            </div>
          );
        return (
          <button
            key={i}
            role="menuitem"
            className={"menu-item" + (it.danger ? " danger" : "")}
            disabled={it.disabled}
            onClick={() => {
              close();
              it.onSelect();
            }}
          >
            <span className="menu-icon">{it.checked ? <Check size={16} /> : it.icon}</span>
            <span className="menu-label">{it.label}</span>
            {it.hint && <span className="menu-hint">{it.hint}</span>}
          </button>
        );
      })}
    </div>
  );
}

interface MenuProps {
  items: MenuEntry[];
  trigger: (p: TriggerProps) => ReactNode;
  align?: "left" | "right";
  up?: boolean;
}

export function Menu({ items, trigger, align, up }: MenuProps) {
  return (
    <Popover trigger={trigger} align={align} up={up} role="menu" className="menu-popover">
      {(close) => <MenuList items={items} close={close} />}
    </Popover>
  );
}
