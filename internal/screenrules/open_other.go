//go:build !unix

package screenrules

import (
	"fmt"
	"os"
)

// openPlain opens a path that was a plain file a moment ago, and makes sure it still is.
func openPlain(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()

		return nil, fmt.Errorf("%s is not a regular file: it is not read", path)
	}

	return f, nil
}
