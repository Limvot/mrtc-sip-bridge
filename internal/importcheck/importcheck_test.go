package importcheck

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/williamkray/mrtc-sip-bridge"

var forbiddenPrefixes = []string{
	module + "/pkg/connector",
	module + "/pkg/hostkit",
	module + "/pkg/widget",
	module + "/cmd",
	"maunium.net/go/mautrix/bridgev2",
	"maunium.net/go/mautrix/appservice",
	"go.mau.fi/util/configupgrade",
	// Core packages use only the OTel API; the SDK, exporters and contrib are host-side (spec 3.1).
	"go.opentelemetry.io/otel/sdk",
	"go.opentelemetry.io/otel/exporters",
	"go.opentelemetry.io/contrib",
	module + "/pkg/telemetry",
}

// Any "go.opentelemetry.io/otel" import in core code must be exactly one of the API paths of
// TELEMETRY-SPEC 3.1 (this also rejects otel/log, otel/semconv, otel/baggage, ...).
var allowedOTel = map[string]bool{
	"go.opentelemetry.io/otel":             true,
	"go.opentelemetry.io/otel/attribute":   true,
	"go.opentelemetry.io/otel/codes":       true,
	"go.opentelemetry.io/otel/trace":       true,
	"go.opentelemetry.io/otel/trace/noop":  true,
	"go.opentelemetry.io/otel/metric":      true,
	"go.opentelemetry.io/otel/metric/noop": true,
	"go.opentelemetry.io/otel/propagation": true,
}

// Any import starting with "maunium.net/go/mautrix" must be exactly one of these.
var allowedMautrix = map[string]bool{
	"maunium.net/go/mautrix/id":    true,
	"maunium.net/go/mautrix/event": true,
}

// confined maps a library import prefix to the only core package directory (directly under pkg/)
// allowed to import it (ADR-009, PHASE2-SPEC D2). The longest matching prefix wins.
var confined = []struct{ Prefix, Dir string }{
	{"github.com/emiago/", "sipua"},
	{"github.com/pion/stun/", "sipua"},
	{"github.com/pion/", "rtc"},
	{"github.com/livekit/", "rtc"},
	{"gopkg.in/hraban/opus", "media"},
}

type violation struct{ File, Import string }

// confinedTo returns the only package directory allowed to import path, if path is a confined library.
func confinedTo(path string) (dir string, ok bool) {
	best := -1
	for _, c := range confined {
		if strings.HasPrefix(path, c.Prefix) && len(c.Prefix) > best {
			best, dir, ok = len(c.Prefix), c.Dir, true
		}
	}
	return dir, ok
}

// isForbidden reports whether a core package may not import path.
func isForbidden(path string) bool {
	for _, p := range forbiddenPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	if strings.HasPrefix(path, "go.opentelemetry.io/otel") && !allowedOTel[path] {
		return true
	}
	return strings.HasPrefix(path, "maunium.net/go/mautrix") && !allowedMautrix[path]
}

// inPkgDir reports whether file is inside the package directory dir directly under root.
func inPkgDir(root, file, dir string) bool {
	rel, err := filepath.Rel(root, file)
	return err == nil && strings.HasPrefix(filepath.ToSlash(rel), dir+"/")
}

// hostOnlyDirs are host-side packages directly under root that are not core packages and are skipped:
// connector (the standalone bridgev2 host), hostkit (bridgev2 glue shared by both hosts, PHASEJ-SPEC
// §3.2), telemetry (the only place the OTel SDK lives) and widget (the Dialpad widget server and
// installer, WIDGET-SPEC §2). Everything else, including sipaccount, is checked as core.
var hostOnlyDirs = []string{"connector", "hostkit", "telemetry", "widget"}

// isHostOnlyDir reports whether name is one of hostOnlyDirs.
func isHostOnlyDir(name string) bool {
	for _, d := range hostOnlyDirs {
		if name == d {
			return true
		}
	}
	return false
}

// checkDir walks root recursively, skipping host-only dirs directly under root and testdata dirs;
// parses every non-_test .go file with parser.ImportsOnly; returns violations.
func checkDir(root string) ([]violation, error) {
	var out []violation
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (d.Name() == "testdata" || (filepath.Dir(path) == root && isHostOnlyDir(d.Name()))) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			outsideConfinement := false
			if dir, ok := confinedTo(p); ok && !inPkgDir(root, path, dir) {
				outsideConfinement = true
			}
			if isForbidden(p) || outsideConfinement {
				out = append(out, violation{File: path, Import: p})
			}
		}
		return nil
	})
	return out, err
}

func TestCorePackagesImportBoundary(t *testing.T) {
	root := "../../pkg"
	violations, err := checkDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Errorf("%s imports forbidden package %s", v.File, v.Import)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	dirs := 0
	for _, e := range entries {
		if e.IsDir() && !isHostOnlyDir(e.Name()) {
			dirs++
		}
	}
	if dirs < 10 {
		t.Fatalf("scanned only %d core package directories, want at least 10 (calls, clock, core, matrixrtc, media, obs, phone, rtc, sipaccount, sipua)", dirs)
	}
}

func TestCheckDirDetectsViolations(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"bad_bridgev2.go":            `maunium.net/go/mautrix/bridgev2`,
		"bad_root.go":                `maunium.net/go/mautrix`,
		"bad_connector.go":           module + `/pkg/connector`,
		"clean.go":                   `maunium.net/go/mautrix/event`,
		"bad_diago.go":               `github.com/emiago/diago`,
		"bad_pion.go":                `github.com/pion/rtp`,
		"sipua/ok.go":                `github.com/emiago/diago`,
		"media/bad_lk.go":            `github.com/livekit/server-sdk-go/v2`,
		"rtc/ok_pion.go":             `github.com/pion/webrtc/v4`,
		"calls/bad_rtp.go":           `github.com/pion/rtp`,
		"media/ok_opus.go":           `gopkg.in/hraban/opus.v2`,
		"sipua/ok_stun.go":           `github.com/pion/stun/v3`,
		"rtc/bad_diago.go":           `github.com/emiago/diago`,
		"rtc/bad_stun.go":            `github.com/pion/stun/v3`,
		"sipua/bad_opus.go":          `gopkg.in/hraban/opus.v2`,
		"connector/x.go":             `maunium.net/go/mautrix/bridgev2`,
		"hostkit/x.go":               `maunium.net/go/mautrix/bridgev2`,
		"hostkit/cfg.go":             `go.mau.fi/util/configupgrade`,
		"sipaccount/bad_bridgev2.go": `maunium.net/go/mautrix/bridgev2`,
		"sipaccount/bad_hostkit.go":  module + `/pkg/hostkit`,
		"sipaccount/ok_sipua.go":     module + `/pkg/sipua`,
		"bad_hostkit.go":             module + `/pkg/hostkit`,
		"bad_widget.go":              module + `/pkg/widget`,
		"calls/bad_widget2.go":       module + `/pkg/widget`,
		"widget/ok.go":               `maunium.net/go/mautrix/bridgev2`,
		"widget/ok_hostkit.go":       module + `/pkg/hostkit`,
		"testdata/y.go":              `maunium.net/go/mautrix/bridgev2`,
		"x_test.go":                  `maunium.net/go/mautrix/bridgev2`,
		"bad_sdk.go":                 `go.opentelemetry.io/otel/sdk/trace`,
		"bad_exporter.go":            `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`,
		"bad_contrib.go":             `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`,
		"bad_telemetry.go":           module + `/pkg/telemetry`,
		"clean_otel.go":              `go.opentelemetry.io/otel/trace`,
		"clean_noop.go":              `go.opentelemetry.io/otel/metric/noop`,
		"bad_otellog.go":             `go.opentelemetry.io/otel/log`,
		"bad_semconv.go":             `go.opentelemetry.io/otel/semconv/v1.26.0`,
		"telemetry/host.go":          `go.opentelemetry.io/otel/sdk/trace`,
		"sdk_test.go":                `go.opentelemetry.io/otel/sdk/trace`,
	}
	for name, imp := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		src := "package fake\n\nimport _ \"" + imp + "\"\n"
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := checkDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var have []string
	for _, v := range got {
		have = append(have, filepath.Base(v.File)+" "+v.Import)
	}
	sort.Strings(have)
	want := []string{
		"bad_bridgev2.go maunium.net/go/mautrix/bridgev2",
		"bad_connector.go " + module + "/pkg/connector",
		"bad_hostkit.go " + module + "/pkg/hostkit",
		"bad_bridgev2.go maunium.net/go/mautrix/bridgev2",
		"bad_hostkit.go " + module + "/pkg/hostkit",
		"bad_widget.go " + module + "/pkg/widget",
		"bad_widget2.go " + module + "/pkg/widget",
		"bad_contrib.go go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp",
		"bad_diago.go github.com/emiago/diago",
		"bad_diago.go github.com/emiago/diago",
		"bad_lk.go github.com/livekit/server-sdk-go/v2",
		"bad_opus.go gopkg.in/hraban/opus.v2",
		"bad_exporter.go go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp",
		"bad_otellog.go go.opentelemetry.io/otel/log",
		"bad_pion.go github.com/pion/rtp",
		"bad_root.go maunium.net/go/mautrix",
		"bad_rtp.go github.com/pion/rtp",
		"bad_sdk.go go.opentelemetry.io/otel/sdk/trace",
		"bad_semconv.go go.opentelemetry.io/otel/semconv/v1.26.0",
		"bad_stun.go github.com/pion/stun/v3",
		"bad_telemetry.go " + module + "/pkg/telemetry",
	}
	sort.Strings(want)
	if strings.Join(have, "\n") != strings.Join(want, "\n") {
		t.Fatalf("violations = %q, want %q", have, want)
	}
}
