package internal_test

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

// The wallet is a hexagon: the core (domain and application) knows nothing of the outside, and
// every adapter plugs into it through a port. The dependency rule is that imports point inward
// only. This test reads every package's imports and fails the build when one points the other way,
// so the rule is enforced, not remembered.

const (
	module       = "github.com/JackMahfouz/e-wallet/services/wallet"
	processMoney = "github.com/JackMahfouz/e-wallet/libs/process-money"
)

// coreBanned is the standard library the core must not touch: the network, files, the process
// environment and SQL belong to adapters.
var coreBanned = map[string]bool{
	"net": true, "net/http": true, "net/rpc": true, "database/sql": true, "os": true, "os/exec": true,
	"os/signal": true, "io/ioutil": true, "plugin": true, "syscall": true,
}

func isStandard(path string) bool { return !strings.Contains(strings.SplitN(path, "/", 2)[0], ".") }

// violation says what is wrong with pkg (a path relative to the module, such as
// "internal/application") importing path, or "" if the import is fine.
func violation(pkg, path string) string {
	inCore := pkg == "internal/domain" || pkg == "internal/application"
	adapter := strings.HasPrefix(pkg, "internal/adapter/")
	inModule := strings.HasPrefix(path, module+"/")
	internalTarget := strings.TrimPrefix(path, module+"/")

	switch {
	case strings.HasPrefix(internalTarget, "cmd/") && inModule:
		return "nothing may import a command"

	case inModule && internalTarget == "internal/config" && pkg != "cmd/server":
		return "only the composition root reads configuration"

	case inCore:
		switch {
		case isStandard(path):
			if coreBanned[path] {
				return "the core must not use " + path + ": that is an adapter's job"
			}

			return ""
		case path == processMoney:
			return ""
		case pkg == "internal/application" && internalTarget == "internal/domain":
			return ""
		default:
			return "the core may import only the standard library, the shared money package" +
				map[bool]string{true: " and the domain", false: ""}[pkg == "internal/application"]
		}

	case adapter:
		switch {
		case !inModule:
			return "" // standard library, gRPC, pgx, generated clients: all fair game for an adapter
		case internalTarget == "internal/domain" || internalTarget == "internal/application":
			return ""
		case strings.HasPrefix(internalTarget, "internal/adapter/"):
			return "an adapter must not import another adapter; they meet only in the core and the composition root"
		case strings.HasPrefix(internalTarget, "internal/testsupport"):
			return "production code must not import test support"
		case strings.HasPrefix(internalTarget, "gen/"):
			return "" // generated protobuf code
		case internalTarget == "migrations" && pkg == "internal/adapter/out/postgres":
			return "" // the SQL schema belongs to the adapter that speaks SQL, and to no one else
		default:
			return "an adapter may import only the core"
		}

	case pkg == "internal/config":
		if !isStandard(path) {
			return "configuration is plain standard-library code"
		}
	}

	return ""
}

func TestTheRuleEngineCatchesViolations(t *testing.T) {
	// If these stopped being violations the real test below would pass for the wrong reason.
	bad := map[[2]string]string{
		{"internal/domain", "net/http"}:                                         "core must not use net/http",
		{"internal/domain", "github.com/jackc/pgx/v5"}:                          "core importing a driver",
		{"internal/domain", module + "/internal/application"}:                   "domain importing application",
		{"internal/application", "database/sql"}:                                "application using SQL",
		{"internal/application", "google.golang.org/grpc"}:                      "application using gRPC",
		{"internal/application", module + "/internal/adapter/out/postgres"}:     "application importing an adapter",
		{"internal/application", "os"}:                                          "application reading the environment",
		{"internal/adapter/in/http", module + "/internal/adapter/out/postgres"}: "adapter importing another adapter",
		{"internal/adapter/out/postgres", module + "/internal/adapter/in/grpc"}: "adapter importing another adapter",
		{"internal/adapter/out/ledger", module + "/internal/testsupport"}:       "production code importing test support",
		{"internal/adapter/in/http", module + "/internal/config"}:               "adapter reading configuration",
		{"internal/adapter/in/http", module + "/cmd/server"}:                    "importing a command",
		{"internal/config", "github.com/jackc/pgx/v5"}:                          "configuration with dependencies",
	}
	for edge, why := range bad {
		if violation(edge[0], edge[1]) == "" {
			t.Errorf("%s: %s -> %s was not reported", why, edge[0], edge[1])
		}
	}

	good := [][2]string{
		{"internal/domain", "time"},
		{"internal/domain", "errors"},
		{"internal/application", module + "/internal/domain"},
		{"internal/application", processMoney},
		{"internal/application", "context"},
		{"internal/adapter/out/postgres", "github.com/jackc/pgx/v5"},
		{"internal/adapter/out/postgres", module + "/internal/application"},
		{"internal/adapter/out/postgres", module + "/migrations"},
		{"internal/adapter/in/http", "net/http"},
		{"internal/adapter/in/grpc", module + "/gen/wallet/v1"},
		{"cmd/server", module + "/internal/config"},
		{"cmd/server", module + "/internal/adapter/out/postgres"},
	}
	for _, edge := range good {
		if v := violation(edge[0], edge[1]); v != "" {
			t.Errorf("%s -> %s was wrongly reported: %s", edge[0], edge[1], v)
		}
	}
}

// goFiles returns the non-test Go files of every package under the module root, by package path.
func goFiles(t *testing.T) map[string][]string {
	t.Helper()

	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}

	packages := map[string][]string{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		packages[filepath.ToSlash(rel)] = append(packages[filepath.ToSlash(rel)], path)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return packages
}

func TestImportsPointInwardOnly(t *testing.T) {
	packages := goFiles(t)
	if len(packages) < 10 {
		t.Fatalf("found only %d packages: the walk is not finding the code", len(packages))
	}

	names := make([]string, 0, len(packages))
	for pkg := range packages {
		names = append(names, pkg)
	}
	sort.Strings(names)

	checked := 0
	for _, pkg := range names {
		for _, file := range packages[pkg] {
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}

			for _, spec := range parsed.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				checked++

				if why := violation(pkg, path); why != "" {
					t.Errorf("%s imports %s: %s", filepath.Base(file), path, why)
				}
			}
		}
	}

	if checked < 100 {
		t.Errorf("checked only %d imports: the test is not reading the code", checked)
	}
}

// The core takes its time from the Clock port. A call to time.Now would make it untestable and
// would let the database and the application disagree about when something happened.
func TestTheCoreNeverReadsTheClockItself(t *testing.T) {
	for _, pkg := range []string{"internal/domain", "internal/application"} {
		files := goFiles(t)[pkg]
		if len(files) == 0 {
			t.Fatalf("no files found for %s", pkg)
		}

		for _, file := range files {
			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(source), "time.Now(") {
				t.Errorf("%s calls time.Now: use the Clock port", file)
			}
		}
	}
}
