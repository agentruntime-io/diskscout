# Contributing to DiskScout

Thanks for helping. DiskScout is a small, careful tool, so a few ground rules keep it that way.

## Setting up

You need Windows 10 or 11 and Go 1.25. There are no other dependencies.

    go test ./...
    go build -trimpath -ldflags "-s -w -H windowsgui" -o DiskScout.exe .

To work on the window (`ui/`: plain HTML, CSS and JavaScript, no build step), serve the files from disk and reload the page
after each edit:

    DiskScout.exe --no-window --ui-dir ui --data %TEMP%\ds-dev

`--no-window` prints the address to open in a browser. Use a throwaway `--data` folder so your own settings and hash cache stay
untouched.

## What matters here

1. **Deleting is the dangerous part.** Anything that changes the disk goes through the guards in `actions.go`: only files the
   app listed, still exactly what the scan saw, never the last copy without an explicit tick, the copy that stays compared byte
   for byte, the Recycle Bin by default. A new way of touching the disk needs those same guards and a test that proves them.
2. **Standard library only.** No third-party modules. It keeps the exe small and the trust surface obvious.
3. **Say what cannot be told.** When the app does not know something (a zip it cannot open, a folder that shares its place with
   other files), it says so instead of guessing. Please keep that.
4. **A calm window.** Neutral greys and one blue; colour is for the kind of file and for status, and status always comes with
   an icon and a label. No glow, no animation for its own sake. Look at both light and dark.
5. **No network, no telemetry.** The app listens on `127.0.0.1` only. The chat panel runs the user's own `claude`; that is the
   only thing that talks to the internet, and it is the user's own installation.
6. **Tests run against real files.** The existing tests use real folders, hardlinks, junctions and the real HTTP layer; follow
   that style rather than mocking the file system.

## Sending a change

- Keep a pull request to one change, and say in it what you checked.
- `gofmt -l .` must print nothing; `go vet ./...` and `go test ./...` must pass. CI runs the same on Windows.
- Match the code around yours: naming, comment density, idiom. Comments say why, not what.
- If behaviour a user can see changes, update the README in the same pull request.
- Do not commit binaries, a `diskscout-data` folder, logs, or a hash cache (`.gitignore` covers them).

## Reporting a bug

Open an issue with the DiskScout version (`DiskScout.exe --version`), your Windows version, and the steps. For anything that
could make the app delete something it should not, or that exposes the local server, please use the private route in
[SECURITY.md](SECURITY.md) instead.

## Releases

Maintainers: push a tag like `v1.8.0` on a commit whose CI run has passed. The Release workflow refuses to run otherwise, then
cross-compiles `DiskScout-1.8.0.exe` on a Linux runner (the version is stamped in with `-X main.version`; there is no cgo, so
it is the same exe a Windows build gives) and attaches it with its SHA-256 to a GitHub release. The tests themselves run on a
Windows runner in CI, which is free while the repository is public. Both workflows skip themselves if the repository is ever
made private, so they cannot quietly use up an organisation's included minutes (Windows minutes count double).

## License

By contributing you agree that your work is released under the [MIT License](LICENSE), like the rest of the project.
