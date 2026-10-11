// Fond de l'extension grab : reçoit les clics, parle au récepteur local de grab (127.0.0.1),
// et tient le menu contextuel « Télécharger avec grab » sur les liens.

const MENU_ID = "grab-download";
const DEFAULT_SERVER = "http://127.0.0.1:48113";

chrome.runtime.onInstalled.addListener(() => {
	chrome.contextMenus.create({ id: MENU_ID, title: "Télécharger avec grab", contexts: ["link"] });
});

async function serverURL() {
	const { server } = await chrome.storage.local.get({ server: "" });
	return server || DEFAULT_SERVER;
}

let badgeTimer;
function badge(text, color) {
	chrome.action.setBadgeBackgroundColor({ color });
	chrome.action.setBadgeText({ text });
	clearTimeout(badgeTimer);
	badgeTimer = setTimeout(() => chrome.action.setBadgeText({ text: "" }), 1800);
}

// addToGrab envoie l'adresse au récepteur local de grab ; renvoie {ok, id ou error}.
async function addToGrab(url, name) {
	const { token } = await chrome.storage.local.get({ token: "" });
	if (!token) {
		badge("?", "#c0392b");
		console.warn("grab : jeton absent — ouvre la pop-up et colle celui de `grab serve`");
		return { ok: false, error: "jeton absent" };
	}
	const base = await serverURL();
	try {
		const res = await fetch(base + "/add", {
			method: "POST",
			headers: { "Content-Type": "application/json", "X-Grab-Token": token },
			body: JSON.stringify({
				url,
				name,
			}),
		});
		const data = await res.json().catch(() => ({}));
		if (!res.ok) {
			badge("!", "#c0392b");
			return { ok: false, error: data.error || "HTTP " + res.status };
		}
		badge("✓", "#1e8f5e");
		return { ok: true, id: data.id };
	} catch {
		badge("!", "#c0392b");
		return { ok: false, error: "serveur absent — lance `grab serve`" };
	}
}

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
	if (msg && msg.type === "grab:add" && msg.url) {
		addToGrab(msg.url, msg.name).then(sendResponse);
		return true; // réponse asynchrone
	}
});

chrome.contextMenus.onClicked.addListener((info, tab) => {
	if (info.menuItemId === MENU_ID && info.linkUrl) {
		addToGrab(info.linkUrl, fileNameHint(info.linkText, info.linkUrl));
	}
});

// fileNameHint : un nom humain si le lien en propose un, sinon rien (le serveur déduira de l'adresse).
function fileNameHint(linkText, linkUrl) {
	if (linkText) {
		const clean = ("" + linkText).trim().replace(/\s+/g, " ").slice(0, 60);
		if (clean) return clean;
	}
	try {
		const last = new URL(linkUrl).pathname.split("/").pop();
		return decodeURIComponent(last) || undefined;
	} catch {
		return undefined;
	}
}
