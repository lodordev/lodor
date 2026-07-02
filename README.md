# Lodor

A small, CGO-free Go engine that syncs a [RomM](https://github.com/rommapp/romm) library to a Linux
retro handheld — the headless core behind [LodorOS](https://github.com/lodordev/lodoros) and the
per-CFW ports.

Lodor's wedge is **transparent mirroring**: instead of browsing and downloading, your whole library
appears on the device as zero-byte stub files. Tap a game and the real ROM is fetched on demand;
saves sync back to the server automatically around each session. The library "isn't there until you
touch it," so a 128 MB handheld can front a 500 GB server.

This repository is the **engine only** — a single static binary plus shell-callable subcommands. It
has no UI; the front-end (menus, onboarding, progress) lives in the OS and port repos that embed it.

## What it does

- **Stub mirror** — write a zero-byte placeholder for every not-yet-downloaded ROM, into the
  device's per-system folders, and emit a resolution index (no database needed).
- **Fetch on launch** — resolve a tapped stub to its RomM ROM, stream the real file to disk, verify it.
- **Save sync** — push local saves to RomM (additive, versioned) and pull/restore newer ones, with a
  pending queue for offline writes. A restore preserves the current save first — pushing it, or staging
  it for deferred upload when offline — so it can never trade away unsynced progress.
- **Catalog & collections** — mirror platforms and RomM collections to the device.
- **Box art & BIOS** — fetch covers (lazily) and per-platform firmware.
- **Onboarding** — pair to a server with a one-time code (no admin credentials on the device).

## Design

- **CGO-free, standard-library only.** No database driver, no SDL, no third-party modules — `go.mod`
  has zero `require`s. Builds to a single static binary that cross-compiles cleanly for ARMv7/ARM64.
- **Honest results.** Every subcommand prints a machine-parseable `RESULT …` line and exits with a
  code the caller switches on. A partial or failed transfer reports `downloaded=0` and leaves the stub
  in place — it never fabricates success or writes a fake placeholder in a real ROM's spot.
- **Newest-wins, additive saves.** Uploads never delete server history; conflicts insert a new
  timestamped revision. Pulls back up the local file before overwriting.

## Build

```sh
# host build
CGO_ENABLED=0 go build -o lodor-sync ./cmd/lodor-sync

# static ARMv7 (most Linux handhelds)
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -o lodor-sync ./cmd/lodor-sync
```

A static binary uses Go's pure resolver, so on-device it expects a usable `/etc/resolv.conf` and a CA
bundle (ship `ca-certificates.crt` and point `SSL_CERT_FILE` at it).

## Usage

Configuration lives in `config.json` next to the binary (see `config.json.example`). Subcommands:

| Flag | Does |
|---|---|
| `--set-server <url>` `[--port N] [--insecure]` | Persist the server URL before pairing. |
| `--pair <code>` | Exchange a RomM pairing code for a client token; write `config.json`. |
| `--register-device <name>` / `--rename-device <name>` | Register/rename this device. |
| `--validate` | Check reachability + auth. Prints `reachable=<0\|1> auth=<0\|1> pairing_expired=<0\|1>`. |
| `--mirror-catalog` | Stub every not-downloaded ROM into `Roms/` (only platforms with an installed emulator pak — never games the device can't launch); write the resolution index. |
| `--mirror-collections` | Write `Collections/<name>.txt` per RomM collection. |
| `--download <rom>` | Fetch one ROM's real file (resolve → stream → verify). Multi-disc aware. |
| `--download-bios` | Fetch BIOS/firmware for every mapped platform. |
| `--sync-save <rom>` | Pull-then-push the save for one ROM. |
| `--list-saves <rom>` / `--restore-save <rom> <id>` | List / restore server save revisions. |
| `--push-pending` | Upload every queued pending save. |
| `--recent` | Print the single most-recently-played game across devices (powers the Continue tile). |
| `--sync-feed` | List recent server saves, newest first. |

Each prints a `RESULT …` summary line.

**Exit codes:** `0` ok · `2` config/flag error · `3` unreachable / could not resolve · `4` ran but
one or more items errored · `6` **pairing expired** — the server rejected this device's client token
(expired or revoked); re-pair the device. On exit 6, modes that print a `RESULT` line also print a
final `PAIRING_EXPIRED` line on stdout; list-output modes signal via the exit code alone. The engine
never deletes or rewrites its config on an auth failure, so a transient server misconfig can't wipe
a valid pairing.

**Save-sync integrity:** an upload is reported pushed only after the server-side copy is *verified*
(the create response's content hash and stored size, else a byte-for-byte re-download); an
unverifiable upload is retried once and then left pending — never silently marked synced. Server
save records with missing/zero-length bytes ("ghost saves") are never pulled over a real local save,
never offered for restore, and are counted (`ghosts=<N>` in `--sync-save`) so a UI can surface them.
A zero-byte local save file is never uploaded.

## Status

The engine is built and proven end-to-end against a live RomM server (pairing, mirror, download —
including multi-disc — and save round-trips). It targets the MinUI save-directory conventions today;
per-CFW save-path data is data-driven so other front-ends can adopt it.

## License

MIT — see [LICENSE](LICENSE). Acknowledgements in [CREDITS.md](CREDITS.md).
