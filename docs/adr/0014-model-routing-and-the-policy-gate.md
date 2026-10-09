# ADR-0014: Per-node model routing, and the policy gate

## Status
Accepted

## Context

Slice 4 added two settings, and it is one ADR because they are the same question asked twice and answered two different ways.

**Routing.** Before it, one run used one model. `agent.model` was sent to the plan node, the code node and every fix attempt alike. That is the wrong shape twice over: the plan is written once and is worth a strong model, while the mechanical middle of a run is not, and a fix attempt is the most expensive thing in a run and the most likely to be worth paying for. A run held to `budget.max_usd` buys more when it spends unevenly. `agent.models` routes a node to a model of its own.

**Policy.** Before it, "the tests pass" was the whole of "this change is acceptable". belay is designed to be pointed at a repository and left alone, and an unattended agent that passes every test can still edit the deployment pipeline, pull in a dependency nobody vetted, grow a change by four thousand lines, or leave a credential in a source file. There was nothing a person could write down that would stop any of it. `policy.*` declares rules a run's change must obey whatever its tests say, and a new node enforces them.

The question both raise is when the setting is read. Wave 0b established a rule — a decision a run depends on is pinned in its manifest when the run is created, never re-read from a `belay.yaml` that may have changed by the time the run is resumed — and the two settings fall on opposite sides of it. That is the decision this ADR exists to record.

## Decision

### Routing is read live

`agent.models` is a mapping of node name to model, read from the configuration on every node execution, and not pinned.

The pinning rule is about the **shape of the graph**: which nodes exist and which edges join them. A run resumed onto a different graph cannot finish the run it started — it would route into a node nothing registered, or finish without a step it was started with. Routing changes neither. It changes what the next step costs, and nothing that has already been recorded.

Reading it live is also the only coherent choice while `agent.model` is read live, which it is. The manifest records `AgentInfo.Model`, but nothing reads it back; every node takes its model from the live configuration. A pinned `agent.models` over a live `agent.model` would mean a person could change a node's model by **deleting** its override and not by editing it, which is a rule nobody could hold in their head.

What the run was started under is still recorded: each routed node appears in the manifest's configuration snapshot as `agent.models.<node>`, so the digest changes when routing does and a resumed run reports the drift. That is the record without the refusal, and it matches what belay already does for the budget ceiling — a run stopped by a setting can be resumed once the setting is fixed, rather than having to be started again.

Only the nodes that ask a backend for a model may be keyed: `plan`, `code` and `fix`. `write`, `test`, `review` and `promote` reach no model at all, and `approve` waits for a person. A key for one of those would be a setting that loads, validates and does nothing, which is the same silence as a typo — so an unrecognised key is refused when the configuration loads, by name and with the names it could have been.

The model **and its price** are the backend's knowledge, not belay's, so both come from behind the `AgentBackend` contract. Every model a run would use is priced before the run starts, walking the route rather than the `agent.models` map: a model routed to a node this run never reaches costs nothing, and refusing to start over it would be belay enforcing a rule about a step that is not going to happen. A backend that declines to say — a replayed run, which spends nothing — has not refused the model, and does not block.

*Amended 2026-10-09:* the Claude Code backend no longer refuses a model its price table does not list. The table is a snapshot, and refusing put every CLI release with a new model, every dated identifier and the aliases `sonnet[1m]` and `opusplan` out of reach. Such a model is priced at the highest rate the table holds, marked estimated, with a warning naming it, which keeps the ceiling at the safe end; alias shapes resolve to the entry they name. The blocker above still stops a run whose backend does refuse a model.

### Policy is pinned

`policy.*` is read once, when the run is created, and recorded in the run's `manifest.json` together with a survey of what the workspace declared at that moment. Every later call for that run, including the one a resume makes days later, uses the pin. `state.Manifest.SetPolicy` is the only way to write it, because the field and the capability an older belay would have to refuse are one decision (ADR-0011); the capability is `policy.gate`.

This is the opposite answer to routing's, for two reasons that are both about the baseline.

**A policy changes the graph's shape.** A guarded run has a `policy` node between `write` and `test` and an unguarded one does not, so this is exactly the case the pinning rule was written about.

**A gate needs a "before", and only the run that took it has one.** `no_new_dependencies` cannot tell a dependency the agent added from one that was always there without a record taken before the agent ran. Once the run is under way that record cannot be reconstructed. So a run that started with no policy and was resumed after somebody added one would route into a gate whose baseline was never captured, and the only thing such a gate could do is fail the run for something it did not do: every dependency in the tree would look new. The reverse is as bad — remove the policy mid-run and the resumed run finishes without the check it was started under, and says nothing.

Both directions are therefore a **warning and not a refusal**: the run finishes the way it started, and the person is told their change applies to the next run. That is what belay already does for `output.mode`.

Defence in depth, because the consequence of getting this wrong is a run failed for nothing: `state.Manifest.PinnedConfig` **clears** the live policy section before a node sees it, so a node cannot reach today's rules even by mistake; `graph.RunContext.Policy` is a pointer, so "this run has no baseline" and "this run's rules are all switched off" can never be the same value; and the gate node, if it is ever reached without a pin, returns a `Result` saying nothing was checked rather than an error. An error there would end a run that has done nothing wrong, and end it again on every resume.

The dispatcher's **limits** are deliberately still read live — `budget.max_usd`, `budget.max_tokens`, `graph.max_steps`, `graph.node_timeout` — so raising one and resuming continues a run that met it. Limits, routing, shape: three categories, and only the third is pinned.

### Two classes of violation, decided by the rule

A violation either ends the run or is handed to the fix loop, and which one is a property of the rule, never of how severe it looks.

`forbidden_paths`, `max_changed_files` and `max_changed_lines` **end the run**. Asking an agent to repair "you touched `infra/`, which you may not touch" is asking it to un-touch a file; the only honest answer is to stop and tell a person. `no_new_dependencies`, `required_tests` and `forbidden_content` go to the **fix loop**, bounded by `graph.give_up` like any other repair, because each of them names something the next attempt can genuinely put right. When both kinds are present the run ends, and the repairable ones are still recorded — a person coming back to a stopped run wants the whole list, not the half that stopped it.

Encoding the split in `belay.Severity` was considered and rejected. Severity answers "how bad is this" and the split answers "who can act on it", and the two come apart immediately: a missing test is serious and repairable, a one-file overrun of a ceiling is trivial and not.

The repair prompt says the one thing this cause needs said: **do not edit `belay.yaml`**. An agent told to fix a list of findings would reasonably try to make the checker happy, and a change that satisfies the gate by rewriting the gate is the one outcome that is never acceptable. It would also be futile, because the run is judged against the pin — so the prompt says that too, and asks the agent to change nothing and say so if a rule genuinely cannot be satisfied.

### Violations travel through the existing quality report contract

A violation is a `belay.Issue` and a gate verdict is a `belay.QualityReport` (ADR-0006), rather than a second contract of policy's own.

This was checked rather than assumed. The fields line up exactly: a rule is a `RuleID` in its own namespace (`policy:forbidden_path`), a violation has a `File` and, for a content match, a `Line`, and a message is a message. `Counts` and `AtOrAbove` already exist. `Effort` is the only field with nothing to put in it, and it is documented as optional. The one thing the contract has no room for — which of the two classes a violation is — is a property of the rule and so is recoverable from the `RuleID` without a field of its own, which is why `pkg/belay` needed no change at all; it is the package that carries a compatibility promise. A test asserts the projection conforms — `Issues` never nil, `Counts` populated, a fail-class violation at `SeverityBlocker`, which the contract defines as work that must not ship — rather than asserting that it does.

Reuse pays for itself immediately downstream. `state.Policy` is the same shape as `state.Review`; the fix node's prompt builder already renders a `belay.Issue`, so a violation reaches an agent in the same words a lint finding does; and the archived report is a `QualityReport` like the review node's. `review.fail_on` deliberately does **not** apply: it is the threshold a linter's findings are judged against, and a policy violation always counts.

### `max_changed_lines` counts whole files, and says so

belay keeps no copy of the tree from before the agent ran, so no rule can compute a true line-level diff. `max_changed_lines` does not pretend to: it counts the whole of every file the change touches as that file now stands.

A one-line edit to a 900-line file therefore counts 900. That over-counts and never under-counts, which is the safe direction for a ceiling — it is a limit on how much of the repository one unattended run may rewrite, and how much a person has to read afterwards, not a diffstat. The alternative measures are worse: a net line-count delta would score a full in-place rewrite as zero, and `max(before, after)` is not an upper bound on a diff either. The documentation says plainly what is counted, and points at `max_changed_files`, which is exact.

### Globs are strict, and a bad one is refused

`internal/glob` matches a pattern anchored at the workspace root, with `*`, `?` and character classes inside one segment and `**` as the only thing that crosses a `/`.

`.gitignore`'s rule — a pattern with no slash matches a file of that name at any depth — was deliberately not taken. It makes what a pattern means depend on whether it happens to contain a slash, and the two readings differ in exactly the direction that matters for a rule protecting a file: `secrets.yaml` would guard every one in the tree while `config/secrets.yaml` guards one, and nothing in the file says so. One rule that always reads the same way is worth two extra characters of `**/secrets.yaml`.

A pattern that could never match a cleaned relative path — empty, absolute, carrying `..`, or gluing `**` to other characters — is refused when the configuration loads. This is the one configuration mistake that must never load: every other bad setting stops something from happening, whereas a bad guardrail lets something happen, and the person who wrote it and saw belay accept it believes their repository is protected. The `..` check runs on the pattern as written rather than after cleaning, because `path.Clean("src/../etc")` is `"etc"` — a rule silently aimed somewhere its author never pointed it.

### `fix` routes back through `write`

Independent of policy, and deferred to this slice by Wave 0b because this slice was already re-routing `write`: a repair now goes `fix → write → policy → test` rather than straight to `test`.

`write` is the only node that proves a claimed path is a regular file inside the workspace — a symbolic link out of it is refused there and nowhere else — and the only one that renders a diff artifact and sets `Code.LastDiff`. Before this, a repair was the one change in a run that nothing verified: the agent's report of what it changed was taken on trust, a run's artifacts held the original change and nothing else, and `LastDiff` still pointed at the first diff however many repairs had happened since. It also means the gate is re-entered on every pass of the repair loop rather than only the first.

### `join` will not promote a candidate that broke the policy

Best-of-N must not be the way around a guardrail. Without this, the route past a rule is to run the change N times and promote whichever copy the gate happened not to stop, so `join`'s eligibility gains one term: tests passed, **and** the candidate's own run completed, **and** its change broke no rule. A candidate with a violation reaches `join` at all because a repairable violation can outlive that candidate's own fix loop giving up.

### Engine and gate ship together

`internal/policy` and the `policy` node merged in the same wave, deliberately. Shipping the configuration keys without the node that enforces them would let a person configure a guardrail that silently does nothing, which is worse than having no feature at all.

## Consequences

### Positive
- **A run can be left alone.** The rules a repository writes down are the rules the run is held to, whatever its tests say and whatever the agent reports about its own work.
- **A guardrail cannot silently do nothing.** An unusable rule is refused when the configuration loads; a rule belay cannot evaluate at gate time is no verdict rather than a pass; and a gate with no edge into it is impossible, because the node and the edge are one wiring option.
- **A run is judged against the rules it started with**, so the verdict is reproducible and a resume cannot change it. `belay run --dry-run` prints every rule and says which ones end a run.
- **The same `budget.max_usd` buys a longer run**, because the expensive model is on the steps that need it.
- **A wrong model stops a run before it spends**, not in the middle of it, and spend is recorded against the model that actually ran each step.
- **One quality contract, not two.** Everything downstream of a gate — the blackboard, the repair prompt, the archived report — treats a policy violation and a lint finding the same way.
- **A repair is verified like any other change**, which closes a hole in `write`'s containment guarantee that had nothing to do with policy.

### Negative
- **A policy applies to the next run, not this one.** Adding rules mid-run is the thing a person will try first, and belay warns rather than obeying. The reason is sound and the surprise is real.
- **`max_changed_lines` over-counts.** A ceiling set from a diffstat intuition will fire earlier than expected. The documentation is explicit, and `max_changed_files` is there for the exact case.
- **`pyproject.toml` dependencies are not read.** A TOML parser and PEP 621's several spellings of a dependency are more than this slice could do correctly, and a parser that got it half right would report dependencies a project does not have or miss ones it does. The documented workaround is to name the file in `forbidden_paths`, which stops the change belay cannot judge rather than waving it through.
- **A guarded run requires a capability.** `policy.gate` means an older belay refuses to resume it, which is the point, but it is one more run a v0.1.x binary cannot pick up. An unguarded run requires nothing.
- **One more step per repair.** `fix → write → policy → test` counts two extra steps against `graph.max_steps`, which is 60 by default against a `give_up` of 3.
- **Two more categories of setting to keep straight.** Limits are live, routing is live, shape is pinned. Each new setting has to be placed in one of the three, and the answer is not obvious from the setting alone.
- **Adding the section changed every configuration digest**, so a run started before this reports drift on resume. That is true of any added section, and the drift is a warning.

### Follow-ups
- Take the change set from `git diff` against the run's base commit in branch mode, so the gate judges what the repository says changed rather than what the agent reported. ADR-0012 already owes this for `write`; the gate inherits it for free.
- Count a deleted file's lines toward `max_changed_lines`. It contributes none today, because the lines it held are the one thing no longer there to count, and a baseline rich enough to know them would mean surveying the whole tree at run creation.
- Read `pyproject.toml` properly, and drop the `forbidden_paths` workaround from the documentation when it lands.
- Consider a `policy.required_tests` rule that maps a source file to *its* test rather than to any test. Today any changed file matching `require` satisfies the rule, because a wrong answer about which test covers which file would either block correct changes or wave through untested ones.

## Alternatives Considered

**Pin routing too, for consistency.** Rejected: it changes no node and no edge, and a pinned `agent.models` over a live `agent.model` gives a person two settings that behave differently for no reason they could infer.

**Read the policy live, and survey the baseline at gate time.** The cheapest thing to build, and wrong. The baseline would then be a survey of the tree *after* the agent edited it, so `no_new_dependencies` would compare the tree to itself and could never fire — a guardrail that loads, validates and does nothing.

**A second report contract for violations.** Considered first, since a violation is not really a "quality issue". Rejected on the evidence above: every field the graph reads has an exact counterpart, the class distinction is recoverable from the rule id, and a second contract would mean a second blackboard block, a second archived-report shape and a second prompt renderer, all of which would drift.

**Severity as the fail/fix split.** Rejected: severity is how bad, the split is who can act, and they disagree in both directions.

**A single `policy.enabled` boolean over a rule set.** Rejected as meaningless — enabled to do what? The rules *are* the feature.

**`gitignore` glob semantics.** Rejected above: a pattern's meaning must not depend on whether it happens to contain a slash.

**Fail every violation, with no fix class.** Simpler and safer-sounding, and it throws away most of the value. A missing test or a new dependency is exactly what an agent can be asked to put right, and ending the run instead spends a whole run's budget to deliver a sentence a person then has to act on by hand.

**Repair every violation, with no fail class.** Rejected as unsafe. A change that touched a path it may not touch has already done the thing the rule forbade, and asking the agent to try again invites it to do it again — or to "satisfy" the rule by editing `belay.yaml`.

## Revisit If
- A run's tree can be cheaply snapshotted before the agent runs. `max_changed_lines` becomes a real diffstat, and the baseline can hold more than dependencies.
- A repository needs different rules for different parts of itself. Today the policy is one set for the whole workspace; per-directory rules are a real request and a much bigger schema.
- A policy needs to be shared across repositories. Nothing in the design prevents it, and nothing in it supports it: the rules come from the same merged configuration as everything else.
- Routing starts changing which nodes a run has — a model so cheap it is worth an extra verification step, say. At that point routing is shape, and it is pinned.
