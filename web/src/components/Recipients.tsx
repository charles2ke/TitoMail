import { useEffect, useRef, useState, type ClipboardEvent, type KeyboardEvent, type ReactNode } from "react";
import { X } from "lucide-react";
import { api } from "../api";
import { useDebounced } from "../hooks";
import type { Address } from "../types";
import { avatarColor, formatAddress, initials, isValidEmail, parseAddresses } from "../util";

interface Props {
  label: string;
  value: Address[];
  onChange: (v: Address[]) => void;
  autoFocus?: boolean;
  extra?: ReactNode;
}

function merge(list: Address[], add: Address[]): Address[] {
  const seen = new Set(list.map((a) => a.address.toLowerCase()));
  const out = [...list];
  for (const a of add) {
    const k = a.address.trim().toLowerCase();
    if (!k || seen.has(k)) continue;
    seen.add(k);
    out.push({ name: a.name.trim(), address: a.address.trim() });
  }
  return out;
}

/** An address field with chips and autocomplete, like Gmail's To/Cc/Bcc fields. */
export function RecipientField({ label, value, onChange, autoFocus, extra }: Props) {
  const [text, setText] = useState("");
  const [suggestions, setSuggestions] = useState<Address[]>([]);
  const [active, setActive] = useState(0);
  const [open, setOpen] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const query = useDebounced(text.trim(), 150);

  useEffect(() => {
    if (!query || query.includes(",")) {
      setSuggestions([]);
      return;
    }
    const ctl = new AbortController();
    api
      .autocomplete(query, ctl.signal)
      .then((list) => {
        const taken = new Set(value.map((a) => a.address.toLowerCase()));
        setSuggestions(list.filter((a) => !taken.has(a.address.toLowerCase())));
        setActive(0);
      })
      .catch(() => {});
    return () => ctl.abort();
  }, [query, value]);

  const commit = (raw = text) => {
    const parsed = parseAddresses(raw);
    if (parsed.length) onChange(merge(value, parsed));
    setText("");
    setSuggestions([]);
  };

  const pick = (a: Address) => {
    onChange(merge(value, [a]));
    setText("");
    setSuggestions([]);
    inputRef.current?.focus();
  };

  const showList = open && suggestions.length > 0 && text.trim().length > 0;

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (showList && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
      e.preventDefault();
      setActive((i) => (i + (e.key === "ArrowDown" ? 1 : suggestions.length - 1)) % suggestions.length);
      return;
    }
    if (e.key === "Escape" && showList) {
      e.stopPropagation();
      setOpen(false);
      return;
    }
    if ((e.key === "Enter" || e.key === "Tab") && text.trim()) {
      if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
        commit();
        return;
      }
      e.preventDefault();
      if (showList) pick(suggestions[active]);
      else commit();
      return;
    }
    if (e.key === "," || e.key === ";") {
      if (text.trim() && !/"[^"]*$/.test(text)) {
        e.preventDefault();
        commit();
      }
      return;
    }
    if (e.key === "Backspace" && !text && value.length) {
      e.preventDefault();
      const last = value[value.length - 1];
      onChange(value.slice(0, -1));
      setText(formatAddress(last));
    }
  };

  const onPaste = (e: ClipboardEvent<HTMLInputElement>) => {
    const t = e.clipboardData.getData("text");
    if (/[,;\n]/.test(t)) {
      e.preventDefault();
      commit(text + t);
    }
  };

  return (
    <div className="rcpt-field" onClick={() => inputRef.current?.focus()}>
      <span className="rcpt-label">{label}</span>
      <div className="rcpt-chips">
        {value.map((a) => {
          const valid = isValidEmail(a.address);
          return (
            <span
              key={a.address.toLowerCase()}
              className={"chip" + (valid ? "" : " invalid")}
              title={valid ? a.address : `“${a.address}” is not a valid email address`}
            >
              <span className="chip-text">{a.name || a.address}</span>
              <button
                type="button"
                className="chip-x"
                aria-label={`Remove ${a.address}`}
                onClick={(e) => {
                  e.stopPropagation();
                  onChange(value.filter((x) => x !== a));
                }}
              >
                <X size={14} />
              </button>
            </span>
          );
        })}
        <div className="rcpt-input-wrap">
          <input
            ref={inputRef}
            aria-label={label}
            value={text}
            autoFocus={autoFocus}
            autoComplete="off"
            spellCheck={false}
            onChange={(e) => {
              setText(e.target.value);
              setOpen(true);
            }}
            onKeyDown={onKeyDown}
            onPaste={onPaste}
            onFocus={() => setOpen(true)}
            onBlur={() => {
              setOpen(false);
              if (text.trim()) commit();
            }}
          />
          {showList && (
            <ul className="suggestions" role="listbox">
              {suggestions.map((s, i) => (
                <li
                  key={s.address}
                  role="option"
                  aria-selected={i === active}
                  className={i === active ? "active" : ""}
                  onMouseDown={(e) => {
                    e.preventDefault();
                    pick(s);
                  }}
                  onMouseEnter={() => setActive(i)}
                >
                  <span className="avatar small" style={{ background: avatarColor(s.address) }}>
                    {initials(s)}
                  </span>
                  <span className="sugg-text">
                    <span className="sugg-name">{s.name || s.address}</span>
                    {s.name && <span className="sugg-addr">{s.address}</span>}
                  </span>
                </li>
              ))}
            </ul>
          )}
        </div>
      </div>
      {extra}
    </div>
  );
}
