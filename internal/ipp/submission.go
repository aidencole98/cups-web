package ipp

import "errors"

// SubmissionUncertainError means Print-Job was attempted but its result cannot
// be established. Retrying automatically could submit the same photo twice.
type SubmissionUncertainError struct {
	Err error
}

func (e *SubmissionUncertainError) Error() string { return e.Err.Error() }
func (e *SubmissionUncertainError) Unwrap() error { return e.Err }

func IsSubmissionUncertain(err error) bool {
	var uncertain *SubmissionUncertainError
	return errors.As(err, &uncertain)
}
