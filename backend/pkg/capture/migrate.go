package capture

import (
	"os"
	"path/filepath"
)

// Migrate rewrites archives written by the previous CAS layout (bytes under
// blobs/<aa>/<sha256>, items without `file`) into the readable files/ tree.
// It is idempotent: items whose file already exists on disk are left alone, and
// an archive only has meta.json and index.txt rewritten when something changed.
func (m *Manager) Migrate() {
	if !m.Enabled() {
		return
	}
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validArchiveID(entry.Name()) {
			continue
		}
		id := entry.Name()
		files, changed, err := m.migrateArchive(id)
		if err != nil {
			m.log.Debug().Err(err).Str("archive", id).Msg("capture: migrate")
			continue
		}
		if files == 0 && !changed {
			continue
		}
		m.log.Info().Str("archive", id).Int("files", files).Msg("capture: migrated")
	}
}

func (m *Manager) migrateArchive(id string) (int, bool, error) {
	dir := archiveDir(m.cfg.Dir, id)
	meta, err := readMeta(dir)
	if err != nil {
		return 0, false, err
	}

	// Paths already backed by a readable file keep their names, so a derived
	// path that collides with them gets the version suffix instead.
	paths := make(map[string]string, len(meta.Items))
	for i := range meta.Items {
		it := &meta.Items[i]
		if it.State != "captured" || !validSHA256(it.SHA256) {
			continue
		}
		if clean, target, ok := recordedFile(dir, it.File); ok && fileExists(target) {
			paths[clean] = it.SHA256
		}
	}

	files := 0
	changed := false
	for i := range meta.Items {
		it := &meta.Items[i]
		if it.State != "captured" || !validSHA256(it.SHA256) {
			continue
		}
		if _, target, ok := recordedFile(dir, it.File); ok && fileExists(target) {
			continue
		}
		base, err := deriveRel(it.URL, it.FinalURL, it.Mime)
		if err != nil {
			continue
		}
		rel := resolvePath(paths, base, it.SHA256)
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if !fileExists(target) {
			src := blobPath(dir, it.SHA256)
			if !fileExists(src) {
				// Legacy bytes are gone; leave the item untouched.
				continue
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, changed, err
			}
			if err := os.Link(src, target); err != nil {
				data, readErr := os.ReadFile(src)
				if readErr != nil {
					return files, changed, readErr
				}
				if err := storeBytes(dir, rel, data); err != nil {
					return files, changed, err
				}
			}
			if err := os.Chmod(target, 0o644); err != nil {
				return files, changed, err
			}
		}
		paths[rel] = it.SHA256
		files++
		if it.File != rel {
			it.File = rel
			changed = true
		}
	}

	// Drop the legacy CAS tree only once every captured body is readable under
	// files/; a partial migration keeps it so Blob's fallback still works.
	if completeReadableTree(dir, meta.Items) {
		if fileExists(filepath.Join(dir, "blobs")) {
			if err := os.RemoveAll(filepath.Join(dir, "blobs")); err != nil {
				return files, changed, err
			}
		}
	} else {
		m.log.Debug().Str("archive", id).Msg("capture: legacy blobs kept (incomplete migration)")
	}

	if !changed {
		return files, false, nil
	}
	if err := writeMeta(dir, meta); err != nil {
		return files, true, err
	}
	if err := writeIndex(dir, meta.Items); err != nil {
		return files, true, err
	}
	return files, true, nil
}

// completeReadableTree reports whether every captured item has a `file` that
// exists under files/.
func completeReadableTree(dir string, items []Item) bool {
	for i := range items {
		it := &items[i]
		if it.State != "captured" || !validSHA256(it.SHA256) {
			continue
		}
		if _, target, ok := recordedFile(dir, it.File); !ok || !fileExists(target) {
			return false
		}
	}
	return true
}

// recordedFile resolves a meta item's file to a path inside the archive and
// reports whether it is a well-formed files/... path.
func recordedFile(dir, file string) (clean, target string, ok bool) {
	clean, err := validFileRel(file)
	if err != nil {
		return "", "", false
	}
	return clean, filepath.Join(dir, filepath.FromSlash(clean)), true
}
