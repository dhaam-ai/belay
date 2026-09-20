## Summary

Teach the docs generator to quote a plan without executing it. A plan looks like
this:

```markdown
## Steps

1. This fenced list is documentation, not this plan's steps.
2. Neither is this.
```

## Steps

1. Add the quoting helper to `internal/docs/quote.go`.
2. Show the shape it must survive:

   ```markdown
   ## Steps

   1. A step inside a fence inside a step.
   ```

   The helper copies it verbatim.

## Tests

`internal/docs/quote_test.go` covers a fence inside a fence.
