package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadEnvFile parses a Docker-compatible env file at path. Each non-blank,
// non-comment line is either:
//
//   KEY=VALUE  — literal assignment; balanced single or double quotes around
//                VALUE are stripped (but VALUE is otherwise taken verbatim —
//                no ${FOO} substitution, no escape processing).
//   KEY        — inherit the value from the daemon / caller's environment.
//
// Leading/trailing whitespace is trimmed from the key. Comments are lines
// whose first non-whitespace character is '#'. Returns a map of key→value.
func LoadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("env-file %s: %w", path, err)
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			// bare KEY — inherit from current env
			key := strings.TrimSpace(line)
			if key == "" {
				continue
			}
			if v, ok := os.LookupEnv(key); ok {
				out[key] = v
			}
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := line[eq+1:]
		if len(val) >= 2 {
			first, last := val[0], val[len(val)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		if key == "" {
			return nil, fmt.Errorf("env-file %s:%d: empty key", path, lineNo)
		}
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("env-file %s: %w", path, err)
	}
	return out, nil
}
