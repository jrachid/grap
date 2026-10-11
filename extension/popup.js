// Pop-up de l'extension grab : colle le jeton du récepteur local, et vérifie qu'il répond.
"use strict";

const $ = (sel) => document.querySelector(sel);

async function load() {
	const { token, server } = await chrome.storage.local.get({ token: "", server: "" });
	$("#token").value = token;
	$("#status").textContent = server
		? "Serveur ciblé : " + server
		: "Serveur par défaut : http://127.0.0.1:48113";
}

$("#save").addEventListener("click", async () => {
	const token = $("#token").value.trim();
	await chrome.storage.local.set({ token });
	$("#status").textContent = token ? "jeton enregistré" : "jeton effacé";
});

$("#test").addEventListener("click", async () => {
	$("#status").textContent = "test en cours…";
	try {
		const res = await fetch("http://127.0.0.1:48113/ping");
		const data = await res.json();
		$("#status").textContent = data.ok ? "serveur répondu (grab " + data.version + ")" : "réponse inattendue";
	} catch {
		$("#status").textContent = "pas de réponse — lance `grab serve` dans un terminal";
	}
});

load();
