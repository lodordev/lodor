package romm

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Heartbeat performs GET /api/heartbeat as a reachability/auth check.
func (c *Client) Heartbeat() error {
	return c.doJSON("GET", "/api/heartbeat", nil, nil)
}

// GetPlatforms returns all platforms (GET /api/platforms).
func (c *Client) GetPlatforms() ([]Platform, error) {
	var out []Platform
	err := c.doJSON("GET", "/api/platforms", nil, &out)
	return out, err
}

// GetPlatform returns one platform by id (GET /api/platforms/{id}).
func (c *Client) GetPlatform(id int) (Platform, error) {
	var out Platform
	err := c.doJSON("GET", fmt.Sprintf("/api/platforms/%d", id), nil, &out)
	return out, err
}

// GetRomsQuery holds the supported filters for GET /api/roms. with_files is always
// sent as true by GetRoms.
type GetRomsQuery struct {
	PlatformIDs  []int
	CollectionID int
	Search       string
	OrderBy      string
	OrderDir     string
	UpdatedAfter string
}

// GetRoms returns every ROM matching the query, transparently paginating through
// the items/total envelope. The query string is hand-built (repeated
// repeated platform_ids entries; the live server ignores the bracketed form) and with_files=true is always set.
func (c *Client) GetRoms(query GetRomsQuery) (PaginatedRoms, error) {
	const pageLimit = 250
	var all PaginatedRoms

	offset := 0
	for {
		v := url.Values{}
		v.Set("offset", strconv.Itoa(offset))
		v.Set("limit", strconv.Itoa(pageLimit))
		v.Set("with_files", "true")
		for _, pid := range query.PlatformIDs {
			v.Add("platform_ids", strconv.Itoa(pid))
		}
		if query.CollectionID > 0 {
			v.Set("collection_id", strconv.Itoa(query.CollectionID))
		}
		if query.Search != "" {
			v.Set("search", query.Search)
		}
		if query.OrderBy != "" {
			v.Set("order_by", query.OrderBy)
		}
		if query.OrderDir != "" {
			v.Set("order_dir", query.OrderDir)
		}
		if query.UpdatedAfter != "" {
			v.Set("updated_after", query.UpdatedAfter)
		}

		var page PaginatedRoms
		if err := c.doJSON("GET", "/api/roms?"+v.Encode(), nil, &page); err != nil {
			return PaginatedRoms{}, err
		}

		all.Items = append(all.Items, page.Items...)
		all.Total = page.Total
		all.Limit = page.Limit
		all.Offset = 0

		offset += len(page.Items)
		if len(page.Items) == 0 || offset >= page.Total {
			break
		}
	}

	return all, nil
}

// GetRom returns one ROM by id (GET /api/roms/{id}).
func (c *Client) GetRom(id int) (Rom, error) {
	var out Rom
	err := c.doJSON("GET", fmt.Sprintf("/api/roms/%d", id), nil, &out)
	return out, err
}

// SaveQuery filters GET /api/saves. Valid requires rom_id OR platform_id.
type SaveQuery struct {
	RomID      int
	PlatformID int
	Emulator   string
	DeviceID   string
	Slot       string
}

// GetSaves returns saves matching the query (GET /api/saves).
func (c *Client) GetSaves(query SaveQuery) ([]Save, error) {
	v := url.Values{}
	if query.RomID != 0 {
		v.Set("rom_id", strconv.Itoa(query.RomID))
	}
	if query.PlatformID != 0 {
		v.Set("platform_id", strconv.Itoa(query.PlatformID))
	}
	if query.Emulator != "" {
		v.Set("emulator", query.Emulator)
	}
	if query.DeviceID != "" {
		v.Set("device_id", query.DeviceID)
	}
	if query.Slot != "" {
		v.Set("slot", query.Slot)
	}

	path := "/api/saves"
	if enc := v.Encode(); enc != "" {
		path += "?" + enc
	}

	var out []Save
	err := c.doJSON("GET", path, nil, &out)
	return out, err
}

// UploadSaveQuery holds the query parameters for POST /api/saves. The file itself
// travels as the multipart saveFile field, not here.
type UploadSaveQuery struct {
	RomID            int
	DeviceID         string
	Slot             string
	Emulator         string
	Overwrite        bool
	Autocleanup      bool
	AutocleanupLimit int
}

// UploadSave uploads savePath to POST /api/saves as multipart (one field saveFile,
// filename = basename of savePath). All other parameters travel as query string.
// A 409 returns a *ConflictError. The returned Save reflects the created record.
func (c *Client) UploadSave(query UploadSaveQuery, savePath string) (Save, error) {
	fileBytes, fileName, err := readFile(savePath)
	if err != nil {
		return Save{}, err
	}

	v := url.Values{}
	if query.RomID != 0 {
		v.Set("rom_id", strconv.Itoa(query.RomID))
	}
	if query.DeviceID != "" {
		v.Set("device_id", query.DeviceID)
	}
	if query.Slot != "" {
		v.Set("slot", query.Slot)
	}
	if query.Emulator != "" {
		v.Set("emulator", query.Emulator)
	}
	if query.Overwrite {
		v.Set("overwrite", "true")
	}
	if query.Autocleanup {
		v.Set("autocleanup", "true")
	}
	if query.AutocleanupLimit != 0 {
		v.Set("autocleanup_limit", strconv.Itoa(query.AutocleanupLimit))
	}

	var out Save
	err = c.doMultipart("/api/saves", v, "saveFile", fileName, fileBytes, &out)
	return out, err
}

// GetFirmware returns the firmware/BIOS records for a platform
// (GET /api/firmware?platform_id=).
func (c *Client) GetFirmware(platformID int) ([]Firmware, error) {
	v := url.Values{}
	v.Set("platform_id", strconv.Itoa(platformID))
	var out []Firmware
	err := c.doJSON("GET", "/api/firmware?"+v.Encode(), nil, &out)
	return out, err
}

// GetCollections returns all collections (GET /api/collections).
func (c *Client) GetCollections() ([]Collection, error) {
	var out []Collection
	err := c.doJSON("GET", "/api/collections", nil, &out)
	return out, err
}

// DownloadSaveContent fetches the raw bytes of a save
// (GET /api/saves/{id}/content?device_id=&optimistic=). The optimistic flag is
// ALWAYS written literally (true|false) — even false — so the server does not mark
// the device synced before the write is confirmed.
func (c *Client) DownloadSaveContent(saveID int, deviceID string, optimistic bool) ([]byte, error) {
	v := url.Values{}
	if deviceID != "" {
		v.Set("device_id", deviceID)
	}
	v.Set("optimistic", strconv.FormatBool(optimistic))
	path := fmt.Sprintf("/api/saves/%d/content?%s", saveID, v.Encode())
	return c.doRaw("GET", path)
}

// DownloadRomContent fetches a ROM's real file bytes
// (GET /api/roms/{id}/content/{fs_name}). fs_name spaces are %20-escaped by doRaw.
//
// NOTE: for a single-file ROM this returns the raw file; for a multi-file ROM with NO
// file_ids selector RomM returns a mod_zip archive of ALL files. Multi-disc games are
// downloaded disc-by-disc via DownloadRomFileTo instead (one file_ids each), which
// streams (no OOM) and yields per-disc bytes that hash-verify cleanly.
func (c *Client) DownloadRomContent(romID int, fsName string) ([]byte, error) {
	path := fmt.Sprintf("/api/roms/%d/content/%s", romID, fsName)
	return c.doRaw("GET", path)
}

// DownloadRomFileTo streams ONE constituent file of a ROM (selected by its file id)
// to dst, returning the bytes written. It targets the same content endpoint but adds
// the single-valued file_ids query param — verified live (2026-06-25): exactly ONE
// file_ids returns that file's raw bytes (Content-Type application/octet-stream, real
// Content-Length), whereas two or more fall back to a mod_zip archive. The body is
// streamed (io.Copy), never buffered, so a 482 MB disc costs near-zero RAM. fsName is
// the ROM's fs_name (the URL still requires the {fs_name} path segment); spaces are
// %20-escaped by buildURL. The caller owns dst.
func (c *Client) DownloadRomFileTo(romID int, fsName string, fileID int, dst io.Writer) (int64, error) {
	path := fmt.Sprintf("/api/roms/%d/content/%s?file_ids=%d", romID, fsName, fileID)
	return c.doRawStreamTo(path, dst)
}

// DownloadFirmwareContent fetches a BIOS/firmware file's bytes
// (GET /api/firmware/{id}/content/{file_name}).
func (c *Client) DownloadFirmwareContent(fwID int, fileName string) ([]byte, error) {
	path := fmt.Sprintf("/api/firmware/%d/content/%s", fwID, fileName)
	return c.doRaw("GET", path)
}

// DownloadCover fetches the raw bytes of a rom's box-art cover from a server-relative
// asset path (rom.CoverPath(), e.g. "/assets/romm/resources/roms/5/10803/cover/small.png?ts=...").
// The "?ts=" cache-buster RomM appends carries a SPACE in its value (a timestamp like
// "2026-06-25 03:00:51"); doRaw's buildURL copies the raw query verbatim, so a raw
// space there would 400. We don't need the cache-buster (every fetch is fresh), so we
// STRIP the query entirely and request just the asset path — buildURL %20-escapes any
// space in the path itself. An empty coverPath is a programmer error (callers must
// check rom.CoverPath() != "" first). The bytes are a PNG (image/png) from RomM /assets.
func (c *Client) DownloadCover(coverPath string) ([]byte, error) {
	if strings.TrimSpace(coverPath) == "" {
		return nil, fmt.Errorf("empty cover path")
	}
	if i := strings.IndexByte(coverPath, '?'); i >= 0 {
		coverPath = coverPath[:i]
	}
	if coverPath == "" {
		return nil, fmt.Errorf("empty cover path")
	}
	if coverPath[0] != '/' {
		coverPath = "/" + coverPath
	}
	return c.doRaw("GET", coverPath)
}
