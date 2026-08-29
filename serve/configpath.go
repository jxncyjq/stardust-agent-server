package serve

import "github.com/stardust/legion-agent/internal/config"

// DefaultConfigDirName is the per-user directory the agent keeps its state and
// its default configuration in, under the user's home: ".stardust".
const DefaultConfigDirName = config.DefaultDirName

// DefaultConfigFileName is the configuration file read from that directory
// when no path is given: "agent.json".
const DefaultConfigFileName = config.DefaultConfigFileName

// ConfigHomeEnvVar relocates the default directory ("STARDUST_HOME").
const ConfigHomeEnvVar = config.HomeEnvVar

// DefaultConfigPath returns the configuration file an embedder gets when it
// passes no path: <STARDUST_HOME or ~/.stardust>/agent.json.
//
// It exists because internal/config is, as the name says, internal: an
// embedder in another module (the Wails GUI) cannot reach it, and duplicating
// the ".stardust" convention on that side is how the two would drift apart.
//
// Existence is not checked — DefaultConfigPathIfPresent answers that.
func DefaultConfigPath() (string, error) { return config.DefaultConfigPath() }

// DefaultConfigPathIfPresent returns the default configuration path when a
// regular file is actually there, and "" when there is nothing to load.
//
// The empty string is a contract-declared absence: running with no config file
// at all is supported (built-in defaults), and it is what every installation
// looks like before its first one.
func DefaultConfigPathIfPresent() string { return config.DefaultConfigPathIfPresent() }
