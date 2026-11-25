// The node release the download block offers (src/components/NodeDownload.astro, on /node/ and
// /node/windows/). After a release is published, set its version and the installer's size in
// bytes here (docs/RELEASING.md).
import { domain } from './site';

export const repoURL = 'https://github.com/aisathome/aisathome';
const latest = `${repoURL}/releases/latest/download`;

export const nodeRelease = {
	version: 'v0.7.0',
	installerBytes: 390_380_154, // AIsAtHome-node-setup.exe (v0.7.0)
	// true from the first release that carries the installer under a name without the version too,
	// AIsAtHome-node-setup.exe (release.yml adds it): the button then links to
	// releases/latest/download, which follows every release with no change here. v0.5.0 does not
	// have it, so until then the button links to that release's versioned file.
	versionFreeInstaller: true,
};

export const installerURL = nodeRelease.versionFreeInstaller
	? `${latest}/AIsAtHome-node-setup.exe`
	: `${repoURL}/releases/download/${nodeRelease.version}/AIsAtHome-node-setup-${nodeRelease.version}.exe`;

// As GitHub and Windows show sizes (MiB, written MB).
export const installerSize = `${Math.round(nodeRelease.installerBytes / 2 ** 20)} MB`;

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
// cards and the reasons: /node/#what-you-need. Driver minimums follow the release's CUDA build.
export const needs = {
	linuxDriver: '525.60.13',
	linuxDriverRTX50: '570.26',
	windowsDriver: '527.41',
	windowsDriverRTX50: '570.65',
	diskGB: 17,
	modelGB: '13-14',
	bundle: 'about 0.5 GB',
};
