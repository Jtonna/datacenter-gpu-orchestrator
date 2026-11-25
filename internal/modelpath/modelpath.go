// Package modelpath is the one rule for a profile's model path, the path of
// a model file relative to the models folder. Every place that joins a
// profile's model to the models folder (the configuration check, the
// scheduler's need, the instance command, the catalog, the file-cache evict
// and the agent's launch) uses it.
//
// The rule: relative, forward slashes only, and no part that starts with a
// dot. The dot-folders of the models folder (.trash, .incoming,
// .potluck-uploads) are Potluck's or the owner's own and are never a
// profile's model. A plain sub-folder ("sub/a.gguf") stays allowed.
package modelpath

import (
	"errors"
	"fmt"
	"strings"
)

// Check reports why model cannot be a profile's model path, or nil.
func Check(model string) error {
	switch {
	case model == "":
		return errors.New("no model file named")
	case strings.ContainsRune(model, 0):
		return errors.New("the model path has a NUL byte")
	case len(model) >= 2 && model[1] == ':' && isLetter(model[0]):
		return fmt.Errorf("model path %q names a drive: name a file relative to models_dir", model)
	case strings.Contains(model, `\`):
		return fmt.Errorf("model path %q has a backslash: use forward slashes", model)
	case strings.HasPrefix(model, "/"):
		return fmt.Errorf("model path %q is absolute: name a file relative to models_dir", model)
	}
	for _, part := range strings.Split(model, "/") {
		switch {
		case part == "":
			return fmt.Errorf("model path %q has an empty part", model)
		case part == "..":
			return fmt.Errorf("model path %q has a \"..\" part: it would leave models_dir", model)
		case strings.HasPrefix(part, "."):
			return fmt.Errorf("model path %q has a part that starts with a dot (%q): the dot-folders of models_dir are not for profiles", model, part)
		}
	}
	return nil
}

// Join is dir joined with a checked model path, using the separator dir
// already uses, so a Linux "/models" never turns into a backslash path on a
// Windows machine and a Windows folder gets backslashes throughout.
func Join(dir, model string) (string, error) {
	if err := Check(model); err != nil {
		return "", err
	}
	if dir == "" {
		return model, nil
	}
	if strings.Contains(dir, `\`) && !strings.Contains(dir, "/") {
		return strings.TrimRight(dir, `\`) + `\` + strings.ReplaceAll(model, "/", `\`), nil
	}
	return strings.TrimRight(dir, `/\`) + "/" + model, nil
}

func isLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
