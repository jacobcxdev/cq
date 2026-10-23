<!-- Parent: ../../AGENTS.md -->

# cmd/cq

CLI entry point using the generated `internal/cli` catalogue and canonical v2 parser.

## Key Files

| File | Description |
|------|-------------|
| `main.go` | Frozen machine ABI classification, signal context and session wiring |
| `cli_v2.go` | Exact leaf registry, family composition and scheduled refresh hook |
| `cli_v2_check.go` | Provider/cache/renderer dependencies for quota checks |
| `cli_v2_integration_test.go` | Registry, pure help, executable dispatch and composition checks |

## Working In This Directory

- `specs/cli-v2/commands.json` owns public grammar; regenerate `internal/cli/catalogue_gen.go` after catalogue or generator changes.
- `main` classifies frozen machine ABI first. Public parse failures never fall back to legacy engines.
- The `check` adapter wires providers, cache and renderer; cache failure remains non-fatal.
- Provider IDs come from `provider.Ordered`; preserve the release-linked Gemini OAuth secret.
- Keep help, version, groups and invalid syntax before environment, credential or service access.
- Tests inject explicit credential home/state/cache roots, including on Windows; native defaults are not isolated by setting HOME alone.
