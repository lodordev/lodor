# Lodor — Releases

One index for every Lodor release across the project's repositories. Lodor ships several front-ends from separate repos; this is the single place to find them all.

## Latest — everything is 0.9.7.1 (in sync)

| Front-end | Version | Download |
|---|---|---|
| **LodorOS** — full-card image (Miyoo) | **0.9.7.1** | [lodoros · v0.9.7.1](https://github.com/lodordev/lodoros/releases/tag/v0.9.7.1) |
| **muOS · Knulli · LodorOS update-overlays** | **0.9.7.1** | [lodor · v0.9.7.1](https://github.com/lodordev/lodor/releases/tag/v0.9.7.1) |
| **NextUI** | **0.9.7** | [lodor-nextui · 0.9.7](https://github.com/lodordev/lodor-nextui/releases/tag/0.9.7) |

Devices self-update via the manifest at `https://lodordev.github.io/lodor/versions.json`.

## Where releases live (the map)

- **[lodordev/lodor](https://github.com/lodordev/lodor/releases)** — the umbrella release page. muOS `.muxapp`, Knulli zip, LodorOS **update-overlay** zips, Android APK, and `versions.json` (the device self-update manifest).
- **[lodordev/lodoros](https://github.com/lodordev/lodoros/releases)** — the LodorOS **full-card** image. New users flash this; updates thereafter come from the umbrella overlays via in-app "Update Lodor".
- **[lodordev/lodor-nextui](https://github.com/lodordev/lodor-nextui/releases)** — the NextUI Pak Store channel.

## How to install each front-end

- **LodorOS (Miyoo: Mini Plus / A30 / Flip V2):** flash the full-card `LodorOS-<v>.zip` to a **FAT32** card with a real `unzip -o` (keep hidden dot-folders), eject cleanly, boot. Then **Update Lodor** keeps it current; config, saves, and ROMs are preserved.
- **Lodor-muOS (H700 Anbernic):** install the `.muxapp` via the muOS App Downloader.
- **Lodor-NextUI (TrimUI):** install the Tool pak from the NextUI Pak Store.

## Full history

### LodorOS full card — lodordev/lodoros
| Version | Notes |
|---|---|
| 0.9.7.1 | Current. In sync with the umbrella; C1 fix + Handoff manifests. |
| 0.9.5 | Superseded. |
| 0.9.4.1 / 0.9.4 | Superseded. |

### Umbrella (updates + muOS/Knulli/Android) — lodordev/lodor
| Version | Notes |
|---|---|
| 0.9.7.1 | Current. |
| 0.9.7 | First Android release. |
| 0.9.6 | Four-lane consolidated release. |

### NextUI — lodordev/lodor-nextui
0.9.7 · 0.9.6 · 0.9.1-beta

---
*Versioning: all front-ends share one version line; installs start from the full card and stay current via the umbrella `versions.json`.*
