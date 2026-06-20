package obs

import (
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/roberto-grasiano/rabbot-seo/internal/fsatomic"
)

// assetsFS embeds the provisioned observability bundle — the committed
// docker-compose topology, the Prometheus scrape config, the Grafana datasource
// + dashboard-provider provisioning, and the Rabbot dashboard JSON. Mirrors the
// store package's migrations embed (internal/store/migrations.go): the binary
// carries the bundle so `rabbot observability init` (and the wizard's technical
// path / --with-grafana) can materialise a ready-to-run stack with no network
// and no external files.
//
//go:embed assets/docker-compose.observability.yml assets/prometheus.yml assets/grafana
var assetsFS embed.FS

// assetsRoot is the embed prefix stripped from each path so the bundle is
// written at the destination root (e.g. assets/prometheus.yml -> prometheus.yml).
const assetsRoot = "assets"

// WriteObservabilityBundle materialises the embedded observability bundle into
// dir, recreating the committed directory layout (docker-compose file,
// prometheus.yml, grafana/provisioning/..., grafana/dashboards/rabbot.json). It
// is the single write path shared by every setup route — `rabbot observability
// init`, the wizard's technical path, and `init --with-grafana`.
//
// It only writes files (Rabbot never runs docker — decision 18). Re-running over
// an existing bundle is byte-identical: the bytes come straight from the embed,
// so an agent can safely retry. Each file is written atomically via fsatomic so a
// crash mid-write never leaves a truncated compose/scrape config behind.
func WriteObservabilityBundle(dir string) error {
	return fs.WalkDir(assetsFS, assetsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, rerr := assetsFS.ReadFile(p)
		if rerr != nil {
			return fmt.Errorf("obs: read embedded bundle asset %s: %w", p, rerr)
		}
		rel, rerr := filepath.Rel(assetsRoot, p)
		if rerr != nil {
			return fmt.Errorf("obs: resolve bundle path %s: %w", p, rerr)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		// Bundle files carry no secrets (Grafana stays at stock admin/admin; no
		// generated credentials), so 0o644/0o755 is correct — readable by the
		// operator and by the docker daemon mounting them.
		if werr := fsatomic.Write(dst, data, 0o644, 0o755); werr != nil {
			return fmt.Errorf("obs: write bundle file %s: %w", dst, werr)
		}
		return nil
	})
}
