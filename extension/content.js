// Dream Download Manager - the "Download" button on videos.
//
// Runs in every page (and every frame in it, so it also reaches videos
// embedded from YouTube and the like). It sends nothing anywhere until you
// click the button.
//
// The difference from IDM: IDM watches the network and offers every video
// stream that loads - which is why it so often offers the ad. We don't look
// at the network at all. The button sends the page's address to Dream Download Manager,
// and yt-dlp finds the page's real video (it skips ads by design). Only if
// yt-dlp doesn't know the site do we fall back to the file the player plays.
//
// Everything here is plain DOM code: no libraries, nothing loaded from
// outside, and the button lives in a "shadow root" so the page's own styles
// can't change it (and ours can't change the page).

(() => {
  // Frames that serve ads: no button inside them at all.
  const AD_HOSTS = [
    "doubleclick.net", "googlesyndication.com", "googleadservices.com", "imasdk.googleapis.com",
    "adnxs.com", "amazon-adsystem.com", "taboola.com", "outbrain.com", "criteo.com", "criteo.net",
    "adsrvr.org", "moatads.com", "teads.tv", "spotxchange.com", "springserve.com", "innovid.com",
    "vidoomy.com", "smartadserver.com", "pubmatic.com", "rubiconproject.com", "yieldmo.com",
  ];
  const host = location.hostname;
  if (AD_HOSTS.some((h) => host === h || host.endsWith("." + h))) return;
  if (!/^https?:$/.test(location.protocol)) return;

  // A video smaller than this is a thumbnail, a preview loop or an ad slot.
  const MIN_WIDTH = 400;
  const MIN_HEIGHT = 225;
  // A short clip is skipped when a longer video is on the page (e.g. a
  // 15-second ad playing next to the real thing).
  const SHORT = 30;

  let enabled = true;
  let hiddenHere = false;
  chrome.storage.local.get(["videoButton", "hiddenSites"]).then(({ videoButton = true, hiddenSites = [] }) => {
    enabled = videoButton;
    hiddenHere = hiddenSites.includes(siteKey());
  });
  chrome.storage.onChanged.addListener((changes) => {
    if (changes.videoButton) enabled = changes.videoButton.newValue !== false;
    if (changes.hiddenSites) hiddenHere = (changes.hiddenSites.newValue || []).includes(siteKey());
    if (!enabled || hiddenHere) hide();
  });

  // The site as the user knows it: the top page's host (in an embedded
  // frame, the page that embeds it - that's where you'd hide the button).
  function siteKey() {
    try {
      return window.top.location.hostname;
    } catch {
      // A frame from another site can't read the top page's address.
      return document.referrer ? new URL(document.referrer).hostname : host;
    }
  }

  // ---- Which videos get a button ----

  function videos() {
    return Array.from(document.querySelectorAll("video"));
  }

  function eligible(v) {
    const r = v.getBoundingClientRect();
    if (r.width < MIN_WIDTH || r.height < MIN_HEIGHT) return false;
    const style = getComputedStyle(v);
    if (style.visibility === "hidden" || style.display === "none" || style.opacity === "0") return false;
    const others = videos().filter((o) => o !== v);
    // A short clip, when a longer video is on the page.
    if (isFinite(v.duration) && v.duration > 0 && v.duration < SHORT &&
        others.some((o) => isFinite(o.duration) && o.duration >= SHORT)) return false;
    // A muted video playing by itself that isn't the page's biggest:
    // a background loop or an ad.
    if (v.muted && v.autoplay && others.some((o) => area(o) > area(v))) return false;
    return true;
  }

  function area(v) {
    const r = v.getBoundingClientRect();
    return r.width * r.height;
  }

  // ---- The button ----

  let box = null;     // the shadow-root host, created on first use
  let current = null; // the video the button is on
  let hideTimer = 0;

  function makeButton() {
    box = document.createElement("dlmanager-video-button");
    box.style.cssText = "position:fixed;z-index:2147483647;top:0;left:0;display:none;";
    const root = box.attachShadow({ mode: "closed" });
    root.innerHTML = `
      <style>
        .bar { display: flex; gap: 1px; font: 600 13px/1 "Segoe UI", system-ui, sans-serif;
               filter: drop-shadow(0 2px 6px rgb(0 0 0 / .4)); }
        button { all: unset; cursor: pointer; display: flex; align-items: center; gap: 7px;
                 padding: 8px 11px; background: #1e232c; color: #e7eaf0; }
        button:hover { background: #2c3340; }
        button:focus-visible { outline: 2px solid #f2a93b; outline-offset: 1px; }
        .main { border-radius: 6px 0 0 6px; }
        .hide { border-radius: 0 6px 6px 0; padding: 8px 9px; color: #8c95a6; }
        svg { width: 15px; height: 15px; fill: #f2a93b; }
        .x { width: 12px; height: 12px; fill: currentColor; }
      </style>
      <div class="bar">
        <button class="main" title="Download this video with Dream Download Manager">
          <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M11 3h2v9.2l3.6-3.6 1.4 1.4-6 6-6-6 1.4-1.4 3.6 3.6zM5 19h14v2H5z"/></svg>
          Download
        </button>
        <button class="hide" title="Don't show this button on this site" aria-label="Don't show on this site">
          <svg class="x" viewBox="0 0 24 24" aria-hidden="true"><path d="M6.4 5 5 6.4 10.6 12 5 17.6 6.4 19l5.6-5.6 5.6 5.6 1.4-1.4-5.6-5.6L19 6.4 17.6 5 12 10.6z"/></svg>
        </button>
      </div>`;
    root.querySelector(".main").addEventListener("click", (e) => {
      e.preventDefault();
      e.stopPropagation();
      download();
    });
    root.querySelector(".hide").addEventListener("click", async (e) => {
      e.preventDefault();
      e.stopPropagation();
      const { hiddenSites = [] } = await chrome.storage.local.get("hiddenSites");
      if (!hiddenSites.includes(siteKey())) hiddenSites.push(siteKey());
      await chrome.storage.local.set({ hiddenSites });
      hide();
    });
    // Moving onto the button must not count as leaving the video.
    box.addEventListener("mouseenter", () => clearTimeout(hideTimer));
    box.addEventListener("mouseleave", scheduleHide);
    document.documentElement.appendChild(box);
  }

  function show(v) {
    if (!enabled || hiddenHere || !eligible(v)) return;
    if (!box) makeButton();
    current = v;
    clearTimeout(hideTimer);
    // Visible first: a hidden element has no width to position it by.
    box.style.visibility = "hidden";
    box.style.display = "block";
    place();
    box.style.visibility = "visible";
  }

  // Top right corner of the video, kept inside the window.
  function place() {
    if (!box || !current) return;
    const r = current.getBoundingClientRect();
    if (r.bottom < 0 || r.top > innerHeight || !current.isConnected) return hide();
    const top = Math.max(8, r.top + 10);
    const right = Math.min(innerWidth - 8, r.right - 10);
    box.style.top = `${top}px`;
    box.style.left = `${Math.max(8, right - box.offsetWidth)}px`;
  }

  function hide() {
    if (box) box.style.display = "none";
    current = null;
  }

  function scheduleHide() {
    clearTimeout(hideTimer);
    hideTimer = setTimeout(hide, 1500);
  }

  // Video players cover the <video> with their own controls, so "the mouse
  // is over the video" is worked out from coordinates, not from which
  // element is under the mouse.
  let lastMove = 0;
  document.addEventListener("mousemove", (e) => {
    const now = Date.now();
    if (now - lastMove < 100) return; // at most 10 checks a second
    lastMove = now;
    const v = videos().find((o) => {
      const r = o.getBoundingClientRect();
      return e.clientX >= r.left && e.clientX <= r.right && e.clientY >= r.top && e.clientY <= r.bottom;
    });
    if (v) show(v);
    else if (current) scheduleHide();
  }, { passive: true });
  addEventListener("scroll", place, { passive: true, capture: true });
  addEventListener("resize", place, { passive: true });
  document.addEventListener("fullscreenchange", hide);

  // ---- Clicking it ----

  // Addresses of a single video or post: /reel/..., /status/123, /watch?v=...
  const VIDEO_PAGE = /\/(reels?|p|tv)\/[\w-]+|\/status\/\d+|\/videos?\/|\/watch\b|\/shorts\/|\/comments\/|\/pin\/\d+|\/video\//;

  // On a feed (Instagram, X, Reddit...) the page's address is the feed, not
  // the video. Look for the post's own link around the video: go up from
  // the video until a container has a link to a single post - but stop
  // before a container that holds other videos too (that's the whole feed).
  function permalink(v) {
    let el = v.parentElement;
    for (let depth = 0; el && el !== document.body && depth < 15; depth++, el = el.parentElement) {
      if (el.querySelectorAll("video").length > 1) break;
      for (const a of el.querySelectorAll("a[href]")) {
        if (a.host === location.host && VIDEO_PAGE.test(a.pathname + a.search)) return a.href;
      }
      if (el.tagName === "ARTICLE") break; // a post ends here
    }
    return "";
  }

  function download() {
    const v = current;
    // The page to ask yt-dlp about: this frame's address if it's a single
    // video's page (in an embedded player, youtube.com/embed/..., it is);
    // on a feed, the post's own link.
    let page = location.href;
    if (v && !VIDEO_PAGE.test(location.pathname + location.search)) page = permalink(v) || page;
    // The file the player plays, as a fallback - only if it's an ordinary
    // web address (a "blob:" is built by the page; only the page has it).
    const src = v && /^https?:/.test(v.currentSrc) ? v.currentSrc : "";
    chrome.runtime.sendMessage({
      type: "video",
      url: page,
      source: src,
      embedded: window !== window.top, // in a player embedded from another site
      loaded: !!(v && (v.currentSrc || v.readyState > 0)), // has the player loaded anything yet?
    });
    hide();
  }
})();
