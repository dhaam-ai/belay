## Summary

サンプル: add a Unicode-safe width helper. 🎯

## Steps

1. Add `Width` to `internal/text/width.go` — it measures 幅, not bytes.
2. Use it for the résumé line and the 🎉 banner, naïvely truncating nothing.
3. Cover 한국어, ｆｕｌｌｗｉｄｔｈ and 👨‍👩‍👧‍👦 in the table.

## Risks

None — the helper is additive.
