//go:build unix

package exec

import (
	"fmt"
	"slices"
	"strings"
)

// baseEnvNames is the entire set of parent environment variables a child
// inherits without being asked for by name.
//
// Deny-by-default is not defence in depth here, it is the primary control.
// The children belay runs include coding agents: processes that can read
// /proc/self/environ, print os.Environ(), and are steered by a prompt that
// belay itself assembled from earlier captured output. An agent that can read
// AWS_SECRET_ACCESS_KEY can exfiltrate it by simply printing it, at which
// point the value lands in .belay/runs/<id>/ on disk and, on the next node,
// inside a prompt sent to a model provider. Redaction is the second line of
// defence for exactly this path; not handing the value over in the first place
// is the first. Every variable a child receives beyond this list is therefore
// an explicit, per-call decision by the caller.
//
// The six entries below are the minimum for a child to locate its own
// toolchain (PATH), find a writable scratch and config root (HOME, TMPDIR),
// produce stable, parseable output (LANG, TERM), and identify the invoking
// account (USER).
//
// USER is here because omitting it broke the only shipped agent backend, and
// broke it misleadingly. The claude CLI resolves its stored credentials
// through the account's keychain, and without USER it cannot; it then reports
// "Not logged in - Please run /login", which sends the user to re-authenticate
// a CLI that is in fact already authenticated. None of it is sensitive: USER
// is the account name, already implicit in HOME.
var baseEnvNames = []string{"HOME", "LANG", "PATH", "TERM", "TMPDIR", "USER"}

// BaseEnvNames returns the always-allowed environment variable names.
//
// The result is a fresh copy; mutating it does not affect later calls.
func BaseEnvNames() []string {
	return slices.Clone(baseEnvNames)
}

// secretNameParts are substrings that mark a variable name as credential
// bearing. Matching is case-insensitive and deliberately broad: a false
// positive costs a redaction marker in captured output, a false negative
// costs a plaintext credential on disk.
var secretNameParts = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD", "PASSPHRASE",
	"APIKEY", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY",
	"CREDENTIAL", "AUTH", "_PAT",
}

// IsSecretName reports whether an environment variable name looks like it
// carries a credential, and so whether its value should seed the redactor.
//
// Names in BaseEnvNames are never secret bearing: redacting the value of HOME
// or TERM would corrupt nearly every captured line.
func IsSecretName(name string) bool {
	if slices.Contains(baseEnvNames, strings.ToUpper(name)) {
		return false
	}
	u := strings.ToUpper(name)
	for _, p := range secretNameParts {
		if strings.Contains(u, p) {
			return true
		}
	}
	return false
}

// buildEnv resolves the child environment from an explicit parent snapshot.
//
// It returns the child environment as sorted "NAME=value" pairs and the
// secrets those values contribute to the redactor. Allowlisted names that are
// absent from the parent are skipped silently: a child asking for an optional
// credential must not fail because the operator did not configure one.
func buildEnv(parent []string, c Command) ([]string, []Secret, error) {
	lookup := make(map[string]string, len(parent))
	for _, kv := range parent {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if _, dup := lookup[kv[:i]]; !dup {
			lookup[kv[:i]] = kv[i+1:]
		}
	}

	secretNames := make(map[string]bool, len(c.SecretEnv))
	for _, n := range c.SecretEnv {
		secretNames[n] = true
	}

	allow := make([]string, 0, len(baseEnvNames)+len(c.EnvAllow)+len(c.SecretEnv))
	allow = append(allow, baseEnvNames...)
	allow = append(allow, c.EnvAllow...)
	allow = append(allow, c.SecretEnv...)

	values := make(map[string]string, len(allow))
	order := make([]string, 0, len(allow))
	for _, name := range allow {
		if err := validEnvName(name); err != nil {
			return nil, nil, err
		}
		if _, dup := values[name]; dup {
			continue
		}
		v, ok := lookup[name]
		if !ok {
			continue
		}
		values[name] = v
		order = append(order, name)
	}

	for _, name := range sortedKeys(c.ExtraEnv) {
		if err := validEnvName(name); err != nil {
			return nil, nil, err
		}
		v := c.ExtraEnv[name]
		if strings.ContainsRune(v, 0) {
			return nil, nil, fmt.Errorf("%w: value of %s", ErrNullByte, name)
		}
		if _, dup := values[name]; !dup {
			order = append(order, name)
		}
		values[name] = v
	}

	slices.Sort(order)
	env := make([]string, 0, len(order))
	var secrets []Secret
	for _, name := range order {
		env = append(env, name+"="+values[name])
		if secretNames[name] || IsSecretName(name) {
			secrets = append(secrets, Secret{Label: name, Value: values[name]})
		}
	}
	return env, secrets, nil
}

func validEnvName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidEnv)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: name contains a null byte", ErrNullByte)
	}
	if strings.ContainsRune(name, '=') {
		return fmt.Errorf("%w: name %q contains %q", ErrInvalidEnv, name, "=")
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
