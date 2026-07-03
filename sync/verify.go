package sync

// Upload verification (task #123 deliverable 1). An HTTP 2xx from POST
// /api/saves proves the server ACCEPTED the request — not that it verifiably
// STORED our bytes (ghost saves, backlog #63, were minted exactly this way). So
// after every save upload the engine now confirms the server-side copy before
// any outcome reports the save as safely on the server (which is what dequeues
// it from pending-saves.txt and lights the launcher's synced-✓).
//
// Verification tiers, cheapest-reliable first (RomM ≥ 4.8 wire shapes):
//
//  1. FREE — POST /api/saves returns the created Save record; its content_hash
//     (the MD5 of the stored bytes, the same signal AlreadyOnServer and the
//     --list-saves CURRENT marker already trust) must equal the local file's
//     MD5, AND file_size_bytes must be > 0. A matching hash with a zero size is
//     NOT accepted at this tier (that is the ghost shape) — it falls through to
//     the byte check.
//  2. ONE GET, DEFINITIVE — GET /api/saves/{id}/content (optimistic=false, no
//     device_id, side-effect-free) re-downloads the just-uploaded record's
//     bytes; their length and MD5 must match the local file. Saves are small
//     (KB–MB), so this is cheap; it runs only when tier 1 could not confirm.
//  3. LIST FALLBACK — GET /api/saves?rom_id= and look for any NON-GHOST record
//     whose content_hash equals the local MD5 (an older server that returns a
//     hashless create response but hashes on list).
//
// A save that fails all tiers is retried once by the caller and then left
// PENDING with outcome HashMismatch — never silently marked synced.

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"lodor/romm"
)

// errVerify* are the stable verification failures. They carry no host, URL, or
// token — safe for PushResult.Err / stderr lines.
var (
	errVerifyMismatch    = syncErr("server copy doesn't match the uploaded save")
	errVerifyUnavailable = syncErr("couldn't confirm the server stored the save")
)

// verifyUploadedSave confirms the server verifiably stored localPath's bytes
// after an upload that returned `uploaded`. romID scopes the tier-3 list
// fallback. A wrapped *romm.AuthError from the verification requests is
// surfaced (errors.As-able) so the caller can map it to PAIRING_EXPIRED.
func verifyUploadedSave(client *romm.Client, romID int, uploaded romm.Save, localPath string) error {
	localMD5, ok := fileMD5(localPath)
	if !ok {
		return syncErr("local save unreadable")
	}
	fi, serr := os.Stat(localPath)
	if serr != nil {
		return syncErr("local save unreadable")
	}
	fetchContent := func(id int) ([]byte, error) {
		// optimistic=false + empty device_id: side-effect-free — the server must not
		// mark any device synced because of a verification read.
		return client.DownloadSaveContent(id, "", false)
	}
	listHashes := func() ([]string, error) {
		saves, err := client.GetSaves(romm.SaveQuery{RomID: romID})
		if err != nil {
			return nil, err
		}
		var hs []string
		for _, s := range saves {
			if IsGhostSave(s) || IsMetaSave(s) {
				continue // a ghost's (or meta record's, #146) hash must never verify a save upload
			}
			if s.ContentHash != nil && *s.ContentHash != "" {
				hs = append(hs, *s.ContentHash)
			}
		}
		return hs, nil
	}
	return verifyStored(uploaded, localMD5, fi.Size(), fetchContent, listHashes)
}

// verifyStored is the tiered verification decision, pure over its inputs so it
// unit-tests without a server. fetchContent and listHashes are the injected
// network reads for tiers 2 and 3.
func verifyStored(uploaded romm.Save, localMD5 string, localSize int64,
	fetchContent func(id int) ([]byte, error), listHashes func() ([]string, error)) error {

	// Tier 1: the create response's own content_hash + a non-zero stored size.
	if uploaded.ContentHash != nil && *uploaded.ContentHash != "" &&
		strings.EqualFold(*uploaded.ContentHash, localMD5) && uploaded.FileSizeBytes > 0 {
		return nil
	}

	// Tier 2: re-download the uploaded record's bytes and compare (definitive).
	if uploaded.ID != 0 {
		data, err := fetchContent(uploaded.ID)
		if err == nil {
			sum := md5.Sum(data)
			if int64(len(data)) == localSize && len(data) > 0 &&
				strings.EqualFold(hex.EncodeToString(sum[:]), localMD5) {
				return nil
			}
			return errVerifyMismatch
		}
		if romm.IsAuthError(err) {
			return fmt.Errorf("%s: %w", errVerifyUnavailable, err)
		}
		// transport blip — fall through to the list fallback
	}

	// Tier 3: any non-ghost server save for this ROM carries our content hash.
	hs, err := listHashes()
	if err != nil {
		if romm.IsAuthError(err) {
			return fmt.Errorf("%s: %w", errVerifyUnavailable, err)
		}
		return errVerifyUnavailable
	}
	for _, h := range hs {
		if strings.EqualFold(h, localMD5) {
			return nil
		}
	}
	return errVerifyMismatch
}

// emptyLocalSave reports whether path is missing, unreadable, or zero bytes —
// the ghost-proof upload guard (#63): a 0-byte local save must never upload
// (it would mint a server record with no useful bytes, and can also be a
// transient emulator state mid-write).
func emptyLocalSave(path string) bool {
	fi, err := os.Stat(path)
	return err != nil || fi.IsDir() || fi.Size() == 0
}
