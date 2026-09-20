## Summary

Fold long timeline notes so they fit the terminal.

## Steps

1. Add `Fold` to `internal/text/fold.go`:

   - it breaks on spaces only
   - it never splits a grapheme cluster

2. Call it from `internal/cli/timeline.go`:

   ```go
   line := text.Fold(note, width)
   ```

   Keep the existing colour handling exactly as it is.

3. Document the wrapping rule in `docs/cli.md`.

## Tests

`internal/text/fold_test.go` covers CJK and emoji widths.
