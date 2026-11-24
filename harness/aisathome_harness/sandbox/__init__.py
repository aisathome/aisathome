# SPDX-License-Identifier: Apache-2.0
"""The hardened container: `sandbox.sandbox` (networks, egress, overlays, limits, the run command) and
`sandbox.egress_forward` (the forwarder that runs in its own container); `sandbox.jail`, a throwaway
container for one host-side command over untrusted text (no network, an unprivileged uid of its own)."""
