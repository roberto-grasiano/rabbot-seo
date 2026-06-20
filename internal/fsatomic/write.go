// Package fsatomic provides a single crash-atomic, power-loss-durable file
// write used wherever Rabbot persists a config or secret-adjacent file.
//
// The write sequence is the well-known temp+fsync+rename dance: create a unique
// temp file in the SAME directory as the target (so the rename is atomic on the
// same filesystem), set its mode, fsync(2) its data to stable storage (Close
// alone does NOT fsync — it only flushes the Go buffer to the kernel, so the
// bytes can still sit in the page cache), then rename it over the target
// (atomic), then fsync the parent directory so the new directory entry is itself
// durable against power loss.
//
// This is durability- and security-sensitive: a SIGKILL or power loss mid-write
// must never leave a truncated config (which would fail to parse on the next
// start) and the final file must always carry the requested owner-only mode even
// if a looser-permissioned file already existed at the path.
package fsatomic

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Write writes data to path atomically and durably.
//
// It creates any missing parent directories at dirMode (an existing directory
// is left untouched — MkdirAll is a no-op there), writes data to a uniquely
// named temp file in the target's directory, sets the temp file's mode to
// fileMode, fsyncs the temp's data, renames it over path (atomic on the same
// filesystem), then best-effort fsyncs the parent directory.
//
// Because the final file is the renamed temp (created/chmodded at fileMode), the
// resulting file always carries exactly fileMode — even when a pre-existing file
// at path had looser permissions, it is tightened. fileMode should be 0o600 for
// configs that may carry inline secrets (webhook URLs, basic-auth creds, the
// control token's neighbours).
//
// The parent-directory fsync is best-effort: some platforms/filesystems
// (notably Windows) refuse to open or fsync a directory, so a failure there is
// ignored — it must not fail an otherwise-completed atomic replace.
func Write(path string, data []byte, fileMode, dirMode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("fsatomic: create dir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".rabbot-atomic-*.tmp")
	if err != nil {
		return fmt.Errorf("fsatomic: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsatomic: write temp: %w", err)
	}
	// Chmod the temp explicitly so the final file is owner-only even if a file
	// already existed at path (it may hold inline secrets).
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsatomic: chmod temp: %w", err)
	}
	// fsync the temp's data+metadata BEFORE the rename: this is what makes the
	// "never truncated after a crash" guarantee hold against power loss, not just
	// process death. Without it the rename can be persisted ahead of the file's
	// data blocks, yielding a zero-length file.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsatomic: fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fsatomic: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("fsatomic: rename temp into %s: %w", path, err)
	}
	// fsync the parent directory so the renamed entry survives power loss.
	// Best-effort: an error here must not fail a completed atomic replace.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
