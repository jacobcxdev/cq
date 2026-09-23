# Example review record

These checks concern authored design fixtures, not deployed behaviour.

- All 89 operational commands, 37 command groups and root help have concrete examples.
- Every help example includes its catalogue-defined local options and all three global options.
- Every command has a representative failure. All available operational commands have a success example; four unavailable candidate transitions intentionally have no invented success.
- Stdout, stderr, invocation and exit status are recorded separately. Pending confirmation is labelled pending, not assigned a completed exit status.
- Output blocks are exported verbatim with source line and SHA-256. Three full shell-completion files match the immutable catalogue exactly.
- 324 distinct authored CQ invocations produced no unexpected parser rejection; intentional exit-2 examples were permitted. This called `cli.Parse`, never an operational handler. The helper ran with the race detector.
- Bash and Zsh completion syntax checks passed; raw Fish bytes match the catalogue. Fish runtime execution was not required for this design task.
- Source review corrected reserve freshness and selectors, exact diagnostics, readiness constants, canary IDs and day buckets, candidate generations and evidence availability, plus partial-result cause details.
- The viewer pairs all 127 command/help pages with examples. Its embedded data and JavaScript syntax were checked. Browser security policy previously blocked local-file automation, so no visual browser verification is claimed.

The examples do not exercise live credentials, credits, services or routes. Product code and installed binaries were not changed for this example-authoring task. Renderer implementation must still test these targets against the relevant domain inputs and width/colour matrix.

## Operational-output discovery correction

- Output, Help and Specification now have separate viewer tabs. Operational commands default to actual result output, with a scenario selector.
- Initial viewer selection is the complete all-account `cq codex reset list` example.
- `operational-outputs.md` collects every command's authored operational scenarios without help blocks.
- Coverage checks verified all 89 commands have operational cases and all 127 help pages retain separate help examples.
- Exports contain 496 fixtures, including the additional all-account reset-list result and three full raw completion scripts.
- Viewer data, navigation source and JavaScript syntax were checked; browser rendering remains unverified.
