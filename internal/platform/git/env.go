package git

import (
	"os"
	"sort"
	"strings"
)

// Environment returns the child process environment.
//
// It is built from nothing rather than inherited, which is the T-A6 control: a
// credential the web process holds — a database URL, a token, a proxy
// credential, a cloud metadata endpoint — cannot reach a Git child by accident,
// because nothing reaches it except the entries listed here.
//
// Every entry is present even when its value is empty, so an operator's
// exported variable is overridden rather than inherited. Omitting the entry
// would leave Git to read the parent's value, which is the case this exists to
// prevent.
//
// Git reads no configuration from the filesystem: not the system file, not a
// global file, not a home directory. Everything it needs arrives as an argument.
func Environment(extra map[string]string) []string {
	environment := map[string]string{
		// Git needs PATH to find its own subcommands (`git-upload-pack` and the
		// like).
		"PATH": os.Getenv("PATH"),

		// No configuration from the filesystem.
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,

		// Never prompt, and never hand a credential to a helper program.
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
		"SSH_ASKPASS":         "",

		// Locale-independent messages, so a parsed message is stable. Git's
		// output is never shown to a client, but a stable form helps an
		// operator and a test.
		"LC_ALL": "C",
		"LANG":   "C",
	}

	// Windows needs its own system variables for a child process to start at
	// all. They locate the system directories and carry no credential.
	if os.PathSeparator == '\\' {
		for _, name := range []string{"SystemRoot", "SystemDrive", "COMSPEC", "PATHEXT", "TEMP", "TMP"} {
			if value, found := os.LookupEnv(name); found {
				environment[name] = value
			}
		}
	}

	for name, value := range extra {
		// A caller may only add something that names no credential. This guards
		// against a future call site passing a secret through; it is not a
		// filter for a client's benefit.
		if isCredentialBearing(name) {
			continue
		}
		upper := strings.ToUpper(name)
		environment[upper] = value
	}

	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, name+"="+environment[name])
	}
	return entries
}

// credentialNames are variables that must never be handed to a Git child, even
// if a caller asks for them.
//
// Proxy and metadata entries are here for a different reason than the secrets:
// they redirect where a child connects or what it reads, so an attacker who
// could set one would turn a Git operation into a read of a directory of their
// choosing. `GIT_ALTERNATE_OBJECT_DIR` is the sharpest of those: it makes a
// repository serve objects from outside itself.
var credentialNames = []string{
	"GITNEST_DATABASE_URL",
	"DATABASE_URL",
	"PGPASSWORD",
	"AWS_ACCESS_KEY_ID",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"AWS_EC2_METADATA_SERVICE_ENDPOINT",
	"HTTP_PROXY",
	"HTTPS_PROXY",
	"ALL_PROXY",
	"NO_PROXY",
	"GIT_ASKPASS",
	"SSH_ASKPASS",
	"SSH_AUTH_SOCK",
	"GIT_SSH",
	"GIT_SSH_COMMAND",
	"GIT_PROXY_COMMAND",
	"GIT_ALTERNATE_OBJECT_DIR",
	"GIT_OBJECT_DIRECTORY",
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_CONFIG",
	"GIT_CONFIG_COUNT",
	"GIT_CONFIG_KEY_0",
	"GIT_CONFIG_VALUE_0",
	"GIT_EXEC_PATH",
	"GIT_TEMPLATE_DIR",
	"GIT_TRACE",
	"GIT_TRACE_PACKET",
	"LD_PRELOAD",
	"LD_LIBRARY_PATH",
}

// isCredentialBearing reports whether a variable name carries a credential or
// redirects child execution.
func isCredentialBearing(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	for _, forbidden := range credentialNames {
		if upper == forbidden {
			return true
		}
	}
	// A variant is refused too: `MY_TOKEN` carries the same risk as the bare
	// name it resembles.
	for _, fragment := range []string{"SECRET", "PASSWORD", "TOKEN", "CREDENTIAL", "PRIVATE_KEY", "PROXY", "ASKPASS"} {
		if strings.Contains(upper, fragment) {
			return true
		}
	}
	return false
}
