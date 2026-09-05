import { useEffect, useMemo, useRef, useState } from "react";

export interface SelectOption {
  value: string;
  label: string;
}

/**
 * Custom combobox used by the table filter bars (no native <select>): a
 * button showing the current choice, an optional type-to-filter input and a
 * keyboard-navigable option list. Click outside or Escape closes it.
 */
export function FilterSelect({
  label,
  allLabel,
  value,
  onChange,
  options,
  searchable = true,
  widthClass = "w-56",
}: {
  label: string;
  allLabel: string;
  value: string;
  onChange: (v: string) => void;
  options: SelectOption[];
  searchable?: boolean;
  widthClass?: string;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [hi, setHi] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const searchRef = useRef<HTMLInputElement>(null);

  const selected = options.find((o) => o.value === value);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  useEffect(() => {
    if (open) {
      setQuery("");
      setHi(0);
      if (searchable) window.setTimeout(() => searchRef.current?.focus(), 0);
    }
  }, [open, searchable]);

  const list = useMemo(() => {
    if (!searchable) return options;
    const q = query.trim().toLowerCase();
    if (!q) return options;
    return options.filter((o) => o.label.toLowerCase().includes(q));
  }, [options, query, searchable]);

  const openList = () => {
    setOpen(true);
  };

  const pick = (v: string) => {
    onChange(v);
    setOpen(false);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (!open) {
      if (e.key === "Enter" || e.key === " " || e.key === "ArrowDown") {
        e.preventDefault();
        setOpen(true);
      }
      return;
    }
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setHi((h) => (list.length === 0 ? 0 : (h + 1) % list.length));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setHi((h) => (list.length === 0 ? 0 : (h - 1 + list.length) % list.length));
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (list[hi]) pick(list[hi].value);
    } else if (e.key === "Escape") {
      setOpen(false);
    }
  };

  return (
    <div ref={rootRef} className={`relative ${widthClass}`}>
      {label ? (
        <label className="mb-1 block text-[11px] font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
          {label}
        </label>
      ) : null}
      <button
        type="button"
        onClick={openList}
        aria-haspopup="listbox"
        aria-expanded={open}
        className="flex w-full items-center justify-between gap-2 rounded border border-slate-300 bg-white px-2.5 py-1.5 text-left text-sm text-slate-800 hover:bg-slate-50 focus:border-[#0073bb] focus:outline-none focus:ring-1 focus:ring-[#0073bb] dark:border-slate-600 dark:bg-[#14171c] dark:text-slate-100 dark:hover:bg-slate-800"
      >
        <span className={`truncate ${selected ? "" : "text-slate-400 dark:text-slate-500"}`}>
          {selected ? selected.label : allLabel}
        </span>
        <svg
          className={`size-3.5 shrink-0 text-slate-400 transition-transform ${open ? "rotate-180" : ""}`}
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2.5"
          aria-hidden="true"
        >
          <path d="m6 9 6 6 6-6" strokeLinecap="round" strokeLinejoin="round" />
        </svg>
      </button>

      {open ? (
        <div className="absolute left-0 right-0 top-[calc(100%+4px)] z-30 max-h-72 overflow-hidden rounded border border-slate-300 bg-white shadow-lg dark:border-slate-600 dark:bg-[#1c2128]">
          {searchable ? (
            <div className="border-b border-slate-200 p-1.5 dark:border-slate-700">
              <input
                ref={searchRef}
                type="text"
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value);
                  setHi(0);
                }}
                placeholder="Type to filter…"
                className="w-full rounded border border-slate-200 bg-slate-50 px-2 py-1 text-xs text-slate-800 placeholder:text-slate-400 focus:border-[#0073bb] focus:outline-none dark:border-slate-600 dark:bg-[#14171c] dark:text-slate-100"
              />
            </div>
          ) : null}

          <div role="listbox" aria-label={label || "select"} className="max-h-56 overflow-y-auto py-1" onKeyDown={onKeyDown}>
            {searchable ? (
              <button
                type="button"
                role="option"
                aria-selected={value === ""}
                onMouseEnter={() => setHi(-1)}
                onClick={() => pick("")}
                className={`flex w-full items-center justify-between px-2.5 py-1.5 text-left text-sm ${
                  value === ""
                    ? "bg-[#e3f0f7] font-medium text-[#0a4f74] dark:bg-[#1e3a4a] dark:text-[#8fd0f0]"
                    : "text-slate-700 hover:bg-slate-100 dark:text-slate-200 dark:hover:bg-slate-800"
                }`}
              >
                {allLabel}
                {value === "" ? (
                  <svg className="size-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" aria-hidden="true">
                    <path d="m5 13 4 4L19 7" strokeLinecap="round" strokeLinejoin="round" />
                  </svg>
                ) : null}
              </button>
            ) : null}

            {list.map((o, i) => (
              <button
                key={o.value}
                type="button"
                role="option"
                aria-selected={value === o.value}
                onMouseEnter={() => setHi(i)}
                onClick={() => pick(o.value)}
                className={`flex w-full items-center justify-between px-2.5 py-1.5 text-left font-mono text-xs ${
                  value === o.value
                    ? "bg-[#e3f0f7] font-medium text-[#0a4f74] dark:bg-[#1e3a4a] dark:text-[#8fd0f0]"
                    : i === hi
                      ? "bg-slate-100 text-slate-800 dark:bg-slate-800 dark:text-slate-100"
                      : "text-slate-700 hover:bg-slate-100 dark:text-slate-200 dark:hover:bg-slate-800"
                }`}
              >
                <span className="truncate">{o.label}</span>
                {value === o.value ? (
                  <svg className="size-3.5 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" aria-hidden="true">
                    <path d="m5 13 4 4L19 7" strokeLinecap="round" strokeLinejoin="round" />
                  </svg>
                ) : null}
              </button>
            ))}

            {list.length === 0 ? (
              <div className="px-2.5 py-2 text-xs text-slate-400">No options match.</div>
            ) : null}
          </div>
        </div>
      ) : null}
    </div>
  );
}
