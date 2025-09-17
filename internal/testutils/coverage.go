// TiCS: disabled // Test helpers.

package testutils

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/termie/go-shutil"
)

var (
	goMainCoverProfile     string
	goMainCoverProfileOnce sync.Once

	coveragesToMerge   []string
	coveragesToMergeMu sync.Mutex

	generateXMLCoverage bool
)

const (
	goCoverage  = "go"  // Go coverage format
	xmlCoverage = "xml" // XML (Cobertura) coverage format
)

type coverageOptions struct {
	coverageFormat string
}

// CoverageOption represents an optional function that can be used to override
// some of the coverage default values.
type CoverageOption func(*coverageOptions)

// WithCoverageFormat overrides the default coverage format, impacting the
// filename of the coverage file.
func WithCoverageFormat(coverageFormat string) CoverageOption {
	return func(o *coverageOptions) {
		if coverageFormat != "" {
			o.coverageFormat = coverageFormat
		}
	}
}

func init() {
	// XML coverage generation is a best effort, so we don't fail if the required tools are not found.
	if commandExists("reportgenerator") && commandExists("gocov") && commandExists("gocov-xml") {
		generateXMLCoverage = true
	}
}

// TrackTestCoverage starts tracking coverage in a dedicated file based on current test name.
// This file will be merged to the current coverage main file.
// It’s up to the test use the returned path to file golang-compatible cover format content.
// To collect all coverages, then MergeCoverages() should be called after m.Run().
// If coverage is not enabled, nothing is done.
func TrackTestCoverage(t *testing.T, opts ...CoverageOption) (testCoverFile string) {
	t.Helper()

	args := coverageOptions{
		coverageFormat: goCoverage,
	}
	for _, o := range opts {
		o(&args)
	}

	goMainCoverProfileOnce.Do(func() {
		for _, arg := range os.Args {
			if !strings.HasPrefix(arg, "-test.coverprofile=") {
				continue
			}
			goMainCoverProfile = strings.TrimPrefix(arg, "-test.coverprofile=")
		}
	})

	if goMainCoverProfile == "" {
		return ""
	}

	coverAbsPath, err := filepath.Abs(goMainCoverProfile)
	require.NoError(t, err, "Setup: can't transform go cover profile to absolute path")

	testCoverFile = fmt.Sprintf("%s.%s.%s",
		coverAbsPath,
		strings.ReplaceAll(strings.ReplaceAll(t.Name(), "/", "_"), "\\", "_"),
		args.coverageFormat,
	)
	coveragesToMergeMu.Lock()
	defer coveragesToMergeMu.Unlock()
	if slices.Contains(coveragesToMerge, testCoverFile) {
		t.Fatalf("Trying to adding a second time %q to the list of file to cover. This will create some overwrite and thus, should be only called once", testCoverFile)
	}
	coveragesToMerge = append(coveragesToMerge, testCoverFile)

	return testCoverFile
}

// MergeCoverages append all coverage files marked for merging to main Go Cover Profile.
// This has to be called after m.Run() in TestMain so that the main go cover profile is created.
// This has no action if profiling is not enabled.
func MergeCoverages() {
	coveragesToMergeMu.Lock()
	defer coveragesToMergeMu.Unlock()

	projectRoot, err := projectRoot(".")
	if err != nil {
		log.Fatalf("Teardown: can't find project root: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(projectRoot, "coverage"), 0700); err != nil {
		log.Fatalf("Teardown: can’t create coverage directory: %v", err)
	}

	// Merge Go coverage files
	for _, cov := range coveragesToMerge {
		// For XML coverage files, we just copy them to a persistent directory
		// for future manipulation.
		if strings.HasSuffix(cov, "."+xmlCoverage) {
			if err := shutil.CopyFile(cov, filepath.Join(projectRoot, "coverage", filepath.Base(cov)), false); err != nil {
				log.Fatalf("Teardown: can’t copy coverage file to project root: %v", err)
			}
			continue
		}

		if err := appendToFile(cov, goMainCoverProfile); err != nil {
			log.Fatalf("Teardown: can’t inject coverage into the golang one: %v", err)
		}
	}
	coveragesToMerge = nil
}

// WantCoverage returns true if coverage was requested in test.
func WantCoverage() bool {
	for _, arg := range os.Args {
		if !strings.HasPrefix(arg, "-test.coverprofile=") {
			continue
		}
		return true
	}
	return false
}

// appendToFile appends src to the dst coverprofile file at the end.
func appendToFile(src, dst string) error {
	f, err := os.Open(filepath.Clean(src))
	if err != nil {
		return fmt.Errorf("can't open coverage file: %w", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			log.Fatalf("can't close %v", err)
		}
	}()

	d, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("can't open golang cover profile file: %w", err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			log.Fatalf("can't close %v", err)
		}
	}()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "mode: ") {
			continue
		}
		if _, err := d.Write([]byte(scanner.Text() + "\n")); err != nil {
			return fmt.Errorf("can't write to golang cover profile file: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error while scanning golang cover profile file: %w", err)
	}
	return nil
}

// fqdnToPath allows to return the fqdn path for this file relative to go.mod.
func fqdnToPath(t *testing.T, path string) string {
	t.Helper()

	absPath, err := filepath.Abs(path)
	require.NoError(t, err, "Setup: can't transform path to absolute path")

	projectRoot, err := projectRoot(path)
	require.NoError(t, err, "Setup: can't find project root")

	f, err := os.Open(filepath.Join(projectRoot, "go.mod"))
	require.NoError(t, err, "Setup: can't open go.mod")

	r := bufio.NewReader(f)
	l, err := r.ReadString('\n')
	require.NoError(t, err, "can't read go.mod first line")
	if !strings.HasPrefix(l, "module ") {
		t.Fatal(`Setup: failed to find "module" line in go.mod`)
	}

	prefix := strings.TrimSpace(strings.TrimPrefix(l, "module "))
	relpath := strings.TrimPrefix(absPath, projectRoot)
	return filepath.Join(prefix, relpath)
}

// projectRoot returns the root of the project by looking for a go.mod file.
func projectRoot(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("can't calculate absolute path: %w", err)
	}

	for absPath != "/" {
		_, err := os.Stat(filepath.Clean(filepath.Join(absPath, "go.mod")))
		if err != nil {
			absPath = filepath.Dir(absPath)
			continue
		}

		return absPath, nil
	}
	return "", fmt.Errorf("failed to find go.mod")
}

// writeGoCoverageLine writes given line in go coverage format to w.
func writeGoCoverageLine(t *testing.T, w io.Writer, file string, lineNum, lineLength int, covered string) {
	t.Helper()

	_, err := fmt.Fprintf(w, "%s:%d.1,%d.%d 1 %s\n", file, lineNum, lineNum, lineLength, covered)
	require.NoErrorf(t, err, "Teardown: can't write a write to golang compatible cover file : %v", err)
}

// commandExists returns true if the command exists in the PATH.
func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}
