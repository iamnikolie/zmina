//go:build !darwin && !linux

package cmd

import "errors"

// statDisk does not know the free space on this system: doctor says so, the beat says nothing.
func statDisk(string) (diskSpace, error) { return diskSpace{}, errors.New("not known on this system") }
