// Local node UI: polls /api/status and drives the controls. Data from the agent (and from the
// gateway, such as close reasons in events) is only ever set as text, never as HTML.
'use strict';
(() => {
	const $ = (id) => document.getElementById(id);
	const el = (tag, cls, text) => {
		const e = document.createElement(tag);
		if (cls) e.className = cls;
		if (text !== undefined) e.textContent = text;
		return e;
	};
	const num = new Intl.NumberFormat();
	const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
	const big = (n) => (n >= 100000 ? compact.format(n) : num.format(n));
	const dur = (ms) => {
		if (ms > 0 && ms < 1000) return '<1s'; // served, but less than a second in total
		const s = Math.floor(ms / 1000);
		if (s < 60) return `${s}s`;
		if (s < 3600) return `${Math.floor(s / 60)}m`;
		if (s < 172800) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
		return `${Math.floor(s / 86400)}d`;
	};
	const words = { serving: 'Serving', connected: 'Connected', paused: 'Paused', disconnected: 'Disconnected', offline: 'No agent' };
	const badKinds = new Set(['disconnected', 'refused', 'engine_unhealthy', 'request_failed']);

	let last = null;
	let timer = null;

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

	function setState(state, line) {
		$('state').dataset.state = state;
		$('state-word').textContent = words[state] || state;
		const sl = $('state-line');
		sl.replaceChildren(...line);
		document.title = `${words[state] || state} · AIs@home node`;
	}

	function stateLine(s) {
		const out = [];
		const add = (t, cls) => out.push(cls ? el('span', cls, t) : document.createTextNode(t));
		if (s.connected) {
			add('Connected to ');
			out.push(el('strong', '', s.gateway || 'the gateway'));
			if (s.connected_since) add(` for ${dur(Date.now() - new Date(s.connected_since).getTime())}`);
			add('. ');
		} else {
			add('Not connected to ', 'bad');
			out.push(el('strong', '', s.gateway || 'the gateway'));
			add('. ');
			if (s.refused) add(`The gateway refused this node agent: ${s.refused}. `, 'bad');
		}
		if (s.paused) add(s.slots_in_use > 0 ? `Paused: finishing ${s.slots_in_use} request(s), taking no new ones. ` : 'Paused: taking no new requests. ');
		if (s.engine === 'unhealthy') add('The engine does not answer its health check. ', 'bad');
		else if (s.engine === 'healthy') add('Engine healthy.');
		return out;
	}

	function counts(target, c) {
		const items = [
			['Requests', big(c.requests)],
			['Tokens in', big(c.prompt_tokens)],
			['Tokens out', big(c.completion_tokens)],
			['Time serving', dur(c.busy_ms)],
		];
		if (c.failed > 0) items.push(['Failed', big(c.failed)]);
		$(target).replaceChildren(...items.map(([k, v]) => {
			const d = el('div');
			d.append(el('dt', '', k), el('dd', '', v));
			return d;
		}));
	}

	function gpus(g) {
		const box = $('gpus');
		box.replaceChildren(...(g.gpus || []).map((x) => {
			const d = el('div', 'gpu');
			d.append(el('p', 'name', x.name));
			if (x.mem_used_mib != null && x.mem_total_mib != null && x.mem_total_mib > 0) {
				const bar = el('div', 'bar');
				const fill = el('span');
				fill.style.width = `${Math.min(100, (100 * x.mem_used_mib) / x.mem_total_mib).toFixed(1)}%`;
				bar.append(fill);
				d.append(bar);
			}
			const facts = el('p', 'facts');
			const fact = (v, unit, cls) => {
				const s = el('span', cls || '');
				s.append(el('b', '', v), document.createTextNode(unit));
				facts.append(s);
			};
			if (x.mem_used_mib != null && x.mem_total_mib != null) fact((x.mem_used_mib / 1024).toFixed(1), ` of ${(x.mem_total_mib / 1024).toFixed(1)} GiB`);
			if (x.temp_c != null) fact(x.temp_c.toFixed(0), ' °C', x.temp_c >= 83 ? 'hot' : '');
			if (x.util_pct != null) fact(x.util_pct.toFixed(0), '% busy');
			if (x.power_w != null) fact(x.power_w.toFixed(0), x.power_limit_w != null ? ` of ${x.power_limit_w.toFixed(0)} W` : ' W');
			d.append(facts);
			return d;
		}));
		$('gpu-note').textContent = g.note || '';
	}

	function events(list) {
		const ol = $('events');
		if (!list || list.length === 0) {
			ol.replaceChildren(el('li', 'empty', 'Nothing yet.'));
			return;
		}
		ol.replaceChildren(...list.slice(0, 25).map((e) => {
			const li = el('li');
			const t = new Date(e.time);
			const tm = el('time', '', t.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false }));
			tm.dateTime = t.toISOString();
			const body = el('span');
			body.append(el('span', badKinds.has(e.kind) ? 'kind bad' : 'kind', e.kind.replace(/_/g, ' ')));
			if (e.text) body.append(document.createTextNode(': '), el('span', 'text', e.text));
			if (e.count > 1) body.append(el('span', 'count', `×${e.count}`));
			li.append(tm, body);
			return li;
		}));
	}

	function controls(s) {
		const c = s.controls;
		$('controls-off').hidden = c.enabled;
		$('controls-off').textContent = c.note || '';
		$('login').hidden = !c.enabled || c.signed_in;
		$('unlocked').hidden = !c.enabled || !c.signed_in;
		const p = $('pause');
		p.textContent = s.paused ? 'Resume' : 'Pause';
		p.dataset.action = s.paused ? 'resume' : 'pause';
		$('restart').hidden = !s.can_restart;
		if (!s.can_restart) showCommand(c.restart_command);
	}

	function showCommand(cmd) {
		$('restart-cmd-text').textContent = cmd;
		$('restart-cmd').hidden = false;
	}

	function render(s) {
		last = s;
		setState(s.state, stateLine(s));
		$('node-id').textContent = s.node_id || 'node';
		$('agent-version').textContent = s.agent ? `${s.agent}${s.protocol ? ` · protocol ${s.protocol}` : ''}` : '';
		if (s.account_url) $('account').href = s.account_url;
		$('slots-used').textContent = s.slots_in_use;
		$('slots-total').textContent = s.slots_total;
		$('slots-bar').style.width = s.slots_total > 0 ? `${(100 * s.slots_in_use) / s.slots_total}%` : '0';
		$('slots-note').textContent = s.paused ? 'Paused: no new requests.' : '';
		$('models').replaceChildren(...(s.models || []).map((m) => {
			const li = el('li');
			li.append(el('span', 'name', m.name), document.createTextNode(
				`${m.slots} slot${m.slots === 1 ? '' : 's'}${m.ctx ? ` × ${num.format(m.ctx)} tokens` : ''}${m.roles && m.roles.length ? ` · ${m.roles.join(', ')}` : ''}`));
			return li;
		}));
		$('preset').textContent = s.preset ? `Preset: ${s.preset}` : '';
		gpus(s.gpu || {});
		counts('today', s.today);
		counts('total', s.total);
		events(s.events);
		controls(s);
	}

	async function refresh() {
		clearTimeout(timer);
		try {
			const r = await api('/api/status');
			if (!r.ok) throw new Error(`HTTP ${r.status}`);
			render(r.data);
		} catch {
			setState('offline', [el('span', 'bad', 'The node agent does not answer. '),
				document.createTextNode('It may be restarting; this page keeps trying. Check: systemctl status aisathome-node')]);
		}
		timer = setTimeout(refresh, document.visibilityState === 'visible' ? 2000 : 15000);
	}

	function message(id, text, bad) {
		const m = $(id);
		m.textContent = text;
		m.classList.toggle('bad', !!bad);
	}

	$('login').addEventListener('submit', async (ev) => {
		ev.preventDefault();
		const pw = $('password').value;
		const r = await api('/api/login', { password: pw });
		if (r.ok) {
			$('password').value = '';
			message('login-msg', '');
		} else {
			message('login-msg', r.data.error || `Failed (HTTP ${r.status})`, true);
		}
		refresh();
	});

	$('pause').addEventListener('click', async (ev) => {
		const b = ev.currentTarget;
		const action = b.dataset.action || 'pause';
		b.disabled = true;
		const r = await api(`/api/${action}`, {});
		b.disabled = false;
		message('control-msg', r.ok ? (action === 'pause' ? 'Paused. Requests in flight finish.' : 'Resumed.') : (r.data.error || `Failed (HTTP ${r.status})`), !r.ok);
		refresh();
	});

	$('restart').addEventListener('click', async (ev) => {
		if (last && last.slots_in_use > 0 && !window.confirm(`${last.slots_in_use} request(s) are in flight and will fail. Restart the engine anyway?`)) return;
		if (!(last && last.slots_in_use > 0) && !window.confirm('Restart llama-server? The node is away until the model is loaded again.')) return;
		const b = ev.currentTarget;
		b.disabled = true;
		const r = await api('/api/restart-engine', {});
		b.disabled = false;
		if (r.data.command) showCommand(r.data.command);
		message('control-msg', r.ok ? r.data.message : (r.data.error || `Failed (HTTP ${r.status})`), !r.ok);
		refresh();
	});

	$('logout').addEventListener('click', async () => {
		await api('/api/logout', {});
		message('control-msg', '');
		refresh();
	});

	$('copy').addEventListener('click', async () => {
		try {
			await navigator.clipboard.writeText($('restart-cmd-text').textContent);
			$('copy').textContent = 'Copied';
		} catch {
			$('copy').textContent = 'Select and copy';
		}
	});

	document.addEventListener('visibilitychange', () => document.visibilityState === 'visible' && refresh());
	refresh();
})();
