package mcpsrv

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/roberto-grasiano/rabbot-seo/internal/fsatomic"
)

// Connect-Claude generates the MCP client config that points an MCP host (Claude
// Desktop / Claude Code, or a project .mcp.json) at this binary's stdio MCP server.
// All targets share ONE launch spec — `<binary> mcp` over stdio — which is the seam
// that keeps working unchanged after Spec 2 grows the server. The control token is
// NEVER embedded: the entry has no env block; the server reads the token file at
// runtime.

// Target identifies where the Connect-Claude config is written (or whether it is
// only printed).
type Target int

const (
	// TargetPrint emits the snippet for the user to copy; it writes no file.
	TargetPrint Target = iota
	// TargetProject writes ./.mcp.json (Claude Code project scope).
	TargetProject
	// TargetClaudeCode writes the same ./.mcp.json mcpServers map as project scope.
	// (`claude mcp add` is the CLI alternative; we do not shell out to it.)
	TargetClaudeCode
	// TargetClaudeDesktop writes the per-OS Claude Desktop config.
	TargetClaudeDesktop
)

const (
	// connectFileMode is owner-only: the file lists a launch command but lives
	// alongside other user configs that may carry secrets, so 0600 is the safe
	// posture, matching control.token / config.yaml.
	connectFileMode = 0o600
	// connectDirMode is owner-only for any parent dir we create.
	connectDirMode = 0o700

	// serverKey is the mcpServers entry name we own; merges touch only this key.
	serverKey = "rabbot"
	// mcpServersKey is the top-level map holding all MCP server entries.
	mcpServersKey = "mcpServers"
)

// ParseTarget maps a flag value (print|project|claude-code|claude-desktop) to a
// Target, erroring on an unknown value.
func ParseTarget(s string) (Target, error) {
	switch s {
	case "print":
		return TargetPrint, nil
	case "project":
		return TargetProject, nil
	case "claude-code":
		return TargetClaudeCode, nil
	case "claude-desktop":
		return TargetClaudeDesktop, nil
	default:
		return TargetPrint, fmt.Errorf("unknown connect target %q (want print|project|claude-code|claude-desktop)", s)
	}
}

// serverEntry is the JSON value for our mcpServers["rabbot"] entry: the launch
// command + args, and deliberately nothing else (no env, no token). dataDir and
// configPath are baked into the args ONLY when non-empty. NOTE: only configPath
// would affect Hop-2 reachability (the control.token lives in the config dir, and
// the child reads it from there keyed off --config); dataDir does NOT — the child
// no longer opens the DB, so --data-dir touches neither the token nor the control
// port and is baked for forward-compat only. A default-dir entry is byte-identical
// to the original {command, args:["mcp"]}.
func serverEntry(bin, dataDir, configPath string) map[string]any {
	args := []any{"mcp"}
	if dataDir != "" {
		args = append(args, "--data-dir", dataDir)
	}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	return map[string]any{
		"command": bin,
		"args":    args,
	}
}

// Snippet returns the canonical, copy-pasteable mcpServers config that launches
// `bin mcp` over stdio, indented with two spaces. It contains no token/env.
func Snippet(bin string) string {
	doc := map[string]any{
		mcpServersKey: map[string]any{
			serverKey: serverEntry(bin, "", ""),
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		// The doc is a fixed, marshalable shape; this is unreachable.
		return "{}"
	}
	return string(b)
}

// SnippetWithDirs is Snippet with optional --data-dir/--config baked into the
// launch args, for a daemon running under a non-default data/config dir. Empty
// strings bake nothing, so SnippetWithDirs(bin, "", "") == Snippet(bin).
func SnippetWithDirs(bin, dataDir, configPath string) string {
	doc := map[string]any{
		mcpServersKey: map[string]any{
			serverKey: serverEntry(bin, dataDir, configPath),
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// defaultRemoteBin is the binary name assumed on the remote host's PATH when the
// user does not override it with --connect-remote-bin. os.Executable() only knows
// the LOCAL path, which is meaningless on the VPS, so we cannot bake it.
const defaultRemoteBin = "rabbot"

// remoteServerEntry is the SSH-transport variant of serverEntry: Claude launches
// `ssh <dest> <remoteBin> mcp`, so the mcp child runs ON the VPS beside the
// daemon — loopback + control.token never leave the box (D9). No env, no token.
func remoteServerEntry(dest, remoteBin string) map[string]any {
	if remoteBin == "" {
		remoteBin = defaultRemoteBin
	}
	return map[string]any{
		"command": "ssh",
		"args":    []any{dest, remoteBin, "mcp"},
	}
}

// RemoteSnippet returns the copyable SSH-transport mcpServers config for a remote
// daemon reached over SSH at dest (e.g. "you@vps"). It carries no token/env; the
// token stays on the VPS and is read by the child there.
func RemoteSnippet(dest, remoteBin string) string {
	doc := map[string]any{
		mcpServersKey: map[string]any{
			serverKey: remoteServerEntry(dest, remoteBin),
		},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ResolveBinary returns the path to use as the MCP launch command: the running
// executable (so the written config points at the actually-installed binary),
// falling back to "rabbot" on PATH if os.Executable fails.
func ResolveBinary() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "rabbot"
}

// TargetPath resolves the on-disk path for a writable Target. TargetPrint has no
// path and returns an error. Unlike WriteConfig, the path does not depend on the
// binary, so TargetPath takes no bin argument.
func TargetPath(t Target) (string, error) {
	switch t {
	case TargetProject, TargetClaudeCode:
		// Project / Claude Code project scope: ./.mcp.json (cwd-relative).
		return filepath.Join(".", ".mcp.json"), nil
	case TargetClaudeDesktop:
		return claudeDesktopPath()
	case TargetPrint:
		return "", fmt.Errorf("print target has no file path")
	default:
		return "", fmt.Errorf("unknown target")
	}
}

// claudeDesktopPath resolves the per-OS Claude Desktop config path:
//
//	macOS:   ~/Library/Application Support/Claude/claude_desktop_config.json
//	Windows: %APPDATA%/Claude/claude_desktop_config.json
//	Linux:   ~/.config/Claude/claude_desktop_config.json
//
// It uses os.UserConfigDir (which honors %APPDATA% on Windows and
// ~/Library/Application Support on macOS / $XDG_CONFIG_HOME on Linux) so the path
// is correct without hardcoding a single OS.
func claudeDesktopPath() (string, error) {
	const file = "claude_desktop_config.json"
	// os.UserConfigDir resolves correctly on every supported OS:
	//   macOS   -> ~/Library/Application Support
	//   Windows -> %APPDATA%
	//   Linux   -> $XDG_CONFIG_HOME (default ~/.config)
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "Claude", file), nil
}

// WriteConfig merge-writes the LOCAL stdio rabbot entry (`<bin> mcp` with
// optional baked dirs) into the MCP host config at path. See writeServerEntry for
// the merge/atomic-write contract.
func WriteConfig(path string, bin string) error {
	return writeServerEntry(path, serverEntry(bin, "", ""))
}

// WriteConfigWithDirs is WriteConfig with non-default --data-dir/--config baked
// into the launch args, for a daemon under a custom dir.
func WriteConfigWithDirs(path, bin, dataDir, configPath string) error {
	return writeServerEntry(path, serverEntry(bin, dataDir, configPath))
}

// WriteRemoteConfig merge-writes the SSH-transport rabbot entry into the MCP
// host config at path, preserving siblings and unrelated keys exactly like
// WriteConfig (atomic temp+fsync+rename, dirs 0700 / file 0600).
func WriteRemoteConfig(path, dest, remoteBin string) error {
	return writeServerEntry(path, remoteServerEntry(dest, remoteBin))
}

// writeServerEntry is the shared merge+atomic-write core: it loads the existing
// JSON (if any) into a map, ensures the mcpServers sub-map, sets ONLY the
// rabbot key to entry, and writes the result back atomically. It MUST NOT
// clobber unrelated top-level keys or sibling servers. Parent dirs are created
// 0700 and the file is written 0600 via temp+fsync+rename so a crash never
// truncates the user's Claude config.
func writeServerEntry(path string, entry map[string]any) error {
	// Load existing document, or start fresh.
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("mcp: parse existing config %s: %w", path, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("mcp: read %s: %w", path, err)
	}
	// A config file whose entire content is the JSON literal `null` unmarshals
	// into a nil map; without this guard the later `doc[mcpServersKey] = servers`
	// write would panic on assignment to a nil map.
	if doc == nil {
		doc = map[string]any{}
	}

	// Ensure/merge the mcpServers sub-map without disturbing unrelated keys.
	servers, _ := doc[mcpServersKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	// Set only our entry; sibling servers are preserved.
	servers[serverKey] = entry
	doc[mcpServersKey] = servers

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("mcp: marshal config: %w", err)
	}
	out = append(out, '\n')

	// Atomic+durable temp+fsync+rename so a crash mid-write never truncates the
	// user's Claude config. Parent dirs we create are 0700; the file is 0600
	// (lives beside other configs that may carry secrets). See internal/fsatomic.
	return fsatomic.Write(path, out, connectFileMode, connectDirMode)
}
