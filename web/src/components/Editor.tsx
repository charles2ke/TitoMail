import { useImperativeHandle, useLayoutEffect, useRef, type ClipboardEvent, type KeyboardEvent, type ReactNode, type Ref } from "react";
import {
  Bold,
  Indent,
  Italic,
  Link,
  List,
  ListOrdered,
  Outdent,
  Redo2,
  RemoveFormatting,
  Strikethrough,
  TextQuote,
  Underline,
  Undo2,
} from "lucide-react";
import { escapeHTML } from "../util";
import { useDialogs } from "./Dialog";

export interface EditorHandle {
  focus: () => void;
  html: () => string;
  append: (html: string) => void;
}

interface Props {
  ref?: Ref<EditorHandle>;
  initialHTML: string;
  onChange: () => void;
  onFiles: (files: File[]) => void;
  onKeyDown?: (e: KeyboardEvent<HTMLDivElement>) => void;
  showToolbar: boolean;
  autoFocus?: boolean;
  placeholder?: string;
  /** Rendered below the text, inside the scrolling area. */
  children?: ReactNode;
}

function normalizeURL(input: string): string | null {
  const t = input.trim();
  if (!t) return null;
  if (/^(https?:|mailto:)/i.test(t)) return t;
  if (/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(t)) return "mailto:" + t;
  if (/^[\w-]+(\.[\w-]+)+([/?#].*)?$/.test(t)) return "https://" + t;
  return null;
}

/** A small rich-text editor built on contentEditable. */
export function Editor({ ref, initialHTML, onChange, onFiles, onKeyDown, showToolbar, autoFocus, placeholder, children }: Props) {
  const el = useRef<HTMLDivElement>(null);
  const dialogs = useDialogs();

  useLayoutEffect(() => {
    const div = el.current!;
    div.innerHTML = initialHTML;
    if (autoFocus) {
      div.focus();
      // Put the caret at the start, above the signature and quote.
      const sel = window.getSelection();
      if (sel) {
        const r = document.createRange();
        r.setStart(div, 0);
        r.collapse(true);
        sel.removeAllRanges();
        sel.addRange(r);
      }
    }
    // Only the initial content is set from props; afterwards the DOM is the source of truth.
  }, []);

  useImperativeHandle(ref, () => ({
    focus: () => el.current?.focus(),
    html: () => el.current?.innerHTML ?? "",
    append: (html: string) => {
      if (!el.current) return;
      el.current.insertAdjacentHTML("beforeend", html);
      onChange();
    },
  }));

  const exec = (cmd: string, value?: string) => {
    el.current?.focus();
    document.execCommand(cmd, false, value);
    onChange();
  };

  const insertLink = async () => {
    const sel = window.getSelection();
    const range = sel && sel.rangeCount && el.current?.contains(sel.anchorNode) ? sel.getRangeAt(0).cloneRange() : null;
    const input = await dialogs.prompt({ title: "Insert link", label: "Web address or email", placeholder: "https://example.com", confirmLabel: "Insert" });
    if (!input) return;
    const url = normalizeURL(input);
    el.current?.focus();
    if (range && sel) {
      sel.removeAllRanges();
      sel.addRange(range);
    }
    if (!url) return;
    if (range && !range.collapsed) exec("createLink", url);
    else exec("insertHTML", `<a href="${escapeHTML(url)}">${escapeHTML(input.trim())}</a>`);
  };

  const onPaste = (e: ClipboardEvent<HTMLDivElement>) => {
    const files = Array.from(e.clipboardData.files);
    if (files.length) {
      e.preventDefault();
      onFiles(files);
    }
  };

  const keyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === "k") {
      e.preventDefault();
      void insertLink();
      return;
    }
    onKeyDown?.(e);
  };

  const tool = (label: string, icon: ReactNode, run: () => void) => (
    <button
      type="button"
      className="icon-btn small"
      title={label}
      aria-label={label}
      onMouseDown={(e) => e.preventDefault()}
      onClick={run}
    >
      {icon}
    </button>
  );

  return (
    <div className="editor-wrap">
      <div className="editor-scroll">
        <div
          ref={el}
          className="editor"
          contentEditable
          role="textbox"
          aria-multiline="true"
          aria-label="Message body"
          data-placeholder={placeholder}
          suppressContentEditableWarning
          onInput={onChange}
          onPaste={onPaste}
          onKeyDown={keyDown}
        />
        {children}
      </div>
      {showToolbar && (
        <div className="format-bar" role="toolbar" aria-label="Formatting">
          {tool("Undo", <Undo2 size={16} />, () => exec("undo"))}
          {tool("Redo", <Redo2 size={16} />, () => exec("redo"))}
          <span className="sep" />
          {tool("Bold (Ctrl+B)", <Bold size={16} />, () => exec("bold"))}
          {tool("Italic (Ctrl+I)", <Italic size={16} />, () => exec("italic"))}
          {tool("Underline (Ctrl+U)", <Underline size={16} />, () => exec("underline"))}
          {tool("Strikethrough", <Strikethrough size={16} />, () => exec("strikeThrough"))}
          <span className="sep" />
          {tool("Numbered list", <ListOrdered size={16} />, () => exec("insertOrderedList"))}
          {tool("Bulleted list", <List size={16} />, () => exec("insertUnorderedList"))}
          {tool("Indent less", <Outdent size={16} />, () => exec("outdent"))}
          {tool("Indent more", <Indent size={16} />, () => exec("indent"))}
          {tool("Quote", <TextQuote size={16} />, () => exec("formatBlock", "blockquote"))}
          <span className="sep" />
          {tool("Insert link (Ctrl+K)", <Link size={16} />, () => void insertLink())}
          {tool("Remove formatting", <RemoveFormatting size={16} />, () => exec("removeFormat"))}
        </div>
      )}
    </div>
  );
}
