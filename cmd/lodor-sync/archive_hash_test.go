package main

import (
	"archive/zip"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"lodor/romm"
)

func TestVerifyRomHashUncompressedSHA1(t *testing.T) {
	payload := []byte("ordinary uncompressed ROM bytes")
	path := filepath.Join(t.TempDir(), "game.bin")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	rom := romm.Rom{Sha1Hash: sha1Hex(payload)}
	if err := verifyRomHash(rom, path); err != nil {
		t.Fatalf("verifyRomHash() error = %v", err)
	}
}

func TestVerifyRomHashZIPRomMStyleSHA1(t *testing.T) {
	payload := []byte("uncompressed Mega Drive ROM bytes")
	path := writeTestZIP(t, []zipEntry{{name: "Blockout (World).md", data: payload}})

	rawZIP, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha1Hex(rawZIP) == sha1Hex(payload) {
		t.Fatal("test fixture does not distinguish ZIP container and member hashes")
	}

	rom := romm.Rom{Sha1Hash: sha1Hex(payload)}
	if err := verifyRomHash(rom, path); err != nil {
		t.Fatalf("verifyRomHash() error = %v", err)
	}
}

func TestVerifyRomHashZIPMismatch(t *testing.T) {
	path := writeTestZIP(t, []zipEntry{{name: "game.gg", data: []byte("downloaded bytes")}})
	rom := romm.Rom{Sha1Hash: sha1Hex([]byte("different expected bytes"))}

	if err := verifyRomHash(rom, path); err == nil {
		t.Fatal("verifyRomHash() error = nil, want hash mismatch")
	}
}

func TestVerifyRomHashNoHash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unreadable-as-zip.zip")
	if err := os.WriteFile(path, []byte("not a ZIP"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := verifyRomHash(romm.Rom{}, path); err != nil {
		t.Fatalf("verifyRomHash() error = %v, want no-hash acceptance", err)
	}
}

func TestVerifyRomHashPreservesCHDBehavior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "game.chd")
	if err := os.WriteFile(path, []byte("CHD container bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	rom := romm.Rom{Sha1Hash: sha1Hex([]byte("internal disc bytes"))}

	if err := verifyRomHash(rom, path); err != nil {
		t.Fatalf("verifyRomHash() error = %v, want CHD container-hash bypass", err)
	}
}

func TestVerifyRomHashPreservesMultiFileBehavior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi.zip")
	if err := os.WriteFile(path, []byte("not a ZIP"), 0o644); err != nil {
		t.Fatal(err)
	}
	rom := romm.Rom{
		HasMultipleFiles: true,
		Sha1Hash:         sha1Hex([]byte("composite multi-file bytes")),
	}

	if err := verifyRomHash(rom, path); err != nil {
		t.Fatalf("verifyRomHash() error = %v, want existing multi-file bypass", err)
	}
}

func TestVerifyRomHashZIPMD5Fallback(t *testing.T) {
	payload := []byte("archived ROM verified with MD5")
	path := writeTestZIP(t, []zipEntry{{name: "game.sms", data: payload}})
	rom := romm.Rom{Md5Hash: md5Hex(payload)}

	if err := verifyRomHash(rom, path); err != nil {
		t.Fatalf("verifyRomHash() error = %v", err)
	}
}

func TestVerifyRomHashCorruptZIP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.zip")
	if err := os.WriteFile(path, []byte("not a ZIP"), 0o644); err != nil {
		t.Fatal(err)
	}
	rom := romm.Rom{Sha1Hash: sha1Hex([]byte("not a ZIP"))}

	if err := verifyRomHash(rom, path); err == nil {
		t.Fatal("verifyRomHash() error = nil, want corrupt ZIP failure")
	}
}

func TestVerifyRomHashZIPAllExcludedUsesRomMRawFallback(t *testing.T) {
	path := writeTestZIP(t, []zipEntry{{name: "notes.log", data: []byte("excluded")}})
	rawZIP, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rom := romm.Rom{Sha1Hash: sha1Hex(rawZIP)}

	if err := verifyRomHash(rom, path); err != nil {
		t.Fatalf("verifyRomHash() error = %v", err)
	}
}

func TestVerifyRomHashZIPMultipleMembersRomMOrder(t *testing.T) {
	alpha := []byte("alpha member bytes")
	zeta := []byte("zeta member bytes")
	path := writeTestZIP(t, []zipEntry{
		{name: "z/game.bin", data: zeta},
		{name: "notes.log", data: []byte("RomM excludes log files")},
		{name: "a/game.bin", data: alpha},
		{name: ".DS_Store", data: []byte("RomM excludes this junk file")},
	})
	rom := romm.Rom{Sha1Hash: sha1Hex(append(append([]byte(nil), alpha...), zeta...))}

	if err := verifyRomHash(rom, path); err != nil {
		t.Fatalf("verifyRomHash() error = %v", err)
	}
}

type zipEntry struct {
	name string
	data []byte
}

func writeTestZIP(t *testing.T, entries []zipEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "game.zip")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, entry := range entries {
		w, err := zw.Create(entry.name)
		if err != nil {
			_ = zw.Close()
			_ = f.Close()
			t.Fatal(err)
		}
		if _, err := w.Write(entry.data); err != nil {
			_ = zw.Close()
			_ = f.Close()
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
