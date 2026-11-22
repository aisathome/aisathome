// Setup page: polls /api/status (which carries the setup's state) and drives the setup steps.
// Text from the agent and the gateway is only ever set as text, never as HTML. A pasted token is
// sent once, in the body of a POST, and never read back. "Link to my account" asks the agent for a
// code and opens the approval page; the token then goes from the gateway to the agent, not through
// this page.
'use strict';
(() => {
	const $ = (id) => document.getElementById(id);
	const num = new Intl.NumberFormat();
	const gb = (b) => `${(b / 1e9).toFixed(1)} GB`;
	let timer = null;
	let last = null;

	async function api(path, body) {
		const res = await fetch(path, {
			method: body === undefined ? 'GET' : 'POST',
			headers: body === undefined ? { accept: 'application/json' } : { 'content-type': 'application/json' },
			body: body === undefined ? undefined : JSON.stringify(body),
			credentials: 'same-origin',
			cache: 'no-store',
		});
		let data = {};
		try { data = await res.json(); } catch { /* empty body */ }
		return { ok: res.ok, status: res.status, data };
	}

	function msg(id, text, bad) {
		const m = $(id);
		m.textContent = text || '';
		m.classList.toggle('bad', !!bad);
	}

	function step(id, done, enabled) {
		const s = $(id);
		s.dataset.done = done ? 'true' : 'false';
		s.setAttribute('aria-disabled', enabled ? 'false' : 'true');
	}

	const httpURL = (u) => typeof u === 'string' && /^https?:\/\/[^\s/@]+(\/[^\s]*)?$/.test(u);
	const hhmm = (t) => { const d = new Date(t); return isNaN(d) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }); };

	// The link to an account: the code and the approval page while it waits; how it ended after.
	function renderLink(st, signedIn) {
		const l = st.link;
		const waiting = signedIn && !!l && l.status === 'waiting';
		$('link-panel').hidden = !waiting;
		if (waiting) {
			$('link-code').textContent = l.code || '';
			const u = httpURL(l.url) ? l.url : '';
			$('link-url').textContent = u;
			$('link-url').href = u || '#';
			$('link-until').textContent = hhmm(l.expires);
		}
		const id = st.mode === 'setup' ? 'link-msg' : 'running-msg';
		if (l && l.status === 'linked') msg(id, `Linked: this is node ${l.node_id} on your account.`);
		else if (l && l.status === 'failed') msg(id, `Not linked: ${l.error}`, true);
	}

	async function startLink(msgID) {
		// The tab is opened in the click itself (a tab opened after the answer would be blocked as a
		// pop-up) and gets the approval page once the agent has the code.
		const w = window.open('', '_blank');
		msg(msgID, 'Asking the gateway for a code...');
		const r = await api('/api/setup/link', { gateway: $('gateway').value || (last && last.setup ? last.setup.gateway : '') });
		const l = r.ok ? r.data.link : null;
		if (w) {
			if (l && httpURL(l.url)) {
				w.opener = null;
				w.location.href = l.url;
			} else {
				w.close();
			}
		}
		msg(msgID, r.ok ? '' : (r.data.error || `Failed (HTTP ${r.status})`), !r.ok);
		refresh();
	}

	function connection(s) {
		if (s.connected) return [`Node ${s.node_id} is connected to ${s.gateway}. It serves requests from the network now.`, false];
		if (s.rejected) return [`The gateway refused this node: ${s.rejected}. Link it to your account again below (the node may have been revoked).`, true];
		if (s.refused) return [`The gateway refused this node agent: ${s.refused}.`, true];
		const ep = s.engine_process;
		if (s.engine === 'stopped' || (ep && ep.state === 'stopped')) return ['The engine is stopped (the GPU is free). Start it on the status page.', false];
		if (s.engine !== 'healthy') return ['Loading the model on the card; the node connects once it answers (up to a few minutes).', false];
		return [`Connecting to ${s.gateway}...`, false];
	}

	function render(s) {
		last = s;
		const st = s.setup;
		const c = s.controls;
		if (!st) { // an agent without a setup page (Linux)
			$('lead').textContent = 'This node has no setup page; its installer sets it up.';
			return;
		}
		const signedIn = c.enabled && c.signed_in;
		$('signin').hidden = !c.enabled || c.signed_in;
		$('pw-hint').textContent = c.password_hint || '';
		if (s.account_url) { $('account').href = s.account_url; $('account2').href = s.account_url; }
		const setupMode = st.mode === 'setup';
		renderLink(st, signedIn);
		$('steps').hidden = !signedIn || !setupMode;
		$('running').hidden = !signedIn || setupMode;
		$('leave-row').hidden = !signedIn || !setupMode || !st.configured;
		if (!c.enabled) {
			$('lead').textContent = c.note || 'Controls are off.';
			return;
		}
		if (!signedIn) {
			$('lead').textContent = 'Unlock this page with the local password to set up the node.';
			return;
		}
		if (!setupMode) {
			$('lead').textContent = st.configured ? 'The node runs with its settings.' : 'Starting the node...';
			$('relink').textContent = s.anonymous ? 'Link to my account' : 'Link to another account';
			const [line, bad] = connection(s);
			$('running-line').textContent = line;
			$('running-line').classList.toggle('bad', bad);
			return;
		}
		$('lead').textContent = 'Four steps. The node stays off until the last one.';
		const busy = st.busy || '';
		// 1. card
		const card = st.card;
		const cardOK = !!card && !st.card_error;
		step('step-card', cardOK, true);
		$('card-line').textContent = card
			? `GPU ${card.index}: ${card.name}, ${num.format(card.total_mib)} MiB (${num.format(card.free_mib)} free), driver ${card.driver}` +
				(card.tier ? `. Tier: ${card.tier} (${card.tier_text}), model ${card.model}` : '') +
				(card.several ? '. The node uses this card only.' : '')
			: '';
		if (busy === 'check') msg('card-msg', 'Checking...');
		else msg('card-msg', st.card_error, !!st.card_error);
		$('check').disabled = !!busy;
		$('check').textContent = card ? 'Check again' : 'Check the card';
		// 2. download
		const d = st.download || {};
		const dlOK = cardOK && !!d.complete;
		step('step-download', dlOK, cardOK);
		$('model-path').textContent = st.model_path || '';
		const total = d.total || (card && card.model_bytes) || 0;
		$('download-bar').style.width = total > 0 ? `${Math.min(100, (100 * (d.done || 0)) / total).toFixed(1)}%` : '0';
		if (busy === 'download') {
			$('download-line').textContent = d.checking
				? `Checking the part already downloaded: ${gb(d.done || 0)} of ${gb(total)}`
				: `${gb(d.done || 0)} of ${gb(total)}`;
		} else if (d.complete) {
			$('download-line').textContent = `Downloaded and checked (${gb(total)}).`;
		} else if (cardOK && total > 0 && d.done > 0) {
			$('download-line').textContent = `${gb(d.done)} of ${gb(total)} so far.`;
		} else {
			$('download-line').textContent = '';
		}
		msg('download-msg', d.error, !!d.error);
		$('download').disabled = !!busy || !cardOK;
		$('download').textContent = d.complete ? 'Check the file again' : (d.done > 0 ? 'Continue the download' : 'Download');
		$('cancel-download').hidden = busy !== 'download';
		// 3. speed
		const sp = st.speed || {};
		$('min-tps').textContent = sp.min_tps;
		step('step-speed', dlOK && !!sp.passed, dlOK);
		if (busy === 'speed') {
			$('speed-line').textContent = sp.phase === 'measuring' ? 'Measuring...' : 'Loading the model on the card...';
		} else if (sp.tps) {
			$('speed-line').textContent = `${sp.tps.toFixed(1)} tokens/s after a ${num.format(sp.prompt_tokens)}-token prompt.`;
		} else {
			$('speed-line').textContent = '';
		}
		msg('speed-msg', sp.error, !!sp.error);
		$('speed').disabled = !!busy || !dlOK;
		$('cancel-speed').hidden = busy !== 'speed';
		// 4. account: link (or a pasted token)
		const ready = dlOK && !!sp.passed;
		step('step-token', false, ready);
		$('start').disabled = !!busy || !ready;
		$('link').disabled = !!busy || !ready || (st.link && st.link.status === 'waiting');
		$('anon-start').disabled = !!busy || !ready || !$('anon-terms').checked;
		if (st.start_error) msg('start-msg', st.start_error, true);
		if (!$('gateway').value) $('gateway').value = st.gateway || '';
	}

	async function refresh() {
		clearTimeout(timer);
		try {
			const r = await api('/api/status');
			if (!r.ok) throw new Error(`HTTP ${r.status}`);
			render(r.data);
		} catch {
			const cmd = last && last.controls ? last.controls.status_command : '';
			$('lead').textContent = `The node agent does not answer. It may be restarting; this page keeps trying.${cmd ? ` Check: ${cmd}` : ''}`;
		}
		const busy = last && last.setup && last.setup.busy;
		timer = setTimeout(refresh, busy ? 700 : (document.visibilityState === 'visible' ? 2000 : 15000));
	}

	async function action(path, msgID, body) {
		const r = await api(path, body === undefined ? {} : body);
		if (!r.ok) msg(msgID, r.data.error || `Failed (HTTP ${r.status})`, true);
		refresh();
		return r;
	}

	$('login').addEventListener('submit', async (ev) => {
		ev.preventDefault();
		const r = await api('/api/login', { password: $('password').value });
		if (r.ok) {
			$('password').value = '';
			msg('login-msg', '');
		} else {
			msg('login-msg', r.data.error || `Failed (HTTP ${r.status})`, true);
		}
		refresh();
	});
	$('check').addEventListener('click', () => action('/api/setup/check', 'card-msg'));
	$('download').addEventListener('click', () => action('/api/setup/download', 'download-msg'));
	$('cancel-download').addEventListener('click', () => action('/api/setup/cancel', 'download-msg'));
	$('speed').addEventListener('click', () => action('/api/setup/speed', 'speed-msg'));
	$('cancel-speed').addEventListener('click', () => action('/api/setup/cancel', 'speed-msg'));
	$('start-form').addEventListener('submit', async (ev) => {
		ev.preventDefault();
		$('start').disabled = true;
		const r = await action('/api/setup/start', 'start-msg', { token: $('token').value, gateway: $('gateway').value });
		if (r.ok) {
			$('token').value = '';
			msg('start-msg', '');
		}
	});
	$('token-form').addEventListener('submit', async (ev) => {
		ev.preventDefault();
		const gw = last && last.setup ? last.setup.gateway : '';
		const r = await action('/api/setup/token', 'running-msg', { token: $('new-token').value, gateway: gw });
		if (r.ok) {
			$('new-token').value = '';
			msg('running-msg', 'Saved. The node reconnects with the new token.');
		}
	});
	$('link').addEventListener('click', () => startLink('link-msg'));
	$('anon-terms').addEventListener('change', () => last && render(last));
	$('anon-start').addEventListener('click', async () => {
		$('anon-start').disabled = true;
		msg('anon-msg', 'Registering this node with the gateway...');
		const r = await action('/api/setup/anonymous', 'anon-msg', { gateway: $('gateway').value, accept_terms: $('anon-terms').checked });
		if (r.ok) msg('anon-msg', 'Started without an account. It answers a few test prompts first and gets science work once they pass.');
	});
	$('relink').addEventListener('click', () => startLink('running-msg'));
	$('link-cancel').addEventListener('click', () => action('/api/setup/link/cancel', 'link-msg'));
	$('again').addEventListener('click', async () => {
		if (!window.confirm('Run the setup again? The node stops serving and its engine stops until you start it again.')) return;
		await action('/api/setup/enter', 'running-msg');
	});
	$('leave').addEventListener('click', () => action('/api/setup/leave', 'start-msg'));

	document.addEventListener('visibilitychange', () => document.visibilityState === 'visible' && refresh());
	refresh();
})();
