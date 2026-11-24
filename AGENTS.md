# Repository working agreement

This repository treats requirements, ontology, tests, and Core contracts as one change set.

## Development quality ownership

The coordinating agent acts as development PM and owns overall quality. For each change or incident, the PM defines the observable acceptance criteria and assigns the affected areas to their lead agents.

| Development lead | Quality responsibility |
| --- | --- |
| FFmpeg and rendering | Reproduce decoding and encoding failures, verify render profiles, markers, QC, and retry behavior. |
| Classification and folder mapping | Verify source classification, grouping and holds across drive layouts. Compare the destination preview with the paths actually published. |
| Metadata extraction and preservation | Trace ffprobe measurements and checksums from the original through package metadata to the destination catalog. Check missing records and reconciliation. |
| STT and subtitles | Verify speech extraction, chunk boundaries, subtitle timing, transcription quality evidence, and recovery from interrupted work. |

Each lead reports the affected requirement, what they reproduced or verified, the supporting test or artifact, and any open defect. The PM resolves ownership across areas, checks that one stage's output works as the next stage's input, and reports integration results and release readiness to the user.

## Before changing behavior

- Read `requirements/catalog.json`, `requirements/README.md`, `ontology/terms.json`, and the owning file under `docs/design/`.
- Preserve requirement IDs. Add one observable Given/When/Then rule and exactly one annotated test for each new implemented requirement.
- Keep hardware- or adapter-dependent behavior `planned` until its real integration or acceptance test exists.
- Tests exist to verify observable requirements, not to raise coverage or freeze incidental implementation details. Do not add behavior outside a requirement merely to satisfy a test.
- When an integration test exposes a technology constraint or a demonstrably better design, revise the owning design, requirement meaning, semantic lock, and its one test together. Do not preserve an obsolete requirement by distorting the Adapter.
- If requirement meaning changes, review the change and then run `make requirements-lock`. Do not update the semantic lock only to silence a failure.

## Core invariants

- A Command Handler returns a staged state change and Events. It must not persist or mutate stored aggregate state directly.
- State, CommandRecord, Events, and idempotency receipt commit through the `command.Store` Unit of Work. Only deterministic failures get failure receipts.
- Every StepState change goes through the workflow transition guard. Preserve the reported WaitReason; do not infer every resource wait as MEMORY.
- Shared enums and value types live in `internal/core/vocabulary`. Do not add package-local string definitions for ActorType, UserRole, WaitReason, ProcessingKind, ResourceClass, ExecutorKind, or ApprovalDecision.
- Approval and destructive Confirmation are immutable records, not booleans. The latest Approval is selected by persisted sequence, not wall-clock time.
- ResourceClass says how expensive a Step is; ExecutorKind says who performs it. Do not infer one from the other.
- Actual storage file access must use `archive.StorageRoot`; do not validate a string path and later reopen it with unrestricted `os` calls.
- External subprocesses receive executable plus argument arrays, write temporary outputs, and publish only after verification.

## Vocabulary

- `ontology/terms.json` is the terminology source. `global_forbidden_words` are mechanically forbidden in Markdown; per-term `forbidden.words` are contextual aliases.
- Add an ontology open question when a term still has competing meanings. Move it to a decided term only when code, requirements, and design documents use the same meaning.
- ActorType and UserRole are separate axes. Only HUMAN principals have OPERATOR or ADMIN roles.

## Verification

Run before handing off a change:

```bash
make verify
go vet ./...
```

`make verify` checks semantic requirement fingerprints, 1:1 requirement tests, ontology consistency, forbidden identifiers/words, shared-vocabulary architecture, and all Go tests.
