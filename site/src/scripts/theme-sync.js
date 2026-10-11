// Shares the light/dark choice with the account pages (served by the gateway on the same
// origin, without scripts): the cookie aisathome_theme = auto | dark | light. Inlined in <head>
// by astro.config.mjs, before Starlight's own theme script reads localStorage, so the page paints
// in the right theme. On load the cookie wins (the account pages may have changed it); changes in
// the theme select write it. The last change wins on both. A constant script, so a future CSP
// can allow it by hash (scripts/check-dist.mjs prints it). SPDX-License-Identifier: Apache-2.0
(function () {
	var key = 'starlight-theme';
	function write(t) {
		document.cookie = 'aisathome_theme=' + t + '; Path=/; Max-Age=31536000; SameSite=Lax; Secure';
	}
	function sync() {
		try {
			var m = document.cookie.match(/(?:^|; )aisathome_theme=(auto|dark|light)(?:;|$)/);
			var stored = localStorage.getItem(key) || 'auto';
			if (!m) {
				if (stored !== 'auto') write(stored); // chosen before the cookie existed
				return false;
			}
			if (m[1] === stored) return false;
			localStorage.setItem(key, m[1] === 'auto' ? '' : m[1]);
			if (m[1] !== 'auto') document.documentElement.dataset.theme = m[1];
			return m[1];
		} catch (e) {
			return false; // no storage (private mode, blocked): Starlight falls back to the system
		}
	}
	sync();
	document.addEventListener('change', function (e) {
		var t = e.target;
		if (t && t.closest && t.closest('starlight-theme-select')) write(t.value === 'dark' || t.value === 'light' ? t.value : 'auto');
	});
	// Back to a page kept in the back/forward cache after a change on the account pages.
	addEventListener('pageshow', function (e) {
		var t = e.persisted && sync();
		if (!t) return;
		if (t === 'auto') document.documentElement.dataset.theme = matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
		if (window.StarlightThemeProvider) StarlightThemeProvider.updatePickers(t);
	});
})();
