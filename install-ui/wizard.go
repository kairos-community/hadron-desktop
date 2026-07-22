package main

import "errors"

// runWizard is implemented in Task 4. Returning an error here keeps the package
// compiling and makes an accidental release of this stub fail loudly rather than
// silently installing with no user configured.
func runWizard() error {
	return errors.New("wizard not implemented")
}
