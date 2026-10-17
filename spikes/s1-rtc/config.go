package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// loadEnvFile loads KEY=VALUE lines into the process environment without
// overriding variables that are already set. A missing file is not an error.
// A file readable by group/other is refused (mode 0600 required), same as s2-sip.
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

// envConfig is the ring subcommand's environment configuration.
type envConfig struct {
	HomeserverURL string // MATRIX_HOMESERVER_URL
	ServerName    string // MATRIX_SERVER_NAME
	ASToken       string // MATRIX_AS_TOKEN (never logged)
	OwnerMXID     string // OWNER_MXID
	LiveKitSvcURL string // LIVEKIT_SERVICE_URL (lk-jwt-service base, without /sfu/get)
}

func readEnvConfig() (*envConfig, error) {
	c := &envConfig{
		HomeserverURL: strings.TrimRight(os.Getenv("MATRIX_HOMESERVER_URL"), "/"),
		ServerName:    os.Getenv("MATRIX_SERVER_NAME"),
		ASToken:       os.Getenv("MATRIX_AS_TOKEN"),
		OwnerMXID:     os.Getenv("OWNER_MXID"),
		LiveKitSvcURL: strings.TrimRight(os.Getenv("LIVEKIT_SERVICE_URL"), "/"),
	}
	var missing []string
	for k, v := range map[string]string{
		"MATRIX_HOMESERVER_URL": c.HomeserverURL, "MATRIX_SERVER_NAME": c.ServerName,
		"MATRIX_AS_TOKEN": c.ASToken, "OWNER_MXID": c.OwnerMXID, "LIVEKIT_SERVICE_URL": c.LiveKitSvcURL,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	return c, nil
}
