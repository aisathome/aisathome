// Setup page: polls /api/status (which carries the setup's state) and drives the setup steps.
// Text from the agent and the gateway is only ever set as text, never as HTML. The token is
// sent once, in the body of a POST, and never read back.
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

	function connection(s) {
		if (s.connected) return [`Node ${s.node_id} is connected to ${s.gateway}. It serves requests from the network now.`, false];
		if (s.rejected) return [`The gateway refused this node: ${s.rejected}. Check the token on your account page and paste it again below.`, true];
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
		// 4. token
		const ready = dlOK && !!sp.passed;
		step('step-token', false, ready);
		$('start').disabled = !!busy || !ready;
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
	$('again').addEventListener('click', async () => {
		if (!window.confirm('Run the setup again? The node stops serving and its engine stops until you start it again.')) return;
		await action('/api/setup/enter', 'running-msg');
	});
	$('leave').addEventListener('click', () => action('/api/setup/leave', 'start-msg'));

	document.addEventListener('visibilitychange', () => document.visibilityState === 'visible' && refresh());
	refresh();
})();
