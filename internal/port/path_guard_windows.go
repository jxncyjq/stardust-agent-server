//go:build windows

package port

import (
	"fmt"
	"path/filepath"
	"strings"
)

// windowsDevices are the names Windows resolves to a device at EVERY directory
// level, whatever the extension: opening <workspace>\NUL or <workspace>\con.txt
// reaches the device, not a file in the workspace. Containment checks pass them
// because the spelling really is inside the root — the escape happens in the
// filesystem, below the path layer.
var windowsDevices = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// checkPlatform rejects the Windows-only spellings that stay lexically inside
// the workspace but do not address a file in it.
//
// Both cases below are refused as ErrPathOutsideWorkspace rather than a
// separate error: from the caller's point of view the effect is identical —
// the path does not name a file inside the workspace, so it must not be used.
func checkPlatform(clean string) error {
	for _, segment := range strings.Split(clean, string(filepath.Separator)) {
		if segment == "" {
			continue
		}
		// A device name wins over any extension: "con.txt" is CON.
		base := segment
		if dot := strings.IndexByte(base, '.'); dot >= 0 {
			base = base[:dot]
		}
		if windowsDevices[strings.ToUpper(strings.TrimSpace(base))] {
			return fmt.Errorf("%w: %s names the %s device", ErrPathOutsideWorkspace, clean, strings.ToUpper(base))
		}
		// An alternate data stream rides on a file name ("notes.txt:hidden")
		// and writes to a stream no workspace listing shows. The drive colon
		// only ever appears in the first segment, which filepath.Split leaves
		// as "C:" — that one is not a stream.
		if colon := strings.IndexByte(segment, ':'); colon >= 0 && !isDriveSegment(segment) {
			return fmt.Errorf("%w: %s names an alternate data stream", ErrPathOutsideWorkspace, clean)
		}
	}
	return nil
}

// isDriveSegment reports whether segment is a bare drive designator ("C:").
func isDriveSegment(segment string) bool {
	return len(segment) == 2 && segment[1] == ':' &&
		((segment[0] >= 'a' && segment[0] <= 'z') || (segment[0] >= 'A' && segment[0] <= 'Z'))
}
