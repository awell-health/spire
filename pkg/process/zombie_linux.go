package process

import (
	"fmt"
	"os"
	"strings"
)

// isZombie returns true when the process is in state Z (zombie/defunct).
// Linux exposes process state as the third whitespace-separated field in
// /proc/<pid>/stat, immediately after the comm field. The comm field is
// wrapped in parentheses and may itself contain '(' ')' or whitespace, so
// we scan for the LAST ')' before splitting.
func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(data)
	end := strings.LastIndex(s, ")")
	if end < 0 || end+2 >= len(s) {
		return false
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) == 0 {
		return false
	}
	return fields[0] == "Z"
}
