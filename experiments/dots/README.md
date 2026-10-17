# Isolated multi-account Dots experiment

This Python experiment exposes two existing account-owned Dots through one unmodified native Codex app. It is separate from CQ's supported CLI and service. It uses private native endpoints and a pinned renderer; an app update requires fresh qualification.

The runtime routes cloud sessions, room requests, text sends, receipts, thinking status, Pause/Resume and computer signalling to each resource's owner. Native request authentication is checked against the real backend before owner credentials are substituted. Read verification caches for at most 60 seconds; HTTP state mutation is disabled and request bodies are bounded.

Explicit Dot-authored CQ blocks can relay in both directions with source attribution, a correlation ID and at most one request plus one reply. Normal history, memories and connectors are not mirrored. Cloud plugins, memories and connector authorisations remain account-owned. The native sender identity remains the signed-in account.

## Scope and limits

This preserves the existing two-owner experiment. It does not create Dots, remote executors or additional onboarding slots. Native cross-account `@` selection and persistent secondary Reboot remain unsupported; Reboot proof code stays disarmed.

The existing secondary research executor can appear Connected. Fresh local-task execution and its nested macOS sandbox remain unqualified. The coordination relay does not require a fresh local task.

The relay polls the last 20 messages every five seconds and can miss messages after an outage or heavy traffic. Unknown send outcomes are not retried automatically. Account removal or credential revocation requires stopping the isolated runtime; removing an account from CQ does not flush already-open owner sessions.

## Private setup

Python `3.14` and `websockets==17.2` are required in the existing private runtime's `venv`. That directory must already contain the experiment's certificates, port file and isolated user data. These lifecycle commands do not install dependencies or copy credentials. There is no first-run provisioning or onboarding workflow in this candidate.

Copy [settings.example.json](runtime/settings.example.json) to a private directory outside the checkout. Set that directory to mode `0700` and the configuration file to `0600`; both must belong to the current user. Fill in the local paths, two existing owners and native profile/root-thread IDs. Set `CQ_DOTS_CONFIG` to the configuration's absolute path. All children inherit this environment variable.

`runtime_root` must be outside the source checkout, native app bundle, account store and production Codex data. The configuration loader rejects overlapping paths. Keep the private configuration outside the public candidate and Git. The candidate includes only an explicit list of Python sources plus a deployment manifest.

Pin `app_version`, `app_build`, `asar_sha256` and `module_asset` to the exact native app qualified for the experiment. The hash covers `Contents/Resources/app.asar`; `module_asset` is the basename of its qualified `app-shared-*.js` module. The example's placeholder values are not a tested pin. Preflight checks the installed app's version, build and renderer hash; it cannot establish that an arbitrary replacement pin works.

## Candidate, rollout and rollback

Use the existing private runtime interpreter. Replace the example absolute paths below with your local paths. Run the commands from this directory. Candidate manifests include local paths and should stay private.

```sh
export CQ_DOTS_CONFIG=/absolute/private/config/settings.json
PY=/absolute/private/runtime/venv/bin/python
CANDIDATE=/absolute/private/candidates/dots-candidate
"$PY" release.py prepare "$CANDIDATE"
"$PY" release.py verify "$CANDIDATE"
"$PY" release.py preflight "$CANDIDATE"
```

Once the candidate has been reviewed and its native pin qualified:

```sh
"$PY" control.py stop
"$PY" release.py activate "$CANDIDATE"
"$PY" control.py start "$CANDIDATE"
"$PY" control.py status
```

Activation refuses running isolated processes, saves allowlisted sources in a private rollback directory and replaces each source atomically. It preserves credentials, user data, certificates, message ledgers and executor registration. Startup checks installed source hashes and native catalog access. Catalog readiness proves reads; message delivery, owner routing and computer video need separate live canaries.

Activation prints the exact rollback directory. Stop the isolated runtime before restoring it:

```sh
"$PY" control.py stop
"$PY" release.py rollback /absolute/private/runtime/rollback-EXACT_ID
```

Restart with the previous matching candidate. Never start a candidate whose installed source hashes differ. Controls verify process ownership, executable, script location and isolated data or working directory before signalling. They do not force termination. These operations do not patch or resign the native app, change the normal CQ service, refresh system credentials or register/delete a remote executor.

## Offline validation

These tests use temporary files and fake processes; they do not launch the native app or call live accounts.

```sh
"$PY" -m unittest discover -s runtime -q
"$PY" -m unittest discover -s tests -q
```

For changes that also touch Go code, run the project checks from the repository root:

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
```
