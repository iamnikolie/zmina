package shift

import "errors"

// ErrPartial marks a command's error after part of it was done: the state is saved with what was done (zmina up of
// several roles, one of which failed). Any other error leaves the state on disk as it was before the command.
var ErrPartial = errors.New("partly done")

type partial struct{ error }

func (p partial) Unwrap() []error { return []error{p.error, ErrPartial} }

// Partial wraps err as ErrPartial, its message unchanged; nil stays nil.
func Partial(err error) error {
	if err == nil {
		return nil
	}

	return partial{err}
}
