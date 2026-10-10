# ADR-0011: Cross-version safety for run state

## Status
Accepted, amended 2026-10-09 (the capabilities that now exist)

*Amended 2026-10-09:* "The set of known capabilities is empty today and nothing sets `requires`" (Decision, below) no longer holds. Four capabilities exist, each set only by the call that records its field: `output.branch` on the manifest of a run whose output is a branch (`Manifest.SetOutput`), `policy.gate` on the manifest of a run with a policy (`Manifest.SetPolicy`, ADR-0014), `fanout.candidate-runs` on the manifest of a run that fans out and of each of its candidate runs (`Manifest.SetFanout`, `Manifest.SetCandidate`, ADR-0019), and `plan.regenerate` on a state.json that carries a request for a new plan not yet served. A run that edits in place with none of these still requires nothing.

## Context
A run lives on disk as two JSON documents: manifest.json (identity, lifecycle, budget ledger) and state.json (the blackboard). Both carry a `schema_version`, and every release so far, v0.1.0 to v0.1.4, reads version 1 and refuses any other. Neither is decoded with `DisallowUnknownFields`: a key the binary does not know is dropped without a word.

That was harmless while every release wrote the same fields. The next slices add fields that change what a run does: where its output goes (a branch rather than the user's checkout), the baseline a policy is judged against, a request to regenerate the plan, whether the winner has been promoted. A binary that predates such a field would decode the run, drop the field, and write the file back without it on its next save. A branch-mode run resumed by an older belay would go back to editing the user's checkout in place. A dropped baseline would make the next resume by a newer belay fail. Nothing would report any of it.

Two belay binaries on one machine is not hypothetical: a second install, a downgrade, or two Homebrew prefixes on one PATH. A review of the designs for the next six slices found that every one of them added fields as "additive, no schema bump", and none considered an older binary touching a newer run. The decision had to be made once, for all of them, before any of them lands.

Two groups of binaries need protecting, and they need different mechanisms:

- **Binaries that already shipped.** Nothing added now changes what they read. The one check they make is `schema_version == 1`.
- **Every binary from here on.** These can be taught a check now. Bumping the schema version again for each control field would stop them too, but every bump strands every run from every earlier binary, whether or not the run uses the new field.

## Decision
manifest.json and state.json move to schema version 2, and both carry a `requires` list: the sorted names of the capabilities the run depends on. A binary refuses a document that requires a capability it does not know, with a `*CapabilityError` (wrapping `ErrUnknownCapability`) that names each one and says to upgrade belay. A migration registered for version 1 of both files stamps version 2 and adds an empty `requires`, so runs that released belay started still load and resume. The set of known capabilities is empty today and nothing sets `requires`. The slice that adds a control field names a capability for it (such as `output.branch`), adds the name to `knownCapabilities`, and lists it in `requires` on every document whose run depends on the field. A field that only adds information gets no capability, and unknown keys in general are still ignored.

## Consequences

### Positive
- **Released binaries refuse newer runs.** v0.1.x rejects either file at version 2 before it writes anything to it. journal.ndjson has no version of its own, but the dispatcher loads manifest.json before it opens the journal, so a manifest at version 2 also keeps a released binary out of the journal. Which of the two this belay saves at version 2 *first* decides what a crash between them leaves behind, and a version-1 manifest over a version-2 state.json is a pair v0.1.x accepts, resumes and writes into. So `Store.Preflight` saves manifest.json at the current version before belay writes anything else to a run: the dispatcher calls it before it opens the journal, and `belay resume` before it records an approval.
- **A run is refused before belay writes to it, not after.** A capability can be declared in state.json alone, and the dispatcher read the manifest, opened the journal and healed a crashed attempt long before it loaded state.json — so it had already written to a run it had no right to read, and the first guard it met ended that run for good. `Store.Preflight` loads state.json before the journal is opened, so the refusal comes first.
- **One bump, not one per slice.** Each later control field costs a capability name, not a schema version and a migration. Slices built in parallel cannot collide on a version number.
- **A binary refuses only the runs that use a feature it lacks.** A run that edits in place requires nothing and stays readable by every belay from this one on, even after `output.branch` exists.
- **The refusal names the cause.** The error names the missing capability and the file that requires it. `requires` is read before the rest of the document, so a field in a shape this binary cannot decode does not hide the capability that explains it.
- **Old runs keep working.** A version-1 run loads, and it resumes with every field intact. Tests pin both against the goldens from the v0.1.3 tag.

### Negative
- **The bump cuts off released binaries completely.** Once this belay has saved a run, including one a released belay started, no released belay can resume it, even when the run needs nothing new. That is the price of protecting binaries that cannot be changed.
- **Released binaries give an unhelpful message.** They say the schema version is unsupported, not "upgrade belay": `belay runs` lists the run as "unsupported manifest version 2", and `belay resume` does not offer it at all. Nothing can change what they print.
- **Capability names become a contract.** A name cannot be renamed or reused for a different meaning, because documents already on disk carry it. Choosing when a field earns one is a judgment each slice has to make and a reviewer has to check: "does dropping this change what the run does?"
- **Each file declares its own requirements.** `Store.ApplyPatch` reads and saves state.json without opening manifest.json, so a field in either file must be listed in that file's own `requires`. A field in state.json whose capability appears only in manifest.json is unprotected.

### Follow-ups
- Before the first slice sets `requires`, teach the CLI to say "needs a newer belay" for a `*CapabilityError`. When this was written, `belay runs` reported such a run as "manifest.json is damaged", and `belay resume` skipped it as if it did not exist. Done: `belay runs` lists such a run, and one at a newer schema version, as "needs a newer belay", and `belay resume` refuses it by name with the error, which says to upgrade.
- The first slice that adds a capability also adds a test that a run using its field lists the capability, so a missing `requires` entry fails a test instead of relying on review.

## Alternatives Considered
- **A `min_belay_version` field.** Rejected because released binaries do not read it, so it protects nobody who needs protecting today. For later binaries it is also coarser than a capability list: it refuses a run that uses nothing new, and a development build or a fork has no meaningful version to compare.
- **`DisallowUnknownFields` on both documents.** Rejected. It turns every additive field into a break: a newer run that only records extra information becomes unreadable to an older binary that could have resumed it safely. It also does nothing for released binaries, which decode leniently whatever this one does.
- **A schema bump for every slice that adds a control field.** Rejected as churn. Each bump needs its own migration and cuts off every earlier binary from every run, whether or not the run uses the feature. Slices built in parallel would also have to renumber their versions in merge order.
- **A `requires` list in manifest.json alone.** Rejected because state.json is loaded and rewritten on its own, and a list in the manifest cannot protect it.
- **A `requires` list without the bump.** Rejected. Released binaries ignore the list, which leaves exactly the binaries the problem was found in unprotected.

## Revisit If
- A slice needs an older binary to carry on without a field rather than refuse the run (an optional capability). The list has no way to say that today.
- A second schema bump is proposed for any reason other than reshaping an existing field. That would mean the capability list failed at its job.
- Users report being locked out of runs by a second belay install often enough that "unsupported manifest version" needs its own entry in docs/troubleshooting.md.
- journal.ndjson gains a record whose loss changes what a resumed run does, and some command writes the journal without loading manifest.json first.

## References
- internal/state/requires.go (`checkRequires`, `CapabilityError`, `knownCapabilities`)
- internal/state/manifest.go (`Store.Preflight`: refuse, then upgrade the manifest, before anything in a run is written)
- internal/state/migrate.go (`migrateFromVersion1`)
- internal/state/state.go package documentation: "Schema versioning" and "Capabilities"
- ADR-0002 (nodes never write either file). The dispatcher writes both during a run, and the CLI writes them only to create a run (`createRun`, whose documents are new and require nothing) and to record an approval (`approvePlan`, after `Store.Preflight`). Every write to an existing run decodes the document first, through `decodeState` or `decodeManifest` (behind `LoadState`, `LoadManifest` and `Store.Preflight`), and both call `checkRequires`, so it guards the CLI's writes exactly as it guards the dispatcher's.
- ADR-0003 (journal.ndjson, which carries no schema version)
