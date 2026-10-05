---
summary: `mesh mcp install --client claude|codex|cursor|all [--dry-run]` writes each client's stdio MCP entry for `<this binary> mcp` and its instruction files from one table (`mcpinstall.Clients()`), one row per client taken from its documentation on a recorded date — an existing mesh entry updated in place, every other byte kept, an unparseable file reported and left alone, a dry run printing every diff and writing nothing.
status: accepted
---

# ADR-025: `mesh mcp install` is one table per client

2026-10-05, goal `mesh-mcp-installs-into-claude-codex-and-cursor`
(item 2). Numbered 025 beside `adr-025-a-node-says-which-sessions-it-takes.md`
and `adr-025-items-that-share-a-file-are-planned-in-a-line.md`; cite
this one as "ADR-025, mesh mcp install". Amends ADR-023's Skill
section, which wrote Claude Code's skill alone and printed `claude mcp
add`.

## Context

`mesh mcp` is a stdio MCP server any client can run, but `mesh mcp
install` knew only Claude Code: a Codex or Cursor user had to write the
server entry by hand and had no `/mesh` instructions at all. Each
client keeps its servers in its own file and format, and those files
are the person's — other servers, settings, comments — so an install
that round-trips them through a map loses what it does not understand.

## Decision

1. **One table.** `internal/mcpinstall/clients.go`'s `Clients()` returns
   one `Client` row per client in a fixed order — claude, codex,
   cursor: `ConfigDir(home)` (honouring the client's own environment
   variable where its documentation names one), the MCP file
   (`MCPConfig` relative to it, or `MCPFile` when the file sits outside
   it, as Claude's `.claude.json` does), its `Format` (JSON or TOML),
   the entry's JSON path `MCPKeys` or `TOMLTable`, its `Instructions`
   (`InstructionFile{Path, Kind whole|marked-section, Marker, Body}`),
   and the `Binaries` `Detect` looks for on PATH beside the config
   dir. A fourth client is one more row.
2. **One source.** Every entry is `Entry(exe, remote)`: the absolute
   path of the running mesh binary with `args ["mcp"]` — JSON
   `{"type": "stdio", "command": …, "args": ["mcp"]}` (`type` because
   both Claude Code's and Cursor's documented stdio examples carry it),
   TOML `command = "…"` / `args = ["mcp"]` quoted as the Codex harness
   quotes (`TOMLString`). Every instruction file renders from
   `meshmcp.Skill()`; a row whose `Body` is nil is recorded here and
   written by item 3 of the goal. `--remote` (item 4) is
   `ErrRemoteNotBuilt` until then.
3. **Never clobber.** `Plan(home, exe, clients, opts)` reads each file
   and upserts with item 1's `UpsertJSONFile`/`UpsertTOMLTableFile`:
   an existing `mesh` entry's span replaced in place, every other byte
   kept. A file its parser cannot read is an `*UnparseableError`,
   printed `<path>: cannot parse (<err>); left alone`; a whole
   instruction file that differs from what mesh-cli ships is a
   `*DiffersError` unless `--force`. Either leaves that file out; the
   other files and clients are still written and the command exits 1.
   `Apply` writes only changed files (`node.WriteFile`, 0644 for a new
   file, directories made), so a re-run touches nothing and reports
   every file `unchanged`; otherwise `wrote` (absent before) or
   `updated`.
4. **Dry run.** `--dry-run` prints `Diff` for every change — the same
   `---`/`+++` header and `+` lines the real run would apply — then
   `would write <path>`, and writes nothing.
5. **Which clients.** `--client` is repeatable or a comma list of
   claude, codex, cursor or all; left out, every client `Detect` finds
   (config dir present, or a binary on PATH), and none found is `no
   client found; pass --client`. The `claude mcp add` line is printed
   only when the Claude row was chosen and its entry could not be
   written.

## The table

Fetched 2026-10-05; each row is what the client's documentation said
that day.

| Client | MCP config path | Format | Entry | Instruction files (kind) | Detect | Documentation (fetched) |
|---|---|---|---|---|---|---|
| claude | `$CLAUDE_CONFIG_DIR/.claude.json`, else `~/.claude.json` (user scope) | JSON | `mcpServers.mesh` = `{"type":"stdio","command":"<exe>","args":["mcp"]}` | `$CLAUDE_CONFIG_DIR/skills/mesh/SKILL.md`, else `~/.claude/skills/mesh/SKILL.md` (whole) | `~/.claude` (or `$CLAUDE_CONFIG_DIR`) exists, or `claude` on PATH | https://code.claude.com/docs/en/mcp, https://code.claude.com/docs/en/skills (2026-10-05) |
| codex | `$CODEX_HOME/config.toml`, else `~/.codex/config.toml` | TOML | `[mcp_servers.mesh]` `command = "<exe>"`, `args = ["mcp"]` | `$CODEX_HOME/prompts/mesh.md` (whole; invoked `/prompts:mesh`), `$CODEX_HOME/AGENTS.md` (marked section `mesh`) | `$CODEX_HOME` (or `~/.codex`) exists, or `codex` on PATH | https://learn.chatgpt.com/docs/extend/mcp?surface=cli, https://learn.chatgpt.com/docs/custom-prompts, https://learn.chatgpt.com/docs/agent-configuration/agents-md (2026-10-05) |
| cursor | `~/.cursor/mcp.json` | JSON | `mcpServers.mesh` = `{"type":"stdio","command":"<exe>","args":["mcp"]}` | `~/.cursor/skills/mesh/SKILL.md` (whole), `~/.cursor/commands/mesh.md` (whole) | `~/.cursor` exists, or `cursor`/`cursor-agent` on PATH | https://cursor.com/docs/context/mcp, https://cursor.com/docs/context/skills, https://cursor.com/docs/context/rules (2026-10-05) |

What the documentation said that shapes the rows:

- **Claude Code**: user-scope servers are stored in `~/.claude.json`
  under `mcpServers`, and the configuration directory moves with
  `CLAUDE_CONFIG_DIR`; personal skills are
  `~/.claude/skills/<name>/SKILL.md`. Writing the entry directly is
  what `claude mcp add --scope user` does, so the old hint is no
  longer needed.
- **Codex**: `[mcp_servers.<name>]` with `command`/`args`/`env`, or
  `url` with `bearer_token_env_var`/`http_headers` (the shape item 4's
  `--remote` writes); `CODEX_HOME` moves the global `AGENTS.md` (an
  `AGENTS.override.md` there wins). Custom prompts live in
  `~/.codex/prompts/` and are documented as **deprecated** in favour of
  skills, which Codex reads from `~/.agents/skills` — outside
  `CODEX_HOME`; the prompt row stands as the goal asked, and moving it
  to a skill is one row change.
- **Cursor**: `~/.cursor/mcp.json` under `mcpServers`, stdio
  `type`/`command`/`args`/`env` or remote `url`/`headers` with
  `${env:NAME}` interpolation; no environment variable moves it.
  **User rules are not files** — they live in Customize → Rules — so
  the goal's "user-level rule" is a user skill at
  `~/.cursor/skills/<name>/SKILL.md` (`name` and `description`
  frontmatter), which the docs name as the global path; custom
  commands are being migrated to skills (`/migrate-to-skills`) and the
  docs no longer name `~/.cursor/commands`, which the row keeps for
  Cursor versions before it.

## Consequences

- A client's file format or path changing is one row and one date
  here, not new code.
- Writing `~/.claude.json` directly races Claude Code rewriting it;
  the upsert is a read-modify-write of one span, and a person running
  install while Claude Code is open may need to run it again.
- Instruction files other than Claude's skill are rows without a Body
  until item 3; `mesh mcp doctor` (item 5) reads the same table.
