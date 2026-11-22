// The node release the download block offers (src/components/NodeDownload.astro, on /node/ and
// /node/windows/). After a release is published, set its version and the installer's size in
// bytes here (docs/RELEASING.md).
export const repoURL = 'https://github.com/aisathome/aisathome';
const latest = `${repoURL}/releases/latest/download`;

export const nodeRelease = {
	version: 'v0.5.0',
	installerBytes: 385_703_228, // AIsAtHome-node-setup-v0.5.0.exe
	// true from the first release that carries the installer under a name without the version too,
	// AIsAtHome-node-setup.exe (release.yml adds it): the button then links to
	// releases/latest/download, which follows every release with no change here. v0.5.0 does not
	// have it, so until then the button links to that release's versioned file.
	versionFreeInstaller: false,
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
export const linuxCommand = [
	`u=${latest}`,
	`curl -fLO $u/${bundle} && curl -fLO $u/SHA256SUMS \\`,
	`  && ${linuxBundleCheck} \\`,
	`  && tar -xJf ${bundle} \\`,
	'  && cd aisathome-node-bundle-linux-amd64 && sudo ./install.sh',
].join('\n');
