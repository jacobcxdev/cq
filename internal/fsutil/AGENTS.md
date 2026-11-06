<!-- Parent: ../AGENTS.md -->

# fsutil

Filesystem interfaces, secure persistence capabilities and test implementations.

| File family | Responsibility |
| --- | --- |
| `fs.go` | Basic `FileSystem`, durable/secure directory and file interfaces, identity and lock capabilities |
| `secure_*` | Platform-specific no-follow access, private authority, atomic/durable commits and locks |
| `memfs.go` | Mutex-protected files/directories, permissions, owner/inode identity and lock modelling for tests |

- `OSFileSystem` is the production implementation. Request the narrow capability required by the owner; a basic `FileSystem` alone does not prove durable or secure mutation support.
- Credential commits fail closed when required durable/secure capabilities are absent. Preserve retained descriptor identity, exclusive creation, sync and lock requirements.
- `MemFS` implements directories and `ReadDir`; it is not a file-only stub. Use an owner-specific fake/wrapper when tests need injected failures or capabilities it does not model.
- Memory tests do not replace native Unix/Windows ownership, ACL, no-follow, locking or crash-safety checks. Use temporary roots for native filesystem tests.
