package tnc

import (
	"fmt"
	"os"
	"os/exec"
)

// knownPaths lists where each backend's binary is looked for after $PATH.
var knownPaths = map[Backend][]string{
	BackendSamoyed:  {"/opt/samoyed/dist/samoyed-direwolf", "/usr/local/bin/samoyed-direwolf"},
	BackendDirewolf: {"/usr/bin/direwolf", "/usr/local/bin/direwolf"},
	BackendPdn:      {"/usr/bin/pdn-soundmodem", "/usr/lib/pdn-soundmodem/pdn-soundmodem"},
}

// BinaryName is the executable name each backend is found by on $PATH.
func BinaryName(b Backend) string {
	switch b {
	case BackendDirewolf:
		return "direwolf"
	case BackendPdn:
		return "pdn-soundmodem"
	default:
		return "samoyed-direwolf"
	}
}

// ResolveBinary finds a backend's executable: explicit if given (it must
// exist), else $PATH, else the usual install locations.
func ResolveBinary(b Backend, explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", err
		}
		return explicit, nil
	}
	name := BinaryName(b)
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, p := range knownPaths[b] {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found in $PATH or common locations", name)
}
