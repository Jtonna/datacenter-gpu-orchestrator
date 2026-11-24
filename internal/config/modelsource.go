package config

import "regexp"

// ModelSourceRE is the form of a model source given to a new machine:
// share:<host>:/<path>, dir:/<path> or push. No space, quote, '$', '%' or
// backslash, so it can be written into a shell script as one quoted word.
var ModelSourceRE = regexp.MustCompile(`^(push|share:[^\s'"\\$%:]+:/[^\s'"\\$%]*|dir:/[^\s'"\\$%]*)$`)
