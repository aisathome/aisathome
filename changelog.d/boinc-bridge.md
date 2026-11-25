---
section: Added
---

- **The BOINC bridge, `aisathome-bridge`.** It runs on a BOINC project's host as the gateway's
  connector: it makes a work unit for each pilot the gateway wants (the credential as an input file,
  never on the command line), cancels unsent ones the gateway no longer wants, tells the gateway who
  runs each and how it ended, writes each pilot's final credit for the project's validator, and
  carries out "Run on BOINC" and claims with BOINC's stock account RPCs. It reads the project's
  database and never writes it. See `boinc/README.md`. (gateway)
