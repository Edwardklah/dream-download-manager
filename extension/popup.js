// The popup: shows whether Dream Download Manager can be reached, and the on/off switch.

const statusEl = document.getElementById("status");
const enabledEl = document.getElementById("enabled");

function show(text, kind) {
  statusEl.textContent = text;
  statusEl.className = "status " + kind;
}

chrome.storage.local.get("enabled").then(({ enabled = true }) => {
  enabledEl.checked = enabled;
});
enabledEl.addEventListener("change", () => {
  chrome.storage.local.set({ enabled: enabledEl.checked });
});

chrome.runtime.sendMessage({ type: "ping" }).then((reply) => {
  if (reply && reply.ok) {
    show("Dream Download Manager is running", "ok");
  } else if (reply && reply.noHost) {
    show("Setup needed: the browser can't find dlm-host.", "bad");
  } else {
    show("Dream Download Manager isn't running. It will start when you download something.", "warn");
  }
});

document.getElementById("open").addEventListener("click", async () => {
  const reply = await chrome.runtime.sendMessage({ type: "open" });
  if (reply && reply.ok) {
    window.close();
  } else {
    show((reply && reply.error) || "Couldn't open Dream Download Manager.", "bad");
  }
});

// The video on this tab: Dream Download Manager asks yt-dlp, which knows over a thousand
// sites, and shows its quality picker.
const videoEl = document.getElementById("video");
chrome.tabs.query({ active: true, currentWindow: true }).then(([tab]) => {
  const url = tab && tab.url;
  if (!url || !/^https?:\/\//.test(url)) {
    videoEl.disabled = true;
    videoEl.title = "Not available on this page";
    return;
  }
  videoEl.addEventListener("click", async () => {
    videoEl.disabled = true;
    const reply = await chrome.runtime.sendMessage({ type: "video", url });
    if (reply && reply.ok) {
      window.close();
    } else {
      videoEl.disabled = false;
      show((reply && reply.error) || "Couldn't reach Dream Download Manager.", "bad");
    }
  });
});

// The button on videos: on/off, and sites where you hid it.
const videoButtonEl = document.getElementById("videoButton");
chrome.storage.local.get(["videoButton", "hiddenSites"]).then(({ videoButton = true, hiddenSites = [] }) => {
  videoButtonEl.checked = videoButton;
  chrome.tabs.query({ active: true, currentWindow: true }).then(([tab]) => {
    let site = "";
    try {
      site = new URL(tab.url).hostname;
    } catch {}
    if (site && hiddenSites.includes(site)) {
      document.getElementById("hiddenNote").hidden = false;
      document.getElementById("unhide").addEventListener("click", async () => {
        const { hiddenSites: list = [] } = await chrome.storage.local.get("hiddenSites");
        await chrome.storage.local.set({ hiddenSites: list.filter((h) => h !== site) });
        document.getElementById("hiddenNote").hidden = true;
      });
    }
  });
});
videoButtonEl.addEventListener("change", () => {
  chrome.storage.local.set({ videoButton: videoButtonEl.checked });
});
