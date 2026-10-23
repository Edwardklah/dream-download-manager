// The window's interface, written with Preact (React's API, much smaller).
//
// How it talks to Go (gui/downloads.go):
//   - DownloadService.* calls the Go method of the same name. Wails generates
//     these TypeScript functions into ../bindings when the app is built.
//   - Go sends the whole download list as a "downloads" event, up to 4 times
//     a second while anything changes. We simply replace our copy with it.

import { render } from "preact";
import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { Events } from "@wailsio/runtime";
import { DownloadService } from "../bindings/dlmanager/gui";
import type {
  ServerLimit,
  ToolsStatus,
  ToolsUpdate,
  VideoChoice,
  VideoLookup,
  VideoSubtitle,
  WhenDoneState,
} from "../bindings/dlmanager/gui";
import type { Item, QueueState, Settings } from "../bindings/dlmanager/manager";
import "./style.css";

type Filter = "all" | "active" | "waiting" | "finished" | "stopped";

// How much the video tools are (video/tools.go): yt-dlp's Linux program
// carries its own Python, so it's twice the Windows one.
const onWindowsPC = navigator.userAgent.includes("Windows");
const TOOLS_MB = onWindowsPC ? 19 : 41; // first download: yt-dlp + QuickJS
const UPDATE_MB = onWindowsPC ? 17 : 39; // an update: yt-dlp alone

// ---- Themes ----

// The window's colour themes (style.css). Black is the default. The choice
// is saved with the settings; a copy in the window's own storage lets the
// right colours show from the first moment, before the settings arrive.
const THEMES: [id: string, name: string, swatch: string[]][] = [
  ["black", "Black", ["#0a0a0a", "#f5f5f5", "#38e8c6"]],
  ["dream", "Dream", ["#141130", "#8b6cff", "#2ee6d6"]],
  ["light", "Light", ["#f4f2ff", "#6a4cf0", "#0a8076"]],
  ["ember", "Ember", ["#1b1210", "#ff7a3d", "#ffc857"]],
  ["forest", "Forest", ["#0f1c16", "#34d399", "#a3e635"]],
  ["neon", "Neon", ["#150534", "#ff2bd6", "#00f0ff"]],
  ["ocean", "Ocean", ["#0b1a2e", "#38bdf8", "#2dd4bf"]],
  ["sand", "Sand", ["#fffaf0", "#1f5f8b", "#0f766e"]],
  ["terminal", "Terminal", ["#0a100a", "#39ff7a", "#e0ff4f"]],
];

function applyTheme(id: string) {
  if (!THEMES.some(([t]) => t === id)) id = "black";
  document.documentElement.dataset.theme = id;
  try {
    localStorage.setItem("dlm.theme", id);
  } catch {}
}

try {
  applyTheme(localStorage.getItem("dlm.theme") || "black");
} catch {
  applyTheme("black");
}

const isActive = (d: Item) => ["starting", "downloading", "retrying"].includes(d.status as string);
const isWaiting = (d: Item) => (d.status as string) === "queued";
const isFinished = (d: Item) => (d.status as string) === "completed";
const isStopped = (d: Item) => ["paused", "failed"].includes(d.status as string);

const rank = (d: Item) => (isActive(d) ? 0 : isWaiting(d) ? 1 : isStopped(d) ? 2 : 3);
const finishedAt = (d: Item) => new Date(d.finishedAt as unknown as string).getTime() || 0;

function App() {
  const [items, setItems] = useState<Item[]>([]);
  const [filter, setFilter] = useState<Filter>("all");
  const [kind, setKind] = useState(""); // "" = every kind; "Video", "Music"... or "Other"
  const [showSettings, setShowSettings] = useState(false);
  const [showHelp, setShowHelp] = useState(false);
  const [videoUrl, setVideoUrl] = useState("");
  const [videoSource, setVideoSource] = useState(""); // fallback: the page's video file
  const [toast, setToast] = useState("");
  const [queue, setQueue] = useState<QueueState | null>(null); // the scheduler
  const [whenDone, setWhenDone] = useState<WhenDoneState | null>(null);
  const [update, setUpdate] = useState<ToolsUpdate | null>(null); // a newer yt-dlp
  const [copied, setCopied] = useState(""); // a download link you copied
  const [showWelcome, setShowWelcome] = useState(false); // the first-run welcome

  useEffect(() => {
    DownloadService.GetSettings().then((st) => {
      if (st?.theme) applyTheme(st.theme);
      if (st && !st.welcomed) setShowWelcome(true);
    });
    DownloadService.List().then((list) => setItems(list ?? []));
    return Events.On("downloads", (ev) => setItems((ev.data as Item[]) ?? []));
  }, []);

  useEffect(() => {
    DownloadService.QueueState().then(setQueue);
    DownloadService.WhenDone().then(setWhenDone);
    DownloadService.PendingToolsUpdate().then(setUpdate);
    const takeCopied = () => DownloadService.TakeClipboardLink().then((l) => l && setCopied(l));
    takeCopied();
    const offs = [
      Events.On("queue-state", (ev) => setQueue(ev.data as QueueState)),
      Events.On("when-done", (ev) => setWhenDone(ev.data as WhenDoneState)),
      Events.On("tools-update", (ev) => setUpdate(ev.data as ToolsUpdate)),
      Events.On("clipboard-link", takeCopied),
    ];
    return () => offs.forEach((off) => off());
  }, []);

  // A video page sent by the browser extension: open the quality picker.
  // (Asked on start too, for one that arrived while the window was closed.)
  useEffect(() => {
    const take = () =>
      DownloadService.TakeVideoRequest().then((r) => {
        if (r?.url) {
          setVideoSource(r.source ?? "");
          setVideoUrl(r.url);
        }
      });
    take();
    return Events.On("video-request", take);
  }, []);

  // A link from the add bar or a drop: a video page opens the quality
  // picker, anything else is downloaded as it is.
  function addLink(link: string, startNow: boolean): Promise<unknown> {
    return DownloadService.IsVideoPage(link).then((isVideo) =>
      isVideo ? (setVideoSource(""), setVideoUrl(link)) : DownloadService.Add(link, startNow),
    );
  }

  // Show an error from Go for a few seconds.
  const toastTimer = useRef<number>();
  function report(err: any) {
    setToast(err?.message ?? String(err));
    clearTimeout(toastTimer.current);
    toastTimer.current = window.setTimeout(() => setToast(""), 5000);
  }
  const act = (p: Promise<unknown>) => p.catch(report);

  const failed = items.filter((d) => (d.status as string) === "failed").length;
  const counts = useMemo(
    () => ({
      all: items.length,
      active: items.filter(isActive).length,
      waiting: items.filter(isWaiting).length,
      finished: items.filter(isFinished).length,
      stopped: items.filter(isStopped).length,
    }),
    [items],
  );
  const kindOf = (d: Item) => d.category || "Other";
  const kindCounts = new Map<string, number>();
  items.forEach((d) => kindCounts.set(kindOf(d), (kindCounts.get(kindOf(d)) ?? 0) + 1));
  const shown = items
    .filter((d) => !kind || kindOf(d) === kind)
    .filter((d) =>
      filter === "all" ? true
      : filter === "active" ? isActive(d)
      : filter === "waiting" ? isWaiting(d)
      : filter === "finished" ? isFinished(d)
      : isStopped(d),
    )
    // What's moving first, then the queue in line order, then stopped, then
    // finished (most recent first). Items arrive in queue order.
    .map((d, i) => ({ d, i }))
    .sort((a, b) => rank(a.d) - rank(b.d) || (isFinished(a.d) ? finishedAt(b.d) - finishedAt(a.d) : a.i - b.i))
    .map(({ d }) => d);
  const totalSpeed = items.reduce((sum, d) => sum + (isActive(d) ? d.speed : 0), 0);
  const queuePos = new Map<number, number>();
  items.filter(isWaiting).forEach((d, i) => queuePos.set(d.id, i + 1));

  // Ctrl+V anywhere in the window (not while typing in a box): add the link.
  useEffect(() => {
    const onPaste = (e: ClipboardEvent) => {
      const t = e.target as HTMLElement;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
      const text = (e.clipboardData?.getData("text") ?? "").trim();
      if (/^https?:\/\/\S+$/.test(text)) {
        e.preventDefault();
        act(addLink(text, false));
      }
    };
    window.addEventListener("paste", onPaste);
    return () => window.removeEventListener("paste", onPaste);
  }, []);

  const addInput = useRef<HTMLInputElement>(null);
  const title = kind || STATUS_NAV.find(([k]) => k === filter)![1];
  const shownActive = shown.filter(isActive).length;

  return (
    <div class="app" onDragOver={(e) => e.preventDefault()} onDrop={(e) => dropLink(e, (l) => act(addLink(l, false)))}>
      <aside class="side">
        <div class="brand">
          <i class="logo" aria-hidden="true" />
          <span>
            Dream <b>DM</b>
          </span>
        </div>
        <button class="add primary" onClick={() => addInput.current?.focus()} title="Add download">
          <Icon name="plus" /> Add download
        </button>

        <nav class="nav" aria-label="Show">
          {STATUS_NAV.map(([key, label, icon]) => {
            // Badges for the compact sidebar (style.css), where the counts
            // are hidden: what's downloading and waiting, and failures.
            const [n, tone] =
              key === "active" ? [counts.active, "on"]
              : key === "waiting" ? [counts.waiting, ""]
              : key === "stopped" ? [failed, "bad"]
              : [0, ""];
            return (
            <button
              key={key}
              title={n > 0 ? `${label} (${key === "stopped" ? `${n} failed` : n})` : label}
              class={!kind && filter === key ? "on" : ""}
              aria-current={!kind && filter === key}
              onClick={() => {
                setFilter(key);
                setKind("");
              }}
            >
              <Icon name={icon} />
              <span>{label}</span>
              {counts[key] > 0 && <span class="count">{counts[key]}</span>}
              {n > 0 && <b class={`badge ${tone}`}>{n > 99 ? "99+" : n}</b>}
            </button>
            );
          })}
          {kindCounts.size > 0 && <h4>Categories</h4>}
          {KINDS.filter((k) => (kindCounts.get(k) ?? 0) > 0 || kind === k).map((k) => (
            <button
              key={k}
              title={k}
              data-kind={k}
              class={kind === k ? "on" : ""}
              aria-current={kind === k}
              onClick={() => {
                setKind(k);
                setFilter("all");
              }}
            >
              <Icon name={KIND_ICON[k]} cls="kind-icon" />
              <span>{k}</span>
              <span class="count">{kindCounts.get(k) ?? 0}</span>
            </button>
          ))}
        </nav>

        <div class="side-end">
          <button onClick={() => setShowHelp(true)} title="Help">
            <Icon name="help" /> <span>Help</span>
          </button>
          <button onClick={() => setShowSettings(true)} title="Settings">
            <Icon name="gear" /> <span>Settings</span>
          </button>
        </div>
      </aside>

      <div class="main">
        <header class="top">
          <AddBar act={act} addLink={addLink} input={addInput} />
          <IconButton icon="pause" label="Pause all" onClick={() => act(DownloadService.PauseAll())} />
          <IconButton icon="play" label="Resume all" onClick={() => act(DownloadService.ResumeAll())} />
          <button class="clear secondary" onClick={() => act(DownloadService.ClearCompleted())} title="Remove finished downloads from the list (the files stay)">
            <Icon name="sweep" /> Clear finished
          </button>
        </header>

        <div class="heading">
          <h1>{title}</h1>
          <span>
            {shown.length} {shown.length === 1 ? "item" : "items"}
            {shownActive > 0 && `, ${shownActive} active`}
          </span>
        </div>

        <div class="notices">
          {update?.available && <UpdateNotice update={update} dismiss={() => act(DownloadService.DismissToolsUpdate())} />}
          {copied && (
            <CopiedLink
              link={copied}
              close={() => setCopied("")}
              add={(startNow) => act(addLink(copied, startNow).then(() => setCopied("")))}
            />
          )}
        </div>

        <main class="list">
          {shown.length === 0 ? (
            kind && items.length > 0 ? <p class="empty">No {kind.toLowerCase()} here.</p> : <Empty filter={filter} />
          ) : (
            shown.map((d) => <Row key={d.id} d={d} act={act} queuePos={queuePos.get(d.id)} queue={queue} />)
          )}
        </main>
      </div>

      <footer class="statusbar">
        <span class="speed">{counts.active > 0 ? `${size(totalSpeed)}/s` : "Idle"}</span>
        <span>
          {counts.active} downloading
          {counts.waiting > 0 && <>, {counts.waiting} waiting</>}
        </span>
        {queue?.held && <span class="sched">Scheduled: starts at {queue.startsAt}</span>}
        {queue && !queue.held && queue.stopsAt && <span class="sched">Scheduled: stops at {queue.stopsAt}</span>}
        <label class="whendone">
          When done
          <select
            value={whenDone?.action ?? ""}
            onChange={(e) => act(DownloadService.SetWhenDone((e.target as HTMLSelectElement).value).then(setWhenDone))}
          >
            <option value="">do nothing</option>
            <option value="sleep">sleep</option>
            <option value="shutdown">shut down</option>
          </select>
        </label>
      </footer>
      {whenDone && whenDone.secondsLeft > 0 && (
        <Countdown state={whenDone} cancel={() => act(DownloadService.CancelWhenDone())} />
      )}

      {showSettings && <SettingsPanel close={() => setShowSettings(false)} act={act} />}
      {showHelp && (
        <HelpPanel
          close={() => setShowHelp(false)}
          welcome={() => {
            setShowHelp(false);
            setShowWelcome(true);
          }}
        />
      )}
      {showWelcome && <Welcome close={() => setShowWelcome(false)} act={act} />}
      {videoUrl && (
        <VideoPicker
          key={videoUrl}
          url={videoUrl}
          source={videoSource}
          speed={totalSpeed}
          close={() => setVideoUrl("")}
          act={act}
          openSettings={() => {
            setVideoUrl("");
            setShowSettings(true);
          }}
        />
      )}
      <div class={`toast ${toast ? "show" : ""}`} role="status" aria-live="polite">
        {toast}
      </div>
    </div>
  );
}

// The sidebar's list: filter, name, icon.
const STATUS_NAV: [Filter, string, string][] = [
  ["all", "All downloads", "list"],
  ["active", "Downloading", "download"],
  ["waiting", "Waiting", "clock"],
  ["finished", "Finished", "check"],
  ["stopped", "Stopped", "square"],
];

// The kinds of file, as the app sorts them into folders (manager/categories.go).
const KINDS = ["Video", "Music", "Compressed", "Programs", "Documents", "Pictures", "Other"];
const KIND_ICON: Record<string, string> = {
  Video: "video",
  Music: "music",
  Compressed: "archive",
  Programs: "window",
  Documents: "document",
  Pictures: "image",
  Other: "file",
};

// ---- Adding downloads ----

function AddBar({
  act,
  addLink,
  input,
}: {
  act: (p: Promise<unknown>) => void;
  addLink: (link: string, startNow: boolean) => Promise<unknown>;
  input: { current: HTMLInputElement | null };
}) {
  const [url, setUrl] = useState("");

  function add(startNow: boolean) {
    const link = url.trim();
    if (!link) return;
    act(addLink(link, startNow).then(() => setUrl("")));
    input.current?.focus();
  }

  return (
    <form
      class="addbar"
      onSubmit={(e) => {
        e.preventDefault();
        add(false);
      }}
    >
      <Icon name="link" cls="lead" />
      <input
        ref={input}
        type="text"
        placeholder="Paste a download link or a video page"
        aria-label="Download link"
        value={url}
        onInput={(e) => setUrl((e.target as HTMLInputElement).value)}
        autoFocus
        spellcheck={false}
      />
      {url.trim() ? (
        <>
          <button type="submit" class="primary small">
            Download
          </button>
          <button type="button" class="secondary small" onClick={() => add(true)} title="Start right away, without waiting in the queue">
            Start now
          </button>
        </>
      ) : (
        <kbd title="Copy a link, then press Ctrl+V anywhere in the window">Ctrl V</kbd>
      )}
    </form>
  );
}

// A link dragged from a browser onto the window is added like a pasted one.
function dropLink(e: DragEvent, add: (link: string) => void) {
  e.preventDefault();
  const text = e.dataTransfer?.getData("text/uri-list") || e.dataTransfer?.getData("text/plain") || "";
  const link = text.split("\n").map((s) => s.trim()).find((s) => /^https?:\/\//.test(s));
  if (link) add(link);
}

// ---- One download ----

function Row({
  d,
  act,
  queuePos,
  queue,
}: {
  d: Item;
  act: (p: Promise<unknown>) => void;
  queuePos?: number;
  queue: QueueState | null;
}) {
  const [confirming, setConfirming] = useState(false);
  const [open, setOpen] = useState(false); // the connections table
  const status = d.status as string;
  const hasPieces = (d.segments?.length ?? 0) > 0 && status !== "completed";
  const name = d.filename || nameFromUrl(d.url);
  const percent = d.total > 0 ? (d.downloaded / d.total) * 100 : 0;
  const info = infoText(d, queuePos, queue);
  const [big, small] = rateText(d, percent, queuePos, queue);

  return (
    <article class={`row ${status} ${open && hasPieces ? "open" : ""}`} data-kind={d.category || "Other"}>
      <div class="kind" aria-hidden="true">
        {extension(name)}
      </div>

      <div class="body">
        <div class="line1">
          <span class="name" title={d.url}>
            {name}
          </span>
          {d.quality && <span class="tag">{d.quality}</span>}
          {d.subtitle && d.kind !== "subtitles" && (
            <span class="tag" title="Subtitles saved with it">
              CC
            </span>
          )}
          <span class="amount">{amountText(d)}</span>
        </div>

        <PieceStrip d={d} percent={percent} />

        <div class="line2">
          {d.total > 0 && status !== "completed" && <span class="pct">{Math.floor(percent)}%</span>}
          <span class={`info ${info.tone}`} title={info.text}>
            {info.text}
          </span>
        </div>
      </div>

      <div class="rate">
        <b>{big}</b>
        <small>{small}</small>
      </div>

      <div class="actions">
        {!confirming && (
          <>
            {hasPieces ? (
              <button
                class={`icon expand ${open ? "open" : ""}`}
                type="button"
                title={open ? "Hide connections" : "Show connections"}
                aria-label={open ? "Hide connections" : "Show connections"}
                aria-expanded={open}
                onClick={() => setOpen(!open)}
              >
                <Icon name="chevron" />
              </button>
            ) : status === "completed" ? (
              <IconButton icon="open" label="Open file" onClick={() => act(DownloadService.OpenFile(d.id))} />
            ) : (
              <span class="icon-space" />
            )}
            {isActive(d) && <IconButton icon="pause" label="Pause" onClick={() => act(DownloadService.Pause(d.id))} />}
            {status === "queued" && (
              <IconButton icon="play" label="Start now" onClick={() => act(DownloadService.StartNow(d.id))} />
            )}
            {status === "paused" && <IconButton icon="play" label="Resume" onClick={() => act(DownloadService.Resume(d.id))} />}
            {status === "failed" && <IconButton icon="retry" label="Try again" onClick={() => act(DownloadService.Resume(d.id))} />}
            {status === "completed" && (
              <IconButton icon="folder" label="Show in folder" onClick={() => act(DownloadService.ShowInFolder(d.id))} />
            )}
            <IconButton icon="close" label="Remove" onClick={() => setConfirming(true)} />
          </>
        )}
      </div>

      {open && hasPieces && <Connections d={d} />}
      {confirming && (
        <div class="confirm" role="group" aria-label="Remove this download?">
          <span>Remove this download?</span>
          <button onClick={() => act(DownloadService.Remove(d.id, false))}>Remove from list</button>
          {status !== "completed" && d.downloaded > 0 && (
            <button class="danger" onClick={() => act(DownloadService.Remove(d.id, true))}>
              Remove and delete the partial file
            </button>
          )}
          <button onClick={() => setConfirming(false)}>Keep</button>
        </div>
      )}
    </article>
  );
}

// The line under the strip: what's going on, in a word or a warning.
function infoText(d: Item, queuePos?: number, queue?: QueueState | null): { text: string; tone: string } {
  const status = d.status as string;
  if (status === "failed") return { text: d.problem || "Failed", tone: "bad" };
  if (d.problem && (status === "retrying" || status === "downloading" || status === "starting")) {
    return { text: d.problem, tone: "warn" };
  }
  if (d.stage && (status === "starting" || status === "downloading" || status === "retrying")) {
    return { text: d.stage, tone: "" };
  }
  switch (status) {
    case "starting":
      return { text: "Contacting the server", tone: "" };
    case "downloading":
      if (d.hostLimit > 0) return { text: `This server limits connections, so ${d.hostLimit} are used.`, tone: "" };
      return { text: d.connections > 1 ? `${d.active} of ${d.connections} connections` : "Downloading", tone: "" };
    case "retrying":
      return { text: "Reconnecting", tone: "warn" };
    case "queued":
      if (queue?.held) return { text: `Starts at ${queue.startsAt} (scheduled)`, tone: "" };
      return { text: queuePos && queuePos > 1 ? `${ordinal(queuePos)} in line` : "Starts when a slot is free", tone: "" };
    case "paused":
      return { text: "Paused - press play to continue", tone: "" };
    case "completed":
      if (d.note) return { text: d.note, tone: "warn" };
      return { text: d.finishedAt ? `Finished ${timeAgo(d.finishedAt as unknown as string)}` : "Finished", tone: "" };
  }
  return { text: status, tone: "" };
}

// The right-hand column: the speed, big, and the time left under it.
function rateText(d: Item, percent: number, queuePos?: number, queue?: QueueState | null): [string, string] {
  switch (d.status as string) {
    case "downloading":
      return [`${size(d.speed)}/s`, d.etaSeconds >= 0 ? `${duration(d.etaSeconds)} left` : ""];
    case "starting":
      return ["Starting", ""];
    case "retrying":
      return [`${size(0)}/s`, "Reconnecting"];
    case "queued":
      if (queue?.held) return ["Scheduled", `at ${queue.startsAt}`];
      return ["Queued", !queuePos || queuePos === 1 ? "Next up" : `${ordinal(queuePos)} in line`];
    case "paused":
      return ["Paused", d.total > 0 ? `${Math.floor(percent)}%` : ""];
    case "failed":
      return ["Failed", "Try again"];
    case "completed":
      return ["Done", size(d.total > 0 ? d.total : d.downloaded)];
  }
  return ["", ""];
}

// The connections table, like IDM's: one line per piece of the file - how
// much of it has arrived, how fast, and what its connection is doing. The
// speeds are worked out here, from how much each piece grew since the last
// update.
function Connections({ d }: { d: Item }) {
  const prev = useRef(new Map<number, { done: number; at: number; speed: number }>());
  const now = performance.now();
  const segs = d.segments ?? [];
  const rows = segs.map((s, i) => {
    const len = s.end - s.start;
    const p = prev.current.get(s.start);
    let speed = p?.speed ?? 0;
    if (p && now - p.at > 150) {
      const fresh = Math.max(0, ((s.done - p.done) / (now - p.at)) * 1000);
      speed = p.speed * 0.6 + fresh * 0.4; // smoothed, so it doesn't jump about
    }
    if (!s.active) speed = 0;
    return { s, i, len, speed, keep: !p || now - p.at > 150 };
  });
  useEffect(() => {
    const next = new Map<number, { done: number; at: number; speed: number }>();
    for (const r of rows) {
      const p = prev.current.get(r.s.start);
      next.set(r.s.start, r.keep ? { done: r.s.done, at: now, speed: r.speed } : p!);
    }
    prev.current = next;
  });

  const info = (s: (typeof segs)[number], len: number) => {
    if (len > 0 && s.done >= len) return "Complete";
    if (s.retrying) return s.problem ? `Reconnecting: ${s.problem}` : "Reconnecting";
    if (s.active) return "Receiving data";
    if ((d.status as string) === "paused") return "Paused";
    return s.done > 0 ? "Waiting to continue" : "Connecting";
  };

  return (
    <table class="connections">
      <thead>
        <tr>
          <th>#</th>
          <th>Downloaded</th>
          <th class="num">Speed</th>
          <th>Info</th>
        </tr>
      </thead>
      <tbody>
        {rows.map(({ s, i, len, speed }) => {
          const pct = len > 0 ? Math.min(100, (s.done / len) * 100) : 100;
          const state = len > 0 && s.done >= len ? "complete" : s.retrying ? "retrying" : s.active ? "active" : "";
          return (
            <tr key={s.start} class={state}>
              <td class="n">{i + 1}</td>
              <td>
                <span class="mini" aria-hidden="true">
                  <span style={{ width: `${pct}%` }} />
                </span>
                {size(s.done)} of {size(len)}
              </td>
              <td class="num">{speed > 0 ? `${size(speed)}/s` : ""}</td>
              <td class="info" title={s.problem || undefined}>
                {info(s, len)}
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

// The piece strip: each piece of a multi-connection download is drawn at its
// place in the file, filled as far as it's got, and lit while its connection
// is receiving data. When a piece is split, you see it happen.
function PieceStrip({ d, percent }: { d: Item; percent: number }) {
  const segs = d.segments ?? [];
  if (segs.length > 1 && d.total > 0 && (d.status as string) !== "completed") {
    return (
      <div class="strip" role="progressbar" aria-valuenow={Math.round(percent)} aria-valuemin={0} aria-valuemax={100}>
        {segs.map((s, i) => {
          const len = s.end - s.start;
          return (
            <div
              key={`${s.start}-${i}`}
              class={`piece ${s.active ? "active" : ""} ${s.retrying ? "retrying" : ""}`}
              style={{ left: `${(s.start / d.total) * 100}%`, width: `${(len / d.total) * 100}%` }}
            >
              <div class="fill" style={{ width: `${len > 0 ? (s.done / len) * 100 : 100}%` }} />
            </div>
          );
        })}
      </div>
    );
  }
  return (
    <div class="strip" role="progressbar" aria-valuenow={Math.round(percent)} aria-valuemin={0} aria-valuemax={100}>
      <div class={`piece whole ${isActive(d) ? "active" : ""}`} style={{ left: 0, width: "100%" }}>
        <div class="fill" style={{ width: `${(d.status as string) === "completed" ? 100 : percent}%` }} />
      </div>
    </div>
  );
}

function amountText(d: Item): string {
  const status = d.status as string;
  if (status === "completed") return "";
  if (status === "queued" && d.downloaded === 0) return "Waiting";
  if (d.total > 0) return sizePair(d.downloaded, d.total);
  return d.downloaded > 0 ? size(d.downloaded) : "";
}

// "2.3 of 5.7 GB": the unit once, when both are in the same one.
function sizePair(a: number, b: number): string {
  const sa = size(a).split(" ");
  const sb = size(b).split(" ");
  return sa[1] === sb[1] ? `${sa[0]} of ${sb[0]} ${sb[1]}` : `${size(a)} of ${size(b)}`;
}

function Empty({ filter }: { filter: Filter }) {
  const text: Record<Filter, string> = {
    all: "Paste a download link above, or drag one here from your browser.",
    active: "Nothing is downloading right now.",
    waiting: "Nothing is waiting in the queue.",
    finished: "Finished downloads will appear here.",
    stopped: "No paused or failed downloads.",
  };
  return (
    <div class="empty-state">
      <svg viewBox="0 0 120 90" aria-hidden="true">
        <path
          d="M30 62a14 14 0 0 1 2-28 20 20 0 0 1 38-6 16 16 0 0 1 20 16 12 12 0 0 1-2 18z"
          fill="var(--raised)"
          stroke="var(--accent)"
          stroke-width="2.5"
        />
        <path d="M60 40v34M48 62l12 12 12-12" fill="none" stroke="var(--a2)" stroke-width="5" stroke-linecap="round" stroke-linejoin="round" />
        <circle cx="16" cy="18" r="2" fill="var(--accent)" />
        <circle cx="104" cy="14" r="2.5" fill="var(--a2)" />
        <circle cx="100" cy="72" r="1.8" fill="var(--accent)" />
      </svg>
      {filter === "all" && <b>Nothing here yet</b>}
      <span>{text[filter]}</span>
    </div>
  );
}

// ---- Notices ----

// A newer yt-dlp is out. Never downloaded without asking: data can be dear.
function UpdateNotice({ update, dismiss }: { update: ToolsUpdate; dismiss: () => void }) {
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState("");
  useEffect(
    () =>
      Events.On("video-tools", (ev) => {
        const s = ev.data as ToolsStatus;
        if (s.busy && s.total > 0) setProgress(`${size(s.done)} of ${size(s.total)}`);
        if (!s.busy) {
          setBusy(false);
          setProgress(s.error ? `Couldn't update: ${s.error}` : "");
        }
      }),
    [],
  );
  return (
    <div class="notice" role="status">
      <div class="text">
        <b>yt-dlp {update.latest} is out</b> (you have {update.current}). Video sites change often; updating keeps
        video downloads working.
        {progress && <small>{busy ? `Downloading: ${progress}` : progress}</small>}
      </div>
      <button
        class="primary"
        disabled={busy}
        onClick={() => {
          setBusy(true);
          setProgress("");
          DownloadService.InstallVideoTools();
        }}
      >
        {busy ? "Updating…" : `Update (about ${UPDATE_MB} MB)`}
      </button>
      {!busy && (
        <button class="secondary" onClick={dismiss}>
          Not now
        </button>
      )}
    </div>
  );
}

// A download link you copied somewhere.
function CopiedLink({ link, add, close }: { link: string; add: (startNow: boolean) => void; close: () => void }) {
  let host = "";
  try {
    host = new URL(link).hostname;
  } catch {}
  return (
    <div class="notice" role="status">
      <div class="text">
        <b>Download the link you copied?</b>
        <small title={link}>
          {nameFromUrl(link)} · {host}
        </small>
      </div>
      <button class="primary" onClick={() => add(false)}>
        Download
      </button>
      <button class="secondary" onClick={() => add(true)}>
        Start now
      </button>
      <IconButton icon="close" label="No thanks" onClick={close} />
    </div>
  );
}

// The minute before "when done" puts the computer to sleep or shuts it down.
function Countdown({ state, cancel }: { state: WhenDoneState; cancel: () => void }) {
  const [left, setLeft] = useState(state.secondsLeft);
  useEffect(() => {
    const end = Date.now() + state.secondsLeft * 1000;
    const t = window.setInterval(() => setLeft(Math.max(0, Math.round((end - Date.now()) / 1000))), 250);
    return () => clearInterval(t);
  }, [state]);
  const what = state.action === "sleep" ? "go to sleep" : "shut down";
  return (
    <div class="scrim">
      <section class="picker countdown" role="alertdialog" aria-label="Downloads finished">
        <h2>Downloads finished</h2>
        <p>
          The computer will {what} in <b>{left} s</b>.
        </p>
        <footer class="buttons">
          <button class="primary" onClick={cancel} autoFocus>
            Cancel
          </button>
        </footer>
      </section>
    </div>
  );
}

// ---- Videos ----

// The quality picker: yt-dlp lists what the page offers, you pick one.
function VideoPicker({
  url,
  source,
  speed,
  close,
  act,
  openSettings,
}: {
  url: string;
  source: string;
  speed: number; // current total download speed, for the time estimate
  close: () => void;
  act: (p: Promise<unknown>) => void;
  openSettings: () => void;
}) {
  const [look, setLook] = useState<VideoLookup | null>(null);
  const [error, setError] = useState("");
  const [choice, setChoice] = useState("");
  const [name, setName] = useState(""); // the file's name, editable
  const [subtitle, setSubtitle] = useState(""); // a subtitle language to save too
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    setLook(null);
    setError("");
    DownloadService.LookUpVideo(url)
      .then((l) => {
        setLook(l);
        setChoice(l.suggested);
        setName(l.title ?? "");
        setSubtitle(preferredSubtitle(l.subtitles ?? []));
      })
      .catch((e) => setError(e?.message ?? String(e)));
  }, [url, attempt]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && close();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  function add(startNow: boolean) {
    const c = look?.choices?.find((x) => x.id === choice);
    if (!look || !c) return;
    rememberSubtitle(look.subtitles ?? [], subtitle);
    act(DownloadService.AddVideo(look.pageUrl, c, name.trim() || look.title, subtitle, startNow).then(close));
  }

  // Just the subtitles, without the video.
  function addSubtitlesOnly() {
    if (!look || !subtitle) return;
    rememberSubtitle(look.subtitles ?? [], subtitle);
    act(DownloadService.AddSubtitles(look.pageUrl, name.trim() || look.title, subtitle).then(close));
  }

  const ready = look && !look.needsTools && (look.choices?.length ?? 0) > 0;
  return (
    <div class="scrim" onClick={(e) => e.target === e.currentTarget && close()}>
      <section class="picker" role="dialog" aria-label="Download video">
        <header>
          <h2>Download video</h2>
          <IconButton icon="close" label="Close" onClick={close} />
        </header>

        {!look && !error && (
          <div class="looking" role="status">
            <span class="spinner" aria-hidden="true" />
            <div>
              Looking up the video…
              <small>{url}</small>
            </div>
          </div>
        )}

        {error && (
          <div class="looking">
            <p class="error">{error}</p>
            {source && (
              <p class="hint">
                The page's video player is playing an ordinary video file. You can download that file instead.
              </p>
            )}
            <div class="buttons">
              {source && (
                <button class="primary" onClick={() => act(DownloadService.Add(source, false).then(close))}>
                  Download the video file
                </button>
              )}
              <button class="secondary" onClick={() => setAttempt(attempt + 1)}>
                Try again
              </button>
            </div>
          </div>
        )}

        {look?.needsTools && (
          <div class="needs-tools">
            <p>
              Video downloads use <b>yt-dlp</b>, the free open-source tool that reads YouTube and over a thousand
              other sites, plus a small helper YouTube needs. They're downloaded once, about {TOOLS_MB} MB.
            </p>
            <VideoTools onReady={() => setAttempt(attempt + 1)} />
          </div>
        )}

        {ready && look && (
          <>
            <div class="vhead">
              {look.thumbnail && <img class="thumb" src={look.thumbnail} alt="" referrerpolicy="no-referrer" />}
              <div class="vtitle">
                <textarea
                  class="name-input"
                  rows={2}
                  aria-label="File name"
                  title="The file's name - you can change it"
                  value={name}
                  onInput={(e) => setName((e.target as HTMLTextAreaElement).value.replace(/\n/g, " "))}
                  onKeyDown={(e) => e.key === "Enter" && e.preventDefault()}
                  spellcheck={false}
                />
                <p class="meta">
                  {[look.uploader, look.duration > 0 ? clock(look.duration) : "", look.site].filter(Boolean).join(" · ")}
                </p>
              </div>
            </div>

            <fieldset class="choices">
              <legend>Quality</legend>
              {look.choices.map((c: VideoChoice) => (
                <label key={c.id} class={choice === c.id ? "on" : ""}>
                  <input type="radio" name="quality" checked={choice === c.id} onChange={() => setChoice(c.id)} />
                  <span class="label">{c.label}</span>
                  <span class="detail">{c.detail}</span>
                  <span class="vsize">{c.size > 0 ? `${c.sizeApprox ? "≈ " : ""}${size(c.size)}` : ""}</span>
                </label>
              ))}
            </fieldset>

            {(() => {
              const c = look.choices.find((x: VideoChoice) => x.id === choice);
              return c && c.size > 0 && speed > 0 ? (
                <p class="estimate">
                  {c.label} takes about {duration(c.size / speed)} at your current speed of {size(speed)}/s.
                </p>
              ) : null;
            })()}

            {(look.subtitles?.length ?? 0) > 0 && (
              <label class="subtitles">
                <span>Subtitles</span>
                <select value={subtitle} onChange={(e) => setSubtitle((e.target as HTMLSelectElement).value)}>
                  <option value="">None</option>
                  {look.subtitles.map((s: VideoSubtitle) => (
                    <option key={s.id} value={s.id}>
                      {s.name}
                    </option>
                  ))}
                </select>
                <button
                  type="button"
                  class="secondary"
                  disabled={!subtitle}
                  title="Just the subtitle file, without the video"
                  onClick={addSubtitlesOnly}
                >
                  Subtitles only
                </button>
                <small>Saved as a separate file next to the video, or on its own with "Subtitles only".</small>
              </label>
            )}

            <footer class="buttons">
              <p class="fineprint">
                Only download videos you have the right to keep.{" "}
                <button class="link" onClick={openSettings}>
                  Video settings
                </button>
              </p>
              <button class="secondary" onClick={() => add(true)} title="Start right away, without waiting in the queue">
                Start now
              </button>
              <button class="primary" onClick={() => add(false)} disabled={!choice}>
                Download
              </button>
            </footer>
          </>
        )}
        {look && !look.needsTools && !ready && <p class="error">No downloadable qualities were found.</p>}
      </section>
    </div>
  );
}

// The subtitle language last chosen is offered again: "en" matches "en",
// "en-US" and English automatic captions (people-made ones first).
const SUB_KEY = "dlm.subtitleLanguage";
const baseLang = (id: string) => id.replace(/^auto:/, "").replace(/-orig$/, "").split("-")[0].toLowerCase();

function preferredSubtitle(subs: VideoSubtitle[]): string {
  let want = "";
  try {
    want = localStorage.getItem(SUB_KEY) ?? "";
  } catch {}
  if (!want) return "";
  const matches = subs.filter((s) => baseLang(s.id) === want);
  return (matches.find((s) => !s.auto) ?? matches[0])?.id ?? "";
}

function rememberSubtitle(subs: VideoSubtitle[], id: string) {
  // Only remember a choice made where there was one to make.
  if (subs.length === 0) return;
  try {
    localStorage.setItem(SUB_KEY, id ? baseLang(id) : "");
  } catch {}
}

// The video tools (yt-dlp and QuickJS): status, and download or update.
function VideoTools({ onReady }: { onReady?: () => void }) {
  const [st, setSt] = useState<ToolsStatus | null>(null);
  useEffect(() => {
    DownloadService.VideoTools().then(setSt);
    return Events.On("video-tools", (ev) => {
      const s = ev.data as ToolsStatus;
      setSt(s);
      if (!s.busy && s.installed && !s.error) onReady?.();
    });
  }, []);
  if (!st) return null;

  if (st.busy) {
    const pct = st.total > 0 ? (st.done / st.total) * 100 : 0;
    return (
      <div class="tools-status">
        <div class="strip" role="progressbar" aria-valuenow={Math.round(pct)} aria-valuemin={0} aria-valuemax={100}>
          <div class="piece whole active" style={{ left: 0, width: "100%" }}>
            <div class="fill" style={{ width: `${pct}%` }} />
          </div>
        </div>
        <p class="hint">
          {st.file ? `Downloading ${st.file}: ${size(st.done)}${st.total > 0 ? ` of ${size(st.total)}` : ""}` : "Checking for the latest versions…"}
        </p>
      </div>
    );
  }
  return (
    <div class="tools-status">
      {st.error && <p class="error">{st.error}</p>}
      {st.installed ? (
        <div class="field">
          <span>yt-dlp {st.version || ""} is ready.</span>
          <button onClick={() => { setSt({ ...st, busy: true, error: "" }); DownloadService.InstallVideoTools(); }}>
            Check for updates
          </button>
        </div>
      ) : (
        <button class="primary wide" onClick={() => { setSt({ ...st, busy: true, error: "" }); DownloadService.InstallVideoTools(); }}>
          Download video tools (about {TOOLS_MB} MB)
        </button>
      )}
    </div>
  );
}

function clock(seconds: number): string {
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const pad = (n: number) => String(n).padStart(2, "0");
  return h > 0 ? `${h}:${pad(m)}:${pad(s % 60)}` : `${m}:${pad(s % 60)}`;
}

// ---- Settings ----

function SettingsPanel({ close, act }: { close: () => void; act: (p: Promise<unknown>) => void }) {
  const [s, setS] = useState<Settings | null>(null);
  const [limits, setLimits] = useState<ServerLimit[]>([]);
  const [newHost, setNewHost] = useState("");
  const [newMax, setNewMax] = useState(2);
  const [startup, setStartup] = useState(false);

  const loadLimits = () => DownloadService.ServerLimits().then((l) => setLimits(l ?? []));
  useEffect(() => {
    DownloadService.GetSettings().then(setS);
    DownloadService.StartsWithSystem().then(setStartup);
    loadLimits();
    // A download running now may learn a limit while this is open.
    const poll = setInterval(loadLimits, 3000);
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && close();
    window.addEventListener("keydown", onKey);
    return () => {
      clearInterval(poll);
      window.removeEventListener("keydown", onKey);
    };
  }, []);

  function save(next: Settings) {
    setS(next);
    applyTheme(next.theme);
    act(DownloadService.SetSettings(next).then(setS));
  }

  if (!s) return null;
  const sched = s.schedule;
  const setSched = (change: Partial<typeof sched>) => save({ ...s, schedule: { ...sched, ...change } });
  const onWindows = navigator.userAgent.includes("Windows");
  return (
    <div class="scrim" onClick={(e) => e.target === e.currentTarget && close()}>
      <aside class="panel" role="dialog" aria-label="Settings">
        <header>
          <h2>Settings</h2>
          <IconButton icon="close" label="Close settings" onClick={close} />
        </header>

        <section>
          <h3>Theme</h3>
          <div class="themes" role="radiogroup" aria-label="Theme">
            {THEMES.map(([id, name, sw]) => (
              <button
                key={id}
                role="radio"
                aria-checked={s.theme === id}
                class={s.theme === id ? "on" : ""}
                onClick={() => save({ ...s, theme: id })}
              >
                <span class="swatch" style={{ background: sw[0] }}>
                  <i style={{ background: sw[1] }} />
                  <i style={{ background: sw[2] }} />
                </span>
                {name}
              </button>
            ))}
          </div>
        </section>

        <section>
          <label class="field">
            <span>Downloads at the same time</span>
            <Stepper value={s.maxActive} min={1} max={10} onChange={(v) => save({ ...s, maxActive: v })} />
          </label>
          <p class="hint">
            Others wait in line and start as these finish. On a slow connection, fewer at once means each file
            arrives sooner.
          </p>

          <label class="field">
            <span>Connections per download</span>
            <Stepper value={s.connections} min={1} max={32} onChange={(v) => save({ ...s, connections: v })} />
          </label>
          <p class="hint">More connections can be faster, up to what your connection and the server allow.</p>

          <div class="field">
            <span>Save downloads in</span>
            <div class="folder">
              <span class="path" title={s.folder}>
                {s.folder}
              </span>
              <button
                onClick={() =>
                  act(
                    DownloadService.ChooseFolder().then((f) => {
                      if (f) save({ ...s, folder: f });
                    }),
                  )
                }
              >
                Change
              </button>
            </div>
          </div>
        </section>

        <section>
          <Toggle
            label="Sort downloads into folders by kind"
            checked={s.sortIntoFolders}
            onChange={(v) => save({ ...s, sortIntoFolders: v })}
          />
          <p class="hint">
            Video, Music, Compressed, Programs, Documents and Pictures, inside the download folder (the same folders IDM uses). Other files stay
            in the folder itself.
          </p>
        </section>

        <section>
          <h3>Schedule</h3>
          <Toggle label="Download only at set times" checked={sched.enabled} onChange={(v) => setSched({ enabled: v })} />
          <p class="hint">
            Downloads wait for the start time - for a night data bundle, say. "Start now" skips the wait.
          </p>
          {sched.enabled && (
            <>
              <label class="field">
                <span>Start at</span>
                <input
                  type="time"
                  value={sched.start}
                  onChange={(e) => setSched({ start: (e.target as HTMLInputElement).value })}
                />
              </label>
              <label class="field">
                <span>Stop at</span>
                <span class="timepair">
                  <input
                    type="time"
                    value={sched.stop}
                    disabled={!sched.stop}
                    aria-label="Stop time"
                    onChange={(e) => setSched({ stop: (e.target as HTMLInputElement).value })}
                  />
                </span>
              </label>
              <Toggle
                label="Run until the queue is empty instead"
                checked={!sched.stop}
                onChange={(v) => setSched({ stop: v ? "" : "06:00" })}
              />
              <p class="hint">
                {sched.stop
                  ? `At ${sched.stop}, unfinished downloads stop and carry on at ${sched.start} the next day.`
                  : `From ${sched.start} until everything is downloaded. Links added after that wait for the next ${sched.start}.`}
              </p>
            </>
          )}
        </section>

        <section>
          <h3>Catching links</h3>
          <Toggle
            label="Offer download links I copy"
            checked={s.watchClipboard}
            onChange={(v) => save({ ...s, watchClipboard: v })}
          />
          <p class="hint">
            Copy a link to a file - a program, an archive, a video or music file, a PDF - anywhere, and Dream Download Manager offers
            to download it. Web page addresses are left alone.
          </p>
          <Toggle
            label={onWindows ? "Start with Windows" : "Start when I sign in"}
            checked={startup}
            onChange={(v) => act(DownloadService.SetStartWithSystem(v).then(() => setStartup(v)))}
          />
          <p class="hint">
            Starts quietly in the tray (no window), so links from the browser and the schedule work from the start.
            Closing the window always leaves it running in the tray; quit from the tray icon.
          </p>
        </section>

        <section>
          <h3>Video downloads</h3>
          <p class="hint">
            YouTube and other video sites are read by yt-dlp. Sites change often, so check for updates if a video
            stops working.
          </p>
          <VideoTools />
          <Toggle
            label="Tell me when yt-dlp has an update"
            checked={s.checkToolUpdates}
            onChange={(v) => save({ ...s, checkToolUpdates: v })}
          />
          <p class="hint">A tiny check, at most once a week. Updates are only downloaded when you say so.</p>
        </section>

        <section>
          <h3>Servers that limit connections</h3>
          <p class="hint">
            When a server refuses extra connections, the app remembers its limit for a day so the next download
            doesn't get refused. Limits you set yourself are kept until you remove them.
          </p>
          {limits.length === 0 ? (
            <p class="hint">No servers remembered yet.</p>
          ) : (
            <table class="limits">
              <tbody>
                {limits.map((l) => (
                  <tr key={l.host}>
                    <td class="host">{l.host}</td>
                    <td>{l.maxConnections}</td>
                    <td class="how">{l.manual ? "set by you" : "learned"}</td>
                    <td>
                      <button onClick={() => act(DownloadService.ForgetServerLimit(l.host).then(loadLimits))}>
                        Remove
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <form
            class="addlimit"
            onSubmit={(e) => {
              e.preventDefault();
              act(
                DownloadService.SetServerLimit(newHost.trim(), newMax).then(() => {
                  setNewHost("");
                  loadLimits();
                }),
              );
            }}
          >
            <input
              type="text"
              placeholder="Server, e.g. example.com"
              aria-label="Server name"
              value={newHost}
              onInput={(e) => setNewHost((e.target as HTMLInputElement).value)}
            />
            <Stepper value={newMax} min={1} max={32} onChange={setNewMax} />
            <button type="submit" disabled={!newHost.trim()}>
              Add limit
            </button>
          </form>
        </section>
      </aside>
    </div>
  );
}

// ---- Help ----

// Help and frequent questions. Plain text in the app itself: it works
// offline, and costs a few KB.
const HELP: { title: string; items: [string, string][] }[] = [
  {
    title: "Getting started",
    items: [
      [
        "How do I download something?",
        "Paste a link in the box at the top and press Download - or drag a link onto the window. With the browser extension, downloads you start in Chrome or Edge come here by themselves.",
      ],
      [
        "How do I connect the browser extension?",
        "Press Show the welcome again (at the bottom of Help) and go to Browser: it walks you through it and shows when it's connected. In short: type chrome://extensions (Edge: edge://extensions) in the address bar, switch on Developer mode, press Load unpacked and choose the \"extension\" folder where Dream DM is installed - the folder itself, not one inside it. \"Manifest file is missing\" means a folder inside it (like icons) was chosen: choose \"extension\" instead.",
      ],
      [
        "Download or Start now?",
        "Download puts it in the queue: a few run at once (3, by default) and the rest wait their turn. Start now skips the queue - and the schedule.",
      ],
      [
        "Where are my files?",
        "In your download folder (Settings > Save downloads in), sorted into Video, Music, Compressed, Programs, Documents and Pictures if that's switched on. The folder button on a finished download opens its folder with the file selected.",
      ],
      [
        "What does the coloured bar show?",
        "Each piece of the file being downloaded at the same time, at its place in the file. Amber means data is arriving right now. Press the small arrow on a download to see each connection's progress, speed and status.",
      ],
    ],
  },
  {
    title: "Videos",
    items: [
      [
        "Which sites can I download videos from?",
        `YouTube and over a thousand others - Instagram, Facebook, TikTok, X, Vimeo and many more - through yt-dlp, a free open-source tool downloaded once (about ${TOOLS_MB} MB). Point at a video in the browser and press the Download button that appears, or paste the page's address.`,
      ],
      [
        "It says no video was found.",
        "Some players only load the video once you press play: play it, then press Download again. On Instagram or Facebook, make sure you're logged in in the browser. If a site that used to work stops working, update yt-dlp (Settings > Video downloads).",
      ],
      [
        "Why does it say \"Joining picture and sound\"?",
        "In HD, sites send picture and sound as two separate files. Both are downloaded, then put together into one file - without re-encoding, so nothing is lost and it takes seconds.",
      ],
      [
        "How do I get subtitles?",
        "In the video window, choose a language under Subtitles. They're saved next to the video with the same name (Video.en.srt), which players load by themselves. Subtitles only saves just the subtitle file, for a video you already have.",
      ],
      [
        "Can it download paid films or DRM-protected video?",
        "No. Protected streams (Netflix, paid films) are skipped. Only download videos you have the right to keep.",
      ],
    ],
  },
  {
    title: "Saving data and time",
    items: [
      [
        "How do I download only at night?",
        "Settings > Schedule: switch on Download only at set times and choose the start and stop times - for a night bundle, say. Downloads wait until then, and stop (keeping their progress) at the stop time.",
      ],
      [
        "Can it switch off the computer when it's done?",
        "Yes: When done, at the bottom right. After the last download, you get a minute to cancel, then the computer sleeps or shuts down. It happens once and goes back to do nothing.",
      ],
      [
        "Will it use my data without asking?",
        "Only for what you download. yt-dlp updates are offered, never downloaded by themselves; the check for one is a tiny request at most once a week.",
      ],
    ],
  },
  {
    title: "Problems",
    items: [
      [
        "A download stopped with an error.",
        "Press Try again: it continues from where it stopped. Links from some sites expire after a while; then start the download again from the site.",
      ],
      [
        "Does a download survive closing the app or a power cut?",
        "Yes. Progress is saved as it goes, and unfinished downloads continue the next time the app runs.",
      ],
      [
        "\"This server limits connections\" - is something wrong?",
        "No. Some servers allow only a few connections per person. The app notices, uses that many, and remembers it for the next download from that server.",
      ],
      [
        "Downloads from the browser don't arrive.",
        "Check that the extension is switched on (its icon in Chrome or Edge). Closing the window doesn't stop the app - it keeps running in the tray, next to the clock - but quitting it from the tray icon does; switch on Start with Windows to always have it ready.",
      ],
    ],
  },
  {
    title: "About",
    items: [
      [
        "Is it really free?",
        "Yes - free, open source, no ads, no account, nothing sent anywhere. Your list, settings and browser cookies stay on your computer.",
      ],
    ],
  },
];

function HelpPanel({ close, welcome }: { close: () => void; welcome: () => void }) {
  const [q, setQ] = useState("");
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && close();
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  const words = q.toLowerCase().split(/\s+/).filter(Boolean);
  const match = ([question, answer]: [string, string]) => {
    const text = (question + " " + answer).toLowerCase();
    return words.every((w) => text.includes(w));
  };
  const sections = HELP.map((s) => ({ ...s, items: s.items.filter(match) })).filter((s) => s.items.length > 0);
  return (
    <div class="scrim" onClick={(e) => e.target === e.currentTarget && close()}>
      <aside class="panel help" role="dialog" aria-label="Help">
        <header>
          <h2>Help</h2>
          <IconButton icon="close" label="Close help" onClick={close} />
        </header>
        <input
          type="search"
          class="help-search"
          placeholder="Search the questions"
          aria-label="Search the questions"
          value={q}
          onInput={(e) => setQ((e.target as HTMLInputElement).value)}
          autoFocus
        />
        {sections.length === 0 && <p class="hint">Nothing matches “{q}”.</p>}
        {sections.map((s) => (
          <section key={s.title}>
            <h3>{s.title}</h3>
            {s.items.map(([question, answer]) => (
              <details key={question} open={words.length > 0}>
                <summary>{question}</summary>
                <p>{answer}</p>
              </details>
            ))}
          </section>
        ))}
        <section>
          <button type="button" onClick={welcome}>
            Show the welcome again
          </button>
        </section>
      </aside>
    </div>
  );
}

// ---- First run ----

// The welcome a new user sees once: pick a look, how downloading works,
// connecting the browser, and a few choices. Finishing or skipping saves
// "welcomed", so it doesn't come back (Help can show it again).
const WELCOME_STEPS = ["Look", "Downloading", "Browser", "Choices"];

function Welcome({ close, act }: { close: () => void; act: (p: Promise<unknown>) => void }) {
  const [step, setStep] = useState(0);
  const [s, setS] = useState<Settings | null>(null);

  useEffect(() => {
    DownloadService.GetSettings().then(setS);
  }, []);

  function save(next: Settings) {
    setS(next);
    applyTheme(next.theme);
    act(DownloadService.SetSettings(next).then(setS));
  }

  function finish() {
    if (s) act(DownloadService.SetSettings({ ...s, welcomed: true }));
    close();
  }

  if (!s) return null;
  const last = step === WELCOME_STEPS.length - 1;
  return (
    <div class="scrim">
      <section class="picker welcome" role="dialog" aria-label="Welcome to Dream Download Manager">
        <header>
          <ol class="steps">
            {WELCOME_STEPS.map((name, i) => (
              <li key={name} class={i === step ? "on" : i < step ? "done" : ""} aria-current={i === step ? "step" : undefined}>
                {name}
              </li>
            ))}
          </ol>
          <button type="button" class="link" onClick={finish}>
            Skip
          </button>
        </header>

        {step === 0 && <WelcomeLook s={s} save={save} />}
        {step === 1 && <WelcomeDownloading s={s} save={save} />}
        {step === 2 && <WelcomeBrowser act={act} />}
        {step === 3 && <WelcomeChoices s={s} save={save} act={act} />}

        <div class="buttons">
          {step > 0 && (
            <button type="button" class="secondary" onClick={() => setStep(step - 1)}>
              Back
            </button>
          )}
          <button type="button" class="primary" onClick={() => (last ? finish() : setStep(step + 1))}>
            {last ? "Start downloading" : "Next"}
          </button>
        </div>
      </section>
    </div>
  );
}

function WelcomeLook({ s, save }: { s: Settings; save: (s: Settings) => void }) {
  return (
    <>
      <div class="hello">
        <i class="logo" aria-hidden="true" />
        <div>
          <h2>Welcome to Dream Download Manager</h2>
          <p class="hint">Free and open source, with no ads and no account.</p>
        </div>
      </div>
      <p>Pick a look to start with. You can change it any time in Settings.</p>
      <div class="themes" role="radiogroup" aria-label="Theme">
        {THEMES.map(([id, name, sw]) => (
          <button
            type="button"
            key={id}
            role="radio"
            aria-checked={s.theme === id}
            class={s.theme === id ? "on" : ""}
            onClick={() => save({ ...s, theme: id })}
          >
            <span class="swatch" style={{ background: sw[0] }}>
              <i style={{ background: sw[1] }} />
              <i style={{ background: sw[2] }} />
            </span>
            {name}
          </button>
        ))}
      </div>
    </>
  );
}

function WelcomeDownloading({ s, save }: { s: Settings; save: (s: Settings) => void }) {
  return (
    <>
      <h2>How to download</h2>
      <ul class="howto">
        <li>
          <Icon name="link" />
          <div>
            <b>Paste a link</b>
            <p class="hint">
              Press Ctrl+V anywhere in the window, or paste it in the box at the top. Download puts it in line with the
              others; Start now begins straight away.
            </p>
          </div>
        </li>
        <li>
          <Icon name="download" />
          <div>
            <b>Or just copy it</b>
            <p class="hint">
              Copy a link to a file anywhere - a program, an archive, a video or music file, a PDF - and Dream DM offers to
              download it.
            </p>
            <Toggle
              label="Offer download links I copy"
              checked={s.watchClipboard}
              onChange={(v) => save({ ...s, watchClipboard: v })}
            />
          </div>
        </li>
        <li>
          <Icon name="video" />
          <div>
            <b>Videos</b>
            <p class="hint">
              Paste the page's address - YouTube, Instagram, TikTok and many more - and choose the quality and subtitles. The
              first time, it asks before downloading its free video tools (about {TOOLS_MB} MB).
            </p>
          </div>
        </li>
      </ul>
    </>
  );
}

const BROWSER_NAMES: Record<string, string> = { chrome: "Chrome", edge: "Edge" };
const EXTENSION_PAGES: Record<string, string> = { chrome: "chrome://extensions", edge: "edge://extensions" };

function WelcomeBrowser({ act }: { act: (p: Promise<unknown>) => void }) {
  const [browsers, setBrowsers] = useState<string[]>([]);
  const [folder, setFolder] = useState("");
  const [connected, setConnected] = useState(false);
  const [copiedWhat, setCopiedWhat] = useState(""); // what was just copied, for "Copied"

  useEffect(() => {
    DownloadService.Browsers().then((b) => setBrowsers(b ?? []));
    DownloadService.ExtensionFolder().then(setFolder);
    DownloadService.BrowserConnected().then(setConnected);
    return Events.On("browser-connected", () => setConnected(true));
  }, []);

  function copy(text: string) {
    act(
      navigator.clipboard.writeText(text).then(() => {
        setCopiedWhat(text);
        setTimeout(() => setCopiedWhat(""), 2500);
      }),
    );
  }

  return (
    <>
      <h2>Catch downloads from your browser</h2>
      <p class="hint">
        With the browser extension, downloads you start in Chrome or Edge come straight here, and video pages get a Download
        button. Setting it up takes a minute:
      </p>
      <ol class="setup">
        <li>
          Open your browser's extensions page: copy its address, paste it in the browser's address bar and press Enter.
          (Browsers don't let other programs open this page for you.)
          <div class="row-buttons">
            {(browsers.length > 0 ? browsers : ["chrome", "edge"]).map((b) => {
              const page = EXTENSION_PAGES[b];
              return (
                <button type="button" class="secondary" key={b} onClick={() => copy(page)}>
                  {copiedWhat === page ? "Copied" : `${BROWSER_NAMES[b]}: copy ${page}`}
                </button>
              );
            })}
          </div>
        </li>
        <li>
          Switch on <b>Developer mode</b> (top right in Chrome, bottom left in Edge).
        </li>
        <li>
          Press <b>Load unpacked</b> and choose this folder:
          {folder ? (
            <>
              <code class="folder-path">{folder}</code>
              <div class="row-buttons">
                <button type="button" class="secondary" onClick={() => copy(folder)}>
                  {copiedWhat === folder ? "Copied" : "Copy the folder's address"}
                </button>
                <button type="button" class="secondary" onClick={() => act(DownloadService.OpenExtensionFolder())}>
                  Open the folder
                </button>
              </div>
              <p class="hint">
                Tip: paste the address into the folder window's address bar, then press <b>Select Folder</b>. Choose the{" "}
                <b>extension</b> folder itself - don't go into it (choosing the <b>icons</b> folder inside gives "Manifest file is
                missing").
              </p>
            </>
          ) : (
            <p class="hint">The extension's files weren't found. Reinstalling Dream DM puts them back.</p>
          )}
        </li>
        <li>
          Press the Dream DM button in the browser's toolbar (it may be under the puzzle-piece icon - pin it to keep it
          handy).
          <p class={connected ? "connection ok" : "connection"} role="status">
            {connected ? (
              <>
                <Icon name="check" /> Connected - downloads from your browser will come here.
              </>
            ) : (
              "Waiting for the extension..."
            )}
          </p>
        </li>
      </ol>
      <p class="hint">
        Your browser may warn about extensions in developer mode, because this one isn't from a web store. It's safe to keep
        it on. You can also do this later: Help explains it again.
      </p>
    </>
  );
}

function WelcomeChoices({ s, save, act }: { s: Settings; save: (s: Settings) => void; act: (p: Promise<unknown>) => void }) {
  const [startup, setStartup] = useState(false);
  const onWindows = navigator.userAgent.includes("Windows");
  useEffect(() => {
    DownloadService.StartsWithSystem().then(setStartup);
  }, []);
  return (
    <>
      <h2>A few choices</h2>
      <Toggle
        label={onWindows ? "Start with Windows" : "Start when I sign in"}
        checked={startup}
        onChange={(v) => act(DownloadService.SetStartWithSystem(v).then(() => setStartup(v)))}
      />
      <p class="hint">
        Starts quietly in the tray, so downloads from the browser and the schedule work from the moment you sign in.
      </p>
      <Toggle
        label="Tell me when the video tools have an update"
        checked={s.checkToolUpdates}
        onChange={(v) => save({ ...s, checkToolUpdates: v })}
      />
      <p class="hint">A tiny check, at most once a week. Nothing is downloaded unless you say so.</p>
      <p class="hint">
        Downloads are saved in <b>{s.folder}</b>
        {s.sortIntoFolders && ", sorted into Video, Music, Compressed, Programs, Documents and Pictures"}. On a night data
        bundle? <b>Settings &gt; Schedule</b> makes downloads wait for the time you choose.
      </p>
    </>
  );
}

function Toggle({ label, checked, onChange }: { label: string; checked: boolean; onChange: (v: boolean) => void }) {
  return (
    <label class="toggle">
      <span>{label}</span>
      <input
        type="checkbox"
        role="switch"
        checked={checked}
        onChange={(e) => onChange((e.target as HTMLInputElement).checked)}
      />
    </label>
  );
}

function Stepper({ value, min, max, onChange }: { value: number; min: number; max: number; onChange: (v: number) => void }) {
  return (
    <span class="stepper">
      <button type="button" aria-label="Fewer" disabled={value <= min} onClick={() => onChange(value - 1)}>
        −
      </button>
      <span class="value">{value}</span>
      <button type="button" aria-label="More" disabled={value >= max} onClick={() => onChange(value + 1)}>
        +
      </button>
    </span>
  );
}

// ---- Icons (inline SVG, so nothing extra to load) ----

// Line icons (drawn with strokes, so they follow the text colour and weight).
const ICONS: Record<string, string> = {
  pause: "M9 5v14M15 5v14",
  play: "M7 4.5v15l12-7.5z",
  close: "M6 6l12 12M18 6 6 18",
  retry: "M20 12a8 8 0 1 1-2.3-5.7M20 4v4.5h-4.5",
  open: "M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5",
  folder: "M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z",
  chevron: "M6 9l6 6 6-6",
  help: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM9.5 9.3a2.6 2.6 0 0 1 5 .9c0 1.8-2.5 2.3-2.5 3.8M12 17h.01",
  gear: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19.4 15a1.6 1.6 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.6 1.6 0 0 0-2.7 1.1V21a2 2 0 1 1-4 0v-.1a1.6 1.6 0 0 0-2.7-1.1l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1A1.6 1.6 0 0 0 3.1 14H3a2 2 0 1 1 0-4h.1a1.6 1.6 0 0 0 1.1-2.7l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1A1.6 1.6 0 0 0 10 3.1V3a2 2 0 1 1 4 0v.1a1.6 1.6 0 0 0 2.7 1.1l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.6 1.6 0 0 0 1.1 2.7h.1a2 2 0 1 1 0 4h-.1a1.6 1.6 0 0 0-1.2 1.3z",
  // A bin with a tick: "clear the finished ones" (from the list - the files stay).
  sweep: "M4 7h16M10 11v6M14 11v6M5.5 7l1 12a1 1 0 0 0 1 1h9a1 1 0 0 0 1-1l1-12M9 7V4.5a.5.5 0 0 1 .5-.5h5a.5.5 0 0 1 .5.5V7", // a bin: clear
  plus: "M12 5v14M5 12h14",
  link: "M10 14a4.5 4.5 0 0 0 6.4 0l3-3a4.5 4.5 0 0 0-6.4-6.4l-1 1M14 10a4.5 4.5 0 0 0-6.4 0l-3 3a4.5 4.5 0 0 0 6.4 6.4l1-1",
  list: "M9 6h11M9 12h11M9 18h11M4.5 6h.01M4.5 12h.01M4.5 18h.01",
  download: "M12 4v11M7 10.5l5 5 5-5M5 20h14",
  clock: "M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7v5l3 2",
  check: "M5 12.5l4.5 4.5L19 7.5",
  square: "M7 7h10v10H7z",
  video: "M4 5h16a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1zM10 9.5v5l4.5-2.5z",
  music: "M9 18V6l11-2v12M9 18a3 3 0 1 1-6 0 3 3 0 0 1 6 0zM20 16a3 3 0 1 1-6 0 3 3 0 0 1 6 0z",
  archive: "M3 4h18v4H3zM5 8v11a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1V8M10 12h4",
  window: "M4 4h16a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1zM3 9h18",
  document: "M14 3H7a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V7zM14 3v4h4M9 13h6M9 17h6",
  image: "M4 4h16a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1zM9 10.5a1.5 1.5 0 1 0 0-.01M21 15l-5-5L5 20",
  file: "M14 3H7a1 1 0 0 0-1 1v16a1 1 0 0 0 1 1h10a1 1 0 0 0 1-1V7zM14 3v4h4",
};

function Icon({ name, cls = "" }: { name: string; cls?: string }) {
  return (
    <svg class={`ico ${cls}`} viewBox="0 0 24 24" aria-hidden="true">
      <path d={ICONS[name]} />
    </svg>
  );
}

function IconButton({ icon, label, onClick }: { icon: string; label: string; onClick: () => void }) {
  return (
    <button class="icon" type="button" onClick={onClick} title={label} aria-label={label}>
      <Icon name={icon} />
    </button>
  );
}

// ---- Formatting ----

function size(bytes: number): string {
  if (!(bytes >= 0)) return "?";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (bytes >= 1024 && i < units.length - 1) {
    bytes /= 1024;
    i++;
  }
  return `${bytes.toFixed(i === 0 || bytes >= 100 ? 0 : 1)} ${units[i]}`;
}

function duration(seconds: number): string {
  const s = Math.round(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h} h ${m} min`;
  if (m > 0) return `${m} min ${s % 60} s`;
  return `${s} s`;
}

function timeAgo(iso: string): string {
  const then = new Date(iso).getTime();
  if (!then) return "";
  const mins = Math.round((Date.now() - then) / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins} min ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours} h ago`;
  return new Date(iso).toLocaleDateString();
}

function ordinal(n: number): string {
  const s = ["th", "st", "nd", "rd"];
  const v = n % 100;
  return n + (s[(v - 20) % 10] || s[v] || s[0]);
}

function nameFromUrl(url: string): string {
  try {
    const last = new URL(url).pathname.split("/").filter(Boolean).pop();
    return last ? decodeURIComponent(last) : url;
  } catch {
    return url;
  }
}

function extension(name: string): string {
  const m = /\.([a-z0-9]{1,5})$/i.exec(name);
  return m ? m[1].toLowerCase() : "file";
}

render(<App />, document.getElementById("app")!);
