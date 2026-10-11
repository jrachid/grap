// Script de page de l'extension grab : détecte les lecteurs vidéo sur la page,
// colle un bouton au-dessus de chacun, et envoie son adresse au fond de l'extension au clic.

"use strict";

const TRACKED = new WeakSet();
const ITEMS = []; // { btn, el, anchor }

function directURL(video) {
	if (video.src && /^https?:/.test(video.src)) return video.src;
	if (video.currentSrc && /^https?:/.test(video.currentSrc)) return video.currentSrc;
	for (const source of video.querySelectorAll("source")) {
		if (source.src && /^https?:/.test(source.src)) return source.src;
	}
	return ""; // blob: / MediaSource : pas de fichier direct, pas encore pris en charge
}

// place colle le bouton au-dessus du lecteur, et le masque si le lecteur est trop petit.
function place(item) {
	const rect = item.el.getBoundingClientRect();
	if (rect.width < 60 || rect.height < 60) {
		item.btn.style.display = "none";
		return;
	}
	item.btn.style.display = "";
	const left = Math.max(0, rect.left + window.scrollX + 8);
	const top = Math.max(0, rect.top + window.scrollY - 30);
	item.btn.style.left = left + "px";
	item.btn.style.top = top + "px";
}

function nameHint(url) {
	try {
		const last = new URL(url).pathname.split("/").pop();
		const decoded = decodeURIComponent(last);
		return decoded || undefined;
	} catch {
		return undefined;
	}
}

function track(video) {
	if (TRACKED.has(video)) return;
	TRACKED.add(video);

	const url = directURL(video);
	const btn = document.createElement("button");
	btn.className = "grab-btn";
	btn.type = "button";

	if (url) {
		btn.textContent = "⤓ grab";
		btn.title = "Télécharger avec grab";
		btn.addEventListener("click", async (event) => {
			event.preventDefault();
			event.stopPropagation();
			btn.disabled = true;
			btn.textContent = "⋯ grab";
			const res = await new Promise((done) =>
				chrome.runtime.sendMessage({ type: "grab:add", url, name: nameHint(url) }, (r) => done(r))
			);
			if (res && res.ok) {
				btn.textContent = "✓ grab #" + (res.id || "").slice(0, 4);
				setTimeout(() => {
					btn.textContent = "⤓ grab";
					btn.disabled = false;
				}, 2400);
			} else {
				btn.textContent = "✗ " + (res && res.error ? res.error : "échec");
				btn.disabled = false;
			}
		});
	} else {
		btn.textContent = "⤓ grab";
		btn.disabled = true;
		btn.title = "Le lecteur sert la vidéo par petites tranches (flux protégé) : pas encore pris en charge";
	}

	document.documentElement.appendChild(btn);
	const item = { btn, el: video };
	ITEMS.push(item);
	requestAnimationFrame(() => place(item));
}

// scan repère les nouveaux lecteurs, réaligne les boutons, et abandonne les morts.
function scan() {
	for (const video of document.querySelectorAll("video")) {
		track(video);
	}
	for (let i = ITEMS.length - 1; i >= 0; i--) {
		const item = ITEMS[i];
		if (!item.el.isConnected) {
			item.btn.remove();
			ITEMS.splice(i, 1);
			continue;
		}
		if (document.hidden) continue;
		place(item);
	}
}

setInterval(scan, 1200);
window.addEventListener("resize", scan, { passive: true });
window.addEventListener("scroll", scan, { passive: true, capture: true });
scan();
