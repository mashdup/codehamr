// Package update performs a passive, fire-and-forget freshness check against
// the latest GitHub release. It hashes the running executable with sha256 and
// compares that against the row for the current os/arch in the published
// `codehamr_checksums.txt` asset that goreleaser uploads with every release.
// Mismatch = the user's local binary is stale.
//
// Both callsites are in main.go's maybeSelfUpdate, which runs once before
// the TUI starts: Check decides whether an update exists, Apply atomically
// replaces the running binary on disk, and the caller's syscall.Exec then
// re-enters the new version in place — no second restart visible to the
// user. The TUI itself carries no update awareness; one strategy, one
// trigger point.
//
// Any network hiccup, offline machine, missing asset, or parse glitch
// returns "no update" rather than surfacing an error — a startup banner
// that shouts when the internet is flaky is worse than one that stays
// quiet. CODEHAMR_NO_UPDATE_CHECK=1 is the user escape hatch for
// air-gapped setups, CI, and the post-update re-exec (which sets it so
// the replacement child doesn't loop into a second check).
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// checksumsURL is the "latest" redirect GitHub serves for the goreleaser
// checksums asset. Direct CDN download — no GitHub API call, so no 60/hour
// rate limit to worry about even for users who start many TUI sessions.
//
// `var` rather than `const` so tests can point both URLs at an httptest
// server; production code never reassigns them.
var checksumsURL = "https://github.com/codehamr/codehamr/releases/latest/download/codehamr_checksums.txt"

// releaseBase is the stable "latest" redirect for individual binary assets.
// Paired with asset names from assetName() to form the download URL in Apply.
var releaseBase = "https://github.com/codehamr/codehamr/releases/latest/download/"

// fetchTimeout bounds the checksums.txt GET. Matches the TUI's own ping
// budget so a silent network can't extend startup.
const fetchTimeout = 2 * time.Second

// promoteRename indirects the final, dangerous rename in Apply — the one that
// moves the verified download onto execPath after the running binary has been
// moved aside to execPath+".old". Same `var`-for-tests pattern as
// checksumsURL/releaseBase above: production never reassigns it; the only
// reason it isn't a direct os.Rename call is that the restore-on-failure
// branch (which leaves the user with no executable if it regresses) is
// otherwise impossible to drive deterministically and root-safely in a test.
var promoteRename = os.Rename

// Check compares the local binary's sha256 against the remote asset's
// recorded hash and reports whether they differ. ctx is honoured so a parent
// cancel (Ctrl+C during startup) propagates into the HTTP request. Returns
// false on any failure — see package doc for the rationale.
func Check(ctx context.Context, execPath string) bool {
	if os.Getenv("CODEHAMR_NO_UPDATE_CHECK") == "1" {
		return false
	}
	asset, ok := assetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return false
	}
	local, err := hashFile(execPath)
	if err != nil {
		return false
	}
	remote, err := fetchHash(ctx, asset)
	if err != nil || remote == "" {
		return false
	}
	return !strings.EqualFold(local, remote)
}

// assetName mirrors the name_template in .goreleaser.yaml. Every goos
// goreleaser builds for must be reachable here — TestAssetNameCoversEvery
// ReleasedPlatform pins this contract so a future target added to
// .goreleaser.yaml without a corresponding switch case can't ship a
// "release" that silently locks one platform's users out of auto-updates,
// which is exactly the regression Windows hit pre-2026-05.
//
// Truly unsupported platforms (e.g. freebsd, plan9, linux/386) return
// ok=false so Check short-circuits before touching the network.
func assetName(goos, goarch string) (string, bool) {
	ext := ""
	switch goos {
	case "linux":
		// keep as-is
	case "darwin":
		goos = "macos"
	case "windows":
		// goreleaser appends .exe to Windows binary archives; the manifest
		// row reads `<hash>  codehamr-windows-<arch>.exe` — match that or
		// the asset 404s on download.
		ext = ".exe"
	default:
		return "", false
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", false
	}
	return fmt.Sprintf("codehamr-%s-%s%s", goos, goarch, ext), true
}

// hashFile streams a file through sha256. Used against os.Executable(); a
// ~10MB Go binary hashes in a few ms, so no streaming optimisation needed.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Apply downloads the current platform's binary from the "latest" release,
// verifies its sha256 against the published `codehamr_checksums.txt`, and
// atomically replaces execPath with it. Intended to be called at startup
// from main.go — the caller is expected to syscall.Exec afterwards so the
// running process turns into the new binary without an intermediate
// user-visible restart.
//
// The checksum verification closes the supply-chain hole that an unchecked
// download would leave open: a corrupted CDN response, a TLS-MITM corporate
// proxy, or a release tarball where the binary was swapped but the manifest
// wasn't would all install whatever bytes arrived. With verification, any
// such mismatch returns a clear error before the binary is promoted onto
// the running path.
//
// The temp file is created in the same directory as execPath so os.Rename
// stays an atomic intra-filesystem move. If the directory is read-only
// (typical for `/usr/local/bin` without sudo), os.CreateTemp fails with
// EACCES — the error is returned verbatim so main.go can print a helpful
// hint about rerunning with sudo or using a user-local PREFIX.
//
// ctx governs both fetches; no http.Client.Timeout is set on the binary
// download so the caller's ctx deadline is the only budget.
func Apply(ctx context.Context, execPath string) error {
	asset, ok := assetName(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	expected, err := fetchHash(ctx, asset)
	if err != nil {
		return fmt.Errorf("checksum lookup: %w", err)
	}
	if expected == "" {
		return fmt.Errorf("checksum lookup: no entry for %s in published manifest", asset)
	}
	tmp, err := os.CreateTemp(filepath.Dir(execPath), ".codehamr-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Belt and braces: a deferred Close on the temp file catches every
	// early-return below without each one needing to spell it out, and
	// the deferred Remove cleans up if anything fails before the rename
	// promotes the temp file. After a successful rename tmpPath no longer
	// exists, so os.Remove returns ENOENT and we ignore it. A Close after
	// an explicit Close on *os.File is harmless.
	defer os.Remove(tmpPath)
	defer tmp.Close()

	req, err := http.NewRequestWithContext(ctx, "GET", releaseBase+asset, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: status %d", resp.StatusCode)
	}
	// Stream-hash while writing so we don't need a second full read of
	// the temp file just to verify. MultiWriter fans the bytes to both
	// sinks in lockstep.
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("checksum mismatch: downloaded %s, expected %s", got, expected)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	// Cross-platform rename-aside. Windows blocks MoveFileEx(...,
	// REPLACE_EXISTING) against a running .exe's sharing lock, so the
	// running binary at execPath cannot be overwritten in place — but
	// Windows DOES allow renaming a running .exe to a new name. So we
	// move the current binary aside to execPath+".old" first, then move
	// the verified download into the now-vacant execPath. Unix doesn't
	// need the dance (the kernel keeps the running inode alive across
	// an overwriting rename) but we do it anyway to keep one identical
	// codepath across linux/macos/windows × amd64/arm64 — the same
	// single-codepath discipline the rest of the package follows.
	// CleanupOld, called from main() at next launch, removes the .old
	// once the previous process has released its handle to it.
	oldPath := execPath + ".old"
	// A leftover .old from a prior failed cleanup would make the next
	// Rename fail on Windows (REPLACE_EXISTING against a locked stale
	// file). Remove eagerly; ENOENT is fine.
	_ = os.Remove(oldPath)
	if err := os.Rename(execPath, oldPath); err != nil {
		return err
	}
	if err := promoteRename(tmpPath, execPath); err != nil {
		// Promote attempt failed after we already moved the running
		// binary aside — restore it so the caller still has something
		// to exec, then surface the error.
		_ = os.Rename(oldPath, execPath)
		return err
	}
	return nil
}

// CleanupOld removes the execPath+".old" left behind by a previous Apply.
// Called from main() at the very start of the next launch — on Windows,
// the .old is locked for the lifetime of the previous codehamr process,
// so unlink-at-Apply-time would fail; unlink-at-next-launch always wins.
// Any failure (file missing, permission denied) is silent — a leftover
// .old wastes disk space but never breaks the running session.
func CleanupOld(execPath string) {
	_ = os.Remove(execPath + ".old")
}

// fetchHash downloads codehamr_checksums.txt and returns the hash for the
// given asset name. Goreleaser's default manifest format is one line per
// asset, "<hex-sha256>  <filename>" — we match against the last field so any
// future prefix tweak still works.
//
// A scanner read error mid-manifest used to be silently dropped (we'd
// return "", nil — same shape as "no entry"). After the Apply checksum
// hardening, "no entry" is treated as a fatal mismatch, so quietly turning
// a network glitch into "manifest claims this asset doesn't exist" would
// be a confusing user-facing error. Surface the read error instead.
func fetchHash(ctx context.Context, asset string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", checksumsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("non-200: %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[len(fields)-1] == asset {
			return fields[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", nil
}
