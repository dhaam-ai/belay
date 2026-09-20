## Steps

1) Read the manifest.
2) Rebuild the registry from its pins.
3) Refuse a run whose `requires` list this binary cannot honour.

## Tests

`internal/state/store_test.go` covers all three.
