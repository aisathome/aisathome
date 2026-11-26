// The node release the Windows button offers (src/components/NodeStart.astro on the home page and /node/,
// src/components/NodeDownload.astro on /node/windows/), and the JSON-LD's softwareVersion (src/data/seo.ts).
//
// In the browser those blocks show the gateway's GET /v1/node/release instead (updateRelease below): the gateway reads
// GitHub's latest release every 10 minutes (docs/gateway.md, "The latest node release"), so a new release shows within
// minutes with no change here. `nodeRelease` is the build's copy: the page without JavaScript, the JSON-LD, and the
// blocks whenever the gateway does not answer or knows only an older release. Refresh it whenever convenient with the
// values /v1/node/release answers (docs/RELEASING.md). Everything else in this file (the driver minimums, disk and
// model sizes in `needs`, the commands) a release does not carry: kept by hand.
import { domain } from './site';

export const repoURL = 'https://github.com/aisathome/aisathome';
const latest = `${repoURL}/releases/latest/download`;

export interface NodeRelease {
	version: string;
	/** AIsAtHome-node-setup.exe (or the versioned name when the release has no version-free one) */
	installerBytes: number;
	/** aisathome-node-bundle-linux-amd64.tar.xz */
	bundleBytes: number;
	/** The release carries the installer under a name without the version too, AIsAtHome-node-setup.exe (release.yml
	 * adds it): the button links to releases/latest/download. Otherwise to the release's versioned file. */
	versionFreeInstaller: boolean;
}

export const nodeRelease: NodeRelease = {
	version: 'v0.7.0',
	installerBytes: 390_380_154,
	bundleBytes: 478_190_808,
	versionFreeInstaller: true,
};

export const installerURLOf = (r: NodeRelease) =>
	r.versionFreeInstaller
		? `${latest}/AIsAtHome-node-setup.exe`
		: `${repoURL}/releases/download/${r.version}/AIsAtHome-node-setup-${r.version}.exe`;
// As GitHub and Windows show sizes (MiB, written MB).
export const installerSizeOf = (bytes: number) => `${Math.round(bytes / 2 ** 20)} MB`;
// A rough figure, as the docs give it.
export const bundleSizeOf = (bytes: number) => `about ${(bytes / 1e9).toFixed(1)} GB`;

/** A release's installer by its own tag (releases/download/<tag>/…), the file the gateway checked: the version-free
 * name when the release has it, else the versioned one. The live block links this, so the version and size it shows
 * are those of the file it links even while releases/latest already moved on; the build's copy keeps the moving
 * releases/latest/download link (installerURLOf), right for every release after it too. */
export const liveInstallerURL = (r: NodeRelease) =>
	`${repoURL}/releases/download/${r.version}/${r.versionFreeInstaller ? 'AIsAtHome-node-setup.exe' : `AIsAtHome-node-setup-${r.version}.exe`}`;

export const installerURL = installerURLOf(nodeRelease);
export const installerSize = installerSizeOf(nodeRelease.installerBytes);

// ---- live: the gateway's latest release, in the browser

export const RELEASE_URL = '/v1/node/release';
// A stable release tag, as the gateway checks it (gateway/noderelease.go).
const TAG = /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/;

const versionKey = (v: string) => TAG.exec(v)!.slice(1).map(Number);
/** a < b, both release tags */
const older = (a: string, b: string) => {
	const [x, y] = [versionKey(a), versionKey(b)];
	const i = x.findIndex((n, k) => n !== y[k]);
	return i >= 0 && x[i] < y[i];
};

/** The gateway's answer as a release the block may show, or null: a malformed answer, or a release older than the
 * build's copy. Deliberate: an older answer is far more likely a gateway that has not read GitHub since the build (it
 * keeps its last good release while GitHub fails) than a release taken back. In that rare case (an older release made
 * latest by hand) the page keeps naming the build's release, whose button links releases/latest/download and so serves
 * the older file under the newer name until the site is rebuilt; a live release is always linked by its own tag
 * (liveInstallerURL), so its version, size and file match. */
export function liveRelease(d: unknown, build: NodeRelease = nodeRelease): NodeRelease | null {
	if (!d || typeof d !== 'object') return null;
	const { version, installerBytes, bundleBytes, versionFreeInstaller } = d as Record<string, unknown>;
	const size = (n: unknown): n is number => typeof n === 'number' && Number.isSafeInteger(n) && n > 0;
	if (typeof version !== 'string' || !TAG.test(version) || !size(installerBytes) || !size(bundleBytes)) return null;
	if (typeof versionFreeInstaller !== 'boolean' || older(version, build.version)) return null;
	return { version, installerBytes, bundleBytes, versionFreeInstaller };
}

/** Shows r in the release's elements under root: [data-release="version" | "installer-size" | "bundle-size"] and the
 * Windows button ([data-dl-windows]), linked to r's own file (liveInstallerURL). */
export function showRelease(root: ParentNode, r: NodeRelease): void {
	const text: Record<string, string> = { version: r.version, 'installer-size': installerSizeOf(r.installerBytes), 'bundle-size': bundleSizeOf(r.bundleBytes) };
	root.querySelectorAll<HTMLElement>('[data-release]').forEach((el) => {
		const t = text[el.dataset.release ?? ''];
		if (t) el.textContent = t;
	});
	root.querySelectorAll<HTMLAnchorElement>('a[data-dl-windows]').forEach((a) => (a.href = liveInstallerURL(r)));
}

/** Reads the gateway's latest release and shows it under root, once; the page keeps the build's copy when the gateway
 * does not answer within 8 s, answers an error, or the release is not one to show. */
export async function updateRelease(root: ParentNode = document): Promise<NodeRelease | null> {
	if (!window.fetch || !root.querySelector('[data-release]')) return null;
	const ctl = new AbortController();
	const timer = setTimeout(() => ctl.abort(), 8000);
	try {
		const res = await fetch(RELEASE_URL, { signal: ctl.signal, headers: { accept: 'application/json' } });
		if (!res.ok) return null;
		const r = liveRelease(await res.json());
		if (r) showRelease(root, r);
		return r;
	} catch {
		return null;
	} finally {
		clearTimeout(timer);
	}
}

// The Linux install in one paste: the prebuilt bundle of the latest release, checked against its
// SHA256SUMS, then the installer (it asks for the node token). The long form is /node/install/.
// Each download on its own with -f and every step joined by &&: a failed download stops it, and
// the check reads the bundle's own line, so it never unpacks a file that was not checked (a stale
// bundle in the directory, code review 2026-11-22). The docs' long form does the same.
const bundle = 'aisathome-node-bundle-linux-amd64.tar.xz';
const linuxBundleCheck = `grep ' ${bundle}$' SHA256SUMS | sha256sum -c -`;
const bundleThen = (script: string) =>
	[
		`u=${latest}`,
		`curl -fLO $u/${bundle} && curl -fLO $u/SHA256SUMS \\`,
		`  && ${linuxBundleCheck} \\`,
		`  && tar -xJf ${bundle} \\`,
		`  && cd aisathome-node-bundle-linux-amd64 && sudo ./${script}`,
	].join('\n');
export const linuxCommand = bundleThen('install.sh');
// Removing a node needs the bundle's uninstall.sh. node.sh unpacks the bundle in a temporary folder
// (under /tmp, which a reboot may clear), so /node/ also gives the same download, checked, ending in
// the uninstaller (it keeps the model; add --purge to remove it too).
export const linuxUninstallCommand = bundleThen('uninstall.sh');

// The one-line start without an account (science only), on the home page and /node/: site/public/node.sh,
// served by this site (a test site serves it with its own defaults, so the command follows the domain). It
// always installs the latest release, so nothing here changes per release. The script's source, to read first:
export const anonCommand = `curl -fsSL https://${domain}/node.sh | sh`;
export const nodeShSource = `${repoURL}/blob/main/site/public/node.sh`;

// What a node needs, as the home page's short list says it. The long form, with the tested
// cards and the reasons: /node/#what-you-need. Kept by hand, except `bundle`: the driver minimums follow the release's
// CUDA build (the CUDA image in release.yml), the disk and model sizes the model pin in node/packaging/linux/install.sh.
export const needs = {
	linuxDriver: '525.60.13',
	linuxDriverRTX50: '570.26',
	windowsDriver: '527.41',
	windowsDriverRTX50: '570.65',
	diskGB: 17,
	modelGB: '13-14',
	bundle: bundleSizeOf(nodeRelease.bundleBytes), // the build's copy; live in the browser
};
