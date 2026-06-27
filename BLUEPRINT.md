# Lodor Engine — clean-room rewrite blueprint (2026-06-24)

From-scratch, CGO-free Go RomM-sync engine; behavioral/wire parity with the grout-derived engine at
`/tmp/mini-flip/grout-src`, owing nothing to its code. Citations are `path:line` into that source.

## Auth (corrected — NO OAuth2 password grant)
- **HTTP Basic** (`Authorization: Basic b64(user:pass)`) OR a **RomM client-token bearer** (`Authorization: Bearer <token>`).
- Token minted via `POST /api/client-tokens/exchange` body `{"code":"<code>"}` → `{raw_token,name,scopes[],expires_at}` (`romm/auth.go:109-117`). Stored in `config.json` `hosts[].token`; thereafter a static bearer header. **This is the onboarding pairing primitive.**
- Required token scopes: `assets.read assets.write devices.read devices.write` (`auth.go:30`).

## §1 RomM API surface used
Base = `host.root_uri`(+`:port`), trailing slash trimmed. Bearer/Basic on every call; JSON bodies `Content-Type: application/json`. Query params: hand-build `url.Values` (drop `sonh/qs`).
- `GET /api/heartbeat` (reachability). `POST /api/login` (Basic validate). `POST /api/client-tokens/exchange`. `GET /api/users/me`. `GET /api/platforms` (also token-validate).
- Platforms: `GET /api/platforms[?updated_after=]`, `GET /api/platforms/{id}` → fields `id,slug,fs_slug,name,custom_name,rom_count,firmware_count,has_bios`.
- ROMs: `GET /api/roms` query `offset,limit,platform_ids[](repeated),collection_id,search,order_by,order_dir,updated_after,with_files(always true)` → `{items[],total,limit,offset}`. `GET /api/roms/{id}`. `GET /api/roms/by-hash?md5_hash=|sha1_hash=|crc_hash=`.
  - **ROM content:** `GET /api/roms/{id}/content/{fs_name}` (literal `fmt.Sprintf("/api/roms/%d/content/%s",id,fs_name)`; %20-escaped). Rom fields: `id,platform_id,platform_fs_slug,platform_display_name,fs_name,fs_name_no_ext,fs_extension,name,md5_hash,sha1_hash,has_multiple_files,files[]{file_name},rom_ids`; `CanonicalLocalBasename` = single/nested → `files[0].file_name` minus ext.
- **Saves (core):**
  - `GET /api/saves?rom_id=&platform_id=&emulator=&device_id=&slot=` (valid if rom_id OR platform_id) → `[]Save`.
  - `GET /api/saves/summary?rom_id=`.
  - **`POST /api/saves`** — multipart: ONE file field **`saveFile`**, filename = local basename (e.g. `Game (USA).gba.sav`), body = raw bytes. Everything else QUERY: `rom_id`(req),`device_id`,`slot`,`emulator`,`overwrite`(bool omitempty),`autocleanup`(bool omitempty),`autocleanup_limit`(int omitempty). 409→ConflictError `{error,message,save_id,current_save_time,device_sync_time}` (or FastAPI `{detail:{...}}`).
  - `PUT /api/saves/{id}` (multipart saveFile only — unused by headless).
  - `GET /api/saves/{id}/content?device_id=&optimistic=` — **`optimistic=false` MUST be sent literally** (no omitempty). `POST /api/saves/{id}/downloaded` (unused by direct pull).
  - Save fields: `id,rom_id,file_name,file_name_no_ext,file_extension,file_size_bytes,updated_at,emulator,slot(*str),content_hash(*str = server MD5 of bytes),device_syncs[]{device_id,device_name,last_synced_at,is_current}`.
- Firmware: `GET /api/firmware?platform_id=` → `[]{id,file_name,md5_hash,sha1_hash}`; download URL client-built `/api/firmware/{id}/content/{file_name}` (no hash verify on BIOS).
- Collections: `GET /api/collections` → `[]{name,rom_ids[]}`.
- Devices/negotiate (default full-sync only; v1 can skip): `POST /api/devices`, `GET/PUT/DELETE /api/devices/{id}`, `POST /api/sync/negotiate`, `POST /api/sync/sessions/{id}/complete`, `GET /api/config`.

## §2 Sync algorithms
- **PushSaveDirect** (`push_direct.go:20-87`): resolve slug+rom; `findLocalSavesForRom`; per save POST `/api/saves` with `{rom_id,device_id,emulator=emuDir,slot="autosave",autocleanup=true,autocleanup_limit=25}`; on conflict (err contains `newer save|conflict|409`) retry with `overwrite=true` (INSERTS a new datetime-tagged row — additive, deletes nothing); on other err, rescue via `AlreadyOnServer`.
- **AlreadyOnServer** (`:93-108`): local file MD5 vs any `save.content_hash` (case-insensitive) → counts a landed-but-errored upload as done.
- **PullSaveDirect** (`pull_direct.go:24-91`): GET saves by rom; newest by `updated_at`; localPath = `GetSaveDirectory(slug)/SaveFileName(romBase, ext)`; newest-wins (skip unless server `updated_at` AFTER local mtime); GET `/api/saves/{id}/content?optimistic=false`; backup local→`.bak`; write `.tmp`→rename.
- **RestoreSave** (`:98-128`): explicit save, no age check, same `.bak`/`.tmp`.
- **findLocalSavesForRom** (`:151-177`): scan `BaseSavePath/<emuDir>` for each emuDir of the slug; file ext ∈ ValidSaveExtensions; stem == `rom.fs_name` OR `fs_name_no_ext`.
- **ValidSaveExtensions:** `.srm .sav .dsv .mcr .mcd .brm .eep .sra .fla .mpk .nv`.

## §3 Download+verify
- ROM: resolve id→GET rom→content URL→buffered GET→`.tmp`→rm stub→rename→`verifyRomHash` (multi-file/no-hash accept; sha1-preferred-then-md5 case-insensitive; mismatch→delete→downloaded=0). progress 0→90→100.
- BIOS: per mapped platform, GET firmware, GET each `/content/`, write to every `GetBIOSFilePaths`. count=successful.

## §4 Catalog/collections mirror
- `--mirror-catalog`: per mapped platform, per non-multi ROM, `path=GetLocalPath` (RomDir/`relative_path`||`RomMFSSlugToCFW(fs_slug)`/`files[0].file_name`), skip save-exts, skip existing, else create 0-byte stub. Out `MIRROR created=.. existing=.. skipped=.. multifile=..`. **Also writes the §5 index.**
- `--mirror-collections`: build rom_id→SDCARD-rel path; per collection write `Collections/<sanitized>.txt` (members that exist on card). sanitize: `/ \ : * ? " < > |`→`-`. Out `COLLECTIONS written=.. empty=.. total=..`.
- DirectoryMappings: `map[fs_slug]→{slug(override), relative_path(folder)}`.

## §5 Resolution WITHOUT sqlite (clean replacement)
`--mirror-catalog` emits `<QueueDir>/catalog-index.json`:
```json
{ "version":1, "platforms": { "gba": {
  "by_basename": {"Game (USA)":1234}, "by_fsname": {"Game (USA).gba":1234} } } }
```
key by_basename = `CanonicalLocalBasename` (single→files[0] minus ext; multi→fs_name_no_ext); by_fsname = `fs_name`. Each mode: `slug=reverseDirectoryMapping(romPath)` (port `slugForRomPath`, pure), then `index[slug].by_basename[nameNoExt]` else `index[slug].by_fsname[base]`. `--download` then `GET /api/roms/{id}` for the full Rom. Removes the entire sqlite `cache/` package + driver.

## §6 Save-dir data (miyoomini/MinUI) — re-express as our own
CFW=`MINUI`. Base = `BASE_PATH` else first of `/mnt/SDCARD`,`/mnt/sdcard`,`/mnt/mmc`. Dirs `Roms/ Bios/ Saves/`.
**Save filename (minarch):** `<full rom filename>.sav` (ROM name WITH rom ext). Save dir = `Saves/<emuFolder[0]>`; discovery scans all folders. BIOS = `Bios/<TAG>/<file>` per tag, else `Bios/<file>`.
**slug→emu-folders** (from `cfw/minui/data/save_directories.json`):
`gba:[GBA,MGBA] gbc:[GBC] gb:[GB] genesis:[MD] snes:[SFC] sfam:[SFC] nes:[FC] famicom:[FC] fds:[FDS] gamegear:[GG] sms:[SMS] sg1000:[SG1000] segacd:[SEGACD] sega32:[32X] dc:[DC] n64:[N64] nds:[NDS] psx:[PS] ps2:[PS2] psp:[PSP] virtualboy:[VB] wonderswan:[WS] wonderswan-color:[WSC] neo-geo-pocket:[NGP] neo-geo-pocket-color:[NGPC] pokemon-mini:[PKM] lynx:[LYNX] tg16:[PCE] msx:[MSX] colecovision:[COLECO] atari2600:[A2600] atari5200:[A5200] atari7800:[A7800] c64:[C64] c128:[C128] cpet:[PET] vic-20:[VIC] acpc:[CPC] amiga:[PUAE] arcade:[FBN] 3ds:[3DS] pico-8:[P8] doom:[PRBOOM] sg1000:[SG1000]` (empty-list slugs → no save dir). platforms.json = display names `"Name (TAG)"`.

## §7 config.json schema (minimal)
`hosts[0]: {root_uri(req), port, username, password, token(bearer,preferred), insecure_skip_verify, device_id(REQUIRED for net modes), device_name}`; `directory_mappings: {fs_slug:{slug,relative_path}}`; `api_timeout`(sec,def 30,clamp>300→30); `download_timeout`(sec,def 3600). Loaded CWD-relative. device_id read-only (minted via POST /api/devices).

## §8 CLI contracts (PARITY-CRITICAL — native launcher parses these)
Exit: 0 ok, 2 config, 3 unreachable/resolve, 4 ran-but-errored. Modes print ONLY their lines.
- `--mirror-catalog` → `MIRROR created=%d existing=%d skipped=%d multifile=%d`
- `--mirror-collections` → `COLLECTIONS written=%d empty=%d total=%d`
- `--download <p>` → `RESULT downloaded=<0|1>` (+ /tmp/dl-progress 0→90→100, /tmp/romm-phase)
- `--download-bios` → `RESULT bios=<n>`
- `--push-pending` → `RESULT pushed=<N> total=<M> stuck=<K>` (reads `<LODOR_PAK_DIR>/pending-saves.txt` — the pak working dir, exported by the launch scripts; falls back to CWD — removes landed; mkdir .queue.lock)
- `--sync-save <p>` → `RESULT pulled=<0|1> pushed=<0|1>`
- `--list-saves <p>` → per save newest-first `<id>\t<YYYY-MM-DD HH:MM>\t<device|emulator>\t<kb>KB`; zero→nothing
- `--restore-save <p> <saveID positional>` → `RESULT restored=<0|1>`
- `--sync-feed` → ≤20 deduped newest-first `<game>\t<YYYY-MM-DD HH:MM>\t<device>` (game=file_name_no_ext, trailing 2-5 ext trimmed)
- side-channels: `/tmp/dl-progress` int 0..100; `/tmp/romm-phase` one-line label.

## §9 Package plan (module `lodor`, CGO-free, stdlib only)
`romm/` (client+types+auth+409 parse, ~600 LOC, M) · `config/` (~150, S) · `platform/` (save-dir DATA + path helpers, ~250, S) · `catalog/` (index + mirror, ~300, M) · `sync/` (push/pull/restore/dedup/discovery, ~400, M) · `cmd/lodor-sync/` (flags + all §8 modes + side-channels + pending, ~500, M). Optional later `sync/negotiate.go` (~700, L).
**Riskiest (test live first):** multipart upload (`saveFile` field, query params, `autocleanup_limit=25`), `optimistic=false` literal, hash verify sha1→md5, additive 409 retry + AlreadyOnServer, newest-wins+`.bak`/`.tmp`, exact stdout+side-channels, path identity (CanonicalLocalBasename/GetLocalPath/SaveFileName).
