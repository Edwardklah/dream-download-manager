// Dream Download Manager browser extension - the background part.
//
// Two ways a link reaches Dream Download Manager:
//   1. Catching: when the browser starts a download, we cancel it at once and
//      hand the link to Dream Download Manager. If Dream Download Manager can't take it, we start the
//      download in the browser again, so it's never lost.
//   2. The right-click menu: "Download with Dream Download Manager" on any link.
//
// Links go to dlm-host.exe through "native messaging" (the browser starts
// that program for each message); dlm-host passes them to the app.

const HOST = "com.dlmanager.host";

// ---- Settings ----

async function catching() {
  const { enabled = true } = await chrome.storage.local.get("enabled");
  return enabled;
}

// ---- Talking to Dream Download Manager ----

async function send(message) {
  try {
    return await chrome.runtime.sendNativeMessage(HOST, message);
  } catch (err) {
    // "Specified native messaging host not found" (dlm-host isn't installed)
    // or "...forbidden" (installed for a different extension ID) mean setup is
    // needed; anything else is a real error.
    const error = String(err && err.message ? err.message : err);
    return { ok: false, error, noHost: /not found|forbidden/i.test(error) };
  }
}

// The cookies the browser would send to this address, as one header value.
// They let Dream Download Manager download files that need you to be logged in.
async function cookiesFor(url) {
  try {
    const list = await chrome.cookies.getAll({ url });
    return list.map((c) => `${c.name}=${c.value}`).join("; ");
  } catch {
    return "";
  }
}

async function sendLink(url, { referrer = "", filename = "" } = {}) {
  return send({
    type: "add",
    url,
    referrer,
    filename,
    cookies: await cookiesFor(url),
    userAgent: navigator.userAgent,
  });
}

function notify(title, message) {
  chrome.notifications.create({
    type: "basic",
    iconUrl: "icons/icon128.png",
    title,
    message,
  });
}

// Tell the user about a missing dlm-host once per browser session, not on
// every download.
let toldAboutSetup = false;
function reportFailure(reply, what) {
  if (reply.noHost) {
    if (!toldAboutSetup) {
      toldAboutSetup = true;
      notify("Dream Download Manager isn't set up", "The browser can't find dlm-host. Run the setup step, then try again. Downloads stay in the browser until then.");
    }
    return;
  }
  notify("Dream Download Manager couldn't take it", `${what}: ${reply.error || "unknown error"}`);
}

const isWebLink = (url) => /^https?:\/\//i.test(url || "");
const baseName = (path) => (path || "").split(/[\\/]/).pop();

// ---- 1. Catching the browser's downloads ----

chrome.downloads.onCreated.addListener(async (item) => {
  const url = item.finalUrl || item.url;
  if (!isWebLink(url)) return;        // blob:, data:, file: - only the browser has those
  if (item.incognito) return;          // private windows stay private
  if (item.state !== "in_progress") return;
  if (item.byExtensionId) return;      // started by an extension (maybe on purpose)
  if (!(await catching())) return;

  // Stop the browser's download straight away, and remove it from the
  // browser's list - Dream Download Manager does the downloading, not both. (Waiting for
  // Dream Download Manager to answer first would leave the browser's copy sitting there
  // while Dream Download Manager starts up.)
  await chrome.downloads.cancel(item.id).catch(() => {});
  await chrome.downloads.erase({ id: item.id }).catch(() => {});

  const reply = await sendLink(url, {
    referrer: item.referrer || "",
    filename: baseName(item.filename),
  });
  if (reply && reply.ok) return;

  if (reply && !reply.noHost && /already in your list/.test(reply.error || "")) {
    notify("Already in Dream Download Manager", "That link is already in your list.");
    return;
  }
  // Dream Download Manager couldn't take it (not set up, or an error): hand the download
  // back to the browser, so it's never lost. Started by this extension, so
  // the check at the top lets it through instead of catching it again.
  chrome.downloads.download({ url: item.url }).catch(() => {});
  reportFailure(reply || {}, "Download");
});

// ---- 2. The right-click menu ----

// Video sites where "Download video" is offered on the page itself. (The
// popup's button works on any site yt-dlp knows.)
const VIDEO_SITES = [
  "*://*.youtube.com/*", "*://youtu.be/*", "*://*.vimeo.com/*", "*://*.dailymotion.com/*",
  "*://*.facebook.com/*", "*://*.instagram.com/*", "*://*.tiktok.com/*", "*://x.com/*",
  "*://twitter.com/*", "*://*.twitch.tv/*", "*://*.reddit.com/*", "*://*.bilibili.com/*",
];

chrome.runtime.onInstalled.addListener(async () => {
  // On an update the old entries may still be there: start clean.
  await chrome.contextMenus.removeAll();
  chrome.contextMenus.create({
    id: "dlmanager-link",
    title: "Download with Dream Download Manager",
    contexts: ["link", "video", "audio"],
  });
  chrome.contextMenus.create({
    id: "dlmanager-page-video",
    title: "Download this video with Dream Download Manager",
    contexts: ["page", "video"],
    documentUrlPatterns: VIDEO_SITES,
  });
});

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId === "dlmanager-page-video") {
    const reply = await sendVideoPage(info.pageUrl || (tab && tab.url));
    if (!reply || !reply.ok) reportFailure(reply || {}, "Download video");
    return;
  }
  // A link to a video page (e.g. a YouTube video in a list) also works: the
  // app recognises it and opens its quality picker.
  const url = info.linkUrl || info.srcUrl;
  if (!isWebLink(url)) {
    // e.g. a video playing from a "blob:" address: the page builds it piece
    // by piece, so there's no single file to download (video sites come later).
    notify("Can't download that", "This isn't an ordinary web link.");
    return;
  }
  const reply = await sendLink(url, { referrer: info.pageUrl || (tab && tab.url) || "" });
  if (!reply || !reply.ok) reportFailure(reply || {}, "Download with Dream Download Manager");
});

// Ask Dream Download Manager to show its quality picker for a video page. source: the
// file the page's player plays, if ordinary - a fallback for sites yt-dlp
// doesn't know.
async function sendVideoPage(url, source = "", extra = {}) {
  if (!isWebLink(url)) return { ok: false, error: "This page isn't a web page Dream Download Manager can read." };
  // Your cookies for the site go along: some sites (Instagram, for one)
  // only show videos to logged-in users. Dream Download Manager hands them to yt-dlp
  // for this page only, and never shows them anywhere.
  return send({
    type: "video",
    url,
    source: isWebLink(source) ? source : "",
    cookies: await cookiesFor(url),
    userAgent: navigator.userAgent,
    ...extra,
  });
}

// ---- Messages from the popup ----

chrome.runtime.onMessage.addListener((msg, _sender, respond) => {
  if (msg && (msg.type === "ping" || msg.type === "open")) {
    send({ type: msg.type }).then(respond);
    return true; // we'll answer asynchronously
  }
  if (msg && msg.type === "video") {
    // From the popup, or from the button on a video (content.js). For the
    // button, the tab's title helps name videos in embedded players (their
    // own page is often just called "Player").
    const extra = {};
    if (_sender.tab) {
      extra.title = _sender.tab.title || "";
      extra.embedded = !!msg.embedded;
      extra.notLoaded = msg.loaded === false;
    }
    sendVideoPage(msg.url, msg.source, extra).then((reply) => {
      if (!reply || !reply.ok) reportFailure(reply || {}, "Download video");
      respond(reply);
    });
    return true;
  }
});
