# Security

## Reporting a vulnerability

Please report it privately, not in a public issue: use **Security > Report a vulnerability** on this repository (GitHub's
private vulnerability reporting). Say what you found, how to reproduce it, and what you think it allows. We will answer, fix it,
and credit you if you wish.

Only the latest release is supported.

## What the app does, and what to look for

DiskScout starts a small HTTP server on `127.0.0.1` for its own window (and for Claude, through MCP), and can delete files. So
the things that matter most are:

- **Reaching the local server from somewhere it should not be reached**: a web page in your browser, another user on the same
  machine, another program. The server is bound to `127.0.0.1`, requires a random per-run token (cookie, `X-Token` or Bearer),
  checks `Host` and `Origin`, and sends a strict Content-Security-Policy.
- **Deleting something it should not**: every delete goes through the guards in `actions.go` (only listed files, still as
  scanned, never the last copy without an explicit confirmation, the surviving copy compared byte for byte, system locations
  off limits, the Recycle Bin by default).
- **Claude doing more than it should**: Claude only has DiskScout's tools, and anything that deletes or runs a non-diagnostic
  command waits for the user to press Approve in the window. `run_command` is not a shell: a program and its arguments, no
  chaining, started directly, with a time limit and capped output.
- **Path handling**: anything that lets a crafted path, archive entry or report reach outside what the user chose.

A bug in any of these is a security bug, even if no one has exploited it.
