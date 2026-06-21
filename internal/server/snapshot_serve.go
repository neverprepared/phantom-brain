package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// handleSnapshotCurrent serves the current snapshot's manifest as
// JSON. The brain's birth flow calls this to learn the gen + sha
// before deciding whether to claim it. Tarball download is a
// separate GET against /snapshot/{gen}/tarball so this handler stays
// cheap.
//
// Returns an empty-gen-0 manifest with status 200 when the vault has
// never been snapshotted; the brain interprets that as "greenfield
// seed".
func (d *Daemon) handleSnapshotCurrent(w http.ResponseWriter, r *http.Request) {
	binding, ok := BindingFromContext(r.Context())
	if !ok {
		WriteErrorEnvelope(w, http.StatusUnauthorized, ErrCodeInvalidToken, "no binding on context (middleware misconfigured)", nil)
		return
	}
	info, err := CurrentSnapshot(d.DataDir, binding.Key.Profile, binding.Key.Vault)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeInternal, err.Error(), nil)
		return
	}
	if info == nil {
		// Brand-new vault — return a sentinel gen=0 manifest. Phase 2.5
		// agents treat gen=0 as "greenfield, no parent".
		writeJSON(w, http.StatusOK, SnapshotManifest{
			Profile: binding.Key.Profile,
			Vault:   binding.Key.Vault,
			Gen:     0,
		})
		return
	}
	writeJSON(w, http.StatusOK, info.Manifest)
}

// handleSnapshotByGen serves the manifest for a specific gen. 404
// NOT_FOUND when the gen is unknown (pruned or never existed).
func (d *Daemon) handleSnapshotByGen(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())
	genStr := chi.URLParam(r, "gen")
	gen, err := strconv.ParseUint(genStr, 10, 64)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "gen must be a positive integer", nil)
		return
	}
	m, err := LoadSnapshotManifest(d.DataDir, binding.Key.Profile, binding.Key.Vault, gen)
	if errors.Is(err, os.ErrNotExist) {
		WriteErrorEnvelope(w, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("snapshot gen=%d not found", gen), nil)
		return
	}
	if err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeInternal, err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleSnapshotTarball streams the tar.zst for a specific gen.
// Content-Type set to application/zstd so curl + browser save with
// a sane suffix. Range requests are NOT supported in Phase 2 — the
// tarball is meant to be downloaded in one shot during birth.
func (d *Daemon) handleSnapshotTarball(w http.ResponseWriter, r *http.Request) {
	binding, _ := BindingFromContext(r.Context())
	genStr := chi.URLParam(r, "gen")
	gen, err := strconv.ParseUint(genStr, 10, 64)
	if err != nil {
		WriteErrorEnvelope(w, http.StatusBadRequest, ErrCodeBadRequest, "gen must be a positive integer", nil)
		return
	}
	tarballPath := d.tarballPath(binding.Key.Profile, binding.Key.Vault, gen)
	f, err := os.Open(tarballPath)
	if errors.Is(err, os.ErrNotExist) {
		WriteErrorEnvelope(w, http.StatusNotFound, ErrCodeNotFound, fmt.Sprintf("snapshot gen=%d tarball not found", gen), nil)
		return
	}
	if err != nil {
		WriteErrorEnvelope(w, http.StatusInternalServerError, ErrCodeInternal, err.Error(), nil)
		return
	}
	defer f.Close()
	st, _ := f.Stat()

	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="snapshot-%d.tar.zst"`, gen))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// tarballPath is a tiny helper so handlers don't repeat the
// filepath.Join + fmt.Sprintf incantation.
func (d *Daemon) tarballPath(profile, vault string, gen uint64) string {
	return fmt.Sprintf("%s/snapshot-%d.tar.zst", d.DataDir.PublishedDir(profile, vault), gen)
}
