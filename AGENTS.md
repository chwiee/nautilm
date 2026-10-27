# AGENTS.md

Instructions for AI coding agents (Codex, Copilot, Kiro, Cursor, Claude Code…).

The full project context is in **[.claude/CLAUDE.md](.claude/CLAUDE.md)** (Brazilian Portuguese).
Read it first; it links to architecture, configuration, security, conventions and the
**organization deployment runbook** ([docs/IMPLANTACAO-EMPRESA.md](docs/IMPLANTACAO-EMPRESA.md)).

Short version:

- Go only. Code, config, logs and UI text in English; docs in Brazilian Portuguese.
- Never put secrets, account IDs, tenant IDs or group IDs in the repository or the database.
- Only nautilm calls the model providers; agents get no Bedrock IAM permission.
- The YAML config is strict: unknown keys stop the startup.
- Check with `gofmt -l . && go vet ./... && go test ./...` before finishing any change.
- Steps that change AWS IAM, Entra ID or paid infrastructure are done by a human: prepare and explain them.
