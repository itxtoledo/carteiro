import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import { FilterSelect, type SelectOption } from "../components/FilterSelect";
import { pageCls, Card, CardHeader, EmptyState, ErrorBox, Spinner, StatusBadge, inputCls } from "../components/ui";
import { api } from "../lib/api";
import type { SendSummary, SendStatus } from "../lib/types";
import { formatBytes, formatTime, timeAgo } from "../lib/types";
import { usePolling } from "../lib/usePolling";

type StatusFilter = "all" | SendStatus;
type SortKey = "subject" | "from" | "to" | "status" | "attempts" | "queued_at" | "size";
type SortDir = 1 | -1;

const PAGE_SIZES = [10, 25, 50, 100];

function domainOf(addr: string): string {
  const at = addr.lastIndexOf("@");
  return at >= 0 ? addr.slice(at + 1).toLowerCase() : addr.toLowerCase();
}

function sortVal(s: SendSummary, k: SortKey): string | number {
  switch (k) {
    case "subject":
      return (s.subject || "").toLowerCase();
    case "from":
      return s.from.toLowerCase();
    case "to":
      return s.to.join(", ").toLowerCase();
    case "status":
      return s.status;
    case "attempts":
      return s.attempts;
    case "queued_at":
      return new Date(s.queued_at).getTime();
    case "size":
      return s.size;
  }
}

function SortHeader({
  k,
  sortKey,
  sortDir,
  onSort,
  children,
  right = false,
}: {
  k: SortKey;
  sortKey: SortKey;
  sortDir: SortDir;
  onSort: (k: SortKey) => void;
  children: React.ReactNode;
  right?: boolean;
}) {
  const active = sortKey === k;
  return (
    <th
      className={`px-4 py-2 font-semibold ${right ? "text-right" : ""}`}
      aria-sort={active ? (sortDir === 1 ? "ascending" : "descending") : "none"}
    >
      <button
        type="button"
        onClick={() => onSort(k)}
        className={`inline-flex items-center gap-1 uppercase tracking-wide hover:text-slate-800 dark:hover:text-slate-100 ${
          active ? "text-[#0073bb] dark:text-[#44b9d6]" : ""
        } ${right ? "flex-row-reverse" : ""}`}
        title={`Sort by ${String(children).toLowerCase()}`}
      >
        {children}
        <svg
          className={`size-3 transition-opacity ${active ? "opacity-100" : "opacity-25"}`}
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="3"
          aria-hidden="true"
        >
          {active && sortDir === 1 ? (
            <path d="m6 15 6-6 6 6" strokeLinecap="round" strokeLinejoin="round" />
          ) : (
            <path d="m6 9 6 6 6-6" strokeLinecap="round" strokeLinejoin="round" />
          )}
        </svg>
      </button>
    </th>
  );
}

export function SendsPage() {
  const { data, error, loading, reload } = usePolling<SendSummary[]>(
    "sends",
    () => api.get<SendSummary[]>("/api/sends?limit=200"),
    4000,
  );

  const [status, setStatus] = useState<StatusFilter>("all");
  const [query, setQuery] = useState("");
  const [sender, setSender] = useState("");
  const [domain, setDomain] = useState("");
  const [sortKey, setSortKey] = useState<SortKey>("queued_at");
  const [sortDir, setSortDir] = useState<SortDir>(-1);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(25);

  const facets = useMemo(() => {
    const senders = new Set<string>();
    const domains = new Set<string>();
    for (const s of data ?? []) {
      senders.add(s.from);
      const fd = domainOf(s.from);
      if (fd) domains.add(fd);
      for (const t of s.to) {
        const td = domainOf(t);
        if (td) domains.add(td);
      }
    }
    return { senders: [...senders].sort(), domains: [...domains].sort() };
  }, [data]);

  const senderOptions = useMemo<SelectOption[]>(
    () => facets.senders.map((s) => ({ value: s, label: s })),
    [facets.senders],
  );
  const domainOptions = useMemo<SelectOption[]>(
    () => facets.domains.map((d) => ({ value: d, label: d })),
    [facets.domains],
  );

  const counts = useMemo(() => {
    const c: Record<string, number> = { all: 0, queued: 0, delivered: 0, dead: 0 };
    for (const s of data ?? []) {
      c.all += 1;
      c[s.status] = (c[s.status] ?? 0) + 1;
    }
    return c;
  }, [data]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    const out = (data ?? []).filter((s) => {
      if (status !== "all" && s.status !== status) return false;
      if (sender && s.from !== sender) return false;
      if (domain) {
        const hits = domainOf(s.from) === domain || s.to.some((t) => domainOf(t) === domain);
        if (!hits) return false;
      }
      if (q) {
        const hay = `${s.subject} ${s.from} ${s.to.join(" ")} ${s.id}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });
    out.sort((a, b) => {
      const va = sortVal(a, sortKey);
      const vb = sortVal(b, sortKey);
      const cmp = va < vb ? -1 : va > vb ? 1 : a.id.localeCompare(b.id);
      return cmp * sortDir;
    });
    return out;
  }, [data, status, query, sender, domain, sortKey, sortDir]);

  const totalPages = Math.max(1, Math.ceil(filtered.length / pageSize));
  useEffect(() => {
    if (page > totalPages) setPage(totalPages);
  }, [page, totalPages]);

  const start = (page - 1) * pageSize;
  const rows = filtered.slice(start, start + pageSize);
  const hasFilters = status !== "all" || query.trim() !== "" || sender !== "" || domain !== "";

  const clear = () => {
    setStatus("all");
    setQuery("");
    setSender("");
    setDomain("");
    setPage(1);
  };

  const onSort = (k: SortKey) => {
    if (k === sortKey) {
      setSortDir((d) => (d === 1 ? -1 : 1));
    } else {
      setSortKey(k);
      setSortDir(k === "queued_at" || k === "attempts" || k === "size" ? -1 : 1);
    }
    setPage(1);
  };

  const retry = async (id: string) => {
    try {
      await api.post(`/api/queue/${id}/retry`);
      reload();
    } catch (err) {
      alert(err instanceof Error ? err.message : String(err));
    }
  };

  const tab = (f: StatusFilter) =>
    `rounded px-3 py-1.5 text-sm font-medium ${
      status === f
        ? "bg-[#e3f0f7] text-[#0a4f74] dark:bg-[#1e3a4a] dark:text-[#8fd0f0]"
        : "text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800"
    }`;

  const recorded = counts.all;

  return (
    <div className={pageCls}>
      <Card>
        <CardHeader
          title="Sends"
          subtitle="Recent messages recorded by the relay — the ring resets on restart; the database queue is the source of truth."
          right={
            <div className="flex gap-1 rounded border border-slate-200 p-0.5 dark:border-slate-700">
              {(["all", "queued", "delivered", "dead"] as StatusFilter[]).map((f) => (
                <button key={f} type="button" className={tab(f)} onClick={() => setStatus(f)}>
                  {f} {f !== "all" ? `(${counts[f] ?? 0})` : `(${counts.all})`}
                </button>
              ))}
            </div>
          }
        />

        {/* Filter toolbar */}
        <div className="flex flex-wrap items-end gap-3 border-b border-slate-200 bg-slate-50/70 px-4 py-3 dark:border-slate-700 dark:bg-slate-900/30">
          <div className="min-w-56 flex-1">
            <label className="mb-1 block text-[11px] font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400" htmlFor="f-query">
              Search
            </label>
            <div className="relative">
              <svg
                className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-slate-400"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
                aria-hidden="true"
              >
                <circle cx="11" cy="11" r="7" />
                <path d="m21 21-4.3-4.3" strokeLinecap="round" />
              </svg>
              <input
                id="f-query"
                type="search"
                className={`${inputCls} pl-8`}
                placeholder="subject, sender, recipient, message id…"
                value={query}
                onChange={(e) => {
                  setQuery(e.target.value);
                  setPage(1);
                }}
              />
            </div>
          </div>

          <FilterSelect
            label="Sender"
            allLabel="All senders"
            value={sender}
            onChange={(v) => {
              setSender(v);
              setPage(1);
            }}
            options={senderOptions}
            widthClass="min-w-44"
          />

          <FilterSelect
            label="Domain"
            allLabel="Any domain"
            value={domain}
            onChange={(v) => {
              setDomain(v);
              setPage(1);
            }}
            options={domainOptions}
            widthClass="min-w-44"
          />

          <button
            type="button"
            onClick={clear}
            disabled={!hasFilters}
            className="rounded border border-slate-300 bg-white px-3 py-1.5 text-sm font-medium text-slate-600 hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-40 dark:border-slate-600 dark:bg-transparent dark:text-slate-300 dark:hover:bg-slate-800"
          >
            Clear
          </button>
        </div>

        {error ? (
          <div className="p-4">
            <ErrorBox message={error} />
          </div>
        ) : loading && !data ? (
          <Spinner label="Loading sends…" />
        ) : filtered.length === 0 ? (
          <EmptyState>
            {recorded === 0
              ? "No sends recorded yet. Messages accepted over SMTP or composed here appear in this feed."
              : "No sends match the current filters."}
          </EmptyState>
        ) : (
          <>
            <div className="overflow-x-auto">
              <table className="w-full text-left text-sm">
                <thead>
                  <tr className="border-b border-slate-200 text-xs text-slate-500 dark:border-slate-700 dark:text-slate-400">
                    <SortHeader k="subject" sortKey={sortKey} sortDir={sortDir} onSort={onSort}>
                      Subject
                    </SortHeader>
                    <SortHeader k="from" sortKey={sortKey} sortDir={sortDir} onSort={onSort}>
                      From
                    </SortHeader>
                    <SortHeader k="to" sortKey={sortKey} sortDir={sortDir} onSort={onSort}>
                      To
                    </SortHeader>
                    <SortHeader k="status" sortKey={sortKey} sortDir={sortDir} onSort={onSort}>
                      Status
                    </SortHeader>
                    <SortHeader k="attempts" sortKey={sortKey} sortDir={sortDir} onSort={onSort} right>
                      Attempts
                    </SortHeader>
                    <SortHeader k="queued_at" sortKey={sortKey} sortDir={sortDir} onSort={onSort}>
                      Queued
                    </SortHeader>
                    <SortHeader k="size" sortKey={sortKey} sortDir={sortDir} onSort={onSort} right>
                      Size
                    </SortHeader>
                    <th className="px-4 py-2 text-xs font-semibold uppercase tracking-wide text-slate-500 dark:text-slate-400">
                      Last error
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((s) => (
                    <tr
                      key={s.id}
                      className="border-b border-slate-100 last:border-0 hover:bg-slate-50 dark:border-slate-800 dark:hover:bg-slate-800/50"
                    >
                      <td className="max-w-0 px-4 py-2.5">
                        <Link
                          to={`/sends/${s.id}`}
                          className="block truncate font-medium text-slate-800 hover:underline dark:text-slate-100"
                        >
                          {s.subject || "(no subject)"}
                        </Link>
                        <div className="truncate font-mono text-[11px] text-slate-400">{s.id}</div>
                      </td>
                      <td className="whitespace-nowrap px-4 py-2.5 font-mono text-xs text-slate-600 dark:text-slate-300">
                        {s.from}
                      </td>
                      <td className="max-w-0 px-4 py-2.5">
                        <div className="truncate font-mono text-xs text-slate-600 dark:text-slate-300">
                          {s.to.join(", ")}
                        </div>
                      </td>
                      <td className="px-4 py-2.5">
                        <div className="flex items-center gap-2">
                          <StatusBadge status={s.status} />
                          {s.status === "dead" ? (
                            <button
                              type="button"
                              onClick={() => retry(s.id)}
                              title="Move back to the queue and reset attempts"
                              className="text-xs font-medium text-[#0073bb] hover:underline dark:text-[#44b9d6]"
                            >
                              retry
                            </button>
                          ) : null}
                        </div>
                      </td>
                      <td className="px-4 py-2.5 text-right font-mono text-xs tabular-nums text-slate-500 dark:text-slate-400">
                        {s.attempts}
                      </td>
                      <td
                        className="whitespace-nowrap px-4 py-2.5 text-xs text-slate-500 dark:text-slate-400"
                        title={formatTime(s.queued_at)}
                      >
                        {timeAgo(s.queued_at)}
                      </td>
                      <td className="px-4 py-2.5 text-right font-mono text-xs tabular-nums text-slate-500 dark:text-slate-400">
                        {formatBytes(s.size)}
                      </td>
                      <td className="max-w-0 px-4 py-2.5">
                        {s.last_error ? (
                          <div
                            className="truncate font-mono text-[11px] text-red-700 dark:text-red-400"
                            title={s.last_error}
                          >
                            {s.last_error}
                          </div>
                        ) : (
                          <span className="text-xs text-slate-300 dark:text-slate-600">—</span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {/* Pagination footer */}
            <div className="flex flex-wrap items-center justify-between gap-3 border-t border-slate-200 px-4 py-2.5 dark:border-slate-700">
              <span className="text-xs text-slate-500 dark:text-slate-400">
                Showing{" "}
                <span className="font-semibold text-slate-700 dark:text-slate-200">
                  {filtered.length === 0 ? 0 : start + 1}–{Math.min(start + pageSize, filtered.length)}
                </span>{" "}
                of {filtered.length} match{filtered.length === 1 ? "" : "es"}
                {filtered.length !== recorded ? (
                  <>
                    {" "}
                    (from {recorded} recorded{hasFilters ? " with filters active" : ""})
                  </>
                ) : null}
              </span>

              <div className="flex items-center gap-3">
                <div className="flex items-center gap-1.5">
                  <span className="text-xs text-slate-500 dark:text-slate-400">Rows</span>
                  <FilterSelect
                    label=""
                    allLabel={String(pageSize)}
                    value={String(pageSize)}
                    onChange={(v) => {
                      setPageSize(Number(v));
                      setPage(1);
                    }}
                    options={PAGE_SIZES.map((n) => ({ value: String(n), label: String(n) }))}
                    searchable={false}
                    widthClass="w-20"
                  />
                </div>

                <div className="flex items-center gap-1">
                  <button
                    type="button"
                    onClick={() => setPage((p) => Math.max(1, p - 1))}
                    disabled={page <= 1}
                    className="rounded border border-slate-300 bg-white px-2 py-1 text-xs font-medium text-slate-600 hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-40 dark:border-slate-600 dark:bg-transparent dark:text-slate-300 dark:hover:bg-slate-800"
                    aria-label="Previous page"
                  >
                    ← Prev
                  </button>
                  <span className="px-2 text-xs text-slate-600 dark:text-slate-300">
                    Page <span className="font-semibold">{page}</span> of {totalPages}
                  </span>
                  <button
                    type="button"
                    onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                    disabled={page >= totalPages}
                    className="rounded border border-slate-300 bg-white px-2 py-1 text-xs font-medium text-slate-600 hover:bg-slate-100 disabled:cursor-not-allowed disabled:opacity-40 dark:border-slate-600 dark:bg-transparent dark:text-slate-300 dark:hover:bg-slate-800"
                    aria-label="Next page"
                  >
                    Next →
                  </button>
                </div>
              </div>
            </div>
          </>
        )}
      </Card>
    </div>
  );
}
