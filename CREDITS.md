# Credits

Lodor stands on the work of others in the RomM and retro-handheld communities.

- **[RomM](https://github.com/rommapp/romm)** — the self-hosted ROM library manager Lodor is a client
  for. Lodor speaks RomM's HTTP API; none of RomM's code is included here.
- **[Grout](https://github.com/rommapp/grout)** — RomM's official handheld client. Lodor was written
  from scratch as a CGO-free engine, but Grout served as the behavioral and wire-protocol reference
  while figuring out RomM's API. MIT.
- **[MinUI](https://github.com/shauninman/MinUI)** by Shaun Inman — the minimalist frontend whose
  on-card layout and save-directory conventions Lodor targets. Used as integration reference; MinUI is
  not redistributed here.
- **[Allium](https://github.com/goweiwen/Allium)** — MIT, Copyright (c) 2025 Wei Wen Goh. Two 0.9.4
  engine designs follow Allium's patterns (code here is original, CGO-free stdlib Go):
  the RetroArch Network Control Interface client (`engine/ranet` — fire-and-forget UDP commands +
  250ms send-recv for the commands that answer, per `common/src/retroarch.rs`), and the playtime
  tracker's schema/merge semantics (`engine/playtime` — per-session rows + rolled-up per-game totals
  with merge-on-conflict, per Allium's games/game_sessions model, re-expressed as JSONL/TSV instead
  of SQLite and extended with cross-device merge over the RomM saves transport).

- **QR encoder (`engine/ui/qr.go`)** — original CGO-free, stdlib-only Go, written from scratch to the
  **ISO/IEC 18004** QR Code specification (byte mode, ECC level M, versions 1-9) for the muOS onboarding
  wizard's Tailscale sign-in screen. No third-party code is vendored. It was **cross-checked module-for-
  module** against **[Project Nayuki's QR Code generator](https://www.nayuki.io/page/qr-code-generator-library)**
  (qrcodegen, MIT) and **[libqrencode](https://github.com/fukuchi/libqrencode)** (LGPL — used only as an
  external oracle during testing, not linked or distributed), and every output decodes cleanly under zbar.

Trademarks and product names belong to their respective owners. Lodor ships **no** BIOS, firmware, or
copyrighted game content — you supply your own, on your own server.
