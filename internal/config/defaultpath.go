package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// DefaultDirName is the per-user directory this project keeps its state in,
// under the user's home. It is the same ".stardust" the workspace root, the
// resident agents.md and the session state already use (see
// internal/sessionstate.ResolveWorkspaceRoot) — the config joins that
// convention rather than inventing a second one.
const DefaultDirName = ".stardust"

// DefaultConfigFileName is the file Load looks for inside DefaultDir when no
// path was given.
const DefaultConfigFileName = "agent.json"

// HomeEnvVar relocates the whole default directory. It exists for two callers:
// a deployment that keeps the agent's state somewhere other than the user's
// home, and tests, which must never read or write the developer's real
// ~/.stardust.
//
// A set-but-empty value is treated as unset: an exported-then-cleared variable
// is how a shell says "I am not using this", and reading it as "the default
// directory is the process working directory" would put agent.json somewhere
// nobody named.
const HomeEnvVar = "STARDUST_HOME"

// DefaultDir returns the directory the agent reads its default configuration
// from: $STARDUST_HOME when set, otherwise <user home>/.stardust.
//
// It does NOT create the directory and does not require it to exist. Deciding
// where the default lives and deciding whether anything is there are separate
// questions, and only the caller knows which one it is asking.
//
// The error is the user home being unresolvable (HOME/USERPROFILE unset —
// service accounts, containers, some Windows services). That is a real
// environment fault and is returned rather than papered over, but see Load for
// why running with no configuration at all is still a supported state.
func DefaultDir() (string, error) {
	if fromEnv := os.Getenv(HomeEnvVar); fromEnv != "" {
		abs, err := filepath.Abs(fromEnv)
		if err != nil {
			return "", fmt.Errorf("resolve %s %q: %w", HomeEnvVar, fromEnv, err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home directory (set %s to choose the directory explicitly): %w",
			HomeEnvVar, err)
	}
	return filepath.Join(home, DefaultDirName), nil
}

// DefaultConfigPath returns the configuration file Load reads when no path was
// given: <DefaultDir>/agent.json.
//
// Existence is not checked here — DefaultConfigPathIfPresent answers that.
func DefaultConfigPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, DefaultConfigFileName), nil
}

// DefaultConfigPathIfPresent returns the default configuration path when a
// regular file is actually there, and "" when there is nothing to load.
//
// The empty string is a CONTRACT-DECLARED absence, not a swallowed failure:
// running with no configuration file is a supported deployment (the agent
// falls back to built-in defaults), and it is the state every installation is
// in before its first `agent` run. An unresolvable home directory is reported
// the same way for the same reason — a service account with no HOME has no
// default config, which is a fact about the environment rather than an error
// in the command the user just typed.
//
// Callers that need to TELL the user which file was used should call
// DefaultConfigPath and stat it themselves, so they can say why.
func DefaultConfigPathIfPresent() string {
	path, err := DefaultConfigPath()
	if err != nil {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}
