# Current checkout versus released v0.32.5 CLI surface

Released source adds:

- `proxy reserve {set,disable,enable,clear,status,windows}` and their port/JSON/window/percent flags.
- `proxy leases invalidate --port`.
- `proxy trace` with session/trace/since/tail/follow/json/payload flags.
- `service {install,restart,status,uninstall}` plus hidden `snapshot`/`restore`; owner, stable executable, snapshot-file and inherited installer lock package ABI.
- Separate `cq-install` executable for installation/uninstallation.
- Linux `proxy __linux-acceptance-helper` and `proxy start --linux-validation-candidate-fd 3`.

Both sources already contain candidate `__runtime`, runtime-role descriptor options, `--migrate-legacy-managed`, Codex Stop hook, account/reset/model/policy/candidate/endpoint/operation families, and validate-http required non-live `--port`. v0.32.5 expands Linux runtime/validation implementations and cleanup but does not newly add validate-http port syntax.

Released ordinary commands no longer call implicit `ensureAgent` after completion; the reviewed checkout still does. Therefore previous uninstall-resurrection finding is version-specific and should not be carried into v0.32.5 requirements as an unfixed fact.

Inventory records 92 command/mode entries, including bare `proxy pin` (displays both pins) and 2 cq-install actions. Global `--help/-h`, `--version/-v`, `--json/-j`, `--refresh/-r` and legacy help aliases are parser surfaces rather than additional leaves. Old globals are not consistently accepted by manual dispatchers; the canonical public spec must explicitly normalise them.

`cmd/cq/main.go`, `cmd/cq/proxy.go`, `cmd/cq/help.go`, `cmd/cq/proxy_commands.go`, and new v0.32.5 files were compared directly. No repository changes made.
