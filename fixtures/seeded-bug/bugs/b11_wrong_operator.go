package bugs

func init() {
	register(Defect{
		ID:          "B11",
		Class:       "wrong-operator",
		File:        "errors.go",
		Symbol:      "IsRetryable",
		Description: "Sentinel comparison with == instead of errors.Is: a wrapped ErrTransient (e.g. via fmt.Errorf's %w) no longer compares equal, so wrapped transient errors are misclassified as non-retryable.",
		Find:        "return errors.Is(err, ErrTransient)",
		Replace:     "return err == ErrTransient",
		BreaksTests: []string{"TestIsRetryable/wrapped_transient_sentinel_is_retryable"},
		DetectedBy:  DetectedByBoth,
		Linter:      "errorlint",
	})
}
