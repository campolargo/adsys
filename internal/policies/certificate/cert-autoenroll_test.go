package certificate_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ubuntu/adsys/internal/testutils"
	"golang.org/x/exp/slices"
)

const advancedConfigurationJSON = `[
  {
    "keyname": "Software\\Policies\\Microsoft\\Cryptography\\PolicyServers\\37c9dc30f207f27f61a2f7c3aed598a6e2920b54",
    "valuename": "AuthFlags",
    "data": 2,
    "type": 4
  },
  {
    "keyname": "Software\\Policies\\Microsoft\\Cryptography\\PolicyServers\\37c9dc30f207f27f61a2f7c3aed598a6e2920b54",
    "valuename": "Cost",
    "data": 2147483645,
    "type": 4
  },
  {
    "keyname": "Software\\Policies\\Microsoft\\Cryptography\\PolicyServers\\37c9dc30f207f27f61a2f7c3aed598a6e2920b54",
    "valuename": "Flags",
    "data": 20,
    "type": 4
  },
  {
    "keyname": "Software\\Policies\\Microsoft\\Cryptography\\PolicyServers\\37c9dc30f207f27f61a2f7c3aed598a6e2920b54",
    "valuename": "FriendlyName",
    "data": "ActiveDirectoryEnrollmentPolicy",
    "type": 1
  },
  {
    "keyname": "Software\\Policies\\Microsoft\\Cryptography\\PolicyServers\\37c9dc30f207f27f61a2f7c3aed598a6e2920b54",
    "valuename": "PolicyID",
    "data": "{A5E9BF57-71C6-443A-B7FC-79EFA6F73EBD}",
    "type": 1
  },
  {
    "keyname": "Software\\Policies\\Microsoft\\Cryptography\\PolicyServers\\37c9dc30f207f27f61a2f7c3aed598a6e2920b54",
    "valuename": "URL",
    "data": "LDAP:",
    "type": 1
  },
  {
    "keyname": "Software\\Policies\\Microsoft\\Cryptography\\PolicyServers",
    "valuename": "Flags",
    "data": 0,
    "type": 4
  }
]`

func TestCertAutoenrollScript(t *testing.T) {
	t.Parallel()

	coverageOn := testutils.PythonCoverageToGoFormat(t, "cert-autoenroll", false)
	certAutoenrollCmd := "./cert-autoenroll"
	if coverageOn {
		certAutoenrollCmd = "cert-autoenroll"
	}

	compactedJSON := &bytes.Buffer{}
	err := json.Compact(compactedJSON, []byte(advancedConfigurationJSON))
	require.NoError(t, err, "Failed to compact JSON")

	// Setup samba mock
	pythonPath, err := filepath.Abs("../../testutils/admock")
	require.NoError(t, err, "Setup: Failed to get current absolute path for mock")

	tests := map[string]struct {
		args []string

		readOnlyPath    bool
		autoenrollError bool

		wantErr bool
	}{
		"Enroll with simple configuration":                   {args: []string{"enroll", "keypress", "example.com"}},
		"Enroll with simple configuration and debug enabled": {args: []string{"enroll", "keypress", "example.com", "--debug"}},
		"Enroll with empty advanced configuration":           {args: []string{"enroll", "keypress", "example.com", "--policy_servers_json", "null"}},
		"Enroll with valid advanced configuration":           {args: []string{"enroll", "keypress", "example.com", "--policy_servers_json", compactedJSON.String()}},

		"Unenroll": {args: []string{"unenroll", "keypress", "example.com"}},

		// Error cases
		"Error on missing arguments": {args: []string{"enroll"}, wantErr: true},
		"Error on invalid flags":     {args: []string{"enroll", "keypress", "example.com", "--invalid_flag"}, wantErr: true},
		"Error on invalid JSON":      {args: []string{"enroll", "keypress", "example.com", "--policy_servers_json", "invalid_json"}, wantErr: true},
		"Error on invalid JSON keys": {
			args: []string{"enroll", "keypress", "example.com", "--policy_servers_json", `[{"key":"Software\\Policies\\Microsoft","value":"MyValue"}]`}, wantErr: true},
		"Error on invalid JSON structure": {
			args: []string{"enroll", "keypress", "example.com", "--policy_servers_json", `{"key":"Software\\Policies\\Microsoft","value":"MyValue"}`}, wantErr: true},
		"Error on read-only path":   {readOnlyPath: true, args: []string{"enroll", "keypress", "example.com"}, wantErr: true},
		"Error on enroll failure":   {autoenrollError: true, args: []string{"enroll", "keypress", "example.com"}, wantErr: true},
		"Error on unenroll failure": {autoenrollError: true, args: []string{"unenroll", "keypress", "example.com"}, wantErr: true},
	}

	for name, tc := range tests {
		tc := tc
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tmpdir := t.TempDir()
			stateDir := filepath.Join(tmpdir, "state")
			privateDir := filepath.Join(tmpdir, "private")
			trustDir := filepath.Join(tmpdir, "trust")
			globalTrustDir := filepath.Join(tmpdir, "ca-certificates")

			if tc.readOnlyPath {
				testutils.MakeReadOnly(t, tmpdir)
			}

			args := append(tc.args, "--samba_cache_dir", stateDir, "--private_dir", privateDir, "--trust_dir", trustDir, "--global_trust_dir", globalTrustDir)

			// #nosec G204: we control the command line name and only change it for tests
			cmd := exec.Command(certAutoenrollCmd, args...)
			cmd.Env = append(os.Environ(), "PYTHONPATH="+pythonPath)
			if tc.autoenrollError {
				cmd.Env = append(os.Environ(), "ADSYS_WANT_AUTOENROLL_ERROR=1")
			}
			out, err := cmd.CombinedOutput()
			if tc.wantErr {
				require.Error(t, err, "cert-autoenroll should have failed but didn’t")
				return
			}
			require.NoErrorf(t, err, "cert-autoenroll should have exited successfully: %s", string(out))

			got := strings.ReplaceAll(string(out), tmpdir, "#TMPDIR#")
			want := testutils.LoadWithUpdateFromGolden(t, got)
			require.Equal(t, want, got, "Unexpected output from cert-autoenroll script")

			if slices.Contains(tc.args, "unenroll") {
				require.NoDirExists(t, filepath.Join(stateDir, "samba"), "Samba cache directory should have been removed on unenroll")
			}
		})
	}
}
