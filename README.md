<!-- Placeholder — T45 replaces this. -->

# belay

belay is a durable, resumable orchestrator for autonomous coding agents. Instead of one unreliable AI call, it runs a state-machine graph — plan → approve → code → write → test → fix → review — where a dispatcher persists state after every node, so a crashed run resumes from the last completed node instead of restarting.

**Status: under construction**

## Building from Source

```bash
make build
./bin/belay --help
```

## Product Pillars

- **Durable + resumable runs** — State persisted after every node; crashed runs resume instead of restart
- **Quality-gated loop** — State machine enforces approvals and gates between phases
- **Best-of-N candidates** — Retries and ranking across multiple candidates per phase
- **CLI-agnostic orchestration** — Orchestration independent of CLI tool choice

## License

Licensed under Apache License 2.0. Copyright (c) The belay Authors.
