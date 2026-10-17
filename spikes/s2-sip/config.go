package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// loadEnvFile loads KEY=VALUE lines into the process environment without
// overriding variables that are already set. A missing file is not an error
// (the credentials may come straight from the environment). A file that is
// readable by group/other is refused, per the design doc (mode 0600).
func loadEnvFile(path string) (loaded bool, err error) {
	if strings.HasPrefix(path, "~/") {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return false, herr
		}
		path = filepath.Join(home, path[2:])
	}
	st, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return false, fmt.Errorf("env file %s has mode %o; run chmod 600 on it", path, st.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return true, sc.Err()
}

// maskUser shows only the first and last two characters, e.g. ab***yz.
func maskUser(u string) string {
	if len(u) <= 4 {
		return "***"
	}
	return u[:2] + "***" + u[len(u)-2:]
}
